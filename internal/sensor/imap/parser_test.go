package imap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gimap "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/require"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

func TestExtractPreview_PlainText(t *testing.T) {
	body := loadFixture(t, "plain.eml")
	p, err := ExtractPreview(body, 4096)
	require.NoError(t, err)
	require.Contains(t, p, "got the patch")
	require.Contains(t, p, "Alice")
	require.Equal(t, strings.TrimSpace(p), p, "no surrounding whitespace")
}

func TestExtractPreview_MultipartAlternative(t *testing.T) {
	body := loadFixture(t, "multipart.eml")
	p, err := ExtractPreview(body, 4096)
	require.NoError(t, err)
	require.Contains(t, p, "Numbers are in")
	require.NotContains(t, p, "<b>", "plain text wins over html")
	require.NotContains(t, p, "<html>")
}

func TestExtractPreview_HTMLOnly(t *testing.T) {
	body := loadFixture(t, "htmlonly.eml")
	p, err := ExtractPreview(body, 4096)
	require.NoError(t, err)
	require.Contains(t, p, "standup at 09:30")
	require.NotContains(t, p, "<b>")
	require.NotContains(t, p, "<html>")
}

func TestExtractPreview_Truncation(t *testing.T) {
	body := []byte("From: a@b\r\nTo: b@c\r\nSubject: long\r\nContent-Type: text/plain\r\n\r\n" +
		strings.Repeat("x", 9000))
	p, err := ExtractPreview(body, 4096)
	require.NoError(t, err)
	require.Equal(t, 4096, len(p))
}

func TestExtractPreview_Empty(t *testing.T) {
	p, err := ExtractPreview(nil, 4096)
	require.NoError(t, err)
	require.Equal(t, "", p)
}

func TestNormalizeAddress_NameAndAddr(t *testing.T) {
	a := &gimap.Address{Name: "Alice Liddell", Mailbox: "alice", Host: "example.test"}
	require.Equal(t, "Alice Liddell <alice@example.test>", NormalizeAddress(a))
}

func TestNormalizeAddress_AddrOnly(t *testing.T) {
	a := &gimap.Address{Mailbox: "alice", Host: "example.test"}
	require.Equal(t, "alice@example.test", NormalizeAddress(a))
}

func TestNormalizeAddress_Nil(t *testing.T) {
	require.Equal(t, "", NormalizeAddress(nil))
}

func TestNormalizeAddresses(t *testing.T) {
	in := []gimap.Address{
		{Mailbox: "a", Host: "x.test"},
		{Name: "Bee", Mailbox: "b", Host: "x.test"},
	}
	got := NormalizeAddresses(in)
	require.Equal(t, []string{"a@x.test", "Bee <b@x.test>"}, got)
}
