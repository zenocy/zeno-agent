package whatsapp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.mau.fi/whatsmeow/types"
)

// TestResolveSenderPNFromAlt_PassthroughForPN — non-LID primary
// returns its own non-AD form unchanged.
func TestResolveSenderPNFromAlt_PassthroughForPN(t *testing.T) {
	primary := types.JID{User: "447700900111", Server: types.DefaultUserServer}
	got := resolveSenderPNFromAlt(primary, types.EmptyJID)
	assert.Equal(t, "447700900111@s.whatsapp.net", got)
}

// TestResolveSenderPNFromAlt_LIDWithPhoneAltUsesAlt — the V2.13.3
// case: LID primary + alt is the phone form → returns the alt.
func TestResolveSenderPNFromAlt_LIDWithPhoneAltUsesAlt(t *testing.T) {
	primary := types.JID{User: "148747753365740", Server: types.HiddenUserServer}
	alt := types.JID{User: "447700900333", Server: types.DefaultUserServer}
	got := resolveSenderPNFromAlt(primary, alt)
	assert.Equal(t, "447700900333@s.whatsapp.net", got, "phone-based alt must win over LID primary")
}

// TestResolveSenderPNFromAlt_LIDNoUsableAltReturnsEmpty — pure
// function returns "" when the Store fallback is needed (the caller's
// resolveSenderPN then queries the persistent LID map).
func TestResolveSenderPNFromAlt_LIDNoUsableAltReturnsEmpty(t *testing.T) {
	primary := types.JID{User: "148747753365740", Server: types.HiddenUserServer}
	// Empty alt → caller must fall back.
	got := resolveSenderPNFromAlt(primary, types.EmptyJID)
	assert.Equal(t, "", got)

	// LID alt → also signals fallback; we don't accept LID for either side.
	altLID := types.JID{User: "999", Server: types.HiddenUserServer}
	got = resolveSenderPNFromAlt(primary, altLID)
	assert.Equal(t, "", got)
}

// TestResolveSenderPNFromAlt_EmptyPrimary — empty input yields empty;
// classifier will drop the message anyway, but the resolver shouldn't
// panic.
func TestResolveSenderPNFromAlt_EmptyPrimary(t *testing.T) {
	got := resolveSenderPNFromAlt(types.EmptyJID, types.EmptyJID)
	assert.Equal(t, "", got)
}

// TestResolveSenderPNFromAlt_StripsAD — both primary and alt branches
// strip the device-ID suffix so the result is comparable to allowlist
// entries (which never include device suffixes).
func TestResolveSenderPNFromAlt_StripsAD(t *testing.T) {
	primary := types.JID{User: "148747753365740", Server: types.HiddenUserServer, Device: 19}
	alt := types.JID{User: "447700900333", Server: types.DefaultUserServer, Device: 4}
	got := resolveSenderPNFromAlt(primary, alt)
	assert.Equal(t, "447700900333@s.whatsapp.net", got, "device suffix must be dropped")
}
