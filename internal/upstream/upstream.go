package upstream

import (
	"context"
	"fmt"

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
	EntityIDs  []string
	FactIDs    []string
	EdgeIDs    []string
	Raw        map[string]any
}

type DependencyError struct {
	Dependency string
	Operation  string
	Status     int
	Retryable  bool
	Cause      error
}

func (e *DependencyError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s %s failed with HTTP %d", e.Dependency, e.Operation, e.Status)
	}
	return fmt.Sprintf("%s %s failed: %v", e.Dependency, e.Operation, e.Cause)
}

func (e *DependencyError) Unwrap() error {
	return e.Cause
}
