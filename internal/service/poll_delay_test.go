package service

import (
	"testing"
	"time"
)

func TestBrainPollDelayStartsFastAndBacksOffToTheConfiguredCeiling(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		ceiling time.Duration
		want    []time.Duration
	}{
		{
			name:    "default ceiling",
			ceiling: 2 * time.Second,
			want: []time.Duration{
				250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second,
			},
		},
		{
			name:    "ceiling below the first delay",
			ceiling: 5 * time.Millisecond,
			want:    []time.Duration{5 * time.Millisecond, 5 * time.Millisecond},
		},
		{
			name:    "unset ceiling falls back to the default",
			ceiling: 0,
			want:    []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, defaultBrainPollInterval},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := &Service{brainPollInterval: test.ceiling}
			for polls, want := range test.want {
				if got := service.brainPollDelay(polls); got != want {
					t.Errorf("delay after %d polls = %s, want %s", polls, got, want)
				}
			}
			// Many polls never overflow or exceed the ceiling.
			ceiling := test.ceiling
			if ceiling <= 0 {
				ceiling = defaultBrainPollInterval
			}
			if got := service.brainPollDelay(10_000); got != ceiling {
				t.Errorf("delay after 10000 polls = %s, want %s", got, ceiling)
			}
		})
	}
}
