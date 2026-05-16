package sensor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
)

// fixedNow returns a Now func pinned to a specific moment so the table
// cases are deterministic across machines and timezones.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

var detectorTime = time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC) // Thu 10:00 UTC

func vipCard(title, sub string) store.Card {
	return store.Card{ID: "vip", Date: "2026-04-30", Title: title, Sub: sub}
}

//  1. VIP email — sender named in today's cards, unread, recent, no deny
//     pattern → fires.
func TestDetect_VIPEmailFires(t *testing.T) {
	deps := InjectDetectorDeps{
		Cards: []store.Card{vipCard("Acuity — Series B review with Saru Patel", "Lin Vega and Park Choi")},
		Threads: []projection.Thread{{
			Subject:      "URGENT — board call moved to 10:30",
			LastSender:   "Saru Patel",
			LastReceived: detectorTime.Add(-10 * time.Minute),
			UnreadCount:  1,
		}},
		Now: fixedNow(detectorTime),
	}
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig, "VIP email within horizon must fire")
	require.Equal(t, "email", sig.Kind)
	require.Contains(t, sig.Subject, "board call")
}

// 2. Newsletter / unsubscribe / auto-reply from a VIP-named sender →
// does NOT fire. Three sub-cases pinning each deny pattern explicitly.
func TestDetect_DenyPatternsBlockEvenFromVIP(t *testing.T) {
	cases := []struct {
		name    string
		subject string
	}{
		{"newsletter prefix", "Newsletter Q3 update from Acuity"},
		{"weekly prefix", "Weekly digest of cap-table churn"},
		{"unsubscribe anywhere", "Pricing Tier Tuesday — to unsubscribe click here"},
		{"auto-reply prefix", "Auto-reply: out until Friday"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := InjectDetectorDeps{
				Cards: []store.Card{vipCard("Note from Saru about pricing", "")},
				Threads: []projection.Thread{{
					Subject:      tc.subject,
					LastSender:   "Saru Patel",
					LastReceived: detectorTime.Add(-5 * time.Minute),
					UnreadCount:  1,
				}},
				Now: fixedNow(detectorTime),
			}
			require.Nilf(t, Detect(deps, DefaultInjectConfig()),
				"deny pattern must block subject %q even from VIP", tc.subject)
		})
	}
}

// 3. Calendar move outside the 4h horizon → does NOT fire.
func TestDetect_CalendarMoveOutsideHorizonDoesNotFire(t *testing.T) {
	farStart := detectorTime.Add(5 * time.Hour) // 5h > 4h horizon
	deps := InjectDetectorDeps{
		Calendar: []projection.CalendarEvent{{
			UID: "ev-1", Title: "Series B review", Start: farStart, End: farStart.Add(time.Hour),
			Attendees:    []string{"a", "b", "c"},
			LastModified: detectorTime.Add(-5 * time.Minute),
		}},
		Now: fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()), "event >4h out must not fire calendar-move")
}

// 4. Calendar move with only 1 attendee → does NOT fire (under floor).
func TestDetect_CalendarMoveWithOneAttendeeDoesNotFire(t *testing.T) {
	start := detectorTime.Add(2 * time.Hour)
	deps := InjectDetectorDeps{
		Calendar: []projection.CalendarEvent{{
			UID: "ev-1", Title: "1:1 with Lin", Start: start, End: start.Add(30 * time.Minute),
			Attendees:    []string{"Lin"},
			LastModified: detectorTime.Add(-5 * time.Minute),
		}},
		Now: fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()), "single-attendee event must not fire calendar-move")
}

//  5. Three simultaneous candidates within debounce window → debounced
//     (no fire). LastFire is 10 min ago — within the 30-min default.
func TestDetect_DebouncedWhenLastFireRecent(t *testing.T) {
	deps := vipScenarioWithCalendarMove(t)
	deps.LastFire = detectorTime.Add(-10 * time.Minute)
	require.Nil(t, Detect(deps, DefaultInjectConfig()), "30-min debounce must hold against fresh candidates")
}

//  6. Three candidates after debounce expires → fires once, calendar-move
//     wins the tie-break.
func TestDetect_DebounceExpiredCalendarMoveWinsTieBreak(t *testing.T) {
	deps := vipScenarioWithCalendarMove(t)
	deps.LastFire = detectorTime.Add(-31 * time.Minute) // outside the 30-min window
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig, "after debounce, the next eligible candidate must fire")
	require.Equal(t, "calendar_move", sig.Kind, "calendar_move trumps email when both qualify")
}

// 7. Email from an unknown sender (name not in any card) → does NOT fire.
func TestDetect_UnknownSenderDoesNotFire(t *testing.T) {
	deps := InjectDetectorDeps{
		Cards: []store.Card{vipCard("Just a calendar review", "")},
		Threads: []projection.Thread{{
			Subject:      "Need redline check",
			LastSender:   "Total Stranger",
			LastReceived: detectorTime.Add(-5 * time.Minute),
			UnreadCount:  1,
		}},
		Now: fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()), "unknown sender must not fire")
}

//  8. Calendar event whose LAST-MODIFIED is older than the 30-min
//     "recently moved" cutoff → does NOT fire (stale event).
func TestDetect_OldLastModifiedDoesNotFire(t *testing.T) {
	start := detectorTime.Add(2 * time.Hour)
	deps := InjectDetectorDeps{
		Calendar: []projection.CalendarEvent{{
			UID: "ev-1", Title: "Series B review", Start: start, End: start.Add(time.Hour),
			Attendees:    []string{"Saru", "Lin", "Park"},
			LastModified: detectorTime.Add(-2 * time.Hour), // far past the 30-min cutoff
		}},
		Now: fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()), "stale LAST-MODIFIED must not fire")
}

// vipScenarioWithCalendarMove constructs deps that satisfy BOTH paths so
// the tie-break + debounce cases exercise the full surface.
func vipScenarioWithCalendarMove(t *testing.T) InjectDetectorDeps {
	t.Helper()
	calStart := detectorTime.Add(2 * time.Hour)
	return InjectDetectorDeps{
		Cards: []store.Card{vipCard("Acuity — Series B review with Saru Patel", "Lin Vega and Park Choi")},
		Calendar: []projection.CalendarEvent{{
			UID: "evt-1", Title: "Series B review (board)",
			Start: calStart, End: calStart.Add(time.Hour),
			Attendees:    []string{"Saru Patel", "Lin Vega", "Park Choi"},
			LastModified: detectorTime.Add(-5 * time.Minute),
		}},
		Threads: []projection.Thread{{
			Subject:      "URGENT — board call moved to 10:30",
			LastSender:   "Saru Patel",
			LastReceived: detectorTime.Add(-2 * time.Minute),
			UnreadCount:  1,
		}},
		Now: fixedNow(detectorTime),
	}
}
