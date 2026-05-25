package hfsplus

import (
	"testing"
	"time"
)

func TestMacTime(t *testing.T) {
	cases := []struct {
		t    time.Time
		want uint32
	}{
		{time.Time{}, 0},
		{macEpoch, 0},
		// Unix epoch (1970-01-01) is 66 years past Mac epoch.
		// 66 * 365.25 * 86400 ≈ 2082844800
		{time.Unix(0, 0).UTC(), 2082844800},
	}
	for _, c := range cases {
		got := MacTime(c.t)
		if got != c.want {
			t.Errorf("MacTime(%v): got %d want %d", c.t, got, c.want)
		}
	}
}
