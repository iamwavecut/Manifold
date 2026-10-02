package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadOpenVikingWriteTimeout(t *testing.T) {
	for _, test := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 5 * time.Minute, false},
		{"8m", 8 * time.Minute, false},
		{"0s", 0, true},
		{"-1s", 0, true},
		{"forever", 0, true},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv("MANIFOLD_OPENVIKING_WRITE_TIMEOUT", test.value)
			cfg, err := Load()
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), "MANIFOLD_OPENVIKING_WRITE_TIMEOUT") {
					t.Fatalf("invalid timeout accepted: %v", err)
				}
				return
			}
			if err != nil || cfg.OpenVikingWriteTimeout != test.want {
				t.Fatalf("timeout = %s, error = %v; want %s", cfg.OpenVikingWriteTimeout, err, test.want)
			}
		})
	}
}
