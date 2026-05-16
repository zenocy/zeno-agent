package synth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// V2.12 — WhatsApp send action tools for the reactive Ask loop.
//
// `resolve_contact` is read-only: maps a free-form query to the canonical
// name + group flag of the matching contact, never returns the JID or any
// phone digits. Used as the disambiguation step before the LLM proposes a
// send.
//
// `send_whatsapp_message` *proposes* the action by dispatching the
// `send_whatsapp` intent in preview mode. The action handler returns a
// preview Result; the tool surfaces the composed body so the model can
// describe what's about to be sent. The actual commit happens when the
// user clicks Send on the resulting card — never inside the tool loop.
//
// Package boundary: action depends on synth (DraftReply, Card), so synth
// must not import action back. Both tools take a callback the wiring in
// cmd/zeno/main.go fills with closures around the resolver and the
// action handler.

// ResolveContactResult is the LLM-visible payload. Carries names only;
// callers (tool wiring + the action surface) keep operational identifiers
// off the prompt graph.
type ResolveContactResult struct {
	OK         bool     `json:"ok"`
	Name       string   `json:"name,omitempty"`
	IsGroup    bool     `json:"is_group,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	NotFound   bool     `json:"not_found,omitempty"`
}

// ResolveContactFn is the callback the wiring layer provides. It MUST
// translate whatsapp.Resolver errors into the corresponding fields:
// ErrAmbiguous → Candidates set, OK=false; ErrContactNotFound → NotFound
// true, OK=false. Never embed JIDs or phone numbers into Name or
// Candidates.
type ResolveContactFn func(ctx context.Context, query string) (ResolveContactResult, error)

// SendWhatsAppPreviewFn is the callback the wiring layer provides. It
// MUST dispatch the `send_whatsapp` intent in preview mode (Confirm:false)
// via action.Handler so the same audit/event trail is written. Returns
// the preview body the model can quote in its reply, plus the
// user-facing toast (used when send is blocked: not-found / ambiguous /
// daily-cap).
type SendWhatsAppPreviewFn func(ctx context.Context, target map[string]any) (preview map[string]any, toast string, err error)

// ----------------------------------------------------------------------
// resolve_contact
// ----------------------------------------------------------------------

// ResolveContactTool exposes whatsapp.Resolver to the LLM tool loop.
type ResolveContactTool struct {
	Resolve ResolveContactFn
}

func (t *ResolveContactTool) Name() string { return "resolve_contact" }

func (t *ResolveContactTool) Description() string {
	return "Look up a WhatsApp contact by name, alias, or relationship (\"my wife\", \"Sam\", \"family group\"). Returns the canonical contact name and whether it's a group, OR a list of candidate names when the query is ambiguous, OR not_found=true when no contact matches. Always call this BEFORE proposing send_whatsapp_message — never invent recipient names or phone numbers. The tool intentionally does NOT expose phone numbers or JIDs."
}

func (t *ResolveContactTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{
		{
			Name:        "query",
			Type:        "string",
			Description: "The recipient as the user phrased it. Required. Examples: \"Sam\", \"my wife\", \"family group\", \"Jamie Reyes\".",
			Required:    true,
		},
	}
}

func (t *ResolveContactTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t.Resolve == nil {
		return "", errors.New("resolve_contact: tool not configured")
	}
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "", fmt.Errorf("resolve_contact: query is required")
	}
	res, err := t.Resolve(ctx, query)
	if err != nil {
		return "", fmt.Errorf("resolve_contact: %w", err)
	}
	b, _ := json.Marshal(res)
	return string(b), nil
}

// ----------------------------------------------------------------------
// send_whatsapp_message
// ----------------------------------------------------------------------

// SendWhatsAppResult is the JSON shape returned to the loop. The model
// uses these fields to quote the draft back to the user; the real send
// happens when the user clicks the action button.
type SendWhatsAppResult struct {
	OK       bool   `json:"ok"`
	Proposed bool   `json:"proposed"`
	ToName   string `json:"to_name,omitempty"`
	IsGroup  bool   `json:"is_group,omitempty"`
	Body     string `json:"body,omitempty"`
	Toast    string `json:"toast,omitempty"`
}

// SendWhatsAppMessageTool proposes the `send_whatsapp` action.
type SendWhatsAppMessageTool struct {
	Preview SendWhatsAppPreviewFn
}

func (t *SendWhatsAppMessageTool) Name() string { return "send_whatsapp_message" }

func (t *SendWhatsAppMessageTool) Description() string {
	return "Propose a WhatsApp message to a contact. The tool composes a draft and surfaces it as a 'Send' card the user clicks to commit — the message is NOT sent inside this tool. ALWAYS call resolve_contact first to confirm the recipient is on the user's contacts list. Pass `recipient` exactly as resolve_contact returned it (the canonical name). Either `message` (pre-composed body) or `steer` (instructions for Zeno to compose) should be set."
}

func (t *SendWhatsAppMessageTool) Parameters() []llm.ToolParamSpec {
	return []llm.ToolParamSpec{
		{
			Name:        "recipient",
			Type:        "string",
			Description: "The canonical contact name returned by resolve_contact. Required.",
			Required:    true,
		},
		{
			Name:        "message",
			Type:        "string",
			Description: "Optional pre-composed message body. When set, Zeno uses it verbatim. Omit to let Zeno compose from `steer` and any context.",
			Required:    false,
		},
		{
			Name:        "steer",
			Type:        "string",
			Description: "Optional natural-language instruction for Zeno to compose the body, e.g. \"share the time and place of tomorrow's 6pm dinner\".",
			Required:    false,
		},
		{
			Name:        "context_kind",
			Type:        "string",
			Description: "Optional context type — \"event\" or \"mail\" — used to ground the composed message.",
			Required:    false,
			Enum:        []string{"event", "mail", "none"},
		},
		{
			Name:        "context_id",
			Type:        "string",
			Description: "Optional context identifier (calendar event UID or mail subject) Zeno uses when composing.",
			Required:    false,
		},
	}
}

func (t *SendWhatsAppMessageTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t.Preview == nil {
		return "", errors.New("send_whatsapp_message: tool not configured")
	}
	recipient := strings.TrimSpace(argString(args, "recipient"))
	if recipient == "" {
		return "", fmt.Errorf("send_whatsapp_message: recipient is required")
	}

	target := map[string]any{"recipient": recipient}
	if v := argString(args, "message"); v != "" {
		target["message"] = v
	}
	if v := argString(args, "steer"); v != "" {
		target["steer"] = v
	}
	if v := argString(args, "context_kind"); v != "" {
		target["context_kind"] = v
	}
	if v := argString(args, "context_id"); v != "" {
		target["context_id"] = v
	}

	preview, toast, err := t.Preview(ctx, target)
	if err != nil {
		return "", fmt.Errorf("send_whatsapp_message: %w", err)
	}
	res := SendWhatsAppResult{Toast: toast}
	if preview == nil {
		// The preview was suppressed — typically because the resolver
		// failed and the executor returned a non-OK Result. The toast
		// already explains why; surface that to the model.
		b, _ := json.Marshal(res)
		return string(b), nil
	}
	res.OK = true
	res.Proposed = true
	if v, ok := preview["to_name"].(string); ok {
		res.ToName = v
	}
	if v, ok := preview["is_group"].(bool); ok {
		res.IsGroup = v
	}
	if v, ok := preview["body"].(string); ok {
		res.Body = v
	}
	b, _ := json.Marshal(res)
	return string(b), nil
}
