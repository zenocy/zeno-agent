package imap

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/config"
)

// Dial routes on cfg.TLS. Unknown values must surface a clear error rather
// than silently picking a default — a typo in deploy/config.yaml otherwise
// connects with the wrong transport.
func TestRealDialer_TLSModeUnknownReturnsError(t *testing.T) {
	d := NewRealDialer(config.IMAPConfig{
		Host: "imap.example.invalid",
		Port: 993,
		TLS:  "totally-not-a-mode",
	})
	_, err := d.Dial(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported tls mode")
}

// The empty TLS mode and "implicit" share the same branch; a no-such-host
// dial fails at the network layer (not the TLS dispatch) so the error must
// mention the address, not "unsupported tls mode".
func TestRealDialer_EmptyTLSDefaultsToImplicit(t *testing.T) {
	d := NewRealDialer(config.IMAPConfig{
		Host: "imap.example.invalid",
		Port: 993,
		TLS:  "",
	})
	_, err := d.Dial(context.Background())
	require.Error(t, err, "dial against an invalid host must fail")
	require.NotContains(t, err.Error(), "unsupported tls mode",
		"empty TLS must dispatch to implicit, not error on the dispatch")
	require.True(t,
		strings.Contains(err.Error(), "dial ") || strings.Contains(err.Error(), "lookup "),
		"error must come from the network layer (got %q)", err.Error())
}
