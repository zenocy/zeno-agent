package caldav

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/sensor"
)

// stubProvider returns a fixed slice of RawEvents (or an error) on each call.
type stubProvider struct {
	mu     sync.Mutex
	events []RawEvent
	err    error
	calls  int
}

func (p *stubProvider) ListEvents(_ context.Context, _, _ time.Time) ([]RawEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return append([]RawEvent(nil), p.events...), nil
}

func (p *stubProvider) set(raws ...RawEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = raws
}

// V2.8 write methods — poller-test stubs only need to satisfy the
// interface. Action-side tests use a richer fake in package action.

func (p *stubProvider) GetEvent(_ context.Context, _ string) (*RawEvent, error) {
	return nil, nil
}
func (p *stubProvider) PutEvent(_ context.Context, _, _ string) (string, string, error) {
	return "", "", nil
}
func (p *stubProvider) UpdateEvent(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}
func (p *stubProvider) DeleteEvent(_ context.Context, _, _ string) error { return nil }

func quietEntry() *logrus.Entry {
	l := logrus.New()
	l.Out = io.Discard
	return l.WithField("c", "caldav-test")
}

func newSensor(p *stubProvider, mem *logtest.MemReader, fakeNow time.Time) *Sensor {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	cfg := config.CalDAVConfig{URL: "https://caldav.test/", Username: "u", Password: "p"}
	s := New(cfg, FixedLocation(loc), p, mem, mem, quietEntry())
	s.WithNow(func() time.Time { return fakeNow })
	return s
}

func makeICS(uid, title, location, tag string, startHour int) string {
	tagLine := ""
	if tag != "" {
		tagLine = "CATEGORIES:" + tag + "\r\n"
	}
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//zeno-test//EN\r\nBEGIN:VEVENT\r\nUID:" + uid +
		"\r\nDTSTART;TZID=America/Los_Angeles:20260425T" + twoDigit(startHour) + "0000\r\nDTEND;TZID=America/Los_Angeles:20260425T" + twoDigit(startHour+1) + "0000\r\nSUMMARY:" + title +
		"\r\nLOCATION:" + location + "\r\n" + tagLine + "END:VEVENT\r\nEND:VCALENDAR\r\n"
}

func twoDigit(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func decodeEvent(t *testing.T, e log.Event) eventPayload {
	t.Helper()
	var p eventPayload
	require.NoError(t, json.Unmarshal(e.Payload, &p))
	return p
}

func loadDataPlain() []RawEvent {
	return []RawEvent{
		{UID: "e1", ICS: makeICS("e1", "Standup", "Slack", "Work", 9), ETag: "etag-1"},
		{UID: "e2", ICS: makeICS("e2", "Lunch", "Tartine", "Personal", 12), ETag: "etag-2"},
		{UID: "e3", ICS: makeICS("e3", "Series B review", "Room 4", "Work", 14), ETag: "etag-3"},
		{UID: "e4", ICS: makeICS("e4", "Drinks", "ABV", "", 18), ETag: "etag-4"},
	}
}

func TestSync_FirstRun(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, loc)

	p := &stubProvider{}
	p.set(loadDataPlain()...)
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, now)

	require.NoError(t, s.Sync(context.Background()))

	seen, _ := mem.ByKind(context.Background(), log.KindCalEventSeen)
	require.Len(t, seen, 4)
	changed, _ := mem.ByKind(context.Background(), log.KindCalEventChanged)
	require.Empty(t, changed)
}

func TestSync_NoChanges(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, loc)

	p := &stubProvider{}
	p.set(loadDataPlain()...)
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, now)

	require.NoError(t, s.Sync(context.Background()))
	require.NoError(t, s.Sync(context.Background()))

	seen, _ := mem.ByKind(context.Background(), log.KindCalEventSeen)
	require.Len(t, seen, 4, "no duplicates on second run with identical events")
	changed, _ := mem.ByKind(context.Background(), log.KindCalEventChanged)
	require.Empty(t, changed)
}

func TestSync_TitleChanged(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, loc)

	p := &stubProvider{}
	p.set(loadDataPlain()...)
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, now)
	require.NoError(t, s.Sync(context.Background()))

	updated := loadDataPlain()
	updated[2].ICS = makeICS("e3", "Series B FINAL review", "Room 4", "Work", 14)
	p.set(updated...)
	require.NoError(t, s.Sync(context.Background()))

	changed, _ := mem.ByKind(context.Background(), log.KindCalEventChanged)
	require.Len(t, changed, 1)
	require.Equal(t, "Series B FINAL review", decodeEvent(t, changed[0]).Title)
}

func TestSync_ETagChangedFieldsSame(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, loc)

	p := &stubProvider{}
	p.set(loadDataPlain()...)
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, now)
	require.NoError(t, s.Sync(context.Background()))

	flipped := loadDataPlain()
	for i := range flipped {
		flipped[i].ETag = "v2-" + flipped[i].ETag
	}
	p.set(flipped...)
	require.NoError(t, s.Sync(context.Background()))

	changed, _ := mem.ByKind(context.Background(), log.KindCalEventChanged)
	require.Empty(t, changed, "etag flip alone must not produce events")
}

func decodeListSnapshot(t *testing.T, e log.Event) listSnapshotPayload {
	t.Helper()
	var p listSnapshotPayload
	require.NoError(t, json.Unmarshal(e.Payload, &p))
	return p
}

func TestSync_EmitsListSnapshot(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, loc)

	p := &stubProvider{}
	p.set(loadDataPlain()...)
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, now)

	require.NoError(t, s.Sync(context.Background()))

	snaps, _ := mem.ByKind(context.Background(), log.KindCalListSnapshot)
	require.Len(t, snaps, 1)
	got := decodeListSnapshot(t, snaps[0])
	require.Equal(t, []string{"e1", "e2", "e3", "e4"}, got.UIDs, "snapshot must list parsed UIDs sorted")
}

func TestSync_ListSnapshotShrinksWhenEventDeleted(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, loc)

	p := &stubProvider{}
	p.set(loadDataPlain()...)
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, now)
	require.NoError(t, s.Sync(context.Background()))

	// Drop e2 from second listing — user deleted it in their calendar UI.
	shorter := loadDataPlain()
	p.set(shorter[0], shorter[2], shorter[3])
	require.NoError(t, s.Sync(context.Background()))

	snaps, _ := mem.ByKind(context.Background(), log.KindCalListSnapshot)
	require.Len(t, snaps, 2)
	require.Equal(t, []string{"e1", "e2", "e3", "e4"}, decodeListSnapshot(t, snaps[0]).UIDs)
	require.Equal(t, []string{"e1", "e3", "e4"}, decodeListSnapshot(t, snaps[1]).UIDs,
		"second snapshot must reflect the deleted UID")
}

func TestSync_DeletedEventNotEmitted(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, loc)

	p := &stubProvider{}
	p.set(loadDataPlain()...)
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, now)
	require.NoError(t, s.Sync(context.Background()))

	// Drop e2 from second listing.
	shorter := loadDataPlain()
	p.set(shorter[0], shorter[2], shorter[3])
	require.NoError(t, s.Sync(context.Background()))

	seen, _ := mem.ByKind(context.Background(), log.KindCalEventSeen)
	require.Len(t, seen, 4, "no new seen events")
	changed, _ := mem.ByKind(context.Background(), log.KindCalEventChanged)
	require.Empty(t, changed, "Phase 1 does not emit deletions")
}

func TestSync_ProviderError(t *testing.T) {
	p := &stubProvider{err: errors.New("network down")}
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, time.Now())

	err := s.Sync(context.Background())
	require.Error(t, err)
	require.Empty(t, mem.Events())
}

// fakePub records events for the V2.4 publish-after-append tests.
type fakePub struct {
	mu     sync.Mutex
	events []eventbus.Event
}

func (f *fakePub) Publish(ev eventbus.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *fakePub) observed() []eventbus.SensorEventObservedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []eventbus.SensorEventObservedEvent
	for _, ev := range f.events {
		if obs, ok := ev.(eventbus.SensorEventObservedEvent); ok {
			out = append(out, obs)
		}
	}
	return out
}

// filterByKind returns only the observations whose Kind_ matches `kind`.
// Used by the V2.4 reactive-trigger tests so they stay pinned to the
// kinds they're asserting on (cal.event_seen / cal.event_changed) and
// don't flap on V2.6+ additions like cal.list_snapshot.
func filterByKind(in []eventbus.SensorEventObservedEvent, kind string) []eventbus.SensorEventObservedEvent {
	out := make([]eventbus.SensorEventObservedEvent, 0, len(in))
	for _, ev := range in {
		if ev.Kind_ == kind {
			out = append(out, ev)
		}
	}
	return out
}

// V2.4: a first sync with new UIDs publishes one cal.event_seen
// SensorEventObservedEvent per appended log row.
//
// V2.6: cal.list_snapshot publishes an additional bus-internal
// observation per sync so the projection publisher can recompute today's
// calendar for the SSE wire. The assertions filter by kind so the
// V2.4 reactive-trigger contract stays pinned to event_seen/event_changed.
func TestSync_PublishesEventSeenForNewUID(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, loc)

	p := &stubProvider{}
	p.set(loadDataPlain()...)
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, now)

	pub := &fakePub{}
	ctx := sensor.ContextWithPublisher(context.Background(), pub)
	require.NoError(t, s.Sync(ctx))

	seen, _ := mem.ByKind(context.Background(), log.KindCalEventSeen)
	require.Len(t, seen, 4)

	obs := filterByKind(pub.observed(), "cal.event_seen")
	require.Len(t, obs, 4, "one publish per cal.event_seen append")

	uidsByObs := map[string]bool{}
	for _, o := range obs {
		require.Equal(t, "cal.event_seen", o.Kind_)
		require.NotEmpty(t, o.EvidenceID, "evidence ID = UID, must be non-empty")
		require.Equal(t, o.EvidenceID, o.Payload["uid"], "evidence ID and payload UID align")
		uidsByObs[o.EvidenceID] = true
	}
	for _, want := range []string{"e1", "e2", "e3", "e4"} {
		require.True(t, uidsByObs[want], "missing publish for UID %s", want)
	}
}

// V2.4: a second sync with a title change on an existing UID publishes
// cal.event_changed (matching the V2.3 fieldsDiffer behavior).
func TestSync_PublishesEventChangedOnFieldDiff(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, loc)

	p := &stubProvider{}
	p.set(loadDataPlain()...)
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, now)

	pub := &fakePub{}
	ctx := sensor.ContextWithPublisher(context.Background(), pub)

	require.NoError(t, s.Sync(ctx))
	seenCount := len(filterByKind(pub.observed(), "cal.event_seen"))
	require.Equal(t, 4, seenCount, "first sync: 4 cal.event_seen publishes")

	updated := loadDataPlain()
	updated[2].ICS = makeICS("e3", "Series B FINAL review", "Room 4", "Work", 14)
	p.set(updated...)
	require.NoError(t, s.Sync(ctx))

	all := pub.observed()
	seen := filterByKind(all, "cal.event_seen")
	changed := filterByKind(all, "cal.event_changed")
	require.Len(t, seen, 4, "first sync 4 event_seen")
	require.Len(t, changed, 1, "second sync: one event_changed for e3")
	require.Equal(t, "e3", changed[0].EvidenceID)
}

// V2.4: a re-sync with identical state must not produce any new
// publishes (matches the V2.3 "no log append on no-change" behavior).
func TestSync_NoPublishForUnchangedEvent(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, loc)

	p := &stubProvider{}
	p.set(loadDataPlain()...)
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, now)

	pub := &fakePub{}
	ctx := sensor.ContextWithPublisher(context.Background(), pub)

	require.NoError(t, s.Sync(ctx))
	require.NoError(t, s.Sync(ctx))

	// V2.4 reactive trigger contract: identical sync → zero new
	// event_seen/event_changed publishes. cal.list_snapshot publishes are
	// orthogonal (V2.6 projection-publisher signal) and excluded here.
	seen := filterByKind(pub.observed(), "cal.event_seen")
	require.Len(t, seen, 4, "second sync with identical state: no new event_seen publishes")
	changed := filterByKind(pub.observed(), "cal.event_changed")
	require.Empty(t, changed, "second sync with identical state: no event_changed publishes")
}

func TestSync_LookbackHonored(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, loc)

	p := &stubProvider{}
	p.set(loadDataPlain()[0])
	mem := logtest.NewMemReader()
	s := newSensor(p, mem, now)

	// Inject an ancient cal.event_seen for e1; the poller's lookback (30d
	// default) ignores it, so e1 is treated as new and re-emitted.
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	mem.AppendEvent(logtest.MakeEvent(log.KindCalEventSeen, "caldav", old, eventPayload{
		UID: "e1", Title: "Standup", Location: "Slack", Tag: "work",
		Start: time.Date(2026, 4, 25, 9, 0, 0, 0, loc),
		End:   time.Date(2026, 4, 25, 10, 0, 0, 0, loc),
	}))

	require.NoError(t, s.Sync(context.Background()))
	seen, _ := mem.ByKind(context.Background(), log.KindCalEventSeen)
	require.Len(t, seen, 2, "ancient history is outside lookback window, so e1 is re-emitted")
}
