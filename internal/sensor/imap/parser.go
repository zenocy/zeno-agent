package imap

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // register charset decoders for non-UTF-8 bodies

	gimap "github.com/emersion/go-imap/v2"
)

// ExtractPreview returns up to max bytes of plain text from the raw mail
// body. Multipart/alternative messages prefer the text/plain part; if only
// text/html is available, tags are stripped. The result is trimmed of
// surrounding whitespace.
func ExtractPreview(body []byte, max int) (string, error) {
	if len(body) == 0 {
		return "", nil
	}
	entity, err := message.Read(bytes.NewReader(body))
	if err != nil && !message.IsUnknownCharset(err) {
		return "", fmt.Errorf("read message: %w", err)
	}

	plain, html, err := walkBody(entity)
	if err != nil {
		return "", err
	}
	out := plain
	if out == "" {
		out = stripHTML(html)
	}
	out = strings.TrimSpace(out)
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// walkBody walks the message tree and returns the first text/plain and
// text/html parts encountered.
func walkBody(e *message.Entity) (plain, html string, err error) {
	if e == nil {
		return "", "", nil
	}
	mt, _, _ := e.Header.ContentType()
	mt = strings.ToLower(mt)

	if !strings.HasPrefix(mt, "multipart/") {
		text, rerr := readBody(e.Body)
		if rerr != nil {
			return "", "", rerr
		}
		switch mt {
		case "text/plain", "":
			return text, "", nil
		case "text/html":
			return "", text, nil
		default:
			// Non-text leaf (image, attachment): ignore.
			return "", "", nil
		}
	}

	mr := e.MultipartReader()
	if mr == nil {
		return "", "", nil
	}
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return plain, html, fmt.Errorf("next part: %w", perr)
		}
		p, h, werr := walkBody(part)
		if werr != nil {
			return plain, html, werr
		}
		if plain == "" && p != "" {
			plain = p
		}
		if html == "" && h != "" {
			html = h
		}
		if plain != "" && html != "" {
			break
		}
	}
	return plain, html, nil
}

func readBody(r io.Reader) (string, error) {
	const maxRead = 1 << 20 // 1 MB cap; preview will truncate further
	b, err := io.ReadAll(io.LimitReader(r, maxRead))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

var htmlTagRE = regexp.MustCompile(`(?s)<[^>]+>`)
var whitespaceRE = regexp.MustCompile(`\s+`)

// stripHTML removes tags and collapses whitespace. Crude on purpose — full
// HTML rendering is out of scope for Phase 1.
func stripHTML(s string) string {
	if s == "" {
		return ""
	}
	noTags := htmlTagRE.ReplaceAllString(s, " ")
	return whitespaceRE.ReplaceAllString(noTags, " ")
}

// NormalizeAddress renders an *imap.Address as either "Name <user@host>" or
// "user@host" depending on whether a display name is present.
func NormalizeAddress(a *gimap.Address) string {
	if a == nil {
		return ""
	}
	addr := a.Addr()
	if a.Name == "" {
		return addr
	}
	if addr == "" {
		return a.Name
	}
	return fmt.Sprintf("%s <%s>", a.Name, addr)
}

// NormalizeAddresses applies NormalizeAddress to a slice.
func NormalizeAddresses(in []gimap.Address) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for i := range in {
		s := NormalizeAddress(&in[i])
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
