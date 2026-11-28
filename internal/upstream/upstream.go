// Package upstream defines the stores Manifold derives from its canonical
// database: the document index and the knowledge graph.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iamwavecut/Manifold/internal/model"
)

// DocumentStore keeps a path tree of canonical documents and searches their
// text by meaning. Locators are opaque to callers; folder locators end with
// a slash.
type DocumentStore interface {
	Health(context.Context) error
	Exists(context.Context, string) (bool, error)
	Mkdir(context.Context, string, string) error
	Write(context.Context, string, string, bool) error
	Move(context.Context, string, string) error
	Delete(context.Context, string, bool) error
	Snapshot(context.Context, string, []string) (string, error)
	Search(context.Context, string, string, int) ([]model.SearchHit, error)
}

// GraphStore extracts and keeps entities, facts and relations.
type GraphStore interface {
	Health(context.Context) error
	IngestDocument(context.Context, GraphDocument) (GraphIngestResult, error)
	Search(context.Context, string, int, bool) ([]model.SearchHit, error)
	IngestFact(context.Context, model.Fact, model.Entity) (string, string, error)
	IngestRelation(context.Context, model.Relation, model.Entity, model.Entity) (string, error)
	// RetractFact retracts a fact or relation and returns the IDs of the facts
	// that became current again because the retracted fact had superseded
	// them.
	RetractFact(context.Context, string, string) ([]string, error)
}

type GraphDocument struct {
	ID         string
	Title      string
	Format     string
	Content    string
	OriginURI  string
	Revision   string
	OccurredAt string
}

// GraphIngestResult lists what one document revision states. The graph IDs
// of its records keep their historical field names; they are opaque.
type GraphIngestResult struct {
	DocumentID string
	OriginURI  string
	Revision   string
	EntityIDs  []string
	FactIDs    []string
	EdgeIDs    []string
	Entities   []GraphEntity
	Facts      []GraphFact
	Relations  []GraphRelation
}

type GraphEntity struct {
	BrainID string
	Name    string
	Type    string
}

type GraphFact struct {
	BrainID          string
	EntityBrainID    string
	Predicate        string
	Object           string
	Status           string
	Confidence       float64
	ValidFrom        time.Time
	ValidUntil       *time.Time
	SourceDocumentID string
}

type GraphRelation struct {
	BrainID           string
	FromEntityBrainID string
	ToEntityBrainID   string
	Predicate         string
	Status            string
	Confidence        float64
	SourceDocumentID  string
}

type DependencyError struct {
	Dependency string
	Operation  string
	Status     int
	Retryable  bool
	RetryAfter time.Duration
	// Refusal is why the model provider refused the call: "billing", "auth",
	// "rate_limit", "outage", or "" when it did not refuse.
	Refusal string
	Cause   error
}

func (e *DependencyError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s %s failed with HTTP %d", e.Dependency, e.Operation, e.Status)
	}
	return fmt.Sprintf("%s %s failed with a request error", e.Dependency, e.Operation)
}

func (e *DependencyError) Unwrap() error {
	return e.Cause
}

// ProviderRefusal reports why the model provider refused the call behind
// err: "billing", "auth", "rate_limit", "outage", or "" when it did not.
func ProviderRefusal(err error) string {
	var dependency *DependencyError
	if errors.As(err, &dependency) {
		return dependency.Refusal
	}
	return ""
}
