package whatsapp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWithAdditionalAllowedDM_Adds(t *testing.T) {
	cfg := RuntimeConfig{AllowedDMs: []string{"447700900111@s.whatsapp.net"}}
	out := cfg.WithAdditionalAllowedDM("447700900333@s.whatsapp.net")
	assert.Equal(t, []string{"447700900111@s.whatsapp.net", "447700900333@s.whatsapp.net"}, out.AllowedDMs)
	// Original untouched.
	assert.Equal(t, []string{"447700900111@s.whatsapp.net"}, cfg.AllowedDMs)
}

func TestWithAdditionalAllowedDM_Idempotent(t *testing.T) {
	cfg := RuntimeConfig{AllowedDMs: []string{"447700900111@s.whatsapp.net"}}
	out := cfg.WithAdditionalAllowedDM("447700900111@s.whatsapp.net")
	assert.Equal(t, []string{"447700900111@s.whatsapp.net"}, out.AllowedDMs)
}

func TestWithAdditionalAllowedDM_EmptyJIDNoOp(t *testing.T) {
	cfg := RuntimeConfig{AllowedDMs: []string{"447700900111@s.whatsapp.net"}}
	out := cfg.WithAdditionalAllowedDM("")
	assert.Equal(t, []string{"447700900111@s.whatsapp.net"}, out.AllowedDMs)
}

func TestWithAdditionalAllowedDM_NoBackingArrayAlias(t *testing.T) {
	// Independent backing arrays — mutating the returned slice must
	// not affect the original.
	cfg := RuntimeConfig{AllowedDMs: []string{"447700900111@s.whatsapp.net"}}
	out := cfg.WithAdditionalAllowedDM("447700900333@s.whatsapp.net")
	out.AllowedDMs[0] = "MUTATED"
	assert.Equal(t, []string{"447700900111@s.whatsapp.net"}, cfg.AllowedDMs, "original must not be aliased")
}

func TestWithAdditionalAllowedDM_NormalizesPlusPrefix(t *testing.T) {
	// V2.13.3c: a `+`-prefixed JID is canonicalized before insertion
	// so it matches inbound JIDs (digit-only) on the wire.
	cfg := RuntimeConfig{AllowedDMs: nil}
	out := cfg.WithAdditionalAllowedDM("+447700900333@s.whatsapp.net")
	assert.Equal(t, []string{"447700900333@s.whatsapp.net"}, out.AllowedDMs)
}

func TestNormalizeJID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"+447700900333@s.whatsapp.net", "447700900333@s.whatsapp.net"},
		{"447700900333@s.whatsapp.net", "447700900333@s.whatsapp.net"},
		{"+447700900333", "447700900333@s.whatsapp.net"},
		{"00447700900333", "447700900333@s.whatsapp.net"},
		{"447700900333@S.WHATSAPP.NET", "447700900333@s.whatsapp.net"},
		{"1203@g.us", "1203@g.us"},
		{"1203@G.US", "1203@g.us"},
		{"148747753365740@lid", "148747753365740@lid"},
		{"  +44 7700 900 333 @ s.whatsapp.net  ", "447700900333@s.whatsapp.net"}, // inner spaces tolerated
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		got := NormalizeJID(tc.in)
		assert.Equal(t, tc.want, got, "input=%q", tc.in)
	}
}

func TestRuntimeConfigNormalize_NormalizesAllowedDMs(t *testing.T) {
	cfg := RuntimeConfig{
		AllowedDMs: []string{
			"+447700900333@s.whatsapp.net",
			"447700900333@s.whatsapp.net", // dup after normalize
			"447700900111",                // bare phone, no @
			"",                            // skipped
		},
	}
	out := cfg.Normalize()
	assert.Equal(t, []string{
		"447700900333@s.whatsapp.net",
		"447700900111@s.whatsapp.net",
	}, out.AllowedDMs)
}
