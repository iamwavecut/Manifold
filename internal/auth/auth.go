package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

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

type Manager struct {
	store *store.Store
}

func NewManager(s *store.Store) *Manager {
	return &Manager{store: s}
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
	keys, err := m.store.FindAPIKeys(ctx)
	if err != nil {
		return Principal{}, err
	}
	for _, key := range keys {
		if VerifySecret(secret, key.SecretHash) {
			return Principal{
				KeyID:        key.ID,
				Capabilities: capabilitySet(key.Capabilities),
			}, nil
		}
	}
	return Principal{}, store.ErrNotFound
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
