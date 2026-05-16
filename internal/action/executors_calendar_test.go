package action

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	logp "github.com/zenocy/zeno-v2/internal/log"
	caldavsensor "github.com/zenocy/zeno-v2/internal/sensor/caldav"
)

// stubCalDAV is the in-memory Provider used by the calendar Executor
// tests. PUTs are recorded; UpdateEvent honors a sticky precondition
// failure flag for testing the 412 path.
type stubCalDAV struct {
	events map[string]*caldavsensor.RawEvent // by UID
	puts   []putRecord
	updates []updateRecord
	deletes []deleteRecord

	updateErr error
	putErr    error
	getErr    error
}

type putRecord struct {
	UID  string
	ICS  string
	Path string
	ETag string
}
type updateRecord struct {
	Path     string
	ICS      string
	IfMatch  string
	NewETag  string
}
type deleteRecord struct {
	Path    string
	IfMatch string
}

func newStubCalDAV() *stubCalDAV {
	return &stubCalDAV{events: map[string]*caldavsensor.RawEvent{}}
}

func (s *stubCalDAV) ListEvents(_ context.Context, _, _ time.Time) ([]caldavsensor.RawEvent, error) {
	out := make([]caldavsensor.RawEvent, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, *e)
	}
	return out, nil
}

func (s *stubCalDAV) GetEvent(_ context.Context, uid string) (*caldavsensor.RawEvent, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if e, ok := s.events[uid]; ok {
		cp := *e
		return &cp, nil
	}
	return nil, nil
}

func (s *stubCalDAV) PutEvent(_ context.Context, uid, ics string) (string, string, error) {
	if s.putErr != nil {
		return "", "", s.putErr
	}
	path := "/cal/" + uid + ".ics"
	etag := `"etag-` + uid + `"`
	s.events[uid] = &caldavsensor.RawEvent{UID: uid, ICS: ics, ETag: etag, Path: path}
	s.puts = append(s.puts, putRecord{UID: uid, ICS: ics, Path: path, ETag: etag})
	return etag, path, nil
}

func (s *stubCalDAV) UpdateEvent(_ context.Context, path, ics, ifMatchETag string) (string, error) {
	if s.updateErr != nil {
		return "", s.updateErr
	}
	for uid, e := range s.events {
		if e.Path == path {
			if ifMatchETag != "" && ifMatchETag != e.ETag {
				return "", caldavsensor.ErrPreconditionFailed
			}
			newETag := `"etag-` + uid + `-v2"`
			e.ICS = ics
			e.ETag = newETag
			s.updates = append(s.updates, updateRecord{Path: path, ICS: ics, IfMatch: ifMatchETag, NewETag: newETag})
			return newETag, nil
		}
	}
	return "", errors.New("not found")
}

func (s *stubCalDAV) DeleteEvent(_ context.Context, path, ifMatchETag string) error {
	for uid, e := range s.events {
		if e.Path == path {
			if ifMatchETag != "" && ifMatchETag != e.ETag {
				return caldavsensor.ErrPreconditionFailed
			}
			delete(s.events, uid)
			s.deletes = append(s.deletes, deleteRecord{Path: path, IfMatch: ifMatchETag})
			return nil
		}
	}
	return nil
}

// ----------------------------------------------------------------------
// AddEventExec
// ----------------------------------------------------------------------

func TestAddEvent_PreflightDoesNotPut(t *testing.T) {
	s := newStubCalDAV()
	ex := &AddEventExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}
	require.Equal(t, ModePreflight, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: false,
		Today:   "2026-05-07",
		TZ:      time.UTC,
		Target:  map[string]any{"title": "Coffee with Lin", "start": "10:00", "end": "10:30"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.True(t, res.NeedsConfirm)
	require.Empty(t, s.puts, "preview must not write")
	require.NotNil(t, res.Preview)
	require.Equal(t, "Coffee with Lin", res.Preview["title"])
}

func TestAddEvent_CommitWrites(t *testing.T) {
	s := newStubCalDAV()
	ex := &AddEventExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}

	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: true,
		Today:   "2026-05-07",
		TZ:      time.UTC,
		Target:  map[string]any{"title": "Coffee with Lin", "start": "10:00", "end": "10:30"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindCalEventCreated, res.EventKind)
	require.Len(t, s.puts, 1)
	require.Contains(t, s.puts[0].ICS, "SUMMARY:Coffee with Lin")
}

// ----------------------------------------------------------------------
// BlockCalendarExec
// ----------------------------------------------------------------------

func TestBlockCalendar_OneClick(t *testing.T) {
	s := newStubCalDAV()
	ex := &BlockCalendarExec{Deps: CalendarDeps{Provider: s}}
	require.Equal(t, Mode1Click, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{
		Today:  "2026-05-07",
		TZ:     time.UTC,
		Target: map[string]any{"title": "Lia's recital", "start": "17:00", "end": "18:30"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindCalEventBlocked, res.EventKind)
	require.Len(t, s.puts, 1)
	require.Contains(t, s.puts[0].ICS, "CATEGORIES:personal")
}

func TestBlockCalendar_BadTimes(t *testing.T) {
	ex := &BlockCalendarExec{Deps: CalendarDeps{Provider: newStubCalDAV()}}
	res, err := ex.Execute(context.Background(), ExecCtx{
		Today:  "2026-05-07",
		TZ:     time.UTC,
		Target: map[string]any{"title": "x", "start": "garbage"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
}

// ----------------------------------------------------------------------
// RSVP Executors
// ----------------------------------------------------------------------

func seedInvite(t *testing.T, s *stubCalDAV, uid, summary, userMail string) {
	t.Helper()
	src := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//test//EN",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:20260507T120000Z",
		"DTSTART:20260507T170000Z",
		"DTEND:20260507T180000Z",
		"SUMMARY:" + summary,
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:" + userMail,
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")
	s.events[uid] = &caldavsensor.RawEvent{
		UID:  uid,
		ICS:  src,
		ETag: `"etag-` + uid + `"`,
		Path: "/cal/" + uid + ".ics",
	}
}

func TestRsvpYes_FlipsPartStat(t *testing.T) {
	s := newStubCalDAV()
	seedInvite(t, s, "evt-1", "Series B narrative review", "user@example.com")

	ex := &RsvpYesExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}
	require.Equal(t, Mode1Click, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{
		Target: map[string]any{"uid": "evt-1"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindCalRSVPSent, res.EventKind)
	require.Len(t, s.updates, 1)
	require.Contains(t, s.updates[0].ICS, "ACCEPTED")
}

func TestRsvpNo(t *testing.T) {
	s := newStubCalDAV()
	seedInvite(t, s, "evt-2", "Lunch with Sam", "user@example.com")
	ex := &RsvpNoExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}
	res, err := ex.Execute(context.Background(), ExecCtx{Target: map[string]any{"uid": "evt-2"}})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Contains(t, s.updates[0].ICS, "DECLINED")
}

func TestRsvpMaybe(t *testing.T) {
	s := newStubCalDAV()
	seedInvite(t, s, "evt-3", "Pickup", "user@example.com")
	ex := &RsvpMaybeExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}
	res, err := ex.Execute(context.Background(), ExecCtx{Target: map[string]any{"uid": "evt-3"}})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Contains(t, s.updates[0].ICS, "TENTATIVE")
}

func TestRsvp_412SurfaceasToast(t *testing.T) {
	s := newStubCalDAV()
	seedInvite(t, s, "evt-4", "Race", "user@example.com")
	s.updateErr = caldavsensor.ErrPreconditionFailed

	ex := &RsvpYesExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}
	res, err := ex.Execute(context.Background(), ExecCtx{Target: map[string]any{"uid": "evt-4"}})
	require.Error(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "changed elsewhere")
}

func TestRsvp_MissingEvent(t *testing.T) {
	s := newStubCalDAV()
	ex := &RsvpYesExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}
	res, err := ex.Execute(context.Background(), ExecCtx{Target: map[string]any{"uid": "missing"}})
	require.NoError(t, err)
	require.False(t, res.OK)
}

// ----------------------------------------------------------------------
// RescheduleEventExec
// ----------------------------------------------------------------------

func TestReschedule_PreflightDoesNotPut(t *testing.T) {
	s := newStubCalDAV()
	seedInvite(t, s, "evt-r1", "Series B", "user@example.com")

	ex := &RescheduleEventExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}
	require.Equal(t, ModePreflight, ex.Mode())

	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: false,
		Today:   "2026-05-07",
		TZ:      time.UTC,
		Target:  map[string]any{"uid": "evt-r1", "start": "16:00", "end": "17:00"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.True(t, res.NeedsConfirm)
	require.Empty(t, s.updates, "preview must not write")
	require.Equal(t, "Series B", res.Preview["title"])
}

func TestReschedule_CommitMutatesTimes(t *testing.T) {
	s := newStubCalDAV()
	seedInvite(t, s, "evt-r2", "Pricing v2", "user@example.com")

	ex := &RescheduleEventExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}
	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: true,
		Today:   "2026-05-07",
		TZ:      time.UTC,
		Target:  map[string]any{"uid": "evt-r2", "start": "16:00", "end": "17:00"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindCalEventRescheduled, res.EventKind)
	require.Len(t, s.updates, 1)
	// New DTSTART/DTEND visible in the updated ICS.
	require.Contains(t, s.updates[0].ICS, "20260507T160000")
}

func TestReschedule_412Surfaces(t *testing.T) {
	s := newStubCalDAV()
	seedInvite(t, s, "evt-r3", "Race", "user@example.com")
	s.updateErr = caldavsensor.ErrPreconditionFailed

	ex := &RescheduleEventExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}
	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: true,
		Today:   "2026-05-07",
		TZ:      time.UTC,
		Target:  map[string]any{"uid": "evt-r3", "start": "10:00", "end": "11:00"},
	})
	require.Error(t, err)
	require.False(t, res.OK)
	require.Contains(t, res.Toast, "changed elsewhere")
}

// ----------------------------------------------------------------------
// CancelEventExec
// ----------------------------------------------------------------------

func TestCancel_RequiresConfirm(t *testing.T) {
	s := newStubCalDAV()
	seedInvite(t, s, "evt-c1", "Lunch", "user@example.com")
	ex := &CancelEventExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}
	require.Equal(t, ModeConfirm, ex.Mode())

	// confirm=false rejected.
	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: false,
		Target:  map[string]any{"uid": "evt-c1"},
	})
	require.NoError(t, err)
	require.False(t, res.OK)
	require.Empty(t, s.deletes)

	// confirm=true commits the delete.
	res, err = ex.Execute(context.Background(), ExecCtx{
		Confirm: true,
		Target:  map[string]any{"uid": "evt-c1"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
	require.Equal(t, logp.KindCalEventCancelled, res.EventKind)
	require.Len(t, s.deletes, 1)
}

func TestCancel_AlreadyGoneIsSuccess(t *testing.T) {
	s := newStubCalDAV() // no event seeded
	ex := &CancelEventExec{Deps: CalendarDeps{Provider: s, UserMail: "mailto:user@example.com"}}
	res, err := ex.Execute(context.Background(), ExecCtx{
		Confirm: true,
		Target:  map[string]any{"uid": "missing"},
	})
	require.NoError(t, err)
	require.True(t, res.OK)
}

func TestUserMailtoFromConfig(t *testing.T) {
	require.Equal(t, "mailto:user@example.com", UserMailtoFromConfig("user@example.com"))
	require.Equal(t, "", UserMailtoFromConfig("jamie")) // not an email
	require.Equal(t, "", UserMailtoFromConfig(""))
}

// resolveStartEnd accepts HH:MM wall-clock + date (the "happy path"
// the prompt asks for).
func TestResolveStartEnd_WallClockWithDate(t *testing.T) {
	tz, _ := time.LoadLocation("Europe/Nicosia")
	target := map[string]any{
		"start": "19:00",
		"end":   "21:00",
		"date":  "2026-05-10",
	}
	start, end, err := resolveStartEnd(target, "2026-05-10", tz)
	require.NoError(t, err)
	require.Equal(t, "2026-05-10T19:00:00+03:00", start.Format(time.RFC3339))
	require.Equal(t, "2026-05-10T21:00:00+03:00", end.Format(time.RFC3339))
}

// LLM regression: the model frequently inlines a full naive datetime
// into `start`/`end` instead of routing to `start_iso`/`end_iso`.
// Pre-fix this concatenated date+naive into "2026-05-10 2026-05-10T19:00:00"
// and tripped the strict format parser. Now `start` containing a `T`
// is parsed as a naive datetime in the user's tz.
func TestResolveStartEnd_NaiveDatetimeInStart(t *testing.T) {
	tz, _ := time.LoadLocation("Europe/Nicosia")
	target := map[string]any{
		"start": "2026-05-10T19:00:00",
		"end":   "2026-05-10T21:00:00",
		"date":  "2026-05-10", // ignored when start carries its own date
	}
	start, end, err := resolveStartEnd(target, "2026-05-10", tz)
	require.NoError(t, err)
	require.Equal(t, "2026-05-10T19:00:00+03:00", start.Format(time.RFC3339))
	require.Equal(t, "2026-05-10T21:00:00+03:00", end.Format(time.RFC3339))
}

// RFC3339 with explicit tz inlined into `start` should also work —
// the LLM occasionally emits this when it picks up the user's tz from
// the prompt context.
func TestResolveStartEnd_RFC3339InStart(t *testing.T) {
	tz, _ := time.LoadLocation("Europe/Nicosia")
	target := map[string]any{
		"start": "2026-05-10T19:00:00+03:00",
		"end":   "2026-05-10T21:00:00+03:00",
	}
	start, end, err := resolveStartEnd(target, "2026-05-10", tz)
	require.NoError(t, err)
	require.True(t, start.Equal(time.Date(2026, 5, 10, 19, 0, 0, 0, tz)))
	require.True(t, end.Equal(time.Date(2026, 5, 10, 21, 0, 0, 0, tz)))
}

// Missing one of start/end should reject cleanly rather than silently
// returning a zero time.
func TestResolveStartEnd_MissingFieldsFails(t *testing.T) {
	tz, _ := time.LoadLocation("Europe/Nicosia")
	_, _, err := resolveStartEnd(map[string]any{"start": "19:00"}, "2026-05-10", tz)
	require.Error(t, err)
	require.Contains(t, err.Error(), "required")
}
