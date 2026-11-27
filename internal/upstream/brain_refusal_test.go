package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The response shape is Brain's GET /v1/documents/:id with
// provider-refusal.patch: a failed run names the class, never the message.
func TestDocumentResultCarriesProviderRefusal(t *testing.T) {
	for _, test := range []struct {
		name, run, want string
	}{
		{"refused", `{"runId":"indexer_run:a","packId":"_general","status":"failed","providerRefusal":"billing"}`, "billing"},
		{"other failure", `{"runId":"indexer_run:a","packId":"_general","status":"failed"}`, ""},
		{"unknown class", `{"runId":"indexer_run:a","packId":"_general","status":"failed","providerRefusal":"surprise"}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/documents/source_document:a/candidates" {
					_, _ = w.Write([]byte(`{"documentId":"source_document:a","candidates":[]}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"source_document:a","status":"indexing",` +
					`"originUri":"manifold://documents/note@r1","runs":[` + test.run + `]}`))
			}))
			defer server.Close()

			state, err := NewBrain(server.URL, "secret", server.Client(), server.Client()).
				DocumentResult(t.Context(), "source_document:a")
			if err != nil {
				t.Fatal(err)
			}
			if state.Status != GraphDocumentFailed {
				t.Fatalf("status = %q, want failed", state.Status)
			}
			if got := ProviderRefusal(state); got != test.want {
				t.Fatalf("ProviderRefusal = %q, want %q", got, test.want)
			}
		})
	}
}
