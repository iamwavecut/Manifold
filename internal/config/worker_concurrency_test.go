package config

import (
	"strings"
	"testing"
)

func TestLoadWorkerConcurrency(t *testing.T) {
	for _, test := range []struct {
		value   string
		want    int
		invalid bool
	}{
		{"", 2, false},
		{"1", 1, false},
		{"16", 16, false},
		{"0", 0, true},
		{"17", 0, true},
		{"many", 0, true},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv("MANIFOLD_WORKER_CONCURRENCY", test.value)
			cfg, err := Load()
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), "MANIFOLD_WORKER_CONCURRENCY") {
					t.Fatalf("invalid worker concurrency accepted: %v", err)
				}
				return
			}
			if err != nil || cfg.WorkerConcurrency != test.want {
				t.Fatalf("worker concurrency = %d, error = %v; want %d", cfg.WorkerConcurrency, err, test.want)
			}
		})
	}
}
