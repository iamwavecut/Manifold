package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iamwavecut/Manifold/internal/auth"
	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/problem"
)

func (a *API) registerGraph() {
	huma.Register(a.huma, huma.Operation{
		OperationID: "create-entity", Method: http.MethodPost, Path: "/api/v1/entities",
		Summary: "Create an explicit entity", Tags: []string{"Graph"}, DefaultStatus: http.StatusCreated,
		Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		Body struct {
			ID       string         `json:"id,omitempty" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" maxLength:"63"`
			Name     string         `json:"name" minLength:"1" maxLength:"240"`
			Kind     string         `json:"kind" minLength:"1" maxLength:"80"`
			Metadata map[string]any `json:"metadata,omitempty"`
		}
	}) (*accepted[model.Entity], error) {
		if _, err := a.require(ctx, auth.WriteGraph); err != nil {
			return nil, err
		}
		slug := input.Body.ID
		if slug == "" {
			slug = identity.Slug(input.Body.Name)
		}
		available, err := a.service.Store().SlugAvailable(ctx, "entity", slug)
		if err != nil {
			return nil, err
		}
		if !available {
			return nil, a.slugTaken(ctx, "entity", slug)
		}
		entity, err := a.service.Store().CreateEntity(ctx, model.Entity{
			ID: slug, Name: input.Body.Name, Kind: input.Body.Kind, Metadata: input.Body.Metadata,
		})
		if err != nil {
			return nil, a.operationError(ctx, "entities", slug, err)
		}
		return &accepted[model.Entity]{Location: "/api/v1/entities/" + entity.ID, Body: entity}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "list-entities", Method: http.MethodGet, Path: "/api/v1/entities",
		Summary: "Find entities", Tags: []string{"Graph"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		Query  string `query:"query" maxLength:"240"`
		Cursor string `query:"cursor" pattern:"^c[0-9a-z]+$"`
		Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
	}) (*body[page[model.Entity]], error) {
		if _, err := a.require(ctx, auth.ReadGraph); err != nil {
			return nil, err
		}
		entities, err := a.service.Store().ListEntities(ctx, input.Query, 10_000)
		if err != nil {
			return nil, err
		}
		return &body[page[model.Entity]]{Body: paginate(entities, input.Cursor, input.Limit)}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-entity", Method: http.MethodGet, Path: "/api/v1/entities/{id}",
		Summary: "Read an entity and its active facts", Tags: []string{"Graph"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		ID string `path:"id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
	}) (*body[map[string]any], error) {
		if _, err := a.require(ctx, auth.ReadGraph); err != nil {
			return nil, err
		}
		entity, err := a.service.Store().GetEntity(ctx, input.ID)
		if err != nil {
			return nil, a.storeError(ctx, "entities", input.ID, err)
		}
		facts, err := a.service.Store().ListFacts(ctx, input.ID, 500)
		if err != nil {
			return nil, err
		}
		relations, err := a.service.Store().ListRelations(ctx, input.ID, 500)
		if err != nil {
			return nil, err
		}
		return &body[map[string]any]{Body: map[string]any{
			"entity": entity, "facts": facts, "relations": relations,
		}}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "create-fact", Method: http.MethodPost, Path: "/api/v1/facts",
		Summary: "Create an authoritative fact", Tags: []string{"Graph"}, DefaultStatus: http.StatusCreated,
		Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		Body struct {
			EntityID         string   `json:"entity_id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
			Predicate        string   `json:"predicate" minLength:"1" maxLength:"256"`
			Object           string   `json:"object" minLength:"1" maxLength:"2000"`
			Confidence       *float64 `json:"confidence,omitempty" minimum:"0" maximum:"1"`
			SourceDocumentID string   `json:"source_document_id,omitempty" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" maxLength:"63"`
			SourceRevision   string   `json:"source_revision,omitempty" pattern:"^r[1-9][0-9]*$"`
			ValidFrom        string   `json:"valid_from,omitempty" format:"date-time"`
			ValidUntil       string   `json:"valid_until,omitempty" format:"date-time"`
		}
	}) (*accepted[map[string]any], error) {
		if _, err := a.require(ctx, auth.WriteGraph); err != nil {
			return nil, err
		}
		if _, err := a.service.Store().GetEntity(ctx, input.Body.EntityID); err != nil {
			return nil, a.storeError(ctx, "entities", input.Body.EntityID, err)
		}
		sourceRevision := input.Body.SourceRevision
		if input.Body.SourceDocumentID == "" && sourceRevision != "" {
			return nil, problem.Validation(ctx, a.config.PublicURL, problem.Violation{
				Pointer: "/body/source_document_id", Code: "required_with_source_revision",
				Message:  "source_document_id is required when source_revision is supplied.",
				Expected: "an active document semantic slug", Received: "",
			})
		}
		if input.Body.SourceDocumentID != "" {
			document, err := a.service.Store().GetDocument(ctx, input.Body.SourceDocumentID, true)
			if err != nil {
				return nil, a.storeError(ctx, "documents", input.Body.SourceDocumentID, err)
			}
			if sourceRevision == "" {
				sourceRevision = document.Revision
			} else {
				number, _ := strconv.Atoi(strings.TrimPrefix(sourceRevision, "r"))
				if _, err := a.service.Store().GetRevision(ctx, input.Body.SourceDocumentID, number); err != nil {
					return nil, a.storeError(
						ctx, "document revisions", input.Body.SourceDocumentID+"/"+sourceRevision, err,
					)
				}
			}
		}
		validFrom := time.Now().UTC()
		if input.Body.ValidFrom != "" {
			validFrom, _ = time.Parse(time.RFC3339, input.Body.ValidFrom)
		}
		var validUntil *time.Time
		if input.Body.ValidUntil != "" {
			parsed, _ := time.Parse(time.RFC3339, input.Body.ValidUntil)
			validUntil = &parsed
		}
		confidence := 1.0
		if input.Body.Confidence != nil {
			confidence = *input.Body.Confidence
		}
		fact, err := a.service.CreateFact(ctx, model.Fact{
			ID: identity.NewXID(), EntityID: input.Body.EntityID, Predicate: input.Body.Predicate,
			Object: input.Body.Object, Origin: "explicit", Status: "active", Confidence: confidence,
			SourceDocumentID: input.Body.SourceDocumentID, SourceRevision: sourceRevision,
			ValidFrom: validFrom, ValidUntil: validUntil,
		})
		if err != nil {
			return nil, a.operationError(ctx, "facts", input.Body.EntityID, err)
		}
		syncStatus := "ready"
		if fact.UpstreamID == "" {
			syncStatus = "partially_ready"
		}
		return &accepted[map[string]any]{
			Location: "/api/v1/facts/" + fact.ID,
			Body:     map[string]any{"fact": fact, "sync_status": syncStatus},
		}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "list-facts", Method: http.MethodGet, Path: "/api/v1/facts",
		Summary: "List facts", Tags: []string{"Graph"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		EntityID string `query:"entity_id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
		Cursor   string `query:"cursor" pattern:"^c[0-9a-z]+$"`
		Limit    int    `query:"limit" minimum:"1" maximum:"500" default:"100"`
	}) (*body[page[model.Fact]], error) {
		if _, err := a.require(ctx, auth.ReadGraph); err != nil {
			return nil, err
		}
		facts, err := a.service.Store().ListFacts(ctx, input.EntityID, 10_000)
		if err != nil {
			return nil, err
		}
		return &body[page[model.Fact]]{Body: paginate(facts, input.Cursor, input.Limit)}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "retract-fact", Method: http.MethodDelete, Path: "/api/v1/facts/{id}",
		Summary: "Retract a fact while preserving history", Tags: []string{"Graph"},
		DefaultStatus: http.StatusNoContent, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		ID     string `path:"id" pattern:"^[0-9a-v]{20}$"`
		Reason string `query:"reason" minLength:"3" maxLength:"500"`
	}) (*struct{}, error) {
		if _, err := a.require(ctx, auth.WriteGraph); err != nil {
			return nil, err
		}
		if err := a.service.RetractFact(ctx, input.ID, input.Reason); err != nil {
			return nil, a.operationError(ctx, "facts", input.ID, err)
		}
		return nil, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "create-relation", Method: http.MethodPost, Path: "/api/v1/relations",
		Summary: "Create an explicit relation", Tags: []string{"Graph"}, DefaultStatus: http.StatusCreated,
		Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		Body struct {
			FromID     string   `json:"from_entity_id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
			ToID       string   `json:"to_entity_id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
			Predicate  string   `json:"predicate" minLength:"1" maxLength:"256"`
			Confidence *float64 `json:"confidence,omitempty" minimum:"0" maximum:"1"`
		}
	}) (*accepted[map[string]any], error) {
		if _, err := a.require(ctx, auth.WriteGraph); err != nil {
			return nil, err
		}
		for _, entityID := range []string{input.Body.FromID, input.Body.ToID} {
			if _, err := a.service.Store().GetEntity(ctx, entityID); err != nil {
				return nil, a.storeError(ctx, "entities", entityID, err)
			}
		}
		confidence := 1.0
		if input.Body.Confidence != nil {
			confidence = *input.Body.Confidence
		}
		relation, err := a.service.CreateRelation(ctx, model.Relation{
			ID: identity.NewXID(), FromID: input.Body.FromID, ToID: input.Body.ToID,
			Predicate: input.Body.Predicate, Origin: "explicit", Status: "active", Confidence: confidence,
		})
		if err != nil {
			return nil, a.operationError(ctx, "relations", "", err)
		}
		syncStatus := "ready"
		if relation.UpstreamID == "" {
			syncStatus = "partially_ready"
		}
		return &accepted[map[string]any]{
			Location: "/api/v1/relations/" + relation.ID,
			Body:     map[string]any{"relation": relation, "sync_status": syncStatus},
		}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "list-relations", Method: http.MethodGet, Path: "/api/v1/relations",
		Summary: "List relations and neighbours", Tags: []string{"Graph"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		EntityID string `query:"entity_id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
		Cursor   string `query:"cursor" pattern:"^c[0-9a-z]+$"`
		Limit    int    `query:"limit" minimum:"1" maximum:"500" default:"100"`
	}) (*body[page[model.Relation]], error) {
		if _, err := a.require(ctx, auth.ReadGraph); err != nil {
			return nil, err
		}
		relations, err := a.service.Store().ListRelations(ctx, input.EntityID, 10_000)
		if err != nil {
			return nil, err
		}
		return &body[page[model.Relation]]{Body: paginate(relations, input.Cursor, input.Limit)}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "delete-relation", Method: http.MethodDelete, Path: "/api/v1/relations/{id}",
		Summary: "Delete an explicit relation", Tags: []string{"Graph"}, DefaultStatus: http.StatusNoContent,
		Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		ID string `path:"id" pattern:"^[0-9a-v]{20}$"`
	}) (*struct{}, error) {
		if _, err := a.require(ctx, auth.WriteGraph); err != nil {
			return nil, err
		}
		if err := a.service.Store().DeleteRelation(ctx, input.ID); err != nil {
			return nil, a.storeError(ctx, "relations", input.ID, err)
		}
		return nil, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "find-graph-path", Method: http.MethodPost, Path: "/api/v1/graph/path",
		Summary: "Find a bounded path between entities", Tags: []string{"Graph"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		Body struct {
			FromID   string `json:"from_entity_id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
			ToID     string `json:"to_entity_id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
			MaxDepth int    `json:"max_depth,omitempty" minimum:"1" maximum:"8" default:"4"`
		}
	}) (*body[map[string]any], error) {
		if _, err := a.require(ctx, auth.ReadGraph); err != nil {
			return nil, err
		}
		path, err := a.service.Store().GraphPath(ctx, input.Body.FromID, input.Body.ToID, input.Body.MaxDepth)
		if err != nil {
			return nil, a.storeError(ctx, "graph paths", input.Body.FromID+"→"+input.Body.ToID, err)
		}
		return &body[map[string]any]{Body: map[string]any{"path": path, "depth": len(path)}}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "list-conflicts", Method: http.MethodGet, Path: "/api/v1/conflicts",
		Summary: "List unresolved and resolved conflicts", Tags: []string{"Graph"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		Cursor string `query:"cursor" pattern:"^c[0-9a-z]+$"`
		Limit  int    `query:"limit" minimum:"1" maximum:"500" default:"100"`
	}) (*body[page[model.Conflict]], error) {
		if _, err := a.require(ctx, auth.ReadGraph); err != nil {
			return nil, err
		}
		conflicts, err := a.service.Store().ListConflicts(ctx, 10_000)
		if err != nil {
			return nil, err
		}
		return &body[page[model.Conflict]]{Body: paginate(conflicts, input.Cursor, input.Limit)}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "resolve-conflict", Method: http.MethodPost, Path: "/api/v1/conflicts/{id}/resolve",
		Summary: "Resolve a conflict explicitly", Tags: []string{"Graph"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		ID   string `path:"id" pattern:"^[0-9a-v]{20}$"`
		Body struct {
			Resolution string `json:"resolution" minLength:"1" maxLength:"1000"`
		}
	}) (*body[model.Conflict], error) {
		if _, err := a.require(ctx, auth.ManageConflicts); err != nil {
			return nil, err
		}
		conflict, err := a.service.Store().ResolveConflict(ctx, input.ID, input.Body.Resolution)
		if err != nil {
			return nil, a.storeError(ctx, "conflicts", input.ID, err)
		}
		return &body[model.Conflict]{Body: conflict}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "list-sources", Method: http.MethodGet, Path: "/api/v1/sources",
		Summary: "List fact provenance", Tags: []string{"Provenance"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		Cursor string `query:"cursor" pattern:"^c[0-9a-z]+$"`
		Limit  int    `query:"limit" minimum:"1" maximum:"500" default:"100"`
	}) (*body[page[model.Source]], error) {
		if _, err := a.require(ctx, auth.ReadGraph); err != nil {
			return nil, err
		}
		sources, err := a.service.Store().ListSources(ctx, 10_000)
		if err != nil {
			return nil, err
		}
		return &body[page[model.Source]]{Body: paginate(sources, input.Cursor, input.Limit)}, nil
	})
}
