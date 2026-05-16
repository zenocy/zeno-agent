package whatsapp

import (
	"strings"
	"time"
)

// RuntimeConfig is the operator-tunable subset of the integration's
// behavior. Stored in the whatsapp_config SQLite table and surfaced via
// PUT /api/whatsapp/config; reloaded into the live Service without a
// restart so an allowlist edit takes effect on the next message.
//
// Defaults are conservative — mention-only group activation, 3s
// per-chat throttle, 4 in-flight syntheses globally — to keep ban risk
// down while the integration soaks. Operators can loosen via the
// Settings UI; loosening below 1s/chat is a footgun and the handler
// rejects it.
type RuntimeConfig struct {
	// MentionName is the textual handle Zeno listens for in groups,
	// e.g. "zeno" → match "@zeno". Case-insensitive, word-boundary.
	// Empty falls back to the default.
	MentionName string

	// AllowedDMs is the JID allowlist for direct messages. Senders
	// outside this list are dropped without logging. Group activation
	// (mention) bypasses this list — anyone in a group can ping Zeno
	// if Zeno is also in that group.
	AllowedDMs []string

	// MinChatInterval is the minimum gap between two replies from
	// Zeno in the same chat. Prevents accidental burst-replies (the
	// dispatcher waits this long after the previous send before
	// processing the next message in the same JID).
	MinChatInterval time.Duration

	// MaxConcurrentSynth caps the number of in-flight synth.Ask calls
	// across all chats. Default 4.
	MaxConcurrentSynth int

	// PerChatBuffer is the per-JID inbox capacity. Drop-oldest on
	// overflow; default 4.
	PerChatBuffer int
}

// DefaultRuntimeConfig returns the v0 defaults.
func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		MentionName:        "zeno",
		AllowedDMs:         nil,
		MinChatInterval:    3 * time.Second,
		MaxConcurrentSynth: 4,
		PerChatBuffer:      4,
	}
}

// Normalize fills in zero-valued fields with defaults. The Service
// calls this on every SetRuntimeConfig so callers can supply partial
// configs without losing the safe defaults.
//
// V2.13.3c: AllowedDMs entries are run through NormalizeJID — strips
// `+`, lowercases the server, fills `@s.whatsapp.net` when given a
// bare phone — so comparisons against inbound JIDs (always digit-only
// on the wire) succeed regardless of how the operator typed them.
func (c RuntimeConfig) Normalize() RuntimeConfig {
	d := DefaultRuntimeConfig()
	if c.MentionName == "" {
		c.MentionName = d.MentionName
	}
	if c.MinChatInterval <= 0 {
		c.MinChatInterval = d.MinChatInterval
	}
	if c.MaxConcurrentSynth <= 0 {
		c.MaxConcurrentSynth = d.MaxConcurrentSynth
	}
	if c.PerChatBuffer <= 0 {
		c.PerChatBuffer = d.PerChatBuffer
	}
	if len(c.AllowedDMs) > 0 {
		out := make([]string, 0, len(c.AllowedDMs))
		seen := make(map[string]struct{}, len(c.AllowedDMs))
		for _, jid := range c.AllowedDMs {
			n := NormalizeJID(jid)
			if n == "" {
				continue
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, n)
		}
		c.AllowedDMs = out
	}
	return c
}

// NormalizeJID returns the canonical comparison form of a WhatsApp JID
// or phone-shaped string. The wire format from whatsmeow is always
// digit-only (no `+`) and lowercase server, so applying this on
// operator-input AllowedDMs and on any defensive-comparison site
// keeps both sides in the same shape.
//
// Inputs handled:
//   - "+447700900333@s.whatsapp.net"        → "447700900333@s.whatsapp.net"
//   - "447700900333@s.whatsapp.net"         → unchanged
//   - "+447700900333"                       → "447700900333@s.whatsapp.net"
//   - "00447700900333"                      → "447700900333@s.whatsapp.net"
//   - "1203@G.US"                          → "1203@g.us"
//   - "+44 7700 900 333 @ s.whatsapp.net"   → "447700900333@s.whatsapp.net"
//   - ""                                   → ""
//
// Group/LID JIDs (`@g.us`, `@lid`) keep their user part verbatim —
// only the phone-based `@s.whatsapp.net` namespace gets digit
// normalization. When the user part of an `s.whatsapp.net` JID has
// no digits, the input is preserved as-is (lowercased server only)
// so callers can compare directly without losing information.
func NormalizeJID(jid string) string {
	s := strings.TrimSpace(jid)
	if s == "" {
		return ""
	}
	at := strings.IndexByte(s, '@')
	if at < 0 {
		// Bare input — try phone normalization; pass through on failure.
		digits := normalizePhoneDigits(s)
		if digits != "" {
			return digits + "@s.whatsapp.net"
		}
		return s
	}
	user := strings.TrimSpace(s[:at])
	server := strings.ToLower(strings.TrimSpace(s[at+1:]))
	if server == "s.whatsapp.net" {
		if digits := normalizePhoneDigits(user); digits != "" {
			user = digits
		}
	}
	return user + "@" + server
}

// normalizePhoneDigits strips spaces, a leading `+`/`00`, and any
// non-digit characters. Mirrors store.NormalizePhone but is kept
// in-package to avoid an internal/store import cycle.
func normalizePhoneDigits(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "+")
	raw = strings.TrimPrefix(raw, "00")
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// allowedDMSet renders AllowedDMs as a lookup map. Empty input → nil
// map (the classifier treats nil as "deny all DMs").
func (c RuntimeConfig) allowedDMSet() map[string]struct{} {
	if len(c.AllowedDMs) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(c.AllowedDMs))
	for _, jid := range c.AllowedDMs {
		out[jid] = struct{}{}
	}
	return out
}

// WithAdditionalAllowedDM returns a copy of c whose AllowedDMs slice
// includes jid (after NormalizeJID). Idempotent: a no-op when jid is
// already in the list. Used by V2.13.3 routeInbound to widen the
// allowlist for a single classification when there's an open
// ExpectedReply for the sender. Returns a copy with a fresh backing
// array so the original config's slice can't be aliased by callers.
func (c RuntimeConfig) WithAdditionalAllowedDM(jid string) RuntimeConfig {
	canonical := NormalizeJID(jid)
	if canonical == "" {
		return c
	}
	for _, existing := range c.AllowedDMs {
		if existing == canonical {
			return c
		}
	}
	out := c
	out.AllowedDMs = make([]string, 0, len(c.AllowedDMs)+1)
	out.AllowedDMs = append(out.AllowedDMs, c.AllowedDMs...)
	out.AllowedDMs = append(out.AllowedDMs, canonical)
	return out
}
