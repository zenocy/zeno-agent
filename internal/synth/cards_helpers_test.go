package synth

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/projection"
)

func TestInferIntent(t *testing.T) {
	cases := []struct {
		label  string
		intent string
	}{
		{"Dismiss", "dismiss"},
		{"snooze", "snooze"},
		{"Mute", "dismiss"},
		{"Mark read", "mark_read"},
		{"Mark as read", "mark_read"},
		{"Reply", "draft_reply"},
		{"Draft a reply", "draft_reply"},
		{"Forward to Aria", "forward"},
		{"Send reply", "send_reply"},
		{"Block 17:00–18:30", "block_calendar"},
		{"Hold a window", "block_calendar"},
		{"Add to 11:00", "add_event"},
		{"Tell Sam yes", "rsvp_yes"},
		{"Decline", "rsvp_no"},
		{"Maybe", "rsvp_maybe"},
		{"Move to Archive", "move_mail"},
		{"Open chart", "open_url"},
		{"Open agenda", "open_url"},
		{"Pick a slot", "ask_followup"},
		{"Show options", "ask_followup"},
		{"Delegate to Mara", "ask_followup"},
		{"Add to concerns", "add_concern"},
		{"Remember this", "add_memory"},
		{"", "dismiss"},                  // empty label → safe fallback
		{"completely unknown verb", "dismiss"}, // unknown → safe fallback
	}
	for _, tc := range cases {
		got := inferIntent(tc.label)
		require.Equal(t, tc.intent, got, "label=%q", tc.label)
	}
}

func TestDropUnwiredActions(t *testing.T) {
	wired := map[string]struct{}{"dismiss": {}, "snooze": {}, "draft_reply": {}}

	// Card with two wired + one unwired action.
	c := &Card{Actions: []Action{
		{Label: "Dismiss", Intent: "dismiss"},
		{Label: "Investigate", Intent: "investigate"},  // unwired → drop
		{Label: "Draft a reply", Intent: "draft_reply"},
	}}
	dropped := dropUnwiredActions(c, wired)
	require.Equal(t, 1, dropped)
	require.Len(t, c.Actions, 2)
	require.Equal(t, "dismiss", c.Actions[0].Intent)
	require.Equal(t, "draft_reply", c.Actions[1].Intent)

	// Card with only unwired actions falls back to a single Dismiss.
	c2 := &Card{Actions: []Action{
		{Label: "Investigate", Intent: "investigate"},
		{Label: "Talk to Mara", Intent: "ask_followup"},
	}}
	dropped = dropUnwiredActions(c2, wired)
	require.Equal(t, 2, dropped)
	require.Len(t, c2.Actions, 1)
	require.Equal(t, "dismiss", c2.Actions[0].Intent)

	// Empty wired set → no-op (replay path).
	c3 := &Card{Actions: []Action{
		{Label: "Anything", Intent: "anything"},
	}}
	dropped = dropUnwiredActions(c3, nil)
	require.Equal(t, 0, dropped)
	require.Len(t, c3.Actions, 1)
}

func TestPostProcessIntent_BackfillsMissingIntents(t *testing.T) {
	c := &Card{
		Actions: []Action{
			{Label: "Draft a reply", Primary: true},
			{Label: "Snooze"},
			{Label: "Block 17:00–18:30", Intent: "block_calendar"}, // already set, do not overwrite
		},
	}
	postProcessIntent(c)
	require.Equal(t, "draft_reply", c.Actions[0].Intent)
	require.Equal(t, "snooze", c.Actions[1].Intent)
	require.Equal(t, "block_calendar", c.Actions[2].Intent, "must not overwrite an already-set intent")
}

func TestSlugFromTitle_StripsStopwords(t *testing.T) {
	s := slugFromTitle("Saru Patel · re: redline")
	require.True(t, strings.HasPrefix(s, "saru-"), "got %q", s)
	require.Len(t, s, len("saru-")+4) // 4-hex-char hash suffix
}

func TestSlugFromTitle_StableAcrossCalls(t *testing.T) {
	a := slugFromTitle("Lia's school recital is Thursday at 17:30")
	b := slugFromTitle("Lia's school recital is Thursday at 17:30")
	require.Equal(t, a, b)
}

func TestSlugFromTitle_DifferentTitlesDifferentSlugs(t *testing.T) {
	a := slugFromTitle("Saru Patel · re: redline")
	b := slugFromTitle("Saru Patel · re: timeline")
	require.NotEqual(t, a, b, "different titles must yield different slugs")
}

func TestSlugFromTitle_PureStopwordsFallback(t *testing.T) {
	s := slugFromTitle("the and or")
	require.True(t, strings.HasPrefix(s, "card-"), "got %q", s)
}

func TestCanonicalizeMarkdown_BoldToItalic(t *testing.T) {
	require.Equal(t, "*One* thing", canonicalizeMarkdown("**One** thing"))
}

func TestCanonicalizeMarkdown_EmTagToItalic(t *testing.T) {
	require.Equal(t, "*board* call", canonicalizeMarkdown("<em>board</em> call"))
}

func TestCanonicalizeMarkdown_LeavesItalicAlone(t *testing.T) {
	require.Equal(t, "*calm* start", canonicalizeMarkdown("*calm* start"))
}

func TestCanonicalizeMarkdown_DropsTrailingUnmatchedAsterisk(t *testing.T) {
	require.Equal(t, "A *calm* start. One", canonicalizeMarkdown("A *calm* start. *One"))
}

func TestBalanceMarkdown_NoOpWhenEven(t *testing.T) {
	require.Equal(t, "*one* *two*", balanceMarkdown("*one* *two*"))
}

func TestBalanceMarkdown_StripsLastWhenOdd(t *testing.T) {
	require.Equal(t, "*one* two", balanceMarkdown("*one* *two"))
}

func TestStripCodeFences_JSONFence(t *testing.T) {
	in := "```json\n{\"a\":1}\n```"
	require.Equal(t, "{\"a\":1}", stripCodeFences(in))
}

func TestStripCodeFences_BareFence(t *testing.T) {
	in := "```\n{\"a\":1}\n```"
	require.Equal(t, "{\"a\":1}", stripCodeFences(in))
}

func TestStripCodeFences_NoFence(t *testing.T) {
	require.Equal(t, "{\"a\":1}", stripCodeFences("  {\"a\":1}  "))
}

func TestBackfillMailTargets_InjectsSubject(t *testing.T) {
	cs := &CardSet{Cards: []Card{{
		ID:     "saru-1234",
		Source: "mail",
		Title:  "Saru Patel · re: redline",
		Sub:    "Walked the redline with Lin.",
		Actions: []Action{
			{Label: "Move to Archive", Intent: "move_mail", Target: map[string]any{"folder": "Archive"}},
		},
	}}}
	threads := []projection.Thread{
		{Subject: "Saru Patel · re: redline", LastReceived: time.Now()},
	}
	backfillMailTargets(cs, threads, nil)
	require.Equal(t, "Saru Patel · re: redline", cs.Cards[0].Actions[0].Target["subject"])
	require.Equal(t, "Archive", cs.Cards[0].Actions[0].Target["folder"], "must not clobber other target keys")
}

func TestBackfillMailTargets_NilTargetAllocated(t *testing.T) {
	cs := &CardSet{Cards: []Card{{
		ID:     "saru-1234",
		Source: "mail",
		Title:  "Saru Patel · re: redline",
		Actions: []Action{
			{Label: "Draft a reply", Intent: "draft_reply", Target: nil},
		},
	}}}
	threads := []projection.Thread{
		{Subject: "Saru Patel · re: redline", LastReceived: time.Now()},
	}
	backfillMailTargets(cs, threads, nil)
	require.NotNil(t, cs.Cards[0].Actions[0].Target)
	require.Equal(t, "Saru Patel · re: redline", cs.Cards[0].Actions[0].Target["subject"])
}

func TestBackfillMailTargets_PreservesExistingSubject(t *testing.T) {
	cs := &CardSet{Cards: []Card{{
		ID:     "saru-1234",
		Source: "mail",
		Title:  "Saru Patel · re: redline",
		Actions: []Action{
			{Label: "Move to Archive", Intent: "move_mail", Target: map[string]any{
				"subject": "Hand-picked subject",
				"folder":  "Archive",
			}},
		},
	}}}
	threads := []projection.Thread{
		{Subject: "Saru Patel · re: redline", LastReceived: time.Now()},
	}
	backfillMailTargets(cs, threads, nil)
	require.Equal(t, "Hand-picked subject", cs.Cards[0].Actions[0].Target["subject"], "must never overwrite a non-empty subject")
}

func TestBackfillMailTargets_NoMatchingThread(t *testing.T) {
	cs := &CardSet{Cards: []Card{{
		ID:     "saru-1234",
		Source: "mail",
		Title:  "Completely Unrelated Topic",
		Sub:    "Nothing in common.",
		Actions: []Action{
			{Label: "Move to Archive", Intent: "move_mail", Target: map[string]any{"folder": "Archive"}},
		},
	}}}
	threads := []projection.Thread{
		{Subject: "Saru Patel · re: redline", LastReceived: time.Now()},
	}
	require.NotPanics(t, func() {
		backfillMailTargets(cs, threads, nil)
	})
	_, hasSubject := cs.Cards[0].Actions[0].Target["subject"]
	require.False(t, hasSubject, "no match → action target untouched")
}

func TestBackfillMailTargets_BestMatchOverFirstMatch(t *testing.T) {
	cs := &CardSet{Cards: []Card{{
		ID:     "saru-1234",
		Source: "mail",
		Title:  "Saru Patel · redline review",
		Sub:    "Walked the redline.",
		Actions: []Action{
			{Label: "Move to Archive", Intent: "move_mail", Target: map[string]any{"folder": "Archive"}},
		},
	}}}
	threads := []projection.Thread{
		{Subject: "redline draft from Lin", LastReceived: time.Now().Add(-time.Hour)},                // 1 token: "redline"
		{Subject: "Saru Patel · redline review", LastReceived: time.Now().Add(-2 * time.Hour)},        // multiple tokens
	}
	backfillMailTargets(cs, threads, nil)
	require.Equal(t, "Saru Patel · redline review", cs.Cards[0].Actions[0].Target["subject"])
}

func TestBackfillMailTargets_TieBreakOnLastReceived(t *testing.T) {
	now := time.Now()
	cs := &CardSet{Cards: []Card{{
		ID:     "redline-1",
		Source: "mail",
		Title:  "redline review",
		Actions: []Action{
			{Label: "Move to Archive", Intent: "move_mail", Target: map[string]any{"folder": "Archive"}},
		},
	}}}
	threads := []projection.Thread{
		{Subject: "redline morning", LastReceived: now.Add(-2 * time.Hour)},
		{Subject: "redline evening", LastReceived: now.Add(-1 * time.Hour)}, // more recent → wins
	}
	backfillMailTargets(cs, threads, nil)
	require.Equal(t, "redline evening", cs.Cards[0].Actions[0].Target["subject"])
}

func TestBackfillMailTargets_WorksAfterSourceFlip(t *testing.T) {
	// A card whose Source got flipped from mail → tasks (because the
	// thread subject reads as deferred work) must still get its mail
	// action target backfilled. Backfill is gated on intent, not Source.
	cs := &CardSet{Cards: []Card{{
		ID:     "owed-memo-1",
		Source: "tasks", // already flipped by normalizeCardSources
		Title:  "Finish the long-deferred narrative memo",
		Actions: []Action{
			{Label: "Draft a reply", Intent: "draft_reply", Target: nil},
		},
	}}}
	threads := []projection.Thread{
		{Subject: "long-deferred narrative memo", LastReceived: time.Now()},
	}
	backfillMailTargets(cs, threads, nil)
	require.NotNil(t, cs.Cards[0].Actions[0].Target)
	require.Equal(t, "long-deferred narrative memo", cs.Cards[0].Actions[0].Target["subject"])
}

func TestBackfillMailTargets_OnlyMailIntents(t *testing.T) {
	cs := &CardSet{Cards: []Card{{
		ID:     "saru-1234",
		Source: "mail",
		Title:  "Saru Patel · redline review",
		Actions: []Action{
			{Label: "Dismiss", Intent: "dismiss"},
			{Label: "Open agenda", Intent: "open_url", Target: map[string]any{"url": "https://x"}},
			{Label: "Track as task", Intent: "add_task", Target: map[string]any{"title": "Reply to Saru"}},
		},
	}}}
	threads := []projection.Thread{
		{Subject: "Saru Patel · redline review", LastReceived: time.Now()},
	}
	backfillMailTargets(cs, threads, nil)
	require.Nil(t, cs.Cards[0].Actions[0].Target, "non-mail intent → untouched")
	_, hasSubject := cs.Cards[0].Actions[1].Target["subject"]
	require.False(t, hasSubject, "non-mail intent → no subject injected")
	_, hasSubject = cs.Cards[0].Actions[2].Target["subject"]
	require.False(t, hasSubject, "non-mail intent → no subject injected")
}

func TestBackfillMailTargets_AllSixIntents(t *testing.T) {
	intents := []string{"mark_read", "move_mail", "draft_reply", "send_reply", "forward", "flag_mail"}
	for _, intent := range intents {
		t.Run(intent, func(t *testing.T) {
			cs := &CardSet{Cards: []Card{{
				ID:     "saru-1234",
				Source: "mail",
				Title:  "Saru Patel · redline review",
				Actions: []Action{
					{Label: "Action", Intent: intent},
				},
			}}}
			threads := []projection.Thread{
				{Subject: "Saru Patel · redline review", LastReceived: time.Now()},
			}
			backfillMailTargets(cs, threads, nil)
			require.NotNil(t, cs.Cards[0].Actions[0].Target, "intent=%s", intent)
			require.Equal(t, "Saru Patel · redline review", cs.Cards[0].Actions[0].Target["subject"], "intent=%s", intent)
		})
	}
}

func TestPostProcessCards_BackfillRunsAfterNormalize(t *testing.T) {
	cs := &CardSet{Cards: []Card{{
		ID:     "model-id",
		Source: "mail",
		Title:  "Saru Patel · redline review",
		Sub:    "Walked the redline.",
		Actions: []Action{
			{Label: "Move to Archive", Intent: "move_mail", Target: map[string]any{"folder": "Archive"}},
		},
	}}}
	threads := []projection.Thread{
		{Subject: "Saru Patel · redline review", LastReceived: time.Now()},
	}
	postProcessCards(cs, "2026-05-10", nil, threads, nil, nil)
	require.Equal(t, "Saru Patel · redline review", cs.Cards[0].Actions[0].Target["subject"])
}

func TestPostProcessCards_NormalizesIDsTitlesDates(t *testing.T) {
	cs := CardSet{Cards: []Card{
		{
			ID:    "model-emitted-id-ignored",
			Date:  "wrong-date",
			Title: "**Saru** Patel · re: redline",
			Sub:   "<em>Walked</em> the redline.",
		},
	}}
	postProcessCards(&cs, "2026-04-25", nil, nil, nil, nil)
	require.Equal(t, "2026-04-25", cs.Cards[0].Date)
	require.Equal(t, "*Saru* Patel · re: redline", cs.Cards[0].Title)
	require.Equal(t, "*Walked* the redline.", cs.Cards[0].Sub)
	require.NotEqual(t, "model-emitted-id-ignored", cs.Cards[0].ID, "IDs are server-generated")
	require.True(t, strings.HasPrefix(cs.Cards[0].ID, "saru-"))
}
