package caldav

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuildEventICS_Roundtrip(t *testing.T) {
	tz := time.UTC
	spec := EventSpec{
		Title:      "Lia's recital",
		Start:      time.Date(2026, 5, 7, 17, 0, 0, 0, tz),
		End:        time.Date(2026, 5, 7, 18, 30, 0, 0, tz),
		Location:   "Mariastrasse 14",
		Categories: []string{"personal"},
	}

	ics, uid, err := BuildEventICS(spec)
	require.NoError(t, err)
	require.NotEmpty(t, uid)
	require.Contains(t, ics, "BEGIN:VCALENDAR")
	require.Contains(t, ics, "BEGIN:VEVENT")
	require.Contains(t, ics, "SUMMARY:Lia's recital")
	require.Contains(t, ics, "LOCATION:Mariastrasse 14")
	require.Contains(t, ics, "CATEGORIES:personal")

	title, start, end, err := EventSummary(ics)
	require.NoError(t, err)
	require.Equal(t, "Lia's recital", title)
	require.True(t, start.Equal(spec.Start), "start mismatch: %v vs %v", start, spec.Start)
	require.True(t, end.Equal(spec.End), "end mismatch: %v vs %v", end, spec.End)
}

func TestBuildEventICS_RequiresValidRange(t *testing.T) {
	tz := time.UTC
	_, _, err := BuildEventICS(EventSpec{
		Title: "x",
		Start: time.Date(2026, 5, 7, 18, 0, 0, 0, tz),
		End:   time.Date(2026, 5, 7, 17, 0, 0, 0, tz), // end before start
	})
	require.Error(t, err)
}

func TestSetAttendeePartStat_UpdatesMatchingMailto(t *testing.T) {
	source := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//test//EN",
		"BEGIN:VEVENT",
		"UID:event-1@example",
		"DTSTAMP:20260507T120000Z",
		"DTSTART:20260507T170000Z",
		"DTEND:20260507T180000Z",
		"SUMMARY:Series B narrative review",
		"ATTENDEE;CN=Saru Patel;PARTSTAT=ACCEPTED:mailto:saru@example.com",
		"ATTENDEE;CN=Jamie Reyes;PARTSTAT=NEEDS-ACTION:mailto:user@example.com",
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")

	updated, err := SetAttendeePartStat(source, "user@example.com", PartStatAccepted)
	require.NoError(t, err)
	require.Contains(t, updated, "ACCEPTED")
	require.Contains(t, updated, "user@example.com")
	// The other attendee's PARTSTAT must be unchanged.
	require.Contains(t, updated, "saru@example.com")
}

func TestSetAttendeePartStat_AppendsWhenMissing(t *testing.T) {
	source := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//test//EN",
		"BEGIN:VEVENT",
		"UID:event-2@example",
		"DTSTAMP:20260507T120000Z",
		"DTSTART:20260507T170000Z",
		"DTEND:20260507T180000Z",
		"SUMMARY:Solo block",
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")

	updated, err := SetAttendeePartStat(source, "user@example.com", PartStatTentative)
	require.NoError(t, err)
	require.Contains(t, updated, "user@example.com")
	require.Contains(t, updated, "TENTATIVE")
}

func TestEventSummary_NoEvent(t *testing.T) {
	src := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nEND:VCALENDAR\r\n"
	_, _, _, err := EventSummary(src)
	require.Error(t, err)
}
