package stock

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInMarketWindow(t *testing.T) {
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		// 2026-05-05 was a Tuesday.
		{"weekday inside window — 14:00 UTC", time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC), true},
		{"weekday at start boundary — 13:00 UTC", time.Date(2026, 5, 5, 13, 0, 0, 0, time.UTC), true},
		{"weekday just before start — 12:59 UTC", time.Date(2026, 5, 5, 12, 59, 0, 0, time.UTC), false},
		{"weekday at end boundary — 21:00 UTC (exclusive)", time.Date(2026, 5, 5, 21, 0, 0, 0, time.UTC), false},
		{"weekday just before end — 20:59 UTC", time.Date(2026, 5, 5, 20, 59, 0, 0, time.UTC), true},
		{"weekday after window — 22:00 UTC", time.Date(2026, 5, 5, 22, 0, 0, 0, time.UTC), false},
		// 2026-05-09 was a Saturday, 2026-05-10 a Sunday.
		{"saturday inside hour window", time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC), false},
		{"sunday inside hour window", time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC), false},
		// Non-UTC input: function normalizes to UTC.
		{"local time that maps to weekday inside window UTC",
			time.Date(2026, 5, 5, 17, 0, 0, 0, mustLoadTZ(t, "Europe/Athens")), true},
		// Athens 23:00 = UTC 20:00 → still inside window.
		{"athens 23:00 → utc 20:00 (inside)",
			time.Date(2026, 5, 5, 23, 0, 0, 0, mustLoadTZ(t, "Europe/Athens")), true},
		// Athens 00:30 Wed = UTC 21:30 Tue → outside window.
		{"athens wed 00:30 → utc tue 21:30 (outside)",
			time.Date(2026, 5, 6, 0, 30, 0, 0, mustLoadTZ(t, "Europe/Athens")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, inMarketWindow(tc.t))
		})
	}
}

func mustLoadTZ(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}
