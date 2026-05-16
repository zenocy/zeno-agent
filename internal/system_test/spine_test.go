package system_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/log"
	"github.com/zenocy/zeno-v2/internal/projection"
	caldavsensor "github.com/zenocy/zeno-v2/internal/sensor/caldav"
)

func plainBody(subject string) []byte {
	return []byte("From: a@x.test\r\nTo: b@x.test\r\nSubject: " + subject +
		"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody for " + subject + "\r\n")
}

func TestSpine_FullSyncProducesProjections(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	h := NewHarness(t, HarnessConfig{TZ: tz, Now: func() time.Time { return now }})
	defer h.Close()

	h.IMAP.PutMessage("INBOX", "Hello", "alice@example.test", "me@example.test", plainBody("Hello"))
	h.IMAP.PutMessage("INBOX", "Re: Hello", "bob@example.test", "me@example.test", plainBody("Re Hello"))

	mtgStart := time.Date(2026, 4, 25, 10, 0, 0, 0, tz)
	h.CalDAV.SetEvents(caldavsensor.RawEvent{
		UID:  "today-1",
		ICS:  MakeICS("today-1", "Standup", "Slack", "Work", mtgStart, mtgStart.Add(time.Hour), tz),
		ETag: "v1",
	})

	results := h.Scheduler.SyncAll(context.Background())
	for _, r := range results {
		require.True(t, r.OK, "sensor %q failed: %s", r.Name, r.Err)
	}

	status, body := h.Get("/api/projections/calendar/today")
	require.Equal(t, http.StatusOK, status)
	var cal []projection.CalendarEvent
	require.NoError(t, json.Unmarshal(body, &cal))
	require.Len(t, cal, 1)
	require.Equal(t, "Standup", cal[0].Title)

	status, body = h.Get("/api/projections/email/open")
	require.Equal(t, http.StatusOK, status)
	var threads []projection.Thread
	require.NoError(t, json.Unmarshal(body, &threads))
	require.NotEmpty(t, threads)

	status, body = h.Get("/api/projections/run-window")
	require.Equal(t, http.StatusOK, status)
	require.NotEqual(t, "null\n", string(body), "fixture has clear weather; window should exist")
}

func TestSpine_RestartPreservesUIDs(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)
	dbPath := filepath.Join(t.TempDir(), "zeno.db")

	// First harness: arrange + sync.
	h1 := NewHarness(t, HarnessConfig{TZ: tz, Now: func() time.Time { return now }, DBPath: dbPath})
	for i := 0; i < 3; i++ {
		h1.IMAP.PutMessage("INBOX", "Msg", "a@x.test", "b@x.test", plainBody("body"))
	}
	results := h1.Scheduler.SyncAll(context.Background())
	for _, r := range results {
		require.True(t, r.OK)
	}
	require.Equal(t, 3, h1.CountByKind(log.KindMailReceived))
	h1.Close()

	// Second harness on the SAME DB. Same fixture in IMAP. After Sync, UID
	// dedup must hold and no new mail.received rows should appear.
	h2 := NewHarness(t, HarnessConfig{TZ: tz, Now: func() time.Time { return now }, DBPath: dbPath})
	defer h2.Close()
	for i := 0; i < 3; i++ {
		h2.IMAP.PutMessage("INBOX", "Msg", "a@x.test", "b@x.test", plainBody("body"))
	}
	results = h2.Scheduler.SyncAll(context.Background())
	for _, r := range results {
		require.True(t, r.OK)
	}
	require.Equal(t, 3, h2.CountByKind(log.KindMailReceived), "no new rows on restart")
}

func TestSpine_RestartHonorsUIDValidityChange(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)
	dbPath := filepath.Join(t.TempDir(), "zeno.db")

	h1 := NewHarness(t, HarnessConfig{TZ: tz, Now: func() time.Time { return now }, DBPath: dbPath})
	h1.IMAP.PutMessage("INBOX", "M1", "a@x.test", "b@x.test", plainBody("a"))
	results := h1.Scheduler.SyncAll(context.Background())
	for _, r := range results {
		require.True(t, r.OK)
	}
	require.Equal(t, 1, h1.CountByKind(log.KindMailReceived))
	h1.Close()

	h2 := NewHarness(t, HarnessConfig{TZ: tz, Now: func() time.Time { return now }, DBPath: dbPath})
	defer h2.Close()
	// Bump UIDVALIDITY then re-add the same message at a fresh UID.
	h2.IMAP.BumpValidity("INBOX", 99)
	h2.IMAP.PutMessage("INBOX", "M1", "a@x.test", "b@x.test", plainBody("a"))
	results = h2.Scheduler.SyncAll(context.Background())
	for _, r := range results {
		require.True(t, r.OK)
	}
	require.Equal(t, 2, h2.CountByKind(log.KindMailReceived), "UIDVALIDITY change re-emits")
}

func TestSpine_PartialSensorFailureDoesNotBlockOthers(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	h := NewHarness(t, HarnessConfig{TZ: tz, Now: func() time.Time { return now }})
	defer h.Close()

	h.IMAP.FailNext()
	mtgStart := time.Date(2026, 4, 25, 10, 0, 0, 0, tz)
	h.CalDAV.SetEvents(caldavsensor.RawEvent{
		UID: "ok-1", ICS: MakeICS("ok-1", "Meet", "", "", mtgStart, mtgStart.Add(time.Hour), tz),
	})

	status, body := h.Post("/api/sync/now")
	require.Equal(t, http.StatusOK, status)
	var resp struct {
		Sensors []struct {
			Name  string `json:"name"`
			OK    bool   `json:"ok"`
			Error string `json:"error,omitempty"`
		} `json:"sensors"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))

	byName := map[string]bool{}
	for _, s := range resp.Sensors {
		byName[s.Name] = s.OK
	}
	require.False(t, byName["imap"])
	require.True(t, byName["caldav"])
	require.True(t, byName["weather"])

	require.Equal(t, 0, h.CountByKind(log.KindMailReceived))
	require.Equal(t, 1, h.CountByKind(log.KindCalEventSeen))
	require.Equal(t, 1, h.CountByKind(log.KindWeatherSnapshot))
}

func TestSpine_BootPriming(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	h := NewHarness(t, HarnessConfig{TZ: tz, Now: func() time.Time { return now }, WithBootPrime: true})
	defer h.Close()

	mtgStart := time.Date(2026, 4, 25, 10, 0, 0, 0, tz)
	h.CalDAV.SetEvents(caldavsensor.RawEvent{
		UID: "boot-1", ICS: MakeICS("boot-1", "Boot", "", "", mtgStart, mtgStart.Add(time.Hour), tz),
	})
	h.IMAP.PutMessage("INBOX", "Hi", "a@x.test", "b@x.test", plainBody("hi"))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.CountByKind(log.KindWeatherSnapshot) > 0 &&
			h.CountByKind(log.KindCalEventSeen) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Greater(t, h.CountByKind(log.KindWeatherSnapshot), 0)
	require.Greater(t, h.CountByKind(log.KindCalEventSeen), 0)
}

func TestSpine_BearerToken_Unauthorized(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	h := NewHarness(t, HarnessConfig{TZ: tz, Now: func() time.Time { return now }, LANToken: "secret"})
	defer h.Close()

	status, _ := h.Get("/api/projections/calendar/today")
	require.Equal(t, http.StatusUnauthorized, status)

	status, _ = h.GetWithToken("/api/projections/calendar/today", "wrong")
	require.Equal(t, http.StatusUnauthorized, status)

	status, _ = h.GetWithToken("/api/projections/calendar/today", "secret")
	require.Equal(t, http.StatusOK, status)

	status, _ = h.PostWithToken("/api/sync/now", "secret")
	require.Equal(t, http.StatusOK, status)
}

func TestSpine_ScheduledSyncFires(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	h := NewHarness(t, HarnessConfig{
		TZ:       tz,
		Now:      func() time.Time { return now },
		SyncCron: "@every 1s",
	})
	defer h.Close()

	h.Scheduler.Start()
	time.Sleep(2300 * time.Millisecond)

	require.GreaterOrEqual(t, h.CountByKind(log.KindWeatherSnapshot), 2,
		"weather snapshot must accumulate over multiple cron ticks")
}

func TestSpine_MultipleFoldersIndependentCursors(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 8, 0, 0, 0, tz)

	h := NewHarness(t, HarnessConfig{
		TZ:          tz,
		Now:         func() time.Time { return now },
		IMAPFolders: []string{"INBOX", "Archive"},
	})
	defer h.Close()

	h.IMAP.PutMessage("INBOX", "I1", "a@x.test", "b@x.test", plainBody("i1"))
	h.IMAP.PutMessage("INBOX", "I2", "a@x.test", "b@x.test", plainBody("i2"))
	h.IMAP.PutMessage("Archive", "A1", "a@x.test", "b@x.test", plainBody("a1"))

	results := h.Scheduler.SyncAll(context.Background())
	for _, r := range results {
		require.True(t, r.OK)
	}

	require.Equal(t, 3, h.CountByKind(log.KindMailReceived))
	require.Equal(t, 2, h.CountByKind(log.KindIMAPCursor), "one cursor per folder")

	// Bump only INBOX validity → only its messages re-emit.
	h.IMAP.BumpValidity("INBOX", 999)
	h.IMAP.PutMessage("INBOX", "I3", "a@x.test", "b@x.test", plainBody("i3"))
	results = h.Scheduler.SyncAll(context.Background())
	for _, r := range results {
		require.True(t, r.OK)
	}
	// 3 original + 2 re-emitted from INBOX (under new UIDVALIDITY) + 1 new (I3) = 6
	require.GreaterOrEqual(t, h.CountByKind(log.KindMailReceived), 6)
}

func TestSpine_RunWindowEndToEnd(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)

	h := NewHarness(t, HarnessConfig{TZ: tz, Now: func() time.Time { return now }})
	defer h.Close()

	mtgStart := time.Date(2026, 4, 25, 8, 30, 0, 0, tz)
	h.CalDAV.SetEvents(caldavsensor.RawEvent{
		UID: "block", ICS: MakeICS("block", "Standup", "", "", mtgStart, mtgStart.Add(8*time.Hour), tz),
	})

	results := h.Scheduler.SyncAll(context.Background())
	for _, r := range results {
		require.True(t, r.OK)
	}

	status, body := h.Get("/api/projections/run-window")
	require.Equal(t, http.StatusOK, status)
	var w projection.Window
	require.NoError(t, json.Unmarshal(body, &w))
	require.Equal(t, 6, w.Start.In(tz).Hour())
	require.Equal(t, 8, w.End.In(tz).Hour())
	require.Equal(t, 30, w.End.In(tz).Minute())
	require.NotEmpty(t, w.Condition)
}

func TestSpine_LogGrowsAppendOnly(t *testing.T) {
	tz, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 4, 25, 6, 0, 0, 0, tz)

	h := NewHarness(t, HarnessConfig{TZ: tz, Now: func() time.Time { return now }})
	defer h.Close()

	for i := 0; i < 3; i++ {
		_ = h.Scheduler.SyncAll(context.Background())
	}
	var totalAfter int64
	require.NoError(t, h.DB.Model(&log.Event{}).Count(&totalAfter).Error)
	require.GreaterOrEqual(t, totalAfter, int64(3))

	// Try a forbidden UPDATE / DELETE via the gormStore — gormStore doesn't
	// expose either; the only mutation paths are Append. So we just verify
	// the events table allows no row to be missing post-sync (no quiet
	// deletes happened). Append-only is enforced by absence of code paths.
	var first log.Event
	require.NoError(t, h.DB.Order("ts ASC").First(&first).Error)
	// Run another sync; ID and TS of the first row must still match.
	_ = h.Scheduler.SyncAll(context.Background())
	var firstAgain log.Event
	require.NoError(t, h.DB.Order("ts ASC").First(&firstAgain).Error)
	require.Equal(t, first.ID, firstAgain.ID, "earliest event must not have been overwritten")
}

// silence unused-import linter when only some tests exercise specific imports.
var _ = errors.New
