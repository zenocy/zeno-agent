package action

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeExec struct {
	mode    Mode
	called  bool
	confirm bool
}

func (f *fakeExec) Mode() Mode { return f.mode }
func (f *fakeExec) Execute(_ context.Context, ec ExecCtx) (Result, error) {
	f.called = true
	f.confirm = ec.Confirm
	return Result{OK: true}, nil
}

func TestRegistry_RegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	dismiss := &fakeExec{mode: Mode1Click}
	preview := &fakeExec{mode: ModePreflight}

	r.Register("dismiss", dismiss)
	r.Register("draft_reply", preview)

	got, ok := r.Lookup("dismiss")
	require.True(t, ok)
	require.Same(t, dismiss, got)

	got, ok = r.Lookup("draft_reply")
	require.True(t, ok)
	require.Same(t, preview, got)

	_, ok = r.Lookup("unknown_verb")
	require.False(t, ok)
}

func TestRegistry_Modes(t *testing.T) {
	r := NewRegistry()
	r.Register("dismiss", &fakeExec{mode: Mode1Click})
	r.Register("draft_reply", &fakeExec{mode: ModePreflight})

	modes := r.Modes()
	require.Equal(t, Mode1Click, modes["dismiss"])
	require.Equal(t, ModePreflight, modes["draft_reply"])
}

func TestCanonicalIntents_All(t *testing.T) {
	require.Len(t, CanonicalIntents, 27, "27 verbs (16 V2.8 + 7 V2.8.1 + complete_task/delete_task in V2.9 + edit_task + send_whatsapp in V2.12)")

	seen := map[string]bool{}
	for _, ci := range CanonicalIntents {
		require.NotEmpty(t, ci.Intent)
		require.NotEmpty(t, ci.Description)
		require.Contains(t, []Mode{Mode1Click, ModePreflight, ModeConfirm}, ci.Mode)
		require.False(t, seen[ci.Intent], "duplicate intent %q", ci.Intent)
		seen[ci.Intent] = true
	}
}

func TestIsCanonical(t *testing.T) {
	require.True(t, IsCanonical("dismiss"))
	require.True(t, IsCanonical("draft_reply"))
	require.True(t, IsCanonical("ask_followup"))
	require.False(t, IsCanonical("garbage"))
	require.False(t, IsCanonical(""))
}

func TestInferIntentForLabel(t *testing.T) {
	cases := []struct {
		label  string
		intent string
	}{
		{"Dismiss", "dismiss"},
		{"snooze", "snooze"},
		{"Mute", "dismiss"},
		{"Mark read", "mark_read"},
		{"Reply", "draft_reply"},
		{"Draft a reply", "draft_reply"},
		{"Open agenda", "open_url"},
		{"Block 17:00–18:30", "block_calendar"},
		{"Tell Sam yes", "rsvp_yes"},
		{"Decline", "rsvp_no"},
		{"Forward to Aria", "forward"},
		{"Add to concerns", "add_concern"},
		{"Remember this", "add_memory"},
		{"Ask Zeno", "ask_followup"},
		{"", ""},                    // empty label → empty intent (handler returns 400)
		{"banana split", "dismiss"}, // unknown → safe fallback
	}
	for _, tc := range cases {
		got := inferIntentForLabel(tc.label)
		require.Equal(t, tc.intent, got, "label=%q", tc.label)
	}
}
