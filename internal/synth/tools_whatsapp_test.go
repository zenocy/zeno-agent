package synth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveContactTool_RejectsMissingArg(t *testing.T) {
	tool := &ResolveContactTool{Resolve: func(ctx context.Context, q string) (ResolveContactResult, error) {
		return ResolveContactResult{OK: true, Name: q}, nil
	}}
	_, err := tool.Execute(context.Background(), map[string]any{})
	require.Error(t, err)
}

func TestResolveContactTool_HitDoesNotLeakJID(t *testing.T) {
	called := ""
	tool := &ResolveContactTool{Resolve: func(ctx context.Context, q string) (ResolveContactResult, error) {
		called = q
		return ResolveContactResult{OK: true, Name: "Sam Carter", IsGroup: false}, nil
	}}
	out, err := tool.Execute(context.Background(), map[string]any{"query": "my wife"})
	require.NoError(t, err)
	require.Equal(t, "my wife", called)
	// The output JSON must contain the canonical name, never a JID/phone.
	require.Contains(t, out, "Sam Carter")
	require.NotContains(t, out, "@s.whatsapp.net")
	require.NotContains(t, out, "@g.us")
	require.NotRegexp(t, `\d{6,}`, out)
}

func TestResolveContactTool_AmbiguousReturnsCandidates(t *testing.T) {
	tool := &ResolveContactTool{Resolve: func(ctx context.Context, q string) (ResolveContactResult, error) {
		return ResolveContactResult{OK: false, Candidates: []string{"Sam Carter", "Sam Other"}}, nil
	}}
	out, err := tool.Execute(context.Background(), map[string]any{"query": "sam"})
	require.NoError(t, err)
	require.Contains(t, out, "Sam Carter")
	require.Contains(t, out, "Sam Other")
}

func TestResolveContactTool_NotFound(t *testing.T) {
	tool := &ResolveContactTool{Resolve: func(ctx context.Context, q string) (ResolveContactResult, error) {
		return ResolveContactResult{OK: false, NotFound: true}, nil
	}}
	out, err := tool.Execute(context.Background(), map[string]any{"query": "ghost"})
	require.NoError(t, err)
	require.Contains(t, out, "not_found")
}

func TestResolveContactTool_UnconfigedReturnsError(t *testing.T) {
	tool := &ResolveContactTool{}
	_, err := tool.Execute(context.Background(), map[string]any{"query": "x"})
	require.Error(t, err)
}

func TestSendWhatsAppMessageTool_ProposalCarriesPreviewBody(t *testing.T) {
	tool := &SendWhatsAppMessageTool{Preview: func(ctx context.Context, target map[string]any) (map[string]any, string, error) {
		return map[string]any{
			"to_name":  "Sam Carter",
			"is_group": false,
			"body":     "Hey Sam — dinner at 6pm tomorrow.",
		}, "", nil
	}}
	out, err := tool.Execute(context.Background(), map[string]any{
		"recipient": "Sam Carter",
		"steer":     "share the time",
	})
	require.NoError(t, err)
	require.Contains(t, out, "Sam Carter")
	require.Contains(t, out, "dinner at 6pm tomorrow")
	require.Contains(t, out, `"proposed":true`)
	require.NotContains(t, out, "@s.whatsapp.net", "tool output must not leak JIDs")
}

func TestSendWhatsAppMessageTool_BlockedSurfacesToast(t *testing.T) {
	tool := &SendWhatsAppMessageTool{Preview: func(ctx context.Context, target map[string]any) (map[string]any, string, error) {
		return nil, "I don't have \"ghost\" saved as a contact.", nil
	}}
	out, err := tool.Execute(context.Background(), map[string]any{"recipient": "ghost"})
	require.NoError(t, err)
	require.Contains(t, out, "ghost")
	require.NotContains(t, out, `"proposed":true`)
}

func TestSendWhatsAppMessageTool_RejectsMissingRecipient(t *testing.T) {
	tool := &SendWhatsAppMessageTool{Preview: func(ctx context.Context, target map[string]any) (map[string]any, string, error) {
		t.Fatal("preview should not be called when recipient is missing")
		return nil, "", nil
	}}
	_, err := tool.Execute(context.Background(), map[string]any{})
	require.Error(t, err)
}

func TestSendWhatsAppMessageTool_PassesThroughError(t *testing.T) {
	tool := &SendWhatsAppMessageTool{Preview: func(ctx context.Context, target map[string]any) (map[string]any, string, error) {
		return nil, "", errors.New("dispatch broken")
	}}
	_, err := tool.Execute(context.Background(), map[string]any{"recipient": "Sam"})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "dispatch broken"))
}

func TestSendWhatsAppMessageTool_PlumbsAllFields(t *testing.T) {
	var got map[string]any
	tool := &SendWhatsAppMessageTool{Preview: func(ctx context.Context, target map[string]any) (map[string]any, string, error) {
		got = target
		return map[string]any{"body": "ok"}, "", nil
	}}
	_, err := tool.Execute(context.Background(), map[string]any{
		"recipient":    "Sam",
		"steer":        "be brief",
		"context_kind": "event",
		"context_id":   "ev-1",
	})
	require.NoError(t, err)
	require.Equal(t, "Sam", got["recipient"])
	require.Equal(t, "be brief", got["steer"])
	require.Equal(t, "event", got["context_kind"])
	require.Equal(t, "ev-1", got["context_id"])
}
