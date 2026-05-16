// Package idgen produces opaque string identifiers used as primary keys for
// runs, traces, cards, concerns, observations, memory facts, and request IDs.
// Centralizing the generator gives tests a single seam if they ever need
// deterministic IDs and keeps the choice of UUID library out of every caller.
package idgen

import "github.com/google/uuid"

// New returns a fresh opaque identifier as a string.
func New() string {
	return uuid.New().String()
}
