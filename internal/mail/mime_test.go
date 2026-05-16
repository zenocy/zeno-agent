package mail

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuild_BasicReply(t *testing.T) {
	raw, err := Build(Message{
		From:       "user@example.com",
		To:         []string{"saru@example.com"},
		Subject:    "Re: redline",
		Body:       "Hi Saru,\n\nLet me come back to this tomorrow.\n\nBest,\nJamie",
		Date:       time.Date(2026, 5, 7, 14, 30, 0, 0, time.UTC),
		MessageID:  "abc-123@zeno.local",
		InReplyTo:  "parent-456@example.com",
		References: []string{"parent-456@example.com"},
	})
	require.NoError(t, err)

	s := string(raw)
	require.Contains(t, s, "From: user@example.com\r\n")
	require.Contains(t, s, "To: saru@example.com\r\n")
	require.Contains(t, s, "Subject: Re: redline\r\n")
	require.Contains(t, s, "Message-ID: <abc-123@zeno.local>\r\n")
	require.Contains(t, s, "In-Reply-To: <parent-456@example.com>\r\n")
	require.Contains(t, s, "References: <parent-456@example.com>\r\n")
	require.Contains(t, s, "MIME-Version: 1.0\r\n")
	require.Contains(t, s, "Content-Type: text/plain; charset=utf-8\r\n")
	require.Contains(t, s, "Content-Transfer-Encoding: quoted-printable\r\n")

	// Header/body separator must be present.
	require.Contains(t, s, "\r\n\r\n")

	// Body line endings normalized to CRLF.
	require.True(t, strings.HasSuffix(s, "Jamie"), "body must end with the closing line; got tail %q", s[max(0, len(s)-40):])
}

func TestBuild_MultipleReferences(t *testing.T) {
	raw, err := Build(Message{
		From:       "a@example.com",
		To:         []string{"b@example.com"},
		Subject:    "Re: chain",
		Body:       "ack",
		References: []string{"id1@x", "id2@x", "id3@x"},
	})
	require.NoError(t, err)
	require.Contains(t, string(raw), "References: <id1@x> <id2@x> <id3@x>\r\n")
}

func TestBuild_NoRecipients(t *testing.T) {
	_, err := Build(Message{From: "a@example.com", Subject: "x", Body: "y"})
	require.Error(t, err)
}

func TestBuild_NoFrom(t *testing.T) {
	_, err := Build(Message{To: []string{"a@example.com"}, Subject: "x", Body: "y"})
	require.Error(t, err)
}

func TestReplySubject(t *testing.T) {
	require.Equal(t, "Re: redline", ReplySubject("redline"))
	require.Equal(t, "Re: redline", ReplySubject("Re: redline"))
	require.Equal(t, "RE: redline", ReplySubject("RE: redline"))
	require.Equal(t, "Re:", ReplySubject(""))
	require.Equal(t, "Re:", ReplySubject("   "))
}

func TestForwardSubject(t *testing.T) {
	require.Equal(t, "Fwd: redline", ForwardSubject("redline"))
	require.Equal(t, "Fwd: redline", ForwardSubject("Fwd: redline"))
	require.Equal(t, "Fw: redline", ForwardSubject("Fw: redline"))
}

func TestQuoteBody(t *testing.T) {
	q := QuoteBody("Saru <saru@example.com>",
		"Hi,\nTwo questions remain.\nOption pool, and 1× preferred.",
		time.Date(2026, 5, 7, 6, 14, 0, 0, time.UTC))
	require.Contains(t, q, "Saru <saru@example.com> wrote:")
	require.Contains(t, q, "> Hi,")
	require.Contains(t, q, "> Two questions remain.")
	require.Contains(t, q, "> Option pool, and 1× preferred.")
}

func TestNewMessageID(t *testing.T) {
	a := NewMessageID("zeno.local")
	b := NewMessageID("zeno.local")
	require.NotEqual(t, a, b, "IDs must be unique")
	require.Contains(t, a, "@zeno.local")
}

// max returns the maximum of two integers — placed here to avoid pulling
// in a builtin polyfill helper just for one test assertion.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
