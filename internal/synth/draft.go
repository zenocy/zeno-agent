package synth

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// DraftReplyContext is the input to DraftReply: the source thread the
// user is replying to plus the user's optional steer ("decline politely",
// "agree to the redline", etc).
type DraftReplyContext struct {
	From        string    // sender name + address as it appears on the source mail
	Subject     string    // the source mail's subject (the drafter prefixes Re: itself)
	BodySnippet string    // 1-3 paragraphs from the source mail; trimmed before prompt
	SentAt      time.Time // header Date of the source mail
	UserSteer   string    // optional: target.steer from the action's Target map
}

// DraftReplyOpts tunes the drafting call. Zero values use sensible defaults.
type DraftReplyOpts struct {
	LLM      *llm.Client
	Voice    string        // typically PromptSet.VoiceShort
	Deadline time.Duration // default 30s
	Stage    string        // observability tag; default "draft_reply"
}

// DraftReply produces a plain-text reply body. Returns an empty string
// only if the LLM fails fatally; callers should fall through to a
// template fallback in that case.
//
// The returned body is meant to be saved to Drafts (or sent verbatim
// when the user clicks Send in the modal). It is plain text, not HTML;
// the MIME builder wraps it in quoted-printable text/plain.
func DraftReply(ctx context.Context, opts DraftReplyOpts, c DraftReplyContext) (string, error) {
	if opts.LLM == nil {
		return "", fmt.Errorf("draft: LLM client is nil")
	}
	deadline := opts.Deadline
	if deadline <= 0 {
		deadline = 30 * time.Second
	}
	stage := opts.Stage
	if stage == "" {
		stage = "draft_reply"
	}

	system := buildDraftSystemPrompt(opts.Voice)
	user := buildDraftUserPrompt(c)

	if opts.LLM.NoThink() && isQwen3(opts.LLM.Model()) {
		system = "/no_think\n\n" + system
	}

	cctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	_ = stage // observability hook reserved for future LoopObserver integration
	cr, err := opts.LLM.ChatCompletion(cctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}, nil, llm.WithTemperature(0.4))
	if err != nil {
		return "", fmt.Errorf("draft: chat: %w", err)
	}
	body := cleanDraftOutput(cr.Content)
	if body == "" {
		return "", fmt.Errorf("draft: empty body from model")
	}
	return body, nil
}

func buildDraftSystemPrompt(voice string) string {
	var b bytes.Buffer
	if voice != "" {
		b.WriteString(voice)
		b.WriteString("\n\n")
	}
	b.WriteString(`You are drafting a reply to an email. Output ONLY the body of the reply — no greeting block headers, no JSON, no markdown code fences, no signature.

Constraints:
- Plain text only.
- 2–6 sentences. Concrete. No filler ("I hope this finds you well").
- Mirror the sender's register; do not invent obligations.
- If the source asks a question you cannot answer from the snippet, say so explicitly rather than fabricate.
- End with a short sign-off line ("Thanks," or "Best,") followed by a placeholder name on its own line; the user replaces this before sending.
`)
	return b.String()
}

func buildDraftUserPrompt(c DraftReplyContext) string {
	var b bytes.Buffer
	b.WriteString("Source message:\n\n")
	if c.From != "" {
		fmt.Fprintf(&b, "From: %s\n", c.From)
	}
	if !c.SentAt.IsZero() {
		fmt.Fprintf(&b, "Date: %s\n", c.SentAt.Format("Mon, 2 Jan 2006 15:04 MST"))
	}
	if c.Subject != "" {
		fmt.Fprintf(&b, "Subject: %s\n", c.Subject)
	}
	if c.BodySnippet != "" {
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(c.BodySnippet))
		b.WriteString("\n")
	}
	b.WriteString("\n---\n")
	if c.UserSteer != "" {
		fmt.Fprintf(&b, "How to reply: %s\n\n", c.UserSteer)
	} else {
		b.WriteString("Draft a calm, concrete reply.\n\n")
	}
	b.WriteString("Output the body only.")
	return b.String()
}

// cleanDraftOutput strips leading/trailing fences, "Body:"/"Reply:"
// labels, and the model's habit of repeating "Subject: ..." inside the
// body. Newlines are normalized to '\n'.
func cleanDraftOutput(s string) string {
	s = strings.TrimSpace(s)
	s = stripCodeFences(s)
	for {
		lower := strings.ToLower(s)
		switch {
		case strings.HasPrefix(lower, "body:"):
			s = strings.TrimSpace(s[len("Body:"):])
			continue
		case strings.HasPrefix(lower, "reply:"):
			s = strings.TrimSpace(s[len("Reply:"):])
			continue
		case strings.HasPrefix(lower, "subject:"):
			// model echoed Subject — drop the line
			if idx := strings.Index(s, "\n"); idx > 0 {
				s = strings.TrimSpace(s[idx+1:])
				continue
			}
		}
		break
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return s
}
