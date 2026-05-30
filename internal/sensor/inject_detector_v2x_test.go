package sensor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/projection"
	"github.com/zenocy/zeno-v2/internal/store"
	"github.com/zenocy/zeno-v2/internal/synth"
)

func cardWithEntity(entityKey, title, sub string) store.Card {
	return store.Card{ID: entityKey, EntityKey: entityKey, Date: "2026-04-30", Title: title, Sub: sub}
}

// thread_reply fires for a fresh unread reply on a thread that ALREADY has
// a card today, even when the sender is not a VIP, and stamps update mode +
// the thread's entity key so the reactive card refreshes in place.
func TestDetect_ThreadReplyOnCardedThread(t *testing.T) {
	subj := "Redline questions from procurement"
	ek := synth.ThreadEntityKey(subj)
	deps := InjectDetectorDeps{
		Cards: []store.Card{cardWithEntity(ek, "Redline thread", "open items")},
		Threads: []projection.Thread{{
			Subject:      subj,
			LastSender:   "Procurement Desk", // not named in the card → not a VIP
			LastReceived: detectorTime.Add(-5 * time.Minute),
			UnreadCount:  1,
		}},
		Now: fixedNow(detectorTime),
	}
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig)
	require.Equal(t, "thread_reply", sig.Kind)
	require.Equal(t, synth.InjectModeUpdate, sig.Mode)
	require.Equal(t, ek, sig.EntityKey)
}

// thread_reply requires a present card — the same reply on an un-carded
// thread from a non-VIP sender produces no signal.
func TestDetect_ThreadReplyRequiresPresentCard(t *testing.T) {
	deps := InjectDetectorDeps{
		Cards: nil,
		Threads: []projection.Thread{{
			Subject:      "Redline questions from procurement",
			LastSender:   "Procurement Desk",
			LastReceived: detectorTime.Add(-5 * time.Minute),
			UnreadCount:  1,
		}},
		Now: fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()), "no card + non-VIP sender → no inject")
}

// A reply the user sent themselves (UnreadCount==0) must not fire.
func TestDetect_ThreadReplyIgnoresOwnSend(t *testing.T) {
	subj := "Redline questions from procurement"
	ek := synth.ThreadEntityKey(subj)
	deps := InjectDetectorDeps{
		Cards: []store.Card{cardWithEntity(ek, "Redline thread", "")},
		Threads: []projection.Thread{{
			Subject:      subj,
			LastSender:   "me",
			LastReceived: detectorTime.Add(-2 * time.Minute),
			UnreadCount:  0,
		}},
		Now: fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()))
}

// A VIP email on an un-carded thread fires in APPEND mode and still carries
// the thread entity key (so the appended card is anchored for future runs).
func TestDetect_VIPEmailStampsAppendMode(t *testing.T) {
	subj := "URGENT — board call moved to 10:30"
	deps := InjectDetectorDeps{
		Cards: []store.Card{vipCard("Acuity — Series B review with Saru Patel", "Lin Vega")},
		Threads: []projection.Thread{{
			Subject:      subj,
			LastSender:   "Saru Patel",
			LastReceived: detectorTime.Add(-10 * time.Minute),
			UnreadCount:  1,
		}},
		Now: fixedNow(detectorTime),
	}
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig)
	require.Equal(t, "email", sig.Kind)
	require.Equal(t, synth.InjectModeAppend, sig.Mode, "no entity-keyed card for this thread → append")
	require.Equal(t, synth.ThreadEntityKey(subj), sig.EntityKey)
}

// calendar_soon refreshes an imminent attendee event that already has a
// card — update only, never append.
func TestDetect_CalendarSoonUpdatesCardedEvent(t *testing.T) {
	ek := synth.EventEntityKey("evt-board-1")
	ev := projection.CalendarEvent{
		UID: "evt-board-1", Title: "Board sync",
		Start:     detectorTime.Add(15 * time.Minute),
		Attendees: []string{"Saru", "Lin"},
		// LastModified zero → the calendar_move path does NOT fire; only
		// calendar_soon should.
	}
	deps := InjectDetectorDeps{
		Calendar: []projection.CalendarEvent{ev},
		Cards:    []store.Card{cardWithEntity(ek, "Board sync", "at 10:15")},
		Now:      fixedNow(detectorTime),
	}
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig)
	require.Equal(t, "calendar_soon", sig.Kind)
	require.Equal(t, synth.InjectModeUpdate, sig.Mode)
	require.Equal(t, ek, sig.EntityKey)
}

// calendar_soon is update-only: the same imminent event with no card today
// produces no signal (it's left to the morning grid).
func TestDetect_CalendarSoonRequiresCard(t *testing.T) {
	ev := projection.CalendarEvent{
		UID: "evt-board-1", Title: "Board sync",
		Start:     detectorTime.Add(15 * time.Minute),
		Attendees: []string{"Saru", "Lin"},
	}
	deps := InjectDetectorDeps{
		Calendar: []projection.CalendarEvent{ev},
		Now:      fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()))
}

// A recently-modified calendar move on a carded event stamps update mode.
func TestDetect_CalendarMoveStampsUpdateMode(t *testing.T) {
	ek := synth.EventEntityKey("evt-board-2")
	ev := projection.CalendarEvent{
		UID: "evt-board-2", Title: "Board call",
		Start:        detectorTime.Add(90 * time.Minute),
		Attendees:    []string{"Saru", "Lin"},
		LastModified: detectorTime.Add(-5 * time.Minute),
	}
	deps := InjectDetectorDeps{
		Calendar: []projection.CalendarEvent{ev},
		Cards:    []store.Card{cardWithEntity(ek, "Board call", "moved")},
		Now:      fixedNow(detectorTime),
	}
	sig := Detect(deps, DefaultInjectConfig())
	require.NotNil(t, sig)
	require.Equal(t, "calendar_move", sig.Kind)
	require.Equal(t, synth.InjectModeUpdate, sig.Mode)
	require.Equal(t, ek, sig.EntityKey)
}

// Debounce still caps to a single signal regardless of how many of the
// broadened triggers would otherwise match.
func TestDetect_DebounceStillCapsBroadenedTriggers(t *testing.T) {
	subj := "Redline questions from procurement"
	ek := synth.ThreadEntityKey(subj)
	deps := InjectDetectorDeps{
		Cards: []store.Card{cardWithEntity(ek, "Redline thread", "")},
		Threads: []projection.Thread{{
			Subject: subj, LastSender: "Procurement Desk",
			LastReceived: detectorTime.Add(-5 * time.Minute), UnreadCount: 1,
		}},
		LastFire: detectorTime.Add(-10 * time.Minute), // inside the 30-min window
		Now:      fixedNow(detectorTime),
	}
	require.Nil(t, Detect(deps, DefaultInjectConfig()), "debounce must suppress even broadened triggers")
}
