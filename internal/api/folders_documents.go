package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iamwavecut/Manifold/internal/auth"
	"github.com/iamwavecut/Manifold/internal/identity"
	"github.com/iamwavecut/Manifold/internal/model"
	"github.com/iamwavecut/Manifold/internal/problem"
	"github.com/iamwavecut/Manifold/internal/store"
	"github.com/iamwavecut/Manifold/internal/upstream"
)

type createFolderRequest struct {
	MutationHeaders
	Body struct {
		ID       string         `json:"id,omitempty" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" maxLength:"63" doc:"Optional semantic slug. Generated from name when omitted."`
		Name     string         `json:"name" minLength:"1" maxLength:"160"`
		ParentID string         `json:"parent_id,omitempty" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" maxLength:"63"`
		Summary  string         `json:"summary,omitempty" maxLength:"4000"`
		Metadata map[string]any `json:"metadata,omitempty"`
	}
}

type createFolderResponse struct {
	Folder model.Folder `json:"folder"`
}

func (a *API) registerFolders() {
	huma.Register(a.huma, huma.Operation{
		OperationID: "create-folder", Method: http.MethodPost, Path: "/api/v1/folders",
		Summary: "Create a folder", Tags: []string{"Folders"}, DefaultStatus: http.StatusCreated,
		Errors: commonErrors(),
	}, func(ctx context.Context, input *createFolderRequest) (*accepted[createFolderResponse], error) {
		if _, err := a.require(ctx, auth.WriteDocuments); err != nil {
			return nil, err
		}
		slug := input.Body.ID
		if slug == "" {
			slug = identity.Slug(input.Body.Name)
		}
		available, err := a.service.Store().SlugAvailable(ctx, "folder", slug)
		if err != nil {
			return nil, err
		}
		if !available {
			return nil, a.slugTaken(ctx, "folder", slug)
		}
		folder, err := a.service.CreateFolder(ctx, model.Folder{
			ID: slug, Name: input.Body.Name, ParentID: input.Body.ParentID,
			Summary: input.Body.Summary, Metadata: input.Body.Metadata,
		})
		if err != nil {
			return nil, a.operationError(ctx, "folders", slug, err)
		}
		response := createFolderResponse{Folder: folder}
		return &accepted[createFolderResponse]{
			Location: "/api/v1/folders/" + folder.ID, Body: response,
		}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "list-folders", Method: http.MethodGet, Path: "/api/v1/folders",
		Summary: "List the folder tree", Tags: []string{"Folders"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		Cursor string `query:"cursor" pattern:"^c[0-9a-z]+$"`
		Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
	}) (*body[page[model.Folder]], error) {
		if _, err := a.require(ctx, auth.ReadDocuments); err != nil {
			return nil, err
		}
		folders, err := a.service.Store().ListFolders(ctx)
		if err != nil {
			return nil, err
		}
		return &body[page[model.Folder]]{Body: paginate(folders, input.Cursor, input.Limit)}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-tree", Method: http.MethodGet, Path: "/api/v1/tree",
		Summary: "Read the path-bearing folder and document tree", Tags: []string{"Folders"}, Errors: commonErrors(),
	}, func(ctx context.Context, _ *struct{}) (*body[map[string]any], error) {
		if _, err := a.require(ctx, auth.ReadDocuments); err != nil {
			return nil, err
		}
		folders, err := a.service.Store().ListFolders(ctx)
		if err != nil {
			return nil, err
		}
		documents, err := a.service.Store().ListDocuments(ctx, "")
		if err != nil {
			return nil, err
		}
		return &body[map[string]any]{Body: map[string]any{
			"folders": folders, "documents": documents,
		}}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-folder", Method: http.MethodGet, Path: "/api/v1/folders/{id}",
		Summary: "Read a folder", Tags: []string{"Folders"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		ID string `path:"id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
	}) (*withETag[model.Folder], error) {
		if _, err := a.require(ctx, auth.ReadDocuments); err != nil {
			return nil, err
		}
		folder, err := a.service.Store().GetFolder(ctx, input.ID)
		if err != nil {
			return nil, a.storeError(ctx, "folders", input.ID, err)
		}
		return &withETag[model.Folder]{ETag: folder.ETag, Body: folder}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "delete-folder", Method: http.MethodDelete, Path: "/api/v1/folders/{id}",
		Summary: "Delete an empty folder", Tags: []string{"Folders"}, DefaultStatus: http.StatusNoContent,
		Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		ID      string `path:"id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
		IfMatch string `header:"If-Match" minLength:"4"`
	}) (*struct{}, error) {
		if _, err := a.require(ctx, auth.WriteDocuments); err != nil {
			return nil, err
		}
		folder, err := a.service.Store().GetFolder(ctx, input.ID)
		if err != nil {
			return nil, a.storeError(ctx, "folders", input.ID, err)
		}
		if folder.ETag != input.IfMatch {
			return nil, a.etagMismatch(ctx, "/api/v1/folders/"+input.ID, folder.ETag)
		}
		if err := a.service.DeleteFolder(ctx, input.ID); err != nil {
			return nil, a.operationError(ctx, "folders", input.ID, err)
		}
		return nil, nil
	})
}

type createDocumentRequest struct {
	MutationHeaders
	Body struct {
		ID       string         `json:"id,omitempty" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" maxLength:"63"`
		FolderID string         `json:"folder_id,omitempty" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" maxLength:"63"`
		Title    string         `json:"title" minLength:"1" maxLength:"240"`
		Format   string         `json:"format,omitempty" enum:"markdown,text,json,yaml,toml,javascript,typescript,python" default:"markdown"`
		Content  string         `json:"content" maxLength:"5000000"`
		Metadata map[string]any `json:"metadata,omitempty"`
		Tags     []string       `json:"tags,omitempty" maxItems:"64"`
	}
}

type documentAccepted struct {
	Document model.Document `json:"document"`
	Job      model.Job      `json:"job"`
}

func (a *API) registerDocuments() {
	huma.Register(a.huma, huma.Operation{
		OperationID: "create-document", Method: http.MethodPost, Path: "/api/v1/documents",
		Summary: "Create and asynchronously index a document", Tags: []string{"Documents"},
		DefaultStatus: http.StatusAccepted, Errors: commonErrors(), MaxBodyBytes: a.config.MaxBodyBytes,
	}, func(ctx context.Context, input *createDocumentRequest) (*accepted[documentAccepted], error) {
		if _, err := a.require(ctx, auth.WriteDocuments); err != nil {
			return nil, err
		}
		slug := input.Body.ID
		if slug == "" {
			slug = identity.Slug(input.Body.Title)
		}
		available, err := a.service.Store().SlugAvailable(ctx, "document", slug)
		if err != nil {
			return nil, err
		}
		if !available {
			return nil, a.slugTaken(ctx, "document", slug)
		}
		format := input.Body.Format
		if format == "" {
			format = "markdown"
		}
		doc, job, err := a.service.CreateDocument(ctx, model.Document{
			ID: slug, FolderID: input.Body.FolderID, Title: input.Body.Title, Format: format,
			Content: input.Body.Content, Metadata: input.Body.Metadata, Tags: input.Body.Tags,
		})
		if err != nil {
			return nil, a.operationError(ctx, "documents", slug, err)
		}
		response := documentAccepted{Document: doc, Job: job}
		return &accepted[documentAccepted]{Location: "/api/v1/jobs/" + job.ID, Body: response}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "import-documents", Method: http.MethodPost, Path: "/api/v1/documents/import",
		Summary: "Import a batch of UTF-8 documents", Tags: []string{"Documents"},
		DefaultStatus: http.StatusAccepted, Errors: commonErrors(), MaxBodyBytes: a.config.MaxBodyBytes,
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		Body struct {
			Documents []struct {
				ID       string         `json:"id,omitempty" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" maxLength:"63"`
				FolderID string         `json:"folder_id,omitempty" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" maxLength:"63"`
				Title    string         `json:"title" minLength:"1" maxLength:"240"`
				Format   string         `json:"format,omitempty" enum:"markdown,text,json,yaml,toml,javascript,typescript,python" default:"markdown"`
				Content  string         `json:"content" maxLength:"5000000"`
				Metadata map[string]any `json:"metadata,omitempty"`
				Tags     []string       `json:"tags,omitempty" maxItems:"64"`
			} `json:"documents" minItems:"1" maxItems:"100"`
		}
	}) (*accepted[map[string]any], error) {
		if _, err := a.require(ctx, auth.WriteDocuments); err != nil {
			return nil, err
		}
		slugs := make([]string, len(input.Body.Documents))
		seen := map[string]struct{}{}
		for index, item := range input.Body.Documents {
			slug := item.ID
			if slug == "" {
				slug = identity.Slug(item.Title)
			}
			if _, duplicate := seen[slug]; duplicate {
				return nil, problem.Validation(ctx, a.config.PublicURL, problem.Violation{
					Pointer:  fmt.Sprintf("/body/documents/%d/id", index),
					Code:     "duplicate_slug",
					Message:  "Each document in an import batch must have a distinct final slug.",
					Expected: "an unused semantic slug",
					Received: slug,
				})
			}
			seen[slug] = struct{}{}
			available, err := a.service.Store().SlugAvailable(ctx, "document", slug)
			if err != nil {
				return nil, err
			}
			if !available {
				return nil, a.slugTaken(ctx, "document", slug)
			}
			slugs[index] = slug
		}
		results := make([]documentAccepted, 0, len(input.Body.Documents))
		for index, item := range input.Body.Documents {
			format := item.Format
			if format == "" {
				format = "markdown"
			}
			doc, job, err := a.service.CreateDocument(ctx, model.Document{
				ID: slugs[index], FolderID: item.FolderID, Title: item.Title, Format: format,
				Content: item.Content, Metadata: item.Metadata, Tags: item.Tags,
			})
			if err != nil {
				return nil, a.operationError(ctx, "documents", slugs[index], err)
			}
			results = append(results, documentAccepted{Document: doc, Job: job})
		}
		return &accepted[map[string]any]{
			Location: "/api/v1/jobs",
			Body:     map[string]any{"items": results, "accepted": len(results)},
		}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "list-documents", Method: http.MethodGet, Path: "/api/v1/documents",
		Summary: "List active documents", Tags: []string{"Documents"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		FolderID string `query:"folder_id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
		Cursor   string `query:"cursor" pattern:"^c[0-9a-z]+$"`
		Limit    int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
	}) (*body[page[model.Document]], error) {
		if _, err := a.require(ctx, auth.ReadDocuments); err != nil {
			return nil, err
		}
		docs, err := a.service.Store().ListDocuments(ctx, input.FolderID)
		if err != nil {
			return nil, err
		}
		return &body[page[model.Document]]{Body: paginate(docs, input.Cursor, input.Limit)}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-document", Method: http.MethodGet, Path: "/api/v1/documents/{id}",
		Summary: "Read the current document", Tags: []string{"Documents"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		ID string `path:"id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
	}) (*withETag[model.Document], error) {
		if _, err := a.require(ctx, auth.ReadDocuments); err != nil {
			return nil, err
		}
		doc, err := a.service.Store().GetDocument(ctx, input.ID, true)
		if err != nil {
			return nil, a.storeError(ctx, "documents", input.ID, err)
		}
		return &withETag[model.Document]{ETag: doc.ETag, Body: doc}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "update-document", Method: http.MethodPut, Path: "/api/v1/documents/{id}",
		Summary: "Create a new document revision", Tags: []string{"Documents"},
		DefaultStatus: http.StatusAccepted, Errors: commonErrors(), MaxBodyBytes: a.config.MaxBodyBytes,
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		ID      string `path:"id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
		IfMatch string `header:"If-Match" minLength:"4"`
		Body    struct {
			Title    string         `json:"title,omitempty" maxLength:"240"`
			Content  string         `json:"content" maxLength:"5000000"`
			Metadata map[string]any `json:"metadata,omitempty"`
			Tags     []string       `json:"tags,omitempty" maxItems:"64"`
		}
	}) (*accepted[documentAccepted], error) {
		if _, err := a.require(ctx, auth.WriteDocuments); err != nil {
			return nil, err
		}
		doc, job, err := a.service.UpdateDocument(ctx, input.ID, input.IfMatch, input.Body.Title,
			input.Body.Content, input.Body.Metadata, input.Body.Tags)
		if errors.Is(err, store.ErrConflict) {
			current, getErr := a.service.Store().GetDocument(ctx, input.ID, false)
			if getErr == nil {
				return nil, a.etagMismatch(ctx, "/api/v1/documents/"+input.ID, current.ETag)
			}
		}
		if err != nil {
			return nil, a.operationError(ctx, "documents", input.ID, err)
		}
		return &accepted[documentAccepted]{
			Location: "/api/v1/jobs/" + job.ID, Body: documentAccepted{Document: doc, Job: job},
		}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "delete-document", Method: http.MethodDelete, Path: "/api/v1/documents/{id}",
		Summary: "Delete a document from the active knowledge base", Tags: []string{"Documents"},
		DefaultStatus: http.StatusAccepted, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		MutationHeaders
		ID      string `path:"id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
		IfMatch string `header:"If-Match" minLength:"4"`
	}) (*accepted[map[string]any], error) {
		if _, err := a.require(ctx, auth.WriteDocuments); err != nil {
			return nil, err
		}
		job, err := a.service.DeleteDocument(ctx, input.ID, input.IfMatch)
		if errors.Is(err, store.ErrConflict) {
			current, getErr := a.service.Store().GetDocument(ctx, input.ID, false)
			if getErr == nil {
				return nil, a.etagMismatch(ctx, "/api/v1/documents/"+input.ID, current.ETag)
			}
		}
		if err != nil {
			return nil, a.operationError(ctx, "documents", input.ID, err)
		}
		return &accepted[map[string]any]{
			Location: "/api/v1/jobs/" + job.ID,
			Body:     map[string]any{"document_id": input.ID, "job": job},
		}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "list-document-revisions", Method: http.MethodGet, Path: "/api/v1/documents/{id}/revisions",
		Summary: "List immutable document revisions", Tags: []string{"Documents"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		ID     string `path:"id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
		Cursor string `query:"cursor" pattern:"^c[0-9a-z]+$"`
		Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
	}) (*body[page[model.Revision]], error) {
		if _, err := a.require(ctx, auth.ReadDocuments); err != nil {
			return nil, err
		}
		if _, err := a.service.Store().GetDocument(ctx, input.ID, false); err != nil {
			return nil, a.storeError(ctx, "documents", input.ID, err)
		}
		revisions, err := a.service.Store().ListRevisions(ctx, input.ID)
		if err != nil {
			return nil, err
		}
		return &body[page[model.Revision]]{Body: paginate(revisions, input.Cursor, input.Limit)}, nil
	})

	huma.Register(a.huma, huma.Operation{
		OperationID: "get-document-revision", Method: http.MethodGet, Path: "/api/v1/documents/{id}/revisions/{revision}",
		Summary: "Read one immutable document revision", Tags: []string{"Documents"}, Errors: commonErrors(),
	}, func(ctx context.Context, input *struct {
		ID       string `path:"id" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$"`
		Revision string `path:"revision" pattern:"^r[1-9][0-9]*$"`
	}) (*body[model.Revision], error) {
		if _, err := a.require(ctx, auth.ReadDocuments); err != nil {
			return nil, err
		}
		number, _ := strconv.Atoi(strings.TrimPrefix(input.Revision, "r"))
		revision, err := a.service.Store().GetRevision(ctx, input.ID, number)
		if err != nil {
			return nil, a.storeError(ctx, "document revisions", input.ID+"/"+input.Revision, err)
		}
		return &body[model.Revision]{Body: revision}, nil
	})
}

func (a *API) slugTaken(ctx context.Context, resourceType, slug string) error {
	suggested, _ := a.service.Store().AvailableSlug(ctx, resourceType, slug)
	p := problem.New(ctx, a.config.PublicURL, http.StatusConflict, "slug_taken", "Slug already in use",
		fmt.Sprintf("The %s slug %q is already in use.", resourceType, slug),
		problem.Remediation{
			Summary: "Choose an unused semantic slug and retry.",
			Steps:   []string{"Use suggested_slug or supply another ASCII kebab-case slug.", "Retry with a new Idempotency-Key."},
		})
	p.SuggestedSlug = suggested
	return p
}

func (a *API) etagMismatch(ctx context.Context, resource, current string) error {
	p := problem.New(ctx, a.config.PublicURL, http.StatusConflict, "etag_mismatch", "Resource changed",
		"The If-Match value does not describe the current resource revision.",
		problem.Remediation{
			Summary: "Read the current resource, reapply the intended change, and retry with its ETag.",
			Steps:   []string{"GET " + resource + ".", "Merge the intended change into the returned state.", "Retry with If-Match: " + current + "."},
		})
	p.CurrentETag = current
	p.Resource = resource
	return p
}

func (a *API) operationError(ctx context.Context, resourceType, id string, err error) error {
	var dependency *upstream.DependencyError
	if errors.As(err, &dependency) {
		p := problem.New(ctx, a.config.PublicURL, http.StatusServiceUnavailable, "dependency_unavailable",
			"Dependency unavailable", fmt.Sprintf("%s could not complete %s.", dependency.Dependency, dependency.Operation),
			problem.Remediation{
				Summary: "Restore the dependency before retrying this operation.",
				Steps:   []string{"Inspect GET /api/v1/status.", "Correct dependency or model-provider configuration.", "Retry with the same Idempotency-Key."},
			})
		p.Retryable = dependency.Retryable
		p.RetryAfter = "30s"
		return p
	}
	return a.storeError(ctx, resourceType, id, err)
}
