package caldav

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

func TestParseVEVENT_TimedEvent(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	ev, err := ParseVEVENT(loadFixture(t, "timed.ics"), loc)
	require.NoError(t, err)
	require.Equal(t, "timed-1@example.test", ev.UID)
	require.Equal(t, "Series B redline review", ev.Title)
	require.Equal(t, "Conference room 4", ev.Location)
	require.Equal(t, "work", ev.Tag)
	require.Equal(t, time.Date(2026, 4, 25, 14, 0, 0, 0, loc), ev.Start.In(loc))
	require.Equal(t, time.Date(2026, 4, 25, 15, 0, 0, 0, loc), ev.End.In(loc))
}

func TestParseVEVENT_AllDay(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	ev, err := ParseVEVENT(loadFixture(t, "allday.ics"), loc)
	require.NoError(t, err)
	require.Equal(t, "Marathon", ev.Title)
	require.Equal(t, time.Date(2026, 4, 25, 0, 0, 0, 0, loc).Day(), ev.Start.In(loc).Day())
	require.True(t, ev.End.After(ev.Start))
}

func TestParseVEVENT_FloatingTime(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	ev, err := ParseVEVENT(loadFixture(t, "floating.ics"), loc)
	require.NoError(t, err)
	require.Equal(t, 10, ev.Start.In(loc).Hour())
	require.Equal(t, "America/Los_Angeles", ev.Start.In(loc).Location().String())
}

func TestParseVEVENT_RecurringFirstInstance(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	ev, err := ParseVEVENT(loadFixture(t, "recurring.ics"), loc)
	require.NoError(t, err)
	require.Equal(t, "Weekly standup", ev.Title)
	require.Equal(t, time.Date(2026, 4, 20, 9, 0, 0, 0, loc), ev.Start.In(loc), "first instance")
}

func TestParseVEVENT_AppleColor(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	ev, err := ParseVEVENT(loadFixture(t, "applecolor.ics"), loc)
	require.NoError(t, err)
	require.Equal(t, "personal", ev.Tag, "green Apple color → personal")
}

func TestParseVEVENT_Malformed(t *testing.T) {
	_, err := ParseVEVENT(loadFixture(t, "malformed.ics"), time.UTC)
	require.Error(t, err)
}

func TestParseVEVENT_EmptyICS(t *testing.T) {
	_, err := ParseVEVENT("", time.UTC)
	require.Error(t, err)
}

func TestParseVEVENT_AttendeesAndLastModified(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	ev, err := ParseVEVENT(loadFixture(t, "attendees.ics"), loc)
	require.NoError(t, err)
	require.Equal(t, []string{"Saru Patel", "Lin Vega", "Park Choi", "bare-no-cn"}, ev.Attendees,
		"CN= preferred; mailto local-part fallback for ATTENDEE without CN; ORGANIZER excluded")
	require.Equal(t, time.Date(2026, 4, 30, 15, 45, 0, 0, time.UTC), ev.LastModified.UTC(),
		"LAST-MODIFIED parsed in UTC")
}

func TestParseVEVENT_NoAttendeesNoLastModified(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	ev, err := ParseVEVENT(loadFixture(t, "timed.ics"), loc)
	require.NoError(t, err)
	require.Empty(t, ev.Attendees)
	require.True(t, ev.LastModified.IsZero(), "missing LAST-MODIFIED → zero time")
}
