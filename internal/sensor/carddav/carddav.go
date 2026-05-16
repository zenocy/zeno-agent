// Package carddav is the V2.12 CardDAV contacts sensor. It periodically
// polls a CardDAV address book (Fastmail today; the discovery flow is
// generic so any RFC 6352 server works) and upserts each vCard into
// internal/store.CardDAVContact rows. Read-side consumers (whatsapp.Resolver)
// query the cache; the live server is never hit on the resolution path.
//
// The sensor implements internal/sensor.Sensor so it plugs into the
// existing scheduler. It does NOT write to the observation log — contact
// rows are reference data, not observations. A boot-time prime sync runs
// the first poll synchronously so the contacts list is non-empty by the
// time the user opens the WhatsApp send action.
package carddav

import (
	"context"

	"github.com/emersion/go-webdav/carddav"
)

// Provider is the seam between the Sensor and a concrete CardDAV server
// integration. The Fastmail implementation wraps emersion/go-webdav/carddav;
// tests inject a stub returning canned AddressObject slices.
type Provider interface {
	// ListAll returns every AddressObject in the discovered address book.
	// V1 ignores the SyncCollection token path and does a full GET on each
	// poll — a hundreds-of-vCards address book is cheap to re-fetch and
	// the UID-keyed upsert collapses to a no-op for unchanged rows.
	// Deletions are computed by diffing the returned UID set against the
	// cached UIDs in the same Sync.
	ListAll(ctx context.Context) ([]carddav.AddressObject, error)
}
