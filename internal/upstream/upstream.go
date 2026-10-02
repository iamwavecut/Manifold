package upstream

import (
	"context"
	"fmt"
	"time"

	"github.com/iamwavecut/Manifold/internal/model"
)

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

type GraphStore interface {
	Health(context.Context) error
	IngestDocument(context.Context, GraphDocument) (GraphIngestResult, error)
	Search(context.Context, string, int, bool) ([]model.SearchHit, error)
	IngestFact(context.Context, model.Fact, model.Entity) (string, string, error)
	IngestRelation(context.Context, model.Relation, model.Entity, model.Entity) (string, error)
	RetractFact(context.Context, string, string) error
	GetEntity(context.Context, string) (map[string]any, error)
}

// AsyncGraphStore extends GraphStore for providers that stage document
// extraction and expose a later result read. The returned document ID is
// the durable checkpoint key; it is not a queue operation ID.
type AsyncGraphStore interface {
	GraphStore
	SubmitDocument(context.Context, GraphDocument) (string, error)
	DocumentResult(context.Context, string) (GraphDocumentState, error)
	RetryDocument(context.Context, string, string) error
}

type GraphDocumentStatus string

const (
	GraphDocumentPending GraphDocumentStatus = "pending"
	GraphDocumentReady   GraphDocumentStatus = "ready"
	GraphDocumentPartial GraphDocumentStatus = "partial"
	GraphDocumentFailed  GraphDocumentStatus = "failed"
)

type GraphIndexerRun struct {
	IndexerID string
	Status    string
	Reason    string
}

type GraphDocumentState struct {
	Status      GraphDocumentStatus
	BrainStatus string
	Runs        []GraphIndexerRun
	Result      GraphIngestResult
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
	Raw        map[string]any
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
	Cause      error
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
