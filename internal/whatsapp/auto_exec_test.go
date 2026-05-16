package whatsapp_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/action"
	"github.com/zenocy/zeno-v2/internal/synth"
	"github.com/zenocy/zeno-v2/internal/whatsapp"
	"github.com/zenocy/zeno-v2/internal/whatsapp/whatsapptest"
)

// fakeDispatcher captures DispatchIntent calls so tests can assert
// what the auto-exec path tried to do without standing up a full
// action.Handler + executor stack.
type fakeDispatcher struct {
	calls   []action.DispatchInput
	results []action.Result
}

func (f *fakeDispatcher) DispatchIntent(_ context.Context, in action.DispatchInput) (action.Result, error) {
	f.calls = append(f.calls, in)
	if len(f.results) > 0 {
		r := f.results[0]
		f.results = f.results[1:]
		return r, nil
	}
	return action.Result{OK: true, Toast: "Done."}, nil
}

func TestSynthHandler_AutoExec_AddTask(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")

	ask := func(_ context.Context, q string, _ *synth.ConversationContext) (synth.Card, error) {
		return synth.Card{
			Title:  "Task captured",
			Speech: "Adding 'buy milk' to your tasks.",
			Actions: []synth.Action{{
				Label:   "Track as task",
				Primary: true,
				Intent:  "add_task",
				Target:  map[string]any{"title": "Buy milk"},
			}},
		}, nil
	}
	dispatcher := &fakeDispatcher{
		results: []action.Result{{OK: true, Toast: "Added task: Buy milk", EventPayload: map[string]any{}}},
	}

	h, w := makeHandler(t, fake, ask)
	h.Action = dispatcher
	require.NoError(t, h.Handle(context.Background(), whatsapp.Decision{
		Action:    whatsapp.ActionProcess,
		ChatJID:   "1@s.whatsapp.net",
		SenderJID: "1@s.whatsapp.net",
		IsDM:      true,
		Text:      "remind me to buy milk",
	}))

	require.Len(t, dispatcher.calls, 1)
	require.Equal(t, "add_task", dispatcher.calls[0].Intent)
	require.Equal(t, "Buy milk", dispatcher.calls[0].Target["title"])

	// The reply contains the LLM speech AND the executor toast AND undo hint.
	sent := fake.SentMessages()
	require.Len(t, sent, 1)
	require.Contains(t, sent[0].Text, "buy milk")
	require.Contains(t, sent[0].Text, "Added task: Buy milk")
	require.Contains(t, sent[0].Text, "undo")

	require.Contains(t, w.kinds(), "whatsapp.message.received")
	require.Contains(t, w.kinds(), "whatsapp.message.sent")
}

func TestSynthHandler_AutoExec_SkippedForNonTaskIntents(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")

	// Card whose primary action is NOT in the auto-exec whitelist.
	ask := func(_ context.Context, _ string, _ *synth.ConversationContext) (synth.Card, error) {
		return synth.Card{
			Title:  "Just info",
			Speech: "Yesterday we last met on Tuesday.",
			Actions: []synth.Action{{Label: "Dismiss", Primary: true, Intent: "dismiss"}},
		}, nil
	}
	dispatcher := &fakeDispatcher{}

	h, _ := makeHandler(t, fake, ask)
	h.Action = dispatcher
	require.NoError(t, h.Handle(context.Background(), whatsapp.Decision{
		Action: whatsapp.ActionProcess, ChatJID: "1@s.whatsapp.net", SenderJID: "1@s.whatsapp.net",
		IsDM: true, Text: "remind me of when we last met",
	}))

	require.Empty(t, dispatcher.calls, "informational reply must NOT trigger auto-exec")
	sent := fake.SentMessages()
	require.Len(t, sent, 1)
	require.NotContains(t, sent[0].Text, "undo")
}

func TestSynthHandler_AutoExec_FailureSurfacesToast(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")

	ask := func(_ context.Context, _ string, _ *synth.ConversationContext) (synth.Card, error) {
		return synth.Card{
			Title: "x", Speech: "OK.",
			Actions: []synth.Action{{
				Primary: true, Intent: "add_task",
				Target: map[string]any{"title": ""},
			}},
		}, nil
	}
	dispatcher := &fakeDispatcher{
		results: []action.Result{{OK: false, Toast: "target.title is required."}},
	}

	h, _ := makeHandler(t, fake, ask)
	h.Action = dispatcher
	require.NoError(t, h.Handle(context.Background(), whatsapp.Decision{
		Action: whatsapp.ActionProcess, ChatJID: "1@s.whatsapp.net", SenderJID: "1@s.whatsapp.net",
		IsDM: true, Text: "track that",
	}))

	sent := fake.SentMessages()
	require.Len(t, sent, 1)
	require.Contains(t, sent[0].Text, "target.title is required.")
	require.NotContains(t, sent[0].Text, "undo", "failed exec must not advertise undo")
}

func TestSynthHandler_Undo_ReversesAddTask(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")

	// First request: add a task.
	ask := func(_ context.Context, _ string, _ *synth.ConversationContext) (synth.Card, error) {
		return synth.Card{
			Speech: "Added.",
			Actions: []synth.Action{{
				Primary: true, Intent: "add_task",
				Target: map[string]any{"title": "Buy milk"},
			}},
		}, nil
	}
	dispatcher := &fakeDispatcher{
		results: []action.Result{
			// V2.11: AddTaskExec writes the new task's UUID into uid;
			// the undo path needs it to delete by ID.
			{OK: true, Toast: "Added task: Buy milk", EventPayload: map[string]any{"uid": "task-abc"}},
			{OK: true, Toast: "Deleted: Buy milk"}, // for the undo
		},
	}
	h, _ := makeHandler(t, fake, ask)
	h.Action = dispatcher

	dec := whatsapp.Decision{
		Action: whatsapp.ActionProcess, ChatJID: "1@s.whatsapp.net", SenderJID: "1@s.whatsapp.net", IsDM: true,
	}

	dec.Text = "remind me to buy milk"
	require.NoError(t, h.Handle(context.Background(), dec))

	dec.Text = "undo"
	require.NoError(t, h.Handle(context.Background(), dec))

	require.Len(t, dispatcher.calls, 2)
	require.Equal(t, "add_task", dispatcher.calls[0].Intent)
	require.Equal(t, "delete_task", dispatcher.calls[1].Intent)
	require.Equal(t, "task-abc", dispatcher.calls[1].Target["uid"], "undo must delete by UUID, not by recomputed hash")

	sent := fake.SentMessages()
	require.Len(t, sent, 2)
	require.Contains(t, strings.ToLower(sent[1].Text), "undone")
}

func TestSynthHandler_Undo_OutsideWindowFallsThrough(t *testing.T) {
	fake := whatsapptest.New()
	fake.SetSession("9@s.whatsapp.net", "Jamie")

	// Ask returns a generic card so the second message (literal "undo")
	// falls through to a normal synth answer when the window has expired.
	count := 0
	ask := func(_ context.Context, _ string, _ *synth.ConversationContext) (synth.Card, error) {
		count++
		if count == 1 {
			return synth.Card{
				Speech: "Added.",
				Actions: []synth.Action{{
					Primary: true, Intent: "add_task",
					Target: map[string]any{"title": "Buy milk"},
				}},
			}, nil
		}
		return synth.Card{Speech: "I don't know what to undo."}, nil
	}
	dispatcher := &fakeDispatcher{
		results: []action.Result{
			{OK: true, Toast: "Added task: Buy milk", EventPayload: map[string]any{}},
		},
	}

	// Override Now so the second invocation is "later" — past the
	// 5-minute window.
	frozen := time.Now()
	advance := false
	h, _ := makeHandler(t, fake, ask)
	h.Action = dispatcher
	h.Now = func() time.Time {
		if advance {
			return frozen.Add(10 * time.Minute)
		}
		return frozen
	}

	dec := whatsapp.Decision{
		Action: whatsapp.ActionProcess, ChatJID: "1@s.whatsapp.net", SenderJID: "1@s.whatsapp.net", IsDM: true,
	}
	dec.Text = "remind me to buy milk"
	require.NoError(t, h.Handle(context.Background(), dec))

	advance = true
	dec.Text = "undo"
	require.NoError(t, h.Handle(context.Background(), dec))

	// The 2nd call should have gone through synth (not delete_task).
	require.Len(t, dispatcher.calls, 1, "stale undo must NOT dispatch a reverse action")
	require.Equal(t, "add_task", dispatcher.calls[0].Intent)
}
