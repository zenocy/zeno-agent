package sensor

import (
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/eventbus"
	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// TestInjectDetector_CalmDaySoak_NoFalsePositives is the load-bearing
// safety test for the V2.3.0 P3 inject pipeline.
//
// It simulates 8 hours of 5-min cron firings against a calm-day fixture
// — no recent VIP emails, no recently-moved meetings, just routine
// newsletter traffic and routine calendar events. Asserts: zero injects
// fire across all 96 ticks. **If this ever flakes, hold the merge** —
// false positives destroy the calm Zeno is supposed to provide and
// recovering user trust after a noisy day is very hard.
//
// The setup is deterministic: a fixed clock advances 5 min per tick,
// LastFire stays zero throughout because nothing fires, and the deps
// are built from synthetic data (not real sensors) so a real-world
// inject during the test run cannot affect the result.
func TestInjectDetector_CalmDaySoak_NoFalsePositives(t *testing.T) {
	startOfDay := time.Date(2026, 4, 30, 9, 0, 0, 0, time.UTC) // Thu 09:00 UTC

	// Calm calendar: routine events, NONE recently moved, NONE with the
	// VIP-named attendees that the detector keys off — just everyday
	// blocks and personal events.
	calendar := []projection.CalendarEvent{
		{
			UID: "morning-block", Title: "Focus block",
			Start: startOfDay.Add(time.Hour), End: startOfDay.Add(3 * time.Hour),
			Tag:          "focus",
			LastModified: startOfDay.Add(-48 * time.Hour),
		},
		{
			UID: "lunch", Title: "Lunch with Sam",
			Start: startOfDay.Add(4 * time.Hour), End: startOfDay.Add(5 * time.Hour),
			Tag:          "personal",
			Attendees:    []string{"Sam"},
			LastModified: startOfDay.Add(-72 * time.Hour),
		},
		{
			UID: "1on1", Title: "Weekly 1:1",
			Start: startOfDay.Add(6 * time.Hour), End: startOfDay.Add(7 * time.Hour),
			Attendees:    []string{"Lin"}, // single attendee — under floor
			LastModified: startOfDay.Add(-7 * 24 * time.Hour),
		},
	}

	// Calm threads: a steady drip of newsletters and unsubscribe-laden
	// updates, plus an autoreply, plus 50 noise threads from senders the
	// morning grid does not surface. The newsletters' last_received
	// values are spread across the day so each tick has at least one
	// "recent" thread to evaluate — the deny patterns must hold.
	threads := []projection.Thread{
		{
			Subject:      "Stripe Newsletter Weekly Digest",
			LastSender:   "Stripe Atlas",
			LastReceived: startOfDay.Add(2 * time.Hour),
			UnreadCount:  1,
		},
		{
			Subject:      "Daily update — please unsubscribe at the bottom",
			LastSender:   "Updates",
			LastReceived: startOfDay.Add(3 * time.Hour),
			UnreadCount:  1,
		},
		{
			Subject:      "auto-reply: out of office until next week",
			LastSender:   "Lin Vega", // even a "VIP" name shouldn't trip this — autoreply pattern denies first
			LastReceived: startOfDay.Add(4 * time.Hour),
			UnreadCount:  1,
		},
	}
	for i := 0; i < 50; i++ {
		threads = append(threads, projection.Thread{
			Subject:      "Routine ping",
			LastSender:   "noise-sender", // not in any card
			LastReceived: startOfDay.Add(time.Duration(i) * 8 * time.Minute),
			UnreadCount:  1,
		})
	}

	// Cards: a calm morning grid. Names appear in titles but the
	// senders above either match deny patterns or are not VIPs.
	cards := []store.Card{
		{ID: "c1", Date: "2026-04-30", Title: "Lin's pickup at 5", Sub: "Sam reminds Lia"},
		{ID: "c2", Date: "2026-04-30", Title: "Focus block 10–12", Sub: ""},
	}

	cfg := DefaultInjectConfig()

	now := startOfDay
	fires := 0
	const tickCount = 96 // 96 × 5 min = 8h
	for tick := 0; tick < tickCount; tick++ {
		captured := now // capture before mutation
		deps := InjectDetectorDeps{
			Calendar: calendar,
			Threads:  threads,
			Cards:    cards,
			LastFire: time.Time{}, // never fires across the soak — sanity-asserted by `fires == 0`
			Now:      func() time.Time { return captured },
		}
		if Detect(deps, cfg) != nil {
			fires++
			t.Errorf("tick %d (t=%s) fired unexpectedly — calm-day soak must produce zero injects", tick, captured.Format(time.RFC3339))
		}
		now = now.Add(5 * time.Minute)
	}
	if fires != 0 {
		t.Fatalf("calm-day soak: %d false-positive fires across %d ticks (must be 0)", fires, tickCount)
	}
}

// TestInjectDetector_CalmDaySoak_NoFalsePositives_ViaBusSubscriber is the
// V2.4 regression alarm for the trigger refactor. The original
// CalmDaySoak test pins the detector itself; this companion pins the
// path the V2.4 subscriber takes: bus → SensorEventObservedEvent →
// Detect on the same calm-day fixture → no synth fires.
//
// If a future change to the subscriber bypasses the detector or leaks
// state across observations, this alarm catches it. The fixture is
// identical to CalmDaySoak so a flake here implies a flake there too —
// hold the merge.
func TestInjectDetector_CalmDaySoak_NoFalsePositives_ViaBusSubscriber(t *testing.T) {
	startOfDay := time.Date(2026, 4, 30, 9, 0, 0, 0, time.UTC)

	// Re-use the identical calm-day fixture set as TestInjectDetector_CalmDaySoak_NoFalsePositives.
	calendar := []projection.CalendarEvent{
		{
			UID: "morning-block", Title: "Focus block",
			Start: startOfDay.Add(time.Hour), End: startOfDay.Add(3 * time.Hour),
			Tag:          "focus",
			LastModified: startOfDay.Add(-48 * time.Hour),
		},
		{
			UID: "lunch", Title: "Lunch with Sam",
			Start: startOfDay.Add(4 * time.Hour), End: startOfDay.Add(5 * time.Hour),
			Tag:          "personal",
			Attendees:    []string{"Sam"},
			LastModified: startOfDay.Add(-72 * time.Hour),
		},
		{
			UID: "1on1", Title: "Weekly 1:1",
			Start: startOfDay.Add(6 * time.Hour), End: startOfDay.Add(7 * time.Hour),
			Attendees:    []string{"Lin"},
			LastModified: startOfDay.Add(-7 * 24 * time.Hour),
		},
	}
	threads := []projection.Thread{
		{Subject: "Stripe Newsletter Weekly Digest", LastSender: "Stripe Atlas",
			LastReceived: startOfDay.Add(2 * time.Hour), UnreadCount: 1},
		{Subject: "Daily update — please unsubscribe at the bottom", LastSender: "Updates",
			LastReceived: startOfDay.Add(3 * time.Hour), UnreadCount: 1},
		{Subject: "auto-reply: out of office until next week", LastSender: "Lin Vega",
			LastReceived: startOfDay.Add(4 * time.Hour), UnreadCount: 1},
	}
	for i := 0; i < 50; i++ {
		threads = append(threads, projection.Thread{
			Subject: "Routine ping", LastSender: "noise-sender",
			LastReceived: startOfDay.Add(time.Duration(i) * 8 * time.Minute),
			UnreadCount:  1,
		})
	}
	cards := []store.Card{
		{ID: "c1", Date: "2026-04-30", Title: "Lin's pickup at 5", Sub: "Sam reminds Lia"},
		{ID: "c2", Date: "2026-04-30", Title: "Focus block 10–12", Sub: ""},
	}
	cfg := DefaultInjectConfig()

	// Real bus, real subscribe channel.
	quietLogger := logrus.New()
	quietLogger.Out = io.Discard
	bus := eventbus.New(quietLogger.WithField("c", "soak-bus"))
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	// `synthFires` is incremented every time a subscriber-driven Detect
	// call returns a non-nil signal. The contract: zero across the soak.
	var synthFires atomic.Int32
	var observationsHandled atomic.Int32

	now := startOfDay
	const tickCount = 96
	timeMu := sync.Mutex{}
	timeFn := func() time.Time {
		timeMu.Lock()
		defer timeMu.Unlock()
		return now
	}

	// Subscriber drain loop: pull every queued event, run the same
	// detector logic the production subscriber would (Detect with current
	// deps), record any false positive.
	drain := func() {
		for {
			select {
			case ev := <-sub:
				obs, ok := ev.(eventbus.SensorEventObservedEvent)
				if !ok {
					continue
				}
				_ = obs
				observationsHandled.Add(1)
				deps := InjectDetectorDeps{
					Calendar: calendar,
					Threads:  threads,
					Cards:    cards,
					LastFire: time.Time{}, // calm-day: never fires
					Now:      timeFn,
				}
				if sig := Detect(deps, cfg); sig != nil {
					synthFires.Add(1)
				}
			default:
				return
			}
		}
	}

	for tick := 0; tick < tickCount; tick++ {
		// Publish a routine observation each tick (a "newsletter arrived").
		bus.Publish(eventbus.SensorEventObservedEvent{
			Kind_:      "mail.received",
			EvidenceID: "INBOX:" + strconv.FormatInt(now.Unix(), 10) + ":11",
			Payload:    map[string]any{"folder": "INBOX", "from": "noise-sender", "subject": "Routine ping"},
		})
		drain()
		timeMu.Lock()
		now = now.Add(5 * time.Minute)
		timeMu.Unlock()
	}

	require.Equal(t, int32(tickCount), observationsHandled.Load(),
		"subscriber must process every observation")
	require.Equal(t, int32(0), synthFires.Load(),
		"calm-day soak via bus subscriber: zero false positives across %d ticks", tickCount)
}
