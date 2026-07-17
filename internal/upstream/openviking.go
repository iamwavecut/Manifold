package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/iamwavecut/Manifold/internal/model"
)

type OpenViking struct {
	http httpClient
}

func NewOpenViking(base, key string, client *http.Client) *OpenViking {
	return &OpenViking{http: httpClient{
		name: "openviking", base: base, key: key, client: client,
		auth: func(req *http.Request, key string) {
			req.Header.Set("X-API-Key", key)
			req.Header.Set("X-OpenViking-Account", "manifold")
			req.Header.Set("X-OpenViking-User", "manifold")
		},
	}}
}

func (o *OpenViking) Health(ctx context.Context) error {
	if o.http.base == "" {
		return &DependencyError{Dependency: "openviking", Operation: "health", Retryable: true, Cause: fmt.Errorf("OPENVIKING_URL is not configured")}
	}
	return o.http.do(ctx, http.MethodGet, "/health", nil, nil)
}

func (o *OpenViking) Exists(ctx context.Context, uri string) (bool, error) {
	var response envelope[map[string]any]
	err := o.http.do(ctx, http.MethodGet, "/api/v1/fs/stat?uri="+url.QueryEscape(uri), nil, &response)
	if err == nil {
		return true, nil
	}
	var dependency *DependencyError
	if errors.As(err, &dependency) && dependency.Status == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func (o *OpenViking) Mkdir(ctx context.Context, uri, description string) error {
	var response envelope[map[string]any]
	return o.http.do(ctx, http.MethodPost, "/api/v1/fs/mkdir",
		map[string]any{"uri": uri, "description": description}, &response)
}

func (o *OpenViking) Write(ctx context.Context, uri, content string, create bool) error {
	mode := "replace"
	if create {
		mode = "create"
	}
	var response envelope[map[string]any]
	return o.http.do(ctx, http.MethodPost, "/api/v1/content/write",
		map[string]any{"uri": uri, "content": content, "mode": mode, "wait": true}, &response)
}

func (o *OpenViking) Move(ctx context.Context, from, to string) error {
	var response envelope[map[string]any]
	return o.http.do(ctx, http.MethodPost, "/api/v1/fs/mv",
		map[string]string{"from_uri": from, "to_uri": to}, &response)
}

func (o *OpenViking) Delete(ctx context.Context, uri string, recursive bool) error {
	path := "/api/v1/fs?uri=" + url.QueryEscape(uri)
	if recursive {
		path += "&recursive=true"
	}
	var response envelope[map[string]any]
	return o.http.do(ctx, http.MethodDelete, path, nil, &response)
}

func (o *OpenViking) Snapshot(ctx context.Context, message string, paths []string) (string, error) {
	var response envelope[struct {
		CommitOID string `json:"commit_oid"`
	}]
	err := o.http.do(ctx, http.MethodPost, "/api/v1/snapshot/commit",
		map[string]any{"message": message, "paths": paths}, &response)
	return response.Result.CommitOID, err
}

func (o *OpenViking) Search(ctx context.Context, query, targetURI string, limit int) ([]model.SearchHit, error) {
	var response envelope[struct {
		Resources []struct {
			URI         string  `json:"uri"`
			Level       int     `json:"level"`
			Score       float64 `json:"score"`
			Abstract    string  `json:"abstract"`
			Overview    string  `json:"overview"`
			MatchReason string  `json:"match_reason"`
		} `json:"resources"`
	}]
	body := map[string]any{
		"query": query, "limit": limit, "context_type": []string{"resource"},
		"include_provenance": true,
	}
	if targetURI != "" {
		body["target_uri"] = targetURI
	}
	if err := o.http.do(ctx, http.MethodPost, "/api/v1/search/find", body, &response); err != nil {
		return nil, err
	}
	hits := make([]model.SearchHit, 0, len(response.Result.Resources))
	for _, resource := range response.Result.Resources {
		snippet := resource.Overview
		if snippet == "" {
			snippet = resource.Abstract
		}
		hits = append(hits, model.SearchHit{
			Kind: "document", ID: resource.URI, Title: resource.URI, Snippet: snippet,
			Score: resource.Score, Source: "openviking",
		})
	}
	return hits, nil
}

type envelope[T any] struct {
	Status string `json:"status"`
	Result T      `json:"result"`
}
