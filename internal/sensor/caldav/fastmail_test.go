package caldav

import (
	"bytes"
	"strings"
	"testing"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
	"github.com/stretchr/testify/require"
)

// supportsEvents must return true when the server omits the
// supported-component-set entirely (Apple iCloud does this on home calendars)
// — assuming "no" would silently hide every calendar from sync.
func TestSupportsEvents_EmptySetIsTrue(t *testing.T) {
	require.True(t, supportsEvents(caldav.Calendar{}))
}

func TestSupportsEvents_EventInSet(t *testing.T) {
	require.True(t, supportsEvents(caldav.Calendar{
		SupportedComponentSet: []string{ical.CompEvent, ical.CompToDo},
	}))
}

// A task-only calendar (CompTodo only, no VEVENT) must be filtered out — its
// QueryCalendar would return empty and waste a round-trip per poll.
func TestSupportsEvents_TodoOnlyExcluded(t *testing.T) {
	require.False(t, supportsEvents(caldav.Calendar{
		SupportedComponentSet: []string{ical.CompToDo},
	}))
}

func TestPrimaryUID_NilCalendar(t *testing.T) {
	require.Equal(t, "", primaryUID(nil))
}

func TestPrimaryUID_FirstVEVENT(t *testing.T) {
	cal := decodeICS(t, sampleTimedICS)
	require.Equal(t, "timed-1@example.test", primaryUID(cal))
}

// Calendar without VEVENT (e.g. a VTODO-only payload) returns "" so the
// caller skips it instead of attaching a useless RawEvent.
func TestPrimaryUID_NoVEVENTReturnsEmpty(t *testing.T) {
	const todoOnly = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//EN
BEGIN:VTODO
UID:todo-1@example.test
SUMMARY:A todo
END:VTODO
END:VCALENDAR
`
	cal := decodeICS(t, todoOnly)
	require.Equal(t, "", primaryUID(cal))
}

func TestEncodeCalendar_RoundTrip(t *testing.T) {
	cal := decodeICS(t, sampleTimedICS)
	out, err := encodeCalendar(cal)
	require.NoError(t, err)
	require.Contains(t, out, "BEGIN:VCALENDAR")
	require.Contains(t, out, "UID:timed-1@example.test")
	require.Contains(t, out, "END:VCALENDAR")
}

func TestEncodeCalendar_NilReturnsError(t *testing.T) {
	_, err := encodeCalendar(nil)
	require.Error(t, err)
}

func decodeICS(t *testing.T, ics string) *ical.Calendar {
	t.Helper()
	dec := ical.NewDecoder(bytes.NewBufferString(strings.TrimSpace(ics)))
	cal, err := dec.Decode()
	require.NoError(t, err)
	return cal
}

const sampleTimedICS = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//zeno-test//EN
BEGIN:VEVENT
UID:timed-1@example.test
DTSTAMP:20260425T120000Z
DTSTART;TZID=America/Los_Angeles:20260425T140000
DTEND;TZID=America/Los_Angeles:20260425T150000
SUMMARY:Series B redline review
LOCATION:Conference room 4
END:VEVENT
END:VCALENDAR
`

// splitOccurrences must keep the original UID untouched for non-recurring
// events — rewriting it would orphan every existing log entry on first
// sync after deploy.
func TestSplitOccurrences_SingleEventKeepsUID(t *testing.T) {
	cal := decodeICS(t, sampleTimedICS)
	occs, err := splitOccurrences(cal)
	require.NoError(t, err)
	require.Len(t, occs, 1)
	require.Equal(t, "timed-1@example.test", occs[0].uid,
		"non-recurring event must keep its original UID")
	require.Contains(t, occs[0].ics, "UID:timed-1@example.test")
}

// When the CalDAV server expands a recurring event server-side
// (RFC 4791 <C:expand>), the response contains multiple VEVENTs sharing
// one UID, each with its own DTSTART and a RECURRENCE-ID. splitOccurrences
// must emit one occurrence per VEVENT with a per-occurrence synthetic UID
// so the projection's `latest[UID] = raw` fold doesn't collapse them.
// This is the heart of the "tomorrow's recurring event missing" bug fix.
func TestSplitOccurrences_ExpandedRecurrenceProducesPerOccurrenceUID(t *testing.T) {
	const expandedICS = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//zeno-test//EN
BEGIN:VEVENT
UID:weekly@example.test
DTSTAMP:20260425T120000Z
DTSTART:20260511T080000Z
DTEND:20260511T090000Z
RECURRENCE-ID:20260511T080000Z
SUMMARY:Weekly standup
END:VEVENT
BEGIN:VEVENT
UID:weekly@example.test
DTSTAMP:20260425T120000Z
DTSTART:20260518T080000Z
DTEND:20260518T090000Z
RECURRENCE-ID:20260518T080000Z
SUMMARY:Weekly standup
END:VEVENT
END:VCALENDAR
`
	cal := decodeICS(t, expandedICS)
	occs, err := splitOccurrences(cal)
	require.NoError(t, err)
	require.Len(t, occs, 2, "must emit one occurrence per expanded VEVENT")
	require.Equal(t, "weekly@example.test#20260511T080000Z", occs[0].uid)
	require.Equal(t, "weekly@example.test#20260518T080000Z", occs[1].uid)
	require.NotEqual(t, occs[0].uid, occs[1].uid,
		"per-occurrence UIDs must be distinct so downstream folds don't collapse them")
}
