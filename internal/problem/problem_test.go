package problem

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSemanticErrorSerializesActionableFieldsWithoutSecrets(t *testing.T) {
	t.Parallel()

	ctx := WithRequestID(t.Context(), "d5vqftidc6b5u5sctq00")
	err := Validation(ctx, "https://manifold.example", Violation{
		Pointer:  "/body/title",
		Code:     "required",
		Message:  "Title is required.",
		Expected: "a non-empty string, for example \"Agent memory\"",
		Received: "",
	})
	data, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	text := string(data)
	for _, required := range []string{
		`"code":"validation_failed"`,
		`"request_id":"d5vqftidc6b5u5sctq00"`,
		`"retryable":false`,
		`"pointer":"/body/title"`,
		`"remediation":`,
		`"documentation_url":`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("serialized error lacks %s: %s", required, text)
		}
	}
	for _, forbidden := range []string{"SELECT ", "stack trace", "Authorization: Bearer"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("serialized error exposed forbidden text %q", forbidden)
		}
	}
}
