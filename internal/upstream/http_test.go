package upstream

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSafeOperationPathRemovesQueryValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "untemplated API path",
			path: "/api/v1/fs/stat?uri=viking://private/resource",
			want: "/api/v1/fs/stat",
		},
		{
			name: "templated document path",
			path: "/v1/documents/source_document:private-id/candidates?token=private",
			want: "/v1/documents/{id}/candidates",
		},
		{
			name: "query-like final segment",
			path: "/v1/documents/source_document:private-id?query=private",
			want: "/v1/documents/{id}",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := safeOperationPath(test.path); got != test.want {
				t.Fatalf("safeOperationPath(%q) = %q, want %q", test.path, got, test.want)
			}
		})
	}
}

func TestTransportDependencyErrorRedactsRequestURL(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(nil)
	base := server.URL
	server.Close()

	client := NewBrain(base, "secret", server.Client(), server.Client())
	err := client.http.do(
		context.Background(),
		"GET",
		"/v1/documents/source_document:private-id?cursor=private-query",
		nil,
		nil,
	)
	if err == nil {
		t.Fatal("request error = nil, want transport failure")
	}
	var urlError *url.Error
	if !errors.As(err, &urlError) {
		t.Fatalf("error = %T %v, want the underlying transport error to remain unwrap-able", err, err)
	}
	if strings.Contains(err.Error(), "source_document:private-id") || strings.Contains(err.Error(), "private-query") || strings.Contains(err.Error(), base) {
		t.Fatalf("error exposed request details: %v", err)
	}
}
