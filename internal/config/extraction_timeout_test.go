package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadBrainExtractionTimeout(t *testing.T) {
	for _, test := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 30 * time.Minute, false},
		{"25m", 25 * time.Minute, false},
		{"0s", 0, true},
		{"-1s", 0, true},
		{"forever", 0, true},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv("MANIFOLD_BRAIN_EXTRACTION_TIMEOUT", test.value)
			cfg, err := Load()
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), "MANIFOLD_BRAIN_EXTRACTION_TIMEOUT") {
					t.Fatalf("invalid timeout accepted: %v", err)
				}
				return
			}
			if err != nil || cfg.BrainExtractionTimeout != test.want {
				t.Fatalf("timeout = %s, error = %v; want %s", cfg.BrainExtractionTimeout, err, test.want)
			}
		})
	}
}
