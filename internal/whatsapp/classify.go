package whatsapp

import (
	"strings"
	"time"
	"unicode"
)

// Action is the verdict the classifier returns for one inbound message.
type Action string

const (
	// ActionDrop means the message must NOT touch the log, the LLM,
	// or any persistent surface. The classifier emits Drop for echoes
	// of Zeno's own sends, history-sync replays, non-allowlisted DMs,
	// and group messages that don't @-mention Zeno.
	ActionDrop Action = "drop"

	// ActionProcess means the message goes through the dispatcher and
	// (typically) ends in a synth-driven reply. When PreCannedReply is
	// non-empty, the dispatcher SHOULD bypass synth and send the fixed
	// text directly — used for unsupported message types like images.
	ActionProcess Action = "process"
)

// Decision is the structured output of Classify, threaded through the
// dispatcher into either synth.Ask or a pre-canned reply path.
type Decision struct {
	Action         Action
	Reason         string // human-readable; logged for debugging at debug level
	PreCannedReply string // when non-empty, dispatcher sends this verbatim and skips synth

	// Conversation context — populated even on Drop so log/debug
	// scopes have full picture if needed. Process decisions feed
	// these into synth.ReactiveDeps.Conversation in Phase 3.
	ChatJID    string
	SenderJID  string
	SenderName string
	GroupName  string
	IsDM       bool
	IsMention  bool
	Text       string
	MessageID  string
	Timestamp  time.Time
}

// historySyncGrace is the slack we leave around pair-time when deciding
// whether an inbound message is from before pairing. WhatsApp's history
// sync can deliver messages with timestamps slightly after pair-time as
// part of the initial backfill — we want to ignore those.
const historySyncGrace = 30 * time.Second

// Classify decides what to do with one inbound message. The rules run
// in this order; the first matching rule wins:
//
//  1. Echoes of Zeno's own sends (IsFromMe) → Drop.
//  2. Pair-time history-sync messages → Drop.
//  3. Unsupported message types (images, voice, video) — IF the message
//     would otherwise be processed (DM allowlist OR group mention) →
//     Process with a fixed PreCannedReply explaining the limitation.
//     Otherwise → Drop (we don't reply with "I can't see images" to
//     random group photos).
//  4. DM whose sender is not in AllowedDMs → Drop.
//  5. Group message that does not @-mention Zeno → Drop.
//  6. Default → Process.
//
// pairTime is when the current session was paired (zero on a never-paired
// service, in which case the history-sync rule is skipped). ownJID is
// the bot's JID — used to detect formal mentions.
func Classify(msg IncomingMessage, cfg RuntimeConfig, ownJID string, pairTime, now time.Time) Decision {
	cfg = cfg.Normalize()
	d := Decision{
		ChatJID:    msg.ChatJID,
		SenderJID:  msg.SenderJID,
		SenderName: msg.SenderName,
		GroupName:  msg.GroupName,
		IsDM:       !msg.IsGroup,
		Text:       msg.Text,
		MessageID:  msg.MessageID,
		Timestamp:  msg.Timestamp,
	}

	if msg.IsFromMe {
		d.Action = ActionDrop
		d.Reason = "echo of own send"
		return d
	}

	if !pairTime.IsZero() && msg.Timestamp.Before(pairTime.Add(-historySyncGrace)) {
		d.Action = ActionDrop
		d.Reason = "history sync (timestamp predates pair)"
		return d
	}

	isMention := false
	if msg.IsGroup {
		isMention = mentionsBot(msg, cfg.MentionName, ownJID)
		d.IsMention = isMention
	}

	allowedDMs := cfg.allowedDMSet()

	// Eligibility for Process before checking message type — we only
	// apologize for unsupported media to senders we'd otherwise talk
	// to.
	eligible := false
	if msg.IsGroup {
		eligible = isMention
	} else {
		_, ok := allowedDMs[msg.SenderJID]
		eligible = ok
	}

	if !eligible {
		d.Action = ActionDrop
		if msg.IsGroup {
			d.Reason = "group message without mention"
		} else {
			d.Reason = "dm sender not in allowlist"
		}
		return d
	}

	if msg.Type != MessageTypeText {
		d.Action = ActionProcess
		d.PreCannedReply = preCannedReplyFor(msg.Type)
		d.Reason = "unsupported message type: " + string(msg.Type)
		return d
	}

	d.Action = ActionProcess
	d.Reason = "ok"
	return d
}

// mentionsBot returns true when the message either (a) lists ownJID in
// its formal MentionedJID list, or (b) contains @<MentionName> as a
// word-boundary substring. The fallback string match is important for
// users whose phone isn't easily able to type a JID-mention for the
// bot's number — they can still write "@zeno" manually.
func mentionsBot(msg IncomingMessage, mentionName, ownJID string) bool {
	if ownJID != "" {
		for _, m := range msg.Mentions {
			if m == ownJID {
				return true
			}
		}
	}
	if mentionName == "" {
		return false
	}
	return containsWordMention(msg.Text, mentionName)
}

// containsWordMention reports whether s contains "@<name>" with a word
// boundary on the right side (left side is the @ sign which already
// acts as a boundary). Case-insensitive on the name. "@zeno-bot" does
// NOT match "@zeno" because '-' is not alphanumeric → boundary check
// counts the trailing '-' as a non-letter, so we'd accept it; tighten
// to require the right-hand boundary to be non-letter-non-digit-non-dash
// so "@zeno-bot" is rejected.
func containsWordMention(s, name string) bool {
	if name == "" {
		return false
	}
	lower := strings.ToLower(s)
	target := "@" + strings.ToLower(name)
	for i := 0; ; {
		idx := strings.Index(lower[i:], target)
		if idx < 0 {
			return false
		}
		end := i + idx + len(target)
		if end >= len(lower) {
			return true
		}
		next := rune(lower[end])
		if !isMentionContinuation(next) {
			return true
		}
		i = end
	}
}

// isMentionContinuation reports whether r is a character that would
// extend a mention name (so "@zeno-bot" or "@zenotech" are NOT mentions
// of "zeno"). We are deliberately strict: only ASCII letters, digits,
// and '-' / '_' are continuations. Punctuation (',', '.', '?', '!') and
// whitespace are boundaries.
func isMentionContinuation(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_'
}

// preCannedReplyFor returns a fixed conversational message for an
// unsupported inbound type. Returned text is rendered verbatim.
func preCannedReplyFor(t MessageType) string {
	switch t {
	case MessageTypeImage:
		return "I can't see images yet — send me text and I'll do my best."
	case MessageTypeVoice:
		return "I can't transcribe voice notes yet — text me and I'm here."
	case MessageTypeVideo:
		return "I can't watch videos yet — text me and I'm here."
	default:
		return "I can only handle text messages right now."
	}
}
