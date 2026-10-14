package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadBrainPollInterval(t *testing.T) {
	for _, test := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 2 * time.Second, false},
		{"750ms", 750 * time.Millisecond, false},
		{"10s", 10 * time.Second, false},
		{"0s", 0, true},
		{"-1s", 0, true},
		{"often", 0, true},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv("MANIFOLD_BRAIN_POLL_INTERVAL", test.value)
			cfg, err := Load()
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), "MANIFOLD_BRAIN_POLL_INTERVAL") {
					t.Fatalf("invalid poll interval accepted: %v", err)
				}
				return
			}
			if err != nil || cfg.BrainPollInterval != test.want {
				t.Fatalf("poll interval = %s, error = %v; want %s", cfg.BrainPollInterval, err, test.want)
			}
		})
	}
}
