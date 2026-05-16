package stock

import "time"

// US-market window in UTC. Covers 9:30am–4:00pm ET across both DST
// regimes (winter EST=UTC−5 → 14:30–21:00; summer EDT=UTC−4 →
// 13:30–20:00) plus ~30 minutes of margin on each side for pre/post
// awareness. Hardcoded — multi-exchange support is a future feature.
const (
	marketWindowStartHourUTC = 13 // 13:00 UTC inclusive
	marketWindowEndHourUTC   = 21 // 21:00 UTC exclusive
)

// inMarketWindow reports whether t falls inside the US trading-hours
// window. Returns false on Saturday and Sunday regardless of hour.
// Pure: no I/O, no goroutines, no globals.
func inMarketWindow(t time.Time) bool {
	u := t.UTC()
	switch u.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	h := u.Hour()
	return h >= marketWindowStartHourUTC && h < marketWindowEndHourUTC
}
