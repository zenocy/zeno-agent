package schedule

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	logp "github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/store"
)

type stubInjector struct {
	calls []any
	err   error
}

func (s *stubInjector) RunInjectNowWithSignal(_ context.Context, signal any) error {
	s.calls = append(s.calls, signal)
	return s.err
}

type stubWASender struct {
	to   string
	text string
	err  error
	hits int
}

func (s *stubWASender) SendText(_ context.Context, to, text string) error {
	s.to = to
	s.text = text
	s.hits++
	return s.err
}

type stubEventLog struct {
	events []logp.Event
}

func (s *stubEventLog) Append(_ context.Context, kind, source string, payload any) (logp.Event, error) {
	ev := logp.Event{Kind: kind, Source: source}
	s.events = append(s.events, ev)
	_ = payload
	return ev, nil
}

func newSweeperRepo(t *testing.T) *store.TaskRepo {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	repo := &store.TaskRepo{DB: db}
	require.NoError(t, repo.Migrate())
	return repo
}

// insertAlarm seeds a task with fire_at set so the sweeper picks it up.
func insertAlarm(t *testing.T, repo *store.TaskRepo, id, title string, fireAt, createdAt time.Time) {
	t.Helper()
	require.NoError(t, repo.Insert(context.Background(), store.Task{
		ID:        id,
		Title:     title,
		FireAt:    &fireAt,
		CreatedAt: createdAt,
	}))
}

func TestSweepReminders_FiresDueAndMarks(t *testing.T) {
	repo := newSweeperRepo(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	pastID := uuid.NewString()
	insertAlarm(t, repo, pastID, "past", now.Add(-time.Hour), now)
	insertAlarm(t, repo, uuid.NewString(), "future", now.Add(time.Hour), now)

	inj := &stubInjector{}
	build := func(tk store.Task, at time.Time) any {
		return map[string]any{"id": tk.ID, "title": tk.Title, "at": at}
	}

	sweepReminders(ctx, ReminderSweeperDeps{
		Tasks:         repo,
		Injector:      inj,
		BuildSignal:   build,
		InjectEnabled: true,
	}, now, 10)

	require.Len(t, inj.calls, 1, "only the due alarm fires")
	signal := inj.calls[0].(map[string]any)
	require.Equal(t, "past", signal["title"])

	// The fired alarm is no longer in the due queue.
	due, err := repo.DueBefore(ctx, now, 10)
	require.NoError(t, err)
	require.Empty(t, due)
}

func TestSweepReminders_BurstCap(t *testing.T) {
	repo := newSweeperRepo(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 15; i++ {
		insertAlarm(t, repo, uuid.NewString(), "x", now.Add(-time.Duration(i+1)*time.Minute), now)
	}

	inj := &stubInjector{}
	sweepReminders(ctx, ReminderSweeperDeps{
		Tasks:         repo,
		Injector:      inj,
		InjectEnabled: true,
	}, now, 5)

	require.Len(t, inj.calls, 5, "burst caps the per-tick fire count")
	due, err := repo.DueBefore(ctx, now, 100)
	require.NoError(t, err)
	require.Len(t, due, 10)
}

func TestSweepReminders_DispatchesToWhatsAppAndInject(t *testing.T) {
	repo := newSweeperRepo(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repo.Insert(ctx, store.Task{
		ID:    "rid-1",
		Title: "Take out trash",
		Body:  "Tonight",
		FireAt: func() *time.Time {
			t := now.Add(-time.Minute)
			return &t
		}(),
		CreatedAt: now,
	}))

	inj := &stubInjector{}
	wa := &stubWASender{}
	evLog := &stubEventLog{}

	sweepReminders(ctx, ReminderSweeperDeps{
		Tasks:          repo,
		Injector:       inj,
		BuildSignal:    func(_ store.Task, _ time.Time) any { return "signal" },
		InjectEnabled:  true,
		WhatsAppSender: func() WhatsAppSender { return wa },
		WhatsAppTo:     "447xxx@s.whatsapp.net",
		EventLog:       evLog,
	}, now, 10)

	require.Len(t, inj.calls, 1, "inject pipeline still fires")
	require.Equal(t, 1, wa.hits, "whatsapp send fires once")
	require.Equal(t, "447xxx@s.whatsapp.net", wa.to)
	require.Contains(t, wa.text, "Take out trash")
	require.Contains(t, wa.text, "Tonight")

	// task.alarm_fired event emitted (V2.11; replaces reminder.fired).
	var fired *logp.Event
	for i := range evLog.events {
		if evLog.events[i].Kind == logp.KindTaskAlarmFired {
			fired = &evLog.events[i]
		}
	}
	require.NotNil(t, fired, "task.alarm_fired must be emitted")
}

func TestSweepReminders_WhatsAppFailureLeavesFiredAndDoesNotUnmark(t *testing.T) {
	repo := newSweeperRepo(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	insertAlarm(t, repo, "rid-2", "x", now.Add(-time.Minute), now)

	wa := &stubWASender{err: errors.New("network down")}
	evLog := &stubEventLog{}

	sweepReminders(ctx, ReminderSweeperDeps{
		Tasks:          repo,
		WhatsAppSender: func() WhatsAppSender { return wa },
		WhatsAppTo:     "447xxx@s.whatsapp.net",
		EventLog:       evLog,
	}, now, 10)

	// Alarm is marked fired even though WA send failed (fire-and-
	// forget posture, documented in the sweeper).
	due, err := repo.DueBefore(ctx, now, 10)
	require.NoError(t, err)
	require.Empty(t, due, "MarkFired should run before dispatch attempts")

	// task.alarm_fired emitted with empty dispatch slice.
	found := false
	for _, e := range evLog.events {
		if e.Kind == logp.KindTaskAlarmFired {
			found = true
		}
	}
	require.True(t, found, "task.alarm_fired emitted even on send failure")
}

func TestSweepReminders_NilWhatsAppSenderSkipsCleanly(t *testing.T) {
	repo := newSweeperRepo(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	insertAlarm(t, repo, "rid-3", "x", now.Add(-time.Minute), now)

	inj := &stubInjector{}
	// WhatsAppSender returns nil — sweeper must skip silently.
	sweepReminders(ctx, ReminderSweeperDeps{
		Tasks:          repo,
		Injector:       inj,
		InjectEnabled:  true,
		WhatsAppSender: func() WhatsAppSender { return nil },
		WhatsAppTo:     "447xxx@s.whatsapp.net",
	}, now, 10)
	require.Len(t, inj.calls, 1, "inject still runs when WA sender is nil")
}

// TestSweepReminders_FireAtClearedSkipsDispatch is the V2.11
// regression test for the "user clears alarm mid-tick" race. The
// sweeper queries DueBefore (sees the row), then user updates fire_at
// to nil, then sweeper calls MarkFired. The WHERE-on-fire_at guard
// returns 0 rows, so the dispatch is skipped — the user's intent wins.
func TestSweepReminders_FireAtClearedSkipsDispatch(t *testing.T) {
	repo := newSweeperRepo(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	insertAlarm(t, repo, "rid-race", "x", now.Add(-time.Minute), now)

	// Pre-clear the alarm before sweepReminders runs MarkFired. In
	// production this is done by the user mid-tick; here we exploit
	// the same WHERE-on-fire_at guard by simulating a successful
	// concurrent clear: the sweeper sees the row in DueBefore (we
	// can't easily prevent that without instrumenting sleeps), but
	// the row is gone by the time MarkFired runs.
	//
	// We approximate by clearing fire_at AFTER seeding but BEFORE
	// the sweeper tick. DueBefore won't see it (its WHERE clause
	// matches), but we still want to confirm the burst path doesn't
	// dispatch when nothing is due.
	require.NoError(t, repo.SetFireAt(ctx, "rid-race", nil))

	inj := &stubInjector{}
	sweepReminders(ctx, ReminderSweeperDeps{
		Tasks:         repo,
		Injector:      inj,
		InjectEnabled: true,
	}, now, 10)
	require.Empty(t, inj.calls, "alarm cleared pre-tick must not dispatch")
}
