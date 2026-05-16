package imap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
	"github.com/zenocy/zeno-v2/internal/sensor"
)

func mustQuiet() *logrus.Entry {
	l := logrus.New()
	l.Out = io.Discard
	return l.WithField("c", "imap-test")
}

func newSensor(srv *stubServer, mem *logtest.MemReader, folders ...string) *Sensor {
	cfg := config.IMAPConfig{
		Host:     "imap.test",
		Port:     993,
		Username: "u",
		Password: "p",
		TLS:      "implicit",
		Folders:  folders,
	}
	return NewWithDialer(cfg, mem, mem, &stubDialer{srv: srv}, mustQuiet())
}

func decodeMail(t *testing.T, e log.Event) mailPayload {
	t.Helper()
	var p mailPayload
	require.NoError(t, json.Unmarshal(e.Payload, &p))
	return p
}

func decodeCursor(t *testing.T, e log.Event) cursorPayload {
	t.Helper()
	var p cursorPayload
	require.NoError(t, json.Unmarshal(e.Payload, &p))
	return p
}

func decodeSnapshot(t *testing.T, e log.Event) inboxSnapshotPayload {
	t.Helper()
	var p inboxSnapshotPayload
	require.NoError(t, json.Unmarshal(e.Payload, &p))
	return p
}

func TestSync_FirstRun(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	body := loadFixture(t, "plain.eml")
	srv.putMessage("INBOX", fixtureMessage(101, "Plain note", "alice@example.test", "bob@example.test", body))
	srv.putMessage("INBOX", fixtureMessage(102, "Quarterly review", "carol@example.test", "dave@example.test", loadFixture(t, "multipart.eml")))
	srv.putMessage("INBOX", fixtureMessage(103, "HTML-only note", "eve@example.test", "frank@example.test", loadFixture(t, "htmlonly.eml")))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")

	require.NoError(t, s.Sync(context.Background()))

	mails, _ := mem.ByKind(context.Background(), log.KindMailReceived)
	require.Len(t, mails, 3)
	cursors, _ := mem.ByKind(context.Background(), log.KindIMAPCursor)
	require.Len(t, cursors, 1)

	c := decodeCursor(t, cursors[0])
	require.Equal(t, "INBOX", c.Folder)
	require.Equal(t, uint32(11), c.UIDValidity)
	require.Equal(t, uint32(104), c.UIDNext) // max(uid)+1

	first := decodeMail(t, mails[0])
	require.Equal(t, "INBOX", first.Folder)
	require.Equal(t, uint32(11), first.UIDValidity)
	require.Equal(t, uint32(101), first.UID)
	require.Equal(t, "alice@example.test", first.From)
	require.Contains(t, first.BodyPreview, "got the patch")
}

func TestSync_Idempotent(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	srv.putMessage("INBOX", fixtureMessage(101, "Plain note", "alice@example.test", "bob@example.test", loadFixture(t, "plain.eml")))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")

	ctx := context.Background()
	require.NoError(t, s.Sync(ctx))
	require.NoError(t, s.Sync(ctx))

	mails, _ := mem.ByKind(ctx, log.KindMailReceived)
	require.Len(t, mails, 1, "second run must not duplicate mail.received")
}

func TestSync_UIDValidityChanged(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 1)
	srv.putMessage("INBOX", fixtureMessage(50, "Plain note", "alice@example.test", "bob@example.test", loadFixture(t, "plain.eml")))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")
	ctx := context.Background()

	require.NoError(t, s.Sync(ctx))
	mails, _ := mem.ByKind(ctx, log.KindMailReceived)
	require.Len(t, mails, 1)
	require.Equal(t, uint32(1), decodeMail(t, mails[0]).UIDValidity)

	// Server bumps UIDVALIDITY → poller must re-emit all messages with the
	// new validity.
	srv.bumpUIDValidity("INBOX", 2)
	require.NoError(t, s.Sync(ctx))

	mails, _ = mem.ByKind(ctx, log.KindMailReceived)
	require.Len(t, mails, 2)
	require.Equal(t, uint32(2), decodeMail(t, mails[1]).UIDValidity)
}

func TestSync_MultipleFolders(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	srv.addFolder("Archive", 22)
	body := loadFixture(t, "plain.eml")
	srv.putMessage("INBOX", fixtureMessage(1, "Inbox-A", "a@x.test", "b@x.test", body))
	srv.putMessage("INBOX", fixtureMessage(2, "Inbox-B", "a@x.test", "b@x.test", body))
	srv.putMessage("Archive", fixtureMessage(99, "Archive-1", "a@x.test", "b@x.test", body))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX", "Archive")

	require.NoError(t, s.Sync(context.Background()))

	cursors, _ := mem.ByKind(context.Background(), log.KindIMAPCursor)
	require.Len(t, cursors, 2)

	byFolder := map[string]cursorPayload{}
	for _, c := range cursors {
		p := decodeCursor(t, c)
		byFolder[p.Folder] = p
	}
	require.Equal(t, uint32(11), byFolder["INBOX"].UIDValidity)
	require.Equal(t, uint32(3), byFolder["INBOX"].UIDNext)
	require.Equal(t, uint32(22), byFolder["Archive"].UIDValidity)
	require.Equal(t, uint32(100), byFolder["Archive"].UIDNext)
}

func TestSync_NoNewMail(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")

	require.NoError(t, s.Sync(context.Background()))

	mails, _ := mem.ByKind(context.Background(), log.KindMailReceived)
	require.Empty(t, mails)
	cursors, _ := mem.ByKind(context.Background(), log.KindIMAPCursor)
	require.Len(t, cursors, 1, "still emit a cursor so UIDVALIDITY change is observable next run")
	snaps, _ := mem.ByKind(context.Background(), log.KindIMAPInboxSnapshot)
	require.Len(t, snaps, 1, "snapshot must fire on empty polls so externally archived mail drops out")
	require.Empty(t, decodeSnapshot(t, snaps[0]).UIDs)
}

func TestSync_EmitsInboxSnapshot(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	body := loadFixture(t, "plain.eml")
	srv.putMessage("INBOX", fixtureMessage(101, "first", "alice@example.test", "bob@example.test", body))
	srv.putMessage("INBOX", fixtureMessage(103, "third", "alice@example.test", "bob@example.test", body))
	srv.putMessage("INBOX", fixtureMessage(102, "second", "alice@example.test", "bob@example.test", body))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")
	require.NoError(t, s.Sync(context.Background()))

	snaps, _ := mem.ByKind(context.Background(), log.KindIMAPInboxSnapshot)
	require.Len(t, snaps, 1)
	p := decodeSnapshot(t, snaps[0])
	require.Equal(t, "INBOX", p.Folder)
	require.Equal(t, uint32(11), p.UIDValidity)
	require.Equal(t, []uint32{101, 102, 103}, p.UIDs, "snapshot must be sorted ascending")
}

func TestSync_EmitsSnapshotOnIdempotentRerun(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	srv.putMessage("INBOX", fixtureMessage(101, "first", "alice@example.test", "bob@example.test", loadFixture(t, "plain.eml")))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")
	ctx := context.Background()
	require.NoError(t, s.Sync(ctx))
	require.NoError(t, s.Sync(ctx))

	snaps, _ := mem.ByKind(ctx, log.KindIMAPInboxSnapshot)
	require.Len(t, snaps, 2, "one snapshot per poll, even on idempotent reruns")
	for _, e := range snaps {
		require.Equal(t, []uint32{101}, decodeSnapshot(t, e).UIDs)
	}
}

func TestSync_SnapshotPerFolder(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	srv.addFolder("Archive", 22)
	body := loadFixture(t, "plain.eml")
	srv.putMessage("INBOX", fixtureMessage(1, "in1", "a@x.test", "b@x.test", body))
	srv.putMessage("Archive", fixtureMessage(99, "ar1", "a@x.test", "b@x.test", body))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX", "Archive")
	require.NoError(t, s.Sync(context.Background()))

	snaps, _ := mem.ByKind(context.Background(), log.KindIMAPInboxSnapshot)
	require.Len(t, snaps, 2)
	byFolder := map[string]inboxSnapshotPayload{}
	for _, e := range snaps {
		p := decodeSnapshot(t, e)
		byFolder[p.Folder] = p
	}
	require.Equal(t, []uint32{1}, byFolder["INBOX"].UIDs)
	require.Equal(t, uint32(11), byFolder["INBOX"].UIDValidity)
	require.Equal(t, []uint32{99}, byFolder["Archive"].UIDs)
	require.Equal(t, uint32(22), byFolder["Archive"].UIDValidity)
}

func TestSync_SnapshotReflectsVanishedUIDs(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	body := loadFixture(t, "plain.eml")
	srv.putMessage("INBOX", fixtureMessage(1, "a", "x@x.test", "y@x.test", body))
	srv.putMessage("INBOX", fixtureMessage(2, "b", "x@x.test", "y@x.test", body))
	srv.putMessage("INBOX", fixtureMessage(3, "c", "x@x.test", "y@x.test", body))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")
	ctx := context.Background()
	require.NoError(t, s.Sync(ctx))

	// User archives UID 2 externally.
	srv.removeMessage("INBOX", 2)
	require.NoError(t, s.Sync(ctx))

	snaps, _ := mem.ByKind(ctx, log.KindIMAPInboxSnapshot)
	require.Len(t, snaps, 2)
	require.Equal(t, []uint32{1, 2, 3}, decodeSnapshot(t, snaps[0]).UIDs)
	require.Equal(t, []uint32{1, 3}, decodeSnapshot(t, snaps[1]).UIDs,
		"second snapshot must reflect the externally archived UID")
}

func TestSync_LoginFailure(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	srv.loginErr = errors.New("auth denied")

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")

	err := s.Sync(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "login")
	require.Empty(t, mem.Events())
}

func TestSync_FetchPartialFailure(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	body := loadFixture(t, "plain.eml")
	for uid := uint32(1); uid <= 60; uid++ {
		srv.putMessage("INBOX", fixtureMessage(uid, "n", "a@x.test", "b@x.test", body))
	}
	// Fail the second batch (batchSize=50 → batch1 succeeds, batch2 fails).
	srv.fetchAfterN = 1
	srv.fetchErr = errors.New("network blip")

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")

	err := s.Sync(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "fetch")

	// First batch's 50 messages should have been written before the failure.
	mails, _ := mem.ByKind(context.Background(), log.KindMailReceived)
	require.Equal(t, 50, len(mails))

	// A cursor should have been written so the next run resumes from UID 51.
	cursors, _ := mem.ByKind(context.Background(), log.KindIMAPCursor)
	require.NotEmpty(t, cursors)
	last := decodeCursor(t, cursors[len(cursors)-1])
	require.Equal(t, uint32(51), last.UIDNext)
}

func TestSync_DedupeWithinBatch(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	body := loadFixture(t, "plain.eml")
	// Two distinct entries with the same UID — simulates a server bug.
	srv.putMessage("INBOX", fixtureMessage(7, "first", "a@x.test", "b@x.test", body))
	srv.putMessage("INBOX", fixtureMessage(7, "duplicate", "a@x.test", "b@x.test", body))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")

	require.NoError(t, s.Sync(context.Background()))

	mails, _ := mem.ByKind(context.Background(), log.KindMailReceived)
	require.Len(t, mails, 1, "same UID written twice in one batch must collapse to one event")
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

func (f *fakePub) snapshot() []eventbus.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]eventbus.Event, len(f.events))
	copy(out, f.events)
	return out
}

// failingWriter wraps a log.Writer and returns errOnAppend on every Append.
type failingWriter struct {
	errOnAppend error
}

func (f *failingWriter) Append(_ context.Context, _, _ string, _ any) (log.Event, error) {
	return log.Event{}, f.errOnAppend
}

// V2.4: a successful Sync that consumes new messages publishes one
// SensorEventObservedEvent per message AFTER each successful log append.
// Evidence ID encodes folder:uid:uidvalidity so it stays globally unique
// across folders and UIDVALIDITY bumps.
func TestSync_PublishesObservedAfterAppend(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	body := loadFixture(t, "plain.eml")
	srv.putMessage("INBOX", fixtureMessage(101, "First", "alice@example.test", "bob@example.test", body))
	srv.putMessage("INBOX", fixtureMessage(102, "Second", "carol@example.test", "dave@example.test", body))
	srv.putMessage("INBOX", fixtureMessage(103, "Third", "eve@example.test", "frank@example.test", body))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")

	pub := &fakePub{}
	ctx := sensor.ContextWithPublisher(context.Background(), pub)
	require.NoError(t, s.Sync(ctx))

	// Log got 3 mail.received events (existing behavior).
	mails, _ := mem.ByKind(context.Background(), log.KindMailReceived)
	require.Len(t, mails, 3)

	// Publisher saw 3 SensorEventObservedEvent entries with matching shape.
	evs := pub.snapshot()
	var observed []eventbus.SensorEventObservedEvent
	for _, ev := range evs {
		if obs, ok := ev.(eventbus.SensorEventObservedEvent); ok {
			observed = append(observed, obs)
		}
	}
	require.Len(t, observed, 3, "one publish per successfully appended message")

	for _, obs := range observed {
		require.Equal(t, "mail.received", obs.Kind_)
		require.Contains(t, obs.EvidenceID, "INBOX:")
		require.Contains(t, obs.EvidenceID, ":11", "evidence ID must include UIDVALIDITY suffix")
		require.Equal(t, "INBOX", obs.Payload["folder"])
		require.Equal(t, uint32(11), obs.Payload["uidvalidity"])
		require.NotEmpty(t, obs.Payload["from"])
	}

	// Spot-check one specific evidence ID.
	wantIDs := map[string]bool{"INBOX:101:11": false, "INBOX:102:11": false, "INBOX:103:11": false}
	for _, obs := range observed {
		if _, ok := wantIDs[obs.EvidenceID]; ok {
			wantIDs[obs.EvidenceID] = true
		}
	}
	for id, seen := range wantIDs {
		require.True(t, seen, "missing publish for evidence ID %s", id)
	}
}

// V2.4: a Sync ctx without an attached publisher must still succeed and
// must not publish anywhere. Pins the no-op contract for unit tests.
func TestSync_NoPublisherInContext_AppendsButDoesNotPublish(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	srv.putMessage("INBOX", fixtureMessage(50, "Hello", "alice@example.test", "bob@example.test", loadFixture(t, "plain.eml")))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")

	// Build a fake publisher and verify it never sees anything since we
	// pass a publisher-less ctx into Sync.
	pub := &fakePub{}
	require.NoError(t, s.Sync(context.Background()))

	mails, _ := mem.ByKind(context.Background(), log.KindMailReceived)
	require.Len(t, mails, 1, "log append still happens (publisher attachment is optional)")
	require.Empty(t, pub.snapshot(), "no publisher attached → no publishes")
}

// V2.4: when writer.Append fails, we must NOT publish. Ordering is
// load-bearing: a failed append means the durable log doesn't have the
// event, so the detector wouldn't see it on a re-fold; a stray publish
// would push the subscriber to synth on stale state.
func TestSync_AppendFailureDoesNotPublish(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	srv.putMessage("INBOX", fixtureMessage(7, "doomed", "alice@example.test", "bob@example.test", loadFixture(t, "plain.eml")))

	mem := logtest.NewMemReader() // satisfies log.Reader for cursor reads
	failW := &failingWriter{errOnAppend: errors.New("disk full")}
	cfg := config.IMAPConfig{
		Host: "imap.test", Port: 993, Username: "u", Password: "p",
		TLS: "implicit", Folders: []string{"INBOX"},
	}
	// Reader = mem (cursor reads pass through), Writer = failing.
	s := NewWithDialer(cfg, mem, failW, &stubDialer{srv: srv}, mustQuiet())

	pub := &fakePub{}
	ctx := sensor.ContextWithPublisher(context.Background(), pub)
	err := s.Sync(ctx)
	require.Error(t, err, "Sync surfaces the writer error")

	require.Empty(t, pub.snapshot(), "failed append → zero publishes (ordering contract)")
}

func TestSync_MultipartPreview(t *testing.T) {
	srv := newStubServer()
	srv.addFolder("INBOX", 11)
	srv.putMessage("INBOX", fixtureMessage(1, "Quarterly", "carol@example.test", "dave@example.test", loadFixture(t, "multipart.eml")))

	mem := logtest.NewMemReader()
	s := newSensor(srv, mem, "INBOX")
	require.NoError(t, s.Sync(context.Background()))

	mails, _ := mem.ByKind(context.Background(), log.KindMailReceived)
	require.Len(t, mails, 1)
	preview := decodeMail(t, mails[0]).BodyPreview
	require.Contains(t, preview, "Numbers are in")
	require.False(t, strings.Contains(preview, "<b>"))
	require.LessOrEqual(t, len(preview), 4*1024)
}
