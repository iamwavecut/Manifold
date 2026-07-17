package model

import (
	"encoding/json"
	"time"

	"github.com/iamwavecut/Manifold/internal/problem"
)

type Folder struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	ParentID  string         `json:"parent_id,omitempty"`
	Path      string         `json:"path"`
	Summary   string         `json:"summary,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	ETag      string         `json:"etag"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type Document struct {
	ID          string         `json:"id"`
	FolderID    string         `json:"folder_id,omitempty"`
	Title       string         `json:"title"`
	Format      string         `json:"format"`
	Content     string         `json:"content,omitempty"`
	ContentHash string         `json:"content_hash"`
	Revision    string         `json:"revision"`
	Status      JobStatus      `json:"status"`
	OVURI       string         `json:"-"`
	BrainID     string         `json:"-"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	ETag        string         `json:"etag"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

type Revision struct {
	DocumentID  string    `json:"document_id"`
	Revision    string    `json:"revision"`
	Content     string    `json:"content"`
	ContentHash string    `json:"content_hash"`
	OVURI       string    `json:"-"`
	SnapshotOID string    `json:"snapshot_oid,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type JobStatus string

const (
	JobAccepted       JobStatus = "accepted"
	JobIndexing       JobStatus = "indexing"
	JobExtracting     JobStatus = "extracting"
	JobReady          JobStatus = "ready"
	JobPartiallyReady JobStatus = "partially_ready"
	JobFailed         JobStatus = "failed"
)

type Job struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Status       JobStatus       `json:"status"`
	Attempts     int             `json:"attempts"`
	Error        *problem.Error  `json:"error,omitempty"`
	Payload      json.RawMessage `json:"-"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
}

type Entity struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	UpstreamID string         `json:"-"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

type Fact struct {
	ID               string     `json:"id"`
	EntityID         string     `json:"entity_id"`
	Predicate        string     `json:"predicate"`
	Object           string     `json:"object"`
	Origin           string     `json:"origin"`
	Status           string     `json:"status"`
	Confidence       float64    `json:"confidence"`
	SourceDocumentID string     `json:"source_document_id,omitempty"`
	SourceRevision   string     `json:"source_revision,omitempty"`
	UpstreamID       string     `json:"-"`
	ValidFrom        time.Time  `json:"valid_from"`
	ValidUntil       *time.Time `json:"valid_until,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

type Relation struct {
	ID         string    `json:"id"`
	FromID     string    `json:"from_entity_id"`
	ToID       string    `json:"to_entity_id"`
	Predicate  string    `json:"predicate"`
	Origin     string    `json:"origin"`
	Status     string    `json:"status"`
	Confidence float64   `json:"confidence"`
	UpstreamID string    `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
}

type Conflict struct {
	ID         string    `json:"id"`
	FactAID    string    `json:"fact_a_id"`
	FactBID    string    `json:"fact_b_id"`
	Status     string    `json:"status"`
	Resolution string    `json:"resolution,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Source struct {
	ID         string    `json:"id"`
	DocumentID string    `json:"document_id"`
	Revision   string    `json:"revision"`
	Heading    string    `json:"heading,omitempty"`
	Excerpt    string    `json:"excerpt,omitempty"`
	Method     string    `json:"method"`
	Confidence float64   `json:"confidence"`
	IngestedAt time.Time `json:"ingested_at"`
}

type SearchMode string

const (
	SearchHybrid   SearchMode = "hybrid"
	SearchSemantic SearchMode = "semantic"
	SearchLexical  SearchMode = "lexical"
	SearchGraph    SearchMode = "graph"
)

type SearchHit struct {
	Kind       string   `json:"kind"`
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Snippet    string   `json:"snippet,omitempty"`
	Score      float64  `json:"score"`
	Source     string   `json:"source"`
	Revision   string   `json:"revision,omitempty"`
	Provenance []Source `json:"provenance,omitempty"`
}

type ContextPack struct {
	Query           string      `json:"query"`
	Items           []SearchHit `json:"items"`
	Text            string      `json:"text"`
	EstimatedTokens int         `json:"estimated_tokens"`
	TokenBudget     int         `json:"token_budget"`
	Truncated       bool        `json:"truncated"`
}

type RenameOperation struct {
	ResourceType string `json:"resource_type" enum:"folder,document,entity,api_key"`
	From         string `json:"from" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" maxLength:"63"`
	To           string `json:"to" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" maxLength:"63"`
}

type TextReplacement struct {
	DocumentID  string `json:"document_id"`
	Revision    string `json:"revision"`
	Occurrences int    `json:"occurrences"`
}

type RenamePreview struct {
	Operations           []RenameOperation `json:"operations"`
	Replacements         []TextReplacement `json:"replacements"`
	StructuredReferences int               `json:"structured_references"`
	Conflicts            []problem.Blocker `json:"conflicts,omitempty"`
	Blockers             []problem.Blocker `json:"blockers,omitempty"`
	StateVersion         int64             `json:"state_version"`
}

type RenamePlan struct {
	ID        string         `json:"id"`
	Status    string         `json:"status"`
	Preview   RenamePreview  `json:"preview"`
	Error     *problem.Error `json:"error,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	AppliedAt *time.Time     `json:"applied_at,omitempty"`
}

type APIKey struct {
	ID           string    `json:"id"`
	Capabilities []string  `json:"capabilities"`
	CreatedAt    time.Time `json:"created_at"`
}
