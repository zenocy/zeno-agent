// Package mail builds RFC 5322 messages used by the V2.8.0 action
// surface (drafts saved via IMAP APPEND, replies/forwards sent via
// SMTP). The builder is deliberately small and dependency-free —
// stdlib net/mail + mime/quotedprintable cover everything we need.
package mail

import (
	"bytes"
	"errors"
	"fmt"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Message is the input to Build. Recipients and Body are required;
// everything else is optional and filled with sensible defaults
// (Date=now, Message-ID generated, MIME headers added).
type Message struct {
	From       string   // "Name <addr@host>" or "addr@host"
	To         []string // each recipient as RFC 5322 mailbox
	Cc         []string
	Bcc        []string
	Subject    string
	Body       string // plain text; encoded as quoted-printable
	Date       time.Time
	MessageID  string // optional override; generated when empty
	InReplyTo  string // RFC 5322 Message-ID of the parent thread
	References []string
	// Headers carries additional headers ('X-Zeno-...', 'Reply-To', ...).
	// Keys are written verbatim (capitalize correctly).
	Headers map[string]string
}

// Build serializes m to RFC 5322 bytes with CRLF line endings.
// The body is always quoted-printable text/plain; charset utf-8.
func Build(m Message) ([]byte, error) {
	if m.From == "" {
		return nil, errors.New("mail.Build: From is required")
	}
	if len(m.To) == 0 && len(m.Cc) == 0 && len(m.Bcc) == 0 {
		return nil, errors.New("mail.Build: at least one recipient (To/Cc/Bcc) is required")
	}

	if _, err := mail.ParseAddress(m.From); err != nil {
		return nil, fmt.Errorf("mail.Build: parse From: %w", err)
	}
	for _, addr := range m.To {
		if _, err := mail.ParseAddress(addr); err != nil {
			return nil, fmt.Errorf("mail.Build: parse To %q: %w", addr, err)
		}
	}

	if m.Date.IsZero() {
		m.Date = time.Now()
	}
	if m.MessageID == "" {
		m.MessageID = NewMessageID(domainOf(m.From))
	}

	var buf bytes.Buffer

	writeHeader(&buf, "From", m.From)
	if len(m.To) > 0 {
		writeHeader(&buf, "To", strings.Join(m.To, ", "))
	}
	if len(m.Cc) > 0 {
		writeHeader(&buf, "Cc", strings.Join(m.Cc, ", "))
	}
	writeHeader(&buf, "Subject", encodeHeaderValue(m.Subject))
	writeHeader(&buf, "Date", m.Date.Format(time.RFC1123Z))
	writeHeader(&buf, "Message-ID", angled(m.MessageID))
	if m.InReplyTo != "" {
		writeHeader(&buf, "In-Reply-To", angled(m.InReplyTo))
	}
	if len(m.References) > 0 {
		ref := make([]string, 0, len(m.References))
		for _, r := range m.References {
			ref = append(ref, angled(r))
		}
		writeHeader(&buf, "References", strings.Join(ref, " "))
	}
	for k, v := range m.Headers {
		writeHeader(&buf, k, v)
	}
	writeHeader(&buf, "MIME-Version", "1.0")
	writeHeader(&buf, "Content-Type", "text/plain; charset=utf-8")
	writeHeader(&buf, "Content-Transfer-Encoding", "quoted-printable")

	buf.WriteString("\r\n") // header/body separator

	qp := quotedprintable.NewWriter(&buf)
	body := strings.ReplaceAll(m.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	if _, err := qp.Write([]byte(body)); err != nil {
		return nil, fmt.Errorf("mail.Build: qp write: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("mail.Build: qp close: %w", err)
	}
	return buf.Bytes(), nil
}

// NewMessageID generates a fresh RFC 5322 Message-ID without the
// surrounding angle brackets. Pass the local part of the From address's
// domain (or any stable string) for the right-hand side; the function
// adds a uuid for global uniqueness.
func NewMessageID(domain string) string {
	if domain == "" {
		domain = "zeno.local"
	}
	return fmt.Sprintf("%s@%s", uuid.NewString(), domain)
}

// ReplySubject prefixes "Re: " when not already present (case-insensitive,
// also matching RE: / re: / Re :). If the subject is empty, returns "Re:".
func ReplySubject(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "Re:"
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "re:") || strings.HasPrefix(lower, "re :") {
		return trimmed
	}
	return "Re: " + trimmed
}

// ForwardSubject prefixes "Fwd: " similarly.
func ForwardSubject(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "Fwd:"
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "fwd:") || strings.HasPrefix(lower, "fw:") {
		return trimmed
	}
	return "Fwd: " + trimmed
}

// QuoteBody renders the original message body as a quoted block per
// RFC 3676 / common mail-client convention: each line prefixed with
// "> " and an "On <date>, <sender> wrote:" attribution line.
func QuoteBody(originalSender, original string, sentAt time.Time) string {
	var b strings.Builder
	if originalSender != "" {
		b.WriteString("On ")
		if !sentAt.IsZero() {
			b.WriteString(sentAt.Format("Mon, 2 Jan 2006 15:04:05 -0700"))
			b.WriteString(", ")
		}
		b.WriteString(originalSender)
		b.WriteString(" wrote:\n")
	}
	for _, line := range strings.Split(original, "\n") {
		line = strings.TrimRight(line, "\r")
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// ----------------------------------------------------------------------
// Internal helpers
// ----------------------------------------------------------------------

func angled(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "<") {
		return s
	}
	return "<" + s + ">"
}

// encodeHeaderValue applies RFC 2047 encoded-word encoding to non-ASCII
// header values (Subject in particular). For ASCII-only strings it
// returns the input unchanged.
func encodeHeaderValue(s string) string {
	if isASCII(s) {
		return s
	}
	// Use a simple base64 encoded-word; word-folding is not
	// implemented (subjects rarely exceed line length after b64).
	return "=?utf-8?B?" + base64Encode(s) + "?="
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

func base64Encode(s string) string {
	const tab = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := []byte(s)
	var out bytes.Buffer
	for i := 0; i < len(b); i += 3 {
		chunk := b[i:min3(i+3, len(b))]
		switch len(chunk) {
		case 3:
			out.WriteByte(tab[chunk[0]>>2])
			out.WriteByte(tab[((chunk[0]&0x03)<<4)|(chunk[1]>>4)])
			out.WriteByte(tab[((chunk[1]&0x0f)<<2)|(chunk[2]>>6)])
			out.WriteByte(tab[chunk[2]&0x3f])
		case 2:
			out.WriteByte(tab[chunk[0]>>2])
			out.WriteByte(tab[((chunk[0]&0x03)<<4)|(chunk[1]>>4)])
			out.WriteByte(tab[(chunk[1]&0x0f)<<2])
			out.WriteByte('=')
		case 1:
			out.WriteByte(tab[chunk[0]>>2])
			out.WriteByte(tab[(chunk[0]&0x03)<<4])
			out.WriteByte('=')
			out.WriteByte('=')
		}
	}
	return out.String()
}

func min3(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func writeHeader(buf *bytes.Buffer, key, val string) {
	buf.WriteString(key)
	buf.WriteString(": ")
	buf.WriteString(val)
	buf.WriteString("\r\n")
}

func domainOf(addr string) string {
	if a, err := mail.ParseAddress(addr); err == nil {
		if i := strings.LastIndex(a.Address, "@"); i >= 0 {
			return a.Address[i+1:]
		}
	}
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		end := addr[i+1:]
		end = strings.TrimSpace(end)
		end = strings.TrimSuffix(end, ">")
		return end
	}
	return ""
}
