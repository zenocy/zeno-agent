// Package sensor defines the interface every Zeno data source implements.
//
// Sensors are stateless from the caller's perspective: each Sync call reads
// from its source (IMAP, CalDAV, Open-Meteo) and appends events to the
// observation log. Cursor / dedupe state lives in the log itself, not in the
// sensor struct.
//
// Sync must be safe to call concurrently with itself — the cron scheduler may
// fan out multiple SyncAll invocations (manual /api/sync/now plus a scheduled
// tick can overlap).
//
// V2.4 reactive trigger: each Sync implementation SHOULD call PublishObserved
// (this package) immediately AFTER every successful log append. The publisher
// is extracted from ctx via PublisherFromContext; if no publisher is attached,
// PublishObserved is a silent no-op (so unit tests that don't wire a bus
// continue to work without modification).
//
// The bus is best-effort. Durability remains the log; PublishObserved is
// purely a real-time notification path for the inject subscriber.
package sensor

import "context"

// Sensor is the seam every data source plugs into.
type Sensor interface {
	Name() string
	Sync(ctx context.Context) error
}
