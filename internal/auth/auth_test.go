package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iamwavecut/Manifold/internal/store"
)

func TestArgon2IDHashRoundTrip(t *testing.T) {
	t.Parallel()

	const secret = "a-long-cryptographically-random-test-key"
	hash, err := HashSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if hash == secret {
		t.Fatal("API key was stored in plaintext")
	}
	if !VerifySecret(secret, hash) {
		t.Fatal("correct secret did not verify")
	}
	if VerifySecret(secret+"-wrong", hash) {
		t.Fatal("incorrect secret verified")
	}
}

func TestCapabilitiesAreNotImplicitlyExpanded(t *testing.T) {
	t.Parallel()

	reader := NewPrincipal("reader", []string{ReadDocuments, Search}, false, "")
	if !reader.Has(ReadDocuments, Search) {
		t.Fatal("explicit capabilities were not recognized")
	}
	if reader.Has(WriteDocuments) {
		t.Fatal("read-only key unexpectedly gained write_documents")
	}
	admin := NewPrincipal("admin", []string{Admin}, false, "")
	if !admin.Has(WriteDocuments, WriteGraph, ManageConflicts) {
		t.Fatal("admin capability should authorize all operations")
	}
}

func TestAuthenticateRunsArgon2OncePerSecret(t *testing.T) {
	t.Parallel()

	db := openAuthStore(t)
	const secret = "a-long-cryptographically-random-reader-key"
	createKey(t, db, "reader", secret, []string{ReadDocuments})
	manager := NewManager(db)
	verifications := 0
	manager.verify = func(secret, encoded string) bool {
		verifications++
		return VerifySecret(secret, encoded)
	}

	for range 3 {
		principal, err := manager.Authenticate(t.Context(), secret)
		if err != nil {
			t.Fatal(err)
		}
		if principal.KeyID != "reader" || !principal.Has(ReadDocuments) || principal.Has(WriteDocuments) {
			t.Fatalf("principal = %+v", principal)
		}
	}
	if verifications != 1 {
		t.Fatalf("argon2id ran %d times for one secret, want 1", verifications)
	}
	for range 2 {
		if _, err := manager.Authenticate(t.Context(), secret+"-wrong"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("wrong secret: err = %v, want ErrNotFound", err)
		}
	}
	if verifications != 3 {
		t.Fatalf("a rejected secret must be verified every time: argon2id ran %d times, want 3", verifications)
	}
}

func TestAuthenticateBoundsConcurrentArgon2Checks(t *testing.T) {
	t.Parallel()

	db := openAuthStore(t)
	createKey(t, db, "reader", "a-long-cryptographically-random-reader-key", []string{ReadDocuments})
	manager := NewManager(db)
	var active, peak atomic.Int32
	manager.verify = func(string, string) bool {
		now := active.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return false
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if _, err := manager.Authenticate(t.Context(), fmt.Sprintf("wrong-secret-%d", i)); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("wrong secret: err = %v, want ErrNotFound", err)
			}
		})
	}
	wg.Wait()
	// Each check allocates 64 MiB; unbounded checks of random bearer tokens
	// could exhaust the container's memory.
	if got := peak.Load(); got > maxConcurrentVerifications {
		t.Fatalf("%d argon2id checks ran at once, want at most %d", got, maxConcurrentVerifications)
	}
}

func TestAuthenticateStopsWaitingForACheckWhenTheRequestEnds(t *testing.T) {
	t.Parallel()

	db := openAuthStore(t)
	createKey(t, db, "reader", "a-long-cryptographically-random-reader-key", []string{ReadDocuments})
	manager := NewManager(db)
	started, release := make(chan struct{}, maxConcurrentVerifications), make(chan struct{})
	manager.verify = func(string, string) bool {
		started <- struct{}{}
		<-release
		return false
	}
	var wg sync.WaitGroup
	for range maxConcurrentVerifications {
		wg.Go(func() { _, _ = manager.Authenticate(t.Context(), "wrong-secret-holding-a-slot") })
	}
	for range maxConcurrentVerifications {
		<-started
	}

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := manager.Authenticate(ctx, "wrong-secret-waiting")
		result <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiting check: err = %v, want context.Canceled", err)
		}
		close(release)
	case <-time.After(time.Second):
		t.Error("a request that ended still waited for a free argon2id slot")
		close(release)
		<-result
	}
	wg.Wait()
}

func TestAuthenticateRejectsRevokedAndReplacedKeys(t *testing.T) {
	t.Parallel()

	db := openAuthStore(t)
	const secret = "a-long-cryptographically-random-first-key"
	createKey(t, db, "agent", secret, []string{Search})
	manager := NewManager(db)
	if _, err := manager.Authenticate(t.Context(), secret); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteAPIKey(t.Context(), "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Authenticate(t.Context(), secret); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked key: err = %v, want ErrNotFound", err)
	}

	const replacement = "a-long-cryptographically-random-second-key"
	createKey(t, db, "agent", replacement, []string{Search})
	if _, err := manager.Authenticate(t.Context(), secret); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("secret of the replaced key: err = %v, want ErrNotFound", err)
	}
	principal, err := manager.Authenticate(t.Context(), replacement)
	if err != nil || principal.KeyID != "agent" {
		t.Fatalf("replacement key: principal = %+v, err = %v", principal, err)
	}
}

func openAuthStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createKey(t *testing.T, db *store.Store, id, secret string, capabilities []string) {
	t.Helper()
	hash, err := HashSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAPIKey(t.Context(), id, hash, capabilities); err != nil {
		t.Fatal(err)
	}
}
