package dunning

import (
	"testing"
	"time"
)

func TestNextRetry(t *testing.T) {
	p := Policy{Backoffs: []time.Duration{24 * time.Hour, 72 * time.Hour, 168 * time.Hour}}
	now := time.Unix(1_700_000_000, 0).UTC()

	cases := []struct {
		attempts int
		want     time.Duration
		ok       bool
	}{
		{0, 0, false},
		{1, 24 * time.Hour, true},
		{2, 72 * time.Hour, true},
		{3, 168 * time.Hour, true},
		{4, 0, false}, // exhausted — caller should deactivate
	}
	for _, tc := range cases {
		got, ok := p.NextRetry(tc.attempts, now)
		if ok != tc.ok {
			t.Errorf("attempts=%d ok=%v want %v", tc.attempts, ok, tc.ok)
		}
		if ok && !got.Equal(now.Add(tc.want)) {
			t.Errorf("attempts=%d got=%v want=%v", tc.attempts, got, now.Add(tc.want))
		}
	}
}
