// Package caldav is the CalDAV sensor: lists events for the next ~7 days
// and emits cal.event_seen / cal.event_changed events.
//
// Network access is encapsulated by the Provider interface so that one
// implementation per CalDAV server family (Fastmail, iCloud, Nextcloud) can
// drop in without rippling through the poller. Phase 1 ships Fastmail only.
//
// V2.8.0 added the write methods (PutEvent / UpdateEvent / DeleteEvent /
// GetEvent) used by the action surface in internal/action. Conditional
// updates use If-Match against the previous ETag; the server returns
// 412 Precondition Failed on a stale write, which the Executor surfaces
// to the user instead of retrying (rationale: silent retry could clobber
// concurrent edits — see doc/v2.8/Phase2.md).
//
// Deletions are still not emitted by the read path (poller); only the
// V2.8 action surface invokes DeleteEvent.
package caldav

import (
	"context"
	"errors"
	"time"
)

// RawEvent is one untouched calendar object as the CalDAV server sees it.
type RawEvent struct {
	UID  string
	ICS  string
	ETag string
	// Path is the server-side calendar-object path, populated by the
	// V2.8 read path. Required for UpdateEvent / DeleteEvent — the
	// server addresses each object by its path, not its UID.
	Path string
}

// ErrPreconditionFailed is returned by UpdateEvent / DeleteEvent when
// the server's If-Match check fails (HTTP 412). The Executor catches
// this and surfaces it as a "Event was changed elsewhere — open it to
// retry" toast rather than auto-retrying.
var ErrPreconditionFailed = errors.New("caldav: precondition failed (412)")

// Provider lists and mutates calendar events.
type Provider interface {
	// ListEvents returns events whose start falls in [from, to].
	ListEvents(ctx context.Context, from, to time.Time) ([]RawEvent, error)

	// GetEvent fetches a single event by UID, scanning all calendars.
	// Returns nil when the UID is not found. Used by RSVP executors
	// to read-modify-write the existing iCalendar object.
	GetEvent(ctx context.Context, uid string) (*RawEvent, error)

	// PutEvent creates a new calendar object on the user's primary
	// calendar. Returns the assigned ETag and path. Uses If-None-Match:
	// * so a UID collision fails fast instead of overwriting.
	PutEvent(ctx context.Context, uid, ics string) (etag, path string, err error)

	// UpdateEvent rewrites an existing calendar object. ifMatchETag is
	// the ETag from the previous read; on a server-side change this
	// returns ErrPreconditionFailed.
	UpdateEvent(ctx context.Context, path, ics, ifMatchETag string) (newETag string, err error)

	// DeleteEvent removes a calendar object. Returns ErrPrecondition-
	// Failed on stale ETag; nil when the object is gone (HTTP 404 is
	// treated as success — the user already got what they wanted).
	DeleteEvent(ctx context.Context, path, ifMatchETag string) error
}
