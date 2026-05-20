package synth

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// DraftWhatsAppContext is the input to DraftWhatsApp. The send action
// passes either a calendar event, a mail snippet, or no context at all
// when the user supplied a steer that fully describes the message.
type DraftWhatsAppContext struct {
	// RecipientName is the display name of the resolved contact (e.g.
	// "Sam Carter"). The model uses it for tone, never to fabricate
	// addressable identifiers.
	RecipientName string

	// IsGroup true → second-person plural register ("hey all").
	IsGroup bool

	// One of EventTitle / MailSubject is set when the user is sharing a
	// specific item. Both empty → the steer is the only signal.
	EventTitle    string
	EventLocation string
	EventStart    time.Time
	EventEnd      time.Time
	MailSubject   string
	MailSnippet   string
	MailFrom      string

	// UserSteer is the user's natural-language instruction
	// ("share the time and place"; "tell her I'm running late").
	UserSteer string

	// V2.13.0 assistant persona. AssistantName empty → first-person
	// register (legacy "on behalf of the user"); non-empty → third-person
	// EA register, signed `\n— <AssistantName>` on the opening message
	// of a thread. UserName names the principal ("Jamie"); falls back
	// to "the user" when empty.
	AssistantName string
	UserName      string
	AssistantTone string

	// AssistantRegister is the voice-canon register block from
	// `_voice.md`. Pre-loaded by the caller from PromptSet.AssistantRegister
	// so this package stays free of an embed dependency.
	AssistantRegister string
}

// DraftWhatsAppOpts tunes the synth call.
type DraftWhatsAppOpts struct {
	LLM      llm.Provider
	Voice    string        // typically PromptSet.VoiceShort
	Deadline time.Duration // default 20s
}

// DraftWhatsApp produces a 1–3 sentence plain-text WhatsApp message.
// Returns an empty string only when the LLM fails fatally; callers
// should fall back to a template.
//
// Output discipline:
//   - 1–3 short sentences. WhatsApp messages are read on phones; brevity
//     is the format.
//   - No greeting/sign-off lines (different register from email).
//   - No emoji unless the user's steer asks for one.
//   - No quoted/forwarded markup.
//   - No phone numbers, JIDs, or addresses — those are operational
//     identifiers, never user-facing copy.
func DraftWhatsApp(ctx context.Context, opts DraftWhatsAppOpts, c DraftWhatsAppContext) (string, error) {
	if opts.LLM == nil {
		return "", fmt.Errorf("draft_whatsapp: LLM client is nil")
	}
	deadline := opts.Deadline
	if deadline <= 0 {
		deadline = 20 * time.Second
	}

	system := buildWhatsAppSystemPrompt(opts.Voice, c)
	user := buildWhatsAppUserPrompt(c)

	if opts.LLM.NoThink() && isQwen3(opts.LLM.Model()) {
		system = "/no_think\n\n" + system
	}

	cctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	cr, err := opts.LLM.ChatCompletion(cctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}, nil, llm.WithTemperature(0.4))
	if err != nil {
		return "", fmt.Errorf("draft_whatsapp: chat: %w", err)
	}
	body := cleanWhatsAppOutput(cr.Content)
	if body == "" {
		return "", fmt.Errorf("draft_whatsapp: empty body from model")
	}
	return body, nil
}

// FallbackWhatsAppBody returns a minimal, deterministic message string
// when the LLM is unavailable. Same shape philosophy as
// fallbackReplyBody in executors_mail.go: degrade rather than fail the
// preview entirely.
//
// When AssistantName is set the body switches to third-person assistant
// voice and includes the `— <name>` sign-off so the preview reflects
// what a successful synth would have produced.
func FallbackWhatsAppBody(c DraftWhatsAppContext) string {
	body := fallbackBodyCore(c)
	if c.AssistantName != "" && body != "" {
		body = strings.TrimRight(body, " \n")
		body += "\n— " + c.AssistantName
	}
	return body
}

func fallbackBodyCore(c DraftWhatsAppContext) string {
	principal := strings.TrimSpace(c.UserName)
	if principal == "" {
		principal = "the user"
	}
	asAssistant := c.AssistantName != ""
	switch {
	case c.EventTitle != "" && !c.EventStart.IsZero():
		when := c.EventStart.Format("Mon 2 Jan, 15:04")
		if asAssistant {
			if c.EventLocation != "" {
				return fmt.Sprintf("Hi — %s asked me to confirm %s on %s at %s. Does that still work for you?", principal, c.EventTitle, when, c.EventLocation)
			}
			return fmt.Sprintf("Hi — %s asked me to confirm %s on %s. Does that still work for you?", principal, c.EventTitle, when)
		}
		if c.EventLocation != "" {
			return fmt.Sprintf("Heads-up: %s on %s at %s.", c.EventTitle, when, c.EventLocation)
		}
		return fmt.Sprintf("Heads-up: %s on %s.", c.EventTitle, when)
	case c.MailSubject != "":
		if asAssistant {
			return fmt.Sprintf("Hi — %s asked me to flag a thread about %q with you; %s will follow up shortly.", principal, c.MailSubject, principal)
		}
		return fmt.Sprintf("Quick heads-up about %q. I'll fill you in shortly.", c.MailSubject)
	case strings.TrimSpace(c.UserSteer) != "":
		if asAssistant {
			return fmt.Sprintf("Hi — a quick note from %s: %s", principal, strings.TrimSpace(c.UserSteer))
		}
		return strings.TrimSpace(c.UserSteer)
	default:
		if asAssistant {
			return fmt.Sprintf("Hi — quick message from %s.", principal)
		}
		return "Quick message from Zeno."
	}
}

func buildWhatsAppSystemPrompt(voice string, c DraftWhatsAppContext) string {
	var b bytes.Buffer
	if voice != "" {
		b.WriteString(voice)
		b.WriteString("\n\n")
	}
	if c.AssistantName != "" {
		// Assistant register: ordering canon → register → tone-note so
		// the canon's hard rules win on conflict.
		if c.AssistantRegister != "" {
			b.WriteString(c.AssistantRegister)
			b.WriteString("\n\n")
		}
		principal := strings.TrimSpace(c.UserName)
		if principal == "" {
			principal = "the user"
		}
		fmt.Fprintf(&b,
			`You are %s, %s's executive assistant, drafting a WhatsApp message on the principal's behalf. Output ONLY the message body — no headers, no JSON, no markdown, no code fences, no quoted source.

Constraints:
- 1 to 3 short sentences.
- Third person about the principal. Refer to %s by name; do not speak as %s.
- Plain text. No emoji unless the user's instruction explicitly asks for one.
- Warm-professional. No "Dear", no exclamation marks, no honorifics.
- Sign the message with a single trailing line: "\n— %s". Do not sign with the principal's name. No other signature.
- Never include phone numbers, JIDs, or postal addresses in the message body.
- If you are conveying an event, include the title and the time/place concretely — no "soon" or "tonight" when an exact time exists.
- Mirror the user's instructions exactly. If the user says "confirm dinner", confirm; if the user says "ask if 7pm still works", ask that.
`, c.AssistantName, principal, principal, principal, c.AssistantName)
		if tone := strings.TrimSpace(c.AssistantTone); tone != "" {
			fmt.Fprintf(&b, "\nTone steer (refines voice canon; canon wins on conflict): %s\n", tone)
		}
		return b.String()
	}
	b.WriteString(`You are drafting a WhatsApp message on behalf of the user. Output ONLY the message body — no headers, no JSON, no markdown, no code fences, no quoted source.

Constraints:
- 1 to 3 short sentences.
- Plain text. No emoji unless the user's instruction explicitly asks for one.
- Conversational, not formal. No "Dear ..." opening, no signature.
- Never include phone numbers, JIDs, or postal addresses in the message body.
- If you are sharing an event, include the title and the time/place — concretely, no rephrasing as "soon" or "tonight" when an exact time exists.
- Mirror the user's instructions exactly. If the user says "decline politely", decline; if the user says "tell her I'll be late", say that.
`)
	return b.String()
}

func buildWhatsAppUserPrompt(c DraftWhatsAppContext) string {
	var b bytes.Buffer
	if c.RecipientName != "" {
		if c.IsGroup {
			fmt.Fprintf(&b, "Recipient: WhatsApp group %q\n", c.RecipientName)
		} else {
			fmt.Fprintf(&b, "Recipient: %s\n", c.RecipientName)
		}
	}

	if c.EventTitle != "" {
		b.WriteString("\nEvent context:\n")
		fmt.Fprintf(&b, "Title: %s\n", c.EventTitle)
		if !c.EventStart.IsZero() {
			fmt.Fprintf(&b, "Start: %s\n", c.EventStart.Format("Mon, 2 Jan 2006 15:04 MST"))
		}
		if !c.EventEnd.IsZero() {
			fmt.Fprintf(&b, "End: %s\n", c.EventEnd.Format("Mon, 2 Jan 2006 15:04 MST"))
		}
		if c.EventLocation != "" {
			fmt.Fprintf(&b, "Location: %s\n", c.EventLocation)
		}
	}

	if c.MailSubject != "" {
		b.WriteString("\nEmail context:\n")
		if c.MailFrom != "" {
			fmt.Fprintf(&b, "From: %s\n", c.MailFrom)
		}
		fmt.Fprintf(&b, "Subject: %s\n", c.MailSubject)
		if c.MailSnippet != "" {
			b.WriteString("Excerpt: ")
			b.WriteString(strings.TrimSpace(c.MailSnippet))
			b.WriteString("\n")
		}
	}

	b.WriteString("\nInstruction: ")
	if strings.TrimSpace(c.UserSteer) != "" {
		b.WriteString(strings.TrimSpace(c.UserSteer))
	} else if c.EventTitle != "" {
		b.WriteString("Share the event details concisely.")
	} else if c.MailSubject != "" {
		b.WriteString("Pass on what's in the email in your own words.")
	} else {
		b.WriteString("Compose a brief, friendly message.")
	}
	b.WriteString("\n\nOutput the message body only.")
	return b.String()
}

// cleanWhatsAppOutput strips fences, "Message:" labels, and trailing
// signatures the model sometimes adds. Newlines normalised; empty lines
// trimmed.
func cleanWhatsAppOutput(s string) string {
	s = strings.TrimSpace(s)
	s = stripCodeFences(s)
	for {
		lower := strings.ToLower(s)
		switch {
		case strings.HasPrefix(lower, "message:"):
			s = strings.TrimSpace(s[len("Message:"):])
			continue
		case strings.HasPrefix(lower, "body:"):
			s = strings.TrimSpace(s[len("Body:"):])
			continue
		}
		break
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// Collapse 3+ blank lines that the model sometimes emits.
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}
