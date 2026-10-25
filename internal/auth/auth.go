package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"

	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/store"
)

const (
	ReadDocuments   = "read_documents"
	WriteDocuments  = "write_documents"
	Search          = "search"
	ReadGraph       = "read_graph"
	WriteGraph      = "write_graph"
	ManageConflicts = "manage_conflicts"
	Admin           = "admin"
)

var AllCapabilities = []string{
	ReadDocuments, WriteDocuments, Search, ReadGraph, WriteGraph, ManageConflicts, Admin,
}

type Principal struct {
	KeyID        string
	Capabilities map[string]struct{}
	FromSession  bool
	CSRFHash     string
}

type principalContextKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}

func NewPrincipal(keyID string, capabilities []string, fromSession bool, csrfHash string) Principal {
	return Principal{
		KeyID:        keyID,
		Capabilities: capabilitySet(capabilities),
		FromSession:  fromSession,
		CSRFHash:     csrfHash,
	}
}

func (p Principal) Has(required ...string) bool {
	if _, ok := p.Capabilities[Admin]; ok {
		return true
	}
	for _, capability := range required {
		if _, ok := p.Capabilities[capability]; !ok {
			return false
		}
	}
	return true
}

// maxVerifiedSecrets bounds the verified-secret cache; it is cleared when full.
const maxVerifiedSecrets = 1024

type Manager struct {
	store  *store.Store
	verify func(secret, encoded string) bool

	mu sync.Mutex
	// verified maps the SHA-256 digest of a secret that passed argon2id to
	// the key record it matched. argon2id costs about 100 ms per call, and
	// every authenticated request used to pay it.
	verified map[[sha256.Size]byte]verifiedKey
}

type verifiedKey struct {
	id         string
	secretHash string
}

func NewManager(s *store.Store) *Manager {
	return &Manager{store: s, verify: VerifySecret, verified: map[[sha256.Size]byte]verifiedKey{}}
}

func (m *Manager) Bootstrap(ctx context.Context, secret string) error {
	count, err := m.store.APIKeyCount(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if len(secret) < 16 {
		return fmt.Errorf("MANIFOLD_BOOTSTRAP_API_KEY must contain at least 16 characters on first start")
	}
	hash, err := HashSecret(secret)
	if err != nil {
		return err
	}
	_, err = m.store.CreateAPIKey(ctx, "bootstrap-admin", hash, AllCapabilities)
	return err
}

func (m *Manager) Authenticate(ctx context.Context, secret string) (Principal, error) {
	if secret == "" {
		return Principal{}, store.ErrNotFound
	}
	// Keys are read on every request, so a revoked or replaced key stops
	// authenticating at once; only the argon2id check is skipped for a
	// secret that already matched the same stored hash.
	keys, err := m.store.FindAPIKeys(ctx)
	if err != nil {
		return Principal{}, err
	}
	digest := sha256.Sum256([]byte(secret))
	m.mu.Lock()
	cached, ok := m.verified[digest]
	m.mu.Unlock()
	if ok {
		for _, key := range keys {
			if key.ID == cached.id && key.SecretHash == cached.secretHash {
				return principalFor(key), nil
			}
		}
	}
	for _, key := range keys {
		if m.verify(secret, key.SecretHash) {
			m.mu.Lock()
			if len(m.verified) >= maxVerifiedSecrets {
				clear(m.verified)
			}
			m.verified[digest] = verifiedKey{id: key.ID, secretHash: key.SecretHash}
			m.mu.Unlock()
			return principalFor(key), nil
		}
	}
	return Principal{}, store.ErrNotFound
}

func principalFor(key store.APIKeyRecord) Principal {
	return Principal{
		KeyID:        key.ID,
		Capabilities: capabilitySet(key.Capabilities),
	}
}

func HashSecret(secret string) (string, error) {
	salt, err := identity.Secret(16)
	if err != nil {
		return "", err
	}
	saltBytes, err := base64.RawURLEncoding.DecodeString(salt)
	if err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(secret), saltBytes, 3, 64*1024, 2, 32)
	return fmt.Sprintf("argon2id$v=19$m=65536,t=3,p=2$%s$%s",
		base64.RawURLEncoding.EncodeToString(saltBytes),
		base64.RawURLEncoding.EncodeToString(hash)), nil
}

func VerifySecret(secret, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	expected, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	actual := argon2.IDKey([]byte(secret), salt, 3, 64*1024, 2, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func capabilitySet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
