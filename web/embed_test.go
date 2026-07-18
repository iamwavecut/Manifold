package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesEveryWorkbenchRoute(t *testing.T) {
	handler := Handler()
	for _, path := range []string{
		"/app/",
		"/app/future-route",
		"/login",
		"/explorer",
		"/search",
		"/jobs",
		"/graph",
		"/conflicts",
		"/renames",
		"/system",
	} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("GET %s returned %d, want 200", path, response.Code)
			}
			if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
				t.Fatalf("GET %s returned Content-Type %q, want text/html", path, contentType)
			}
			if !strings.Contains(response.Body.String(), `<div id="app"></div>`) {
				t.Fatalf("GET %s did not return the workbench shell", path)
			}
		})
	}
}

func TestHandlerKeepsUnknownPathsOutsideTheSPA(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/not-a-route", nil)
	response := httptest.NewRecorder()

	Handler().ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("GET unknown path returned %d, want 404", response.Code)
	}
}

func TestHandlerRedirectsRootToWorkbench(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()

	Handler().ServeHTTP(response, request)

	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("GET / returned %d, want 307", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/app/" {
		t.Fatalf("GET / redirected to %q, want /app/", location)
	}
}

func TestHandlerServesAssetsWithoutMutatingTheRequestPath(t *testing.T) {
	for _, test := range []struct {
		path        string
		contentType string
	}{
		{path: "/assets/styles.css", contentType: "text/css"},
		{path: "/assets/app.js", contentType: "text/javascript"},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()

			Handler().ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("GET %s returned %d, want 200", test.path, response.Code)
			}
			if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, test.contentType) {
				t.Fatalf("GET %s returned Content-Type %q, want prefix %q", test.path, contentType, test.contentType)
			}
			if request.URL.Path != test.path {
				t.Fatalf("handler mutated request path to %q, want %q", request.URL.Path, test.path)
			}
			if response.Body.Len() == 0 {
				t.Fatalf("GET %s returned an empty asset", test.path)
			}
		})
	}
}
