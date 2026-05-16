package api

import "time"

// tzFrom resolves a `func() *time.Location` getter to a usable Location,
// falling back to UTC. Handlers store TZ as a getter so the live value
// from the SettingsService picks up edits without a restart; tests and
// boot sites can supply a closure that returns a fixed Location.
func tzFrom(getter func() *time.Location) *time.Location {
	if getter == nil {
		return time.UTC
	}
	loc := getter()
	if loc == nil {
		return time.UTC
	}
	return loc
}
