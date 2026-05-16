package projection

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/log/logtest"
)

func mailPayload(subject, from, inReplyTo string, refs []string, date time.Time) any {
	p := map[string]any{
		"folder":      "INBOX",
		"uid":         1,
		"uidvalidity": 1,
		"from":        from,
		"to":          []string{"me@example.test"},
		"subject":     subject,
		"date":        date,
	}
	if inReplyTo != "" {
		p["in_reply_to"] = inReplyTo
	}
	if len(refs) > 0 {
		p["references"] = refs
	}
	return p
}

// mailPayloadFull is the variant for tests that need to exercise
// folder / uid / uidvalidity behaviour (e.g. snapshot filtering).
func mailPayloadFull(folder string, uid, uidvalidity uint32, subject, from string, date time.Time) any {
	return map[string]any{
		"folder":      folder,
		"uid":         uid,
		"uidvalidity": uidvalidity,
		"from":        from,
		"to":          []string{"me@example.test"},
		"subject":     subject,
		"date":        date,
	}
}

func snapshotPayload(folder string, uidvalidity uint32, uids []uint32) any {
	return map[string]any{
		"folder":      folder,
		"uidvalidity": uidvalidity,
		"uids":        uids,
	}
}

func TestOpenEmailThreads_GroupByReferences(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	root := "root@example.test"
	for i := 0; i < 3; i++ {
		mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap",
			now.Add(time.Duration(i)*time.Minute),
			mailPayload(fmt.Sprintf("Re: thread #%d", i), "alice@example.test", "", []string{root}, now.Add(time.Duration(i)*time.Minute))))
	}

	got, err := OpenEmailThreads{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 3, got[0].MessageCount)
}

func TestOpenEmailThreads_GroupByInReplyTo(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now.Add(-2*time.Minute),
		mailPayload("Hello", "a@x.test", "", nil, now.Add(-2*time.Minute))))
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now,
		mailPayload("Re: Hello", "b@x.test", "msg-1@x.test", nil, now)))
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now.Add(time.Minute),
		mailPayload("Re: Hello", "c@x.test", "msg-1@x.test", nil, now.Add(time.Minute))))

	got, err := OpenEmailThreads{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	// Two threads: the bare "Hello" gets its own subject-keyed bucket, then
	// the two replies share an in_reply_to bucket.
	require.Len(t, got, 2)
}

func TestOpenEmailThreads_GroupBySubject(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	subjects := []string{"Foo", "Re: Foo", "RE: foo", "Fwd: Re: Foo"}
	for i, subj := range subjects {
		mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap",
			now.Add(time.Duration(i)*time.Minute),
			mailPayload(subj, "a@x.test", "", nil, now.Add(time.Duration(i)*time.Minute))))
	}

	got, err := OpenEmailThreads{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1, "Foo / Re: Foo / Fwd: Re: Foo all collapse to one")
	require.Equal(t, 4, got[0].MessageCount)
}

func TestOpenEmailThreads_DistinctSubjects(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	for i, subj := range []string{"Alpha", "Beta", "Gamma"} {
		mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap",
			now.Add(time.Duration(i)*time.Minute),
			mailPayload(subj, "x@y.test", "", nil, now.Add(time.Duration(i)*time.Minute))))
	}
	got, err := OpenEmailThreads{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 3)
}

func TestOpenEmailThreads_OrderingAndCap(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)
	cfg := newCfg(now, tz)
	cfg.OpenThreadsMax = 5

	mem := logtest.NewMemReader()
	for i := 0; i < 30; i++ {
		mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap",
			now.Add(time.Duration(i)*time.Minute),
			mailPayload(fmt.Sprintf("Subject %d", i), "x@y.test", "", nil, now.Add(time.Duration(i)*time.Minute))))
	}

	got, err := OpenEmailThreads{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 5, "capped to OpenThreadsMax")
	for i := 1; i < len(got); i++ {
		require.True(t, got[i-1].LastReceived.After(got[i].LastReceived) ||
			got[i-1].LastReceived.Equal(got[i].LastReceived), "newest first")
	}
}

func TestOpenEmailThreads_LookbackBoundary(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)
	cfg := newCfg(now, tz)
	cfg.LookbackDays = 7

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now.Add(-30*24*time.Hour),
		mailPayload("Ancient", "a@x.test", "", nil, now.Add(-30*24*time.Hour))))
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now.Add(-1*time.Hour),
		mailPayload("Fresh", "a@x.test", "", nil, now.Add(-1*time.Hour))))

	got, err := OpenEmailThreads{Cfg: cfg}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "Fresh", got[0].Subject)
}

func TestOpenEmailThreads_EmptyLog(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	mem := logtest.NewMemReader()
	got, err := OpenEmailThreads{Cfg: newCfg(time.Now().In(tz), tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestOpenEmailThreads_NoSnapshot_IncludesAll(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	for i, subj := range []string{"Alpha", "Beta", "Gamma"} {
		mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap",
			now.Add(time.Duration(i)*time.Minute),
			mailPayloadFull("INBOX", uint32(i+1), 1, subj, "x@y.test", now.Add(time.Duration(i)*time.Minute))))
	}

	got, err := OpenEmailThreads{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 3, "no snapshot → first-deploy compat: include all received")
}

func TestOpenEmailThreads_SnapshotFiltersVanished(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	for i, subj := range []string{"Alpha", "Beta", "Gamma"} {
		mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap",
			now.Add(time.Duration(i)*time.Minute),
			mailPayloadFull("INBOX", uint32(101+i), 1, subj, "x@y.test", now.Add(time.Duration(i)*time.Minute))))
	}
	// Snapshot says only UIDs 101 and 103 are still present (UID 102 vanished).
	mem.AppendEvent(logtest.MakeEvent(log.KindIMAPInboxSnapshot, "imap",
		now.Add(5*time.Minute), snapshotPayload("INBOX", 1, []uint32{101, 103})))

	got, err := OpenEmailThreads{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 2)
	subjects := []string{got[0].Subject, got[1].Subject}
	require.NotContains(t, subjects, "Beta", "vanished UID 102 must be excluded")
	require.Contains(t, subjects, "Alpha")
	require.Contains(t, subjects, "Gamma")
}

func TestOpenEmailThreads_LatestSnapshotWins(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	for i, subj := range []string{"Alpha", "Beta", "Gamma"} {
		mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap",
			now.Add(time.Duration(i)*time.Minute),
			mailPayloadFull("INBOX", uint32(1+i), 1, subj, "x@y.test", now.Add(time.Duration(i)*time.Minute))))
	}
	// Older snapshot says everything is present; newer snapshot drops UID 2.
	mem.AppendEvent(logtest.MakeEvent(log.KindIMAPInboxSnapshot, "imap",
		now.Add(-2*time.Hour), snapshotPayload("INBOX", 1, []uint32{1, 2, 3})))
	mem.AppendEvent(logtest.MakeEvent(log.KindIMAPInboxSnapshot, "imap",
		now.Add(-1*time.Hour), snapshotPayload("INBOX", 1, []uint32{1, 3})))

	got, err := OpenEmailThreads{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 2, "latest snapshot must win — UID 2 should be excluded")
}

func TestOpenEmailThreads_UIDValidityBumpDropsOldReceived(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	// Old uidvalidity messages.
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now.Add(-1*time.Hour),
		mailPayloadFull("INBOX", 1, 1, "Old-A", "x@y.test", now.Add(-1*time.Hour))))
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now.Add(-1*time.Hour),
		mailPayloadFull("INBOX", 2, 1, "Old-B", "x@y.test", now.Add(-1*time.Hour))))
	// Re-emitted messages under the new uidvalidity.
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now,
		mailPayloadFull("INBOX", 10, 2, "New", "x@y.test", now)))
	// Snapshot is for the new uidvalidity only.
	mem.AppendEvent(logtest.MakeEvent(log.KindIMAPInboxSnapshot, "imap", now,
		snapshotPayload("INBOX", 2, []uint32{10})))

	got, err := OpenEmailThreads{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1, "old-uidvalidity messages must drop once new-uidvalidity snapshot lands")
	require.Equal(t, "New", got[0].Subject)
}

func TestOpenEmailThreads_SnapshotOlderThanLookback(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now.Add(-1*time.Hour),
		mailPayloadFull("INBOX", 1, 1, "Fresh", "x@y.test", now.Add(-1*time.Hour))))
	// Snapshot 30 days old — well outside the 14-day lookback.
	mem.AppendEvent(logtest.MakeEvent(log.KindIMAPInboxSnapshot, "imap",
		now.Add(-30*24*time.Hour), snapshotPayload("INBOX", 1, []uint32{1})))

	got, err := OpenEmailThreads{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 1, "snapshot is current truth regardless of TS — lookback does not apply to it")
	require.Equal(t, "Fresh", got[0].Subject)
}

func TestOpenEmailThreads_PerFolderIsolation(t *testing.T) {
	tz := mustTZ(t, "America/Los_Angeles")
	now := time.Date(2026, 4, 25, 9, 0, 0, 0, tz)

	mem := logtest.NewMemReader()
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now,
		mailPayloadFull("INBOX", 1, 1, "Inbox-keep", "x@y.test", now)))
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now,
		mailPayloadFull("INBOX", 2, 1, "Inbox-drop", "x@y.test", now)))
	mem.AppendEvent(logtest.MakeEvent(log.KindMailReceived, "imap", now,
		mailPayloadFull("Archive", 99, 1, "Archive-keep", "x@y.test", now)))
	// Only INBOX has a snapshot; Archive is unconstrained.
	mem.AppendEvent(logtest.MakeEvent(log.KindIMAPInboxSnapshot, "imap", now,
		snapshotPayload("INBOX", 1, []uint32{1})))

	got, err := OpenEmailThreads{Cfg: newCfg(now, tz)}.Compute(context.Background(), mem)
	require.NoError(t, err)
	require.Len(t, got, 2, "INBOX UID 2 dropped; Archive UID 99 kept (no snapshot)")
	subjects := []string{got[0].Subject, got[1].Subject}
	require.NotContains(t, subjects, "Inbox-drop")
	require.Contains(t, subjects, "Inbox-keep")
	require.Contains(t, subjects, "Archive-keep")
}

func TestNormalizeSubject(t *testing.T) {
	cases := map[string]string{
		"Re: Foo":          "foo",
		"RE: Re: Foo":      "foo",
		"Fwd: Re: Foo":     "foo",
		"  Re :  spaced  ": "spaced",
		"No prefix":        "no prefix",
	}
	for in, want := range cases {
		require.Equal(t, want, normalizeSubject(in), "input=%q", in)
	}
}
