package whatsapp_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/zenocy/zeno-v2/internal/whatsapp"
)

// TestClassify covers the rule ordering and edge cases of the
// classifier. The classifier is the privacy boundary — every Drop
// reason must remain Drop because anything that promotes to Process
// crosses into the observation log via the dispatcher.
func TestClassify(t *testing.T) {
	pairTime := time.Date(2026, 5, 6, 9, 0, 0, 0, time.UTC)
	now := pairTime.Add(time.Hour)
	ownJID := "999@s.whatsapp.net"
	owner := "1@s.whatsapp.net"

	cfg := whatsapp.RuntimeConfig{
		MentionName: "zeno",
		AllowedDMs:  []string{owner},
	}

	cases := []struct {
		name string
		in   whatsapp.IncomingMessage
		want whatsapp.Action
	}{
		{
			name: "echo of own send dropped",
			in: whatsapp.IncomingMessage{
				SenderJID: ownJID, ChatJID: ownJID, IsFromMe: true,
				Type: whatsapp.MessageTypeText, Text: "hi", Timestamp: now,
			},
			want: whatsapp.ActionDrop,
		},
		{
			name: "history sync dropped",
			in: whatsapp.IncomingMessage{
				SenderJID: owner, ChatJID: owner,
				Type: whatsapp.MessageTypeText, Text: "old",
				Timestamp: pairTime.Add(-2 * time.Minute),
			},
			want: whatsapp.ActionDrop,
		},
		{
			name: "DM from owner processes",
			in: whatsapp.IncomingMessage{
				SenderJID: owner, ChatJID: owner,
				Type: whatsapp.MessageTypeText, Text: "what's on today?",
				Timestamp: now,
			},
			want: whatsapp.ActionProcess,
		},
		{
			name: "DM from stranger dropped",
			in: whatsapp.IncomingMessage{
				SenderJID: "55@s.whatsapp.net", ChatJID: "55@s.whatsapp.net",
				Type: whatsapp.MessageTypeText, Text: "hi", Timestamp: now,
			},
			want: whatsapp.ActionDrop,
		},
		{
			name: "group mention processes",
			in: whatsapp.IncomingMessage{
				SenderJID: "5@s.whatsapp.net", ChatJID: "g@g.us",
				IsGroup: true, Type: whatsapp.MessageTypeText,
				Text: "@zeno when is the meeting?", Timestamp: now,
			},
			want: whatsapp.ActionProcess,
		},
		{
			name: "group without mention dropped",
			in: whatsapp.IncomingMessage{
				SenderJID: "5@s.whatsapp.net", ChatJID: "g@g.us",
				IsGroup: true, Type: whatsapp.MessageTypeText,
				Text: "great weather today", Timestamp: now,
			},
			want: whatsapp.ActionDrop,
		},
		{
			name: "group with formal mention via JID processes",
			in: whatsapp.IncomingMessage{
				SenderJID: "5@s.whatsapp.net", ChatJID: "g@g.us",
				IsGroup: true, Type: whatsapp.MessageTypeText,
				Text:      "@999 ping",
				Mentions:  []string{ownJID},
				Timestamp: now,
			},
			want: whatsapp.ActionProcess,
		},
		{
			name: "ambiguous substring NOT a mention",
			in: whatsapp.IncomingMessage{
				SenderJID: "5@s.whatsapp.net", ChatJID: "g@g.us",
				IsGroup: true, Type: whatsapp.MessageTypeText,
				Text: "have you seen @zenotech yet?", Timestamp: now,
			},
			want: whatsapp.ActionDrop,
		},
		{
			name: "hyphenated mention NOT a mention",
			in: whatsapp.IncomingMessage{
				SenderJID: "5@s.whatsapp.net", ChatJID: "g@g.us",
				IsGroup: true, Type: whatsapp.MessageTypeText,
				Text: "@zeno-test rolls it", Timestamp: now,
			},
			want: whatsapp.ActionDrop,
		},
		{
			name: "image from owner gets pre-canned reply",
			in: whatsapp.IncomingMessage{
				SenderJID: owner, ChatJID: owner,
				Type: whatsapp.MessageTypeImage, Timestamp: now,
			},
			want: whatsapp.ActionProcess,
		},
		{
			name: "image from stranger dropped (no apology)",
			in: whatsapp.IncomingMessage{
				SenderJID: "55@s.whatsapp.net", ChatJID: "55@s.whatsapp.net",
				Type: whatsapp.MessageTypeImage, Timestamp: now,
			},
			want: whatsapp.ActionDrop,
		},
		{
			name: "voice in mentioned group gets pre-canned reply",
			in: whatsapp.IncomingMessage{
				SenderJID: "5@s.whatsapp.net", ChatJID: "g@g.us",
				IsGroup: true, Type: whatsapp.MessageTypeVoice,
				Text: "@zeno", Timestamp: now,
			},
			want: whatsapp.ActionProcess,
		},
		{
			name: "case-insensitive mention",
			in: whatsapp.IncomingMessage{
				SenderJID: "5@s.whatsapp.net", ChatJID: "g@g.us",
				IsGroup: true, Type: whatsapp.MessageTypeText,
				Text: "@Zeno hello", Timestamp: now,
			},
			want: whatsapp.ActionProcess,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := whatsapp.Classify(tc.in, cfg, ownJID, pairTime, now)
			assert.Equal(t, tc.want, got.Action, "reason: %s", got.Reason)
			if tc.want == whatsapp.ActionProcess && tc.in.Type != whatsapp.MessageTypeText {
				assert.NotEmpty(t, got.PreCannedReply, "non-text Process must carry pre-canned reply")
			}
		})
	}
}

func TestClassify_NoPairTimeSkipsHistorySync(t *testing.T) {
	cfg := whatsapp.RuntimeConfig{AllowedDMs: []string{"1@s.whatsapp.net"}}
	in := whatsapp.IncomingMessage{
		SenderJID: "1@s.whatsapp.net", ChatJID: "1@s.whatsapp.net",
		Type: whatsapp.MessageTypeText, Text: "hi",
		Timestamp: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	got := whatsapp.Classify(in, cfg, "", time.Time{}, time.Now())
	assert.Equal(t, whatsapp.ActionProcess, got.Action,
		"with no pair-time the history-sync rule must skip — fixtures use ancient timestamps")
}
