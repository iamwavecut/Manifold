package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/iamwavecut/Manifold/internal/model"
)

type Brain struct {
	http httpClient
}

func NewBrain(base, key string, client *http.Client) *Brain {
	return &Brain{http: httpClient{
		name: "brain", base: base, key: key, client: client,
		auth: func(req *http.Request, key string) { req.Header.Set("Authorization", "Bearer "+key) },
	}}
}

func (b *Brain) Health(ctx context.Context) error {
	if b.http.base == "" {
		return &DependencyError{Dependency: "brain", Operation: "health", Retryable: true, Cause: fmt.Errorf("BRAIN_URL is not configured")}
	}
	return b.http.do(ctx, http.MethodGet, "/health", nil, nil)
}

func (b *Brain) IngestDocument(ctx context.Context, document GraphDocument) (GraphIngestResult, error) {
	body := map[string]any{
		"kind":         document.Format,
		"text":         document.Content,
		"originUri":    document.OriginURI,
		"title":        document.Title,
		"occurredAt":   document.OccurredAt,
		"contextRef":   map[string]string{"vertical": "manifold", "recorder": "manifold"},
		"storeContent": false,
		"indexers":     "general",
		"mode":         "sync",
		"meta":         map[string]string{"manifold_id": document.ID, "revision": document.Revision},
	}
	var response struct {
		DocumentID string `json:"documentId"`
		Committed  struct {
			EntityIDs []string `json:"entityIds"`
			FactIDs   []string `json:"factIds"`
			EdgeIDs   []string `json:"edgeIds"`
		} `json:"committed"`
	}
	if err := b.http.do(ctx, http.MethodPost, "/v1/ingest/document", body, &response); err != nil {
		return GraphIngestResult{}, err
	}
	return GraphIngestResult{
		DocumentID: response.DocumentID,
		EntityIDs:  response.Committed.EntityIDs,
		FactIDs:    response.Committed.FactIDs,
		EdgeIDs:    response.Committed.EdgeIDs,
	}, nil
}

func (b *Brain) Search(ctx context.Context, query string, limit int, includeHistory bool) ([]model.SearchHit, error) {
	body := map[string]any{
		"query": query, "limit": limit, "searchMode": "hybrid",
		"requireProvenance": true, "includeContested": true,
		"includeStale": includeHistory,
	}
	var response struct {
		Results []struct {
			EntityID      string  `json:"entityId"`
			EntityType    string  `json:"entityType"`
			CanonicalName string  `json:"canonicalName"`
			Score         float64 `json:"score"`
			Facts         []struct {
				FactID    string  `json:"factId"`
				Predicate string  `json:"predicate"`
				Object    string  `json:"object"`
				Score     float64 `json:"score"`
				SourceKey string  `json:"sourceKey"`
			} `json:"facts"`
		} `json:"results"`
	}
	if err := b.http.do(ctx, http.MethodPost, "/v1/search", body, &response); err != nil {
		return nil, err
	}
	var hits []model.SearchHit
	for _, entity := range response.Results {
		for _, fact := range entity.Facts {
			hits = append(hits, model.SearchHit{
				Kind: "fact", ID: fact.FactID, Title: entity.CanonicalName,
				Snippet: fmt.Sprintf("%s %s", fact.Predicate, fact.Object),
				Score:   max(entity.Score, fact.Score), Source: "brain",
				UpstreamSourceRef: fact.SourceKey,
			})
		}
	}
	return hits, nil
}

func (b *Brain) IngestFact(ctx context.Context, fact model.Fact, entity model.Entity) (string, string, error) {
	body := map[string]any{
		"entityRef":  map[string]string{"vertical": entity.Kind, "id": entity.ID},
		"predicate":  fact.Predicate,
		"object":     fact.Object,
		"validFrom":  fact.ValidFrom.UTC().Format(time.RFC3339),
		"confidence": fact.Confidence,
		"source": map[string]any{
			"vertical": "manifold", "recorder": "manifold",
			"evidence": []map[string]string{{"kind": "document", "ref": fact.SourceDocumentID}},
		},
		"metadata": map[string]string{"manifold_fact_id": fact.ID},
		"explain":  true,
	}
	if fact.ValidUntil != nil {
		body["validUntil"] = fact.ValidUntil.UTC().Format(time.RFC3339)
	}
	var response struct {
		FactID  string `json:"factId"`
		Outcome string `json:"outcome"`
	}
	err := b.http.do(ctx, http.MethodPost, "/v1/ingest/fact", body, &response)
	return response.FactID, response.Outcome, err
}

func (b *Brain) IngestRelation(ctx context.Context, relation model.Relation, from, to model.Entity) (string, error) {
	body := map[string]any{
		"from":   map[string]string{"vertical": from.Kind, "id": from.ID},
		"to":     map[string]string{"vertical": to.Kind, "id": to.ID},
		"kind":   relation.Predicate,
		"weight": relation.Confidence,
		"source": map[string]string{"vertical": "manifold"},
	}
	var response map[string]any
	err := b.http.do(ctx, http.MethodPost, "/v1/ingest/link", body, &response)
	return stringValue(response, "edgeId"), err
}

func (b *Brain) RetractFact(ctx context.Context, upstreamID, reason string) error {
	path := "/v1/facts/" + url.PathEscape(upstreamID) + "/retract"
	return b.http.do(ctx, http.MethodPost, path, map[string]string{"reason": reason}, nil)
}

func (b *Brain) GetEntity(ctx context.Context, upstreamID string) (map[string]any, error) {
	var response map[string]any
	err := b.http.do(ctx, http.MethodGet, "/v1/entities/"+url.PathEscape(upstreamID), nil, &response)
	return response, err
}

func stringValue(raw map[string]any, key string) string {
	value, _ := raw[key].(string)
	return value
}
