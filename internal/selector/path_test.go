package selector

import "testing"

func TestMatchSupportsBoundedAndRecursiveSegments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		{pattern: "projects/ngbot/**", value: "projects/ngbot", want: true},
		{pattern: "projects/ngbot/**", value: "projects/ngbot/operations/runbook", want: true},
		{pattern: "projects/*/operations", value: "projects/ngbot/operations", want: true},
		{pattern: "**/runbook-?", value: "tasks/auth/runbook-2", want: true},
		{pattern: "shared/**", value: "tasks/auth", want: false},
		{pattern: "projects/*", value: "projects/ngbot/operations", want: false},
	}
	for _, test := range tests {
		if got := Match(test.pattern, test.value); got != test.want {
			t.Errorf("Match(%q, %q) = %v, want %v", test.pattern, test.value, got, test.want)
		}
	}
}

func TestValidateGlobRejectsShellSpecificSyntax(t *testing.T) {
	t.Parallel()

	for _, pattern := range []string{"shared/[ab]", "shared/{a,b}", "shared/Upper", `shared/a\\b`} {
		if err := ValidateGlob(pattern); err == nil {
			t.Errorf("ValidateGlob(%q) unexpectedly succeeded", pattern)
		}
	}
	if err := ValidateGlob("shared/**/runbook-*"); err != nil {
		t.Fatalf("valid glob was rejected: %v", err)
	}
}
