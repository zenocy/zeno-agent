package synth

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestBuildWhatsAppSystemPrompt_LegacyFirstPerson asserts that empty
// AssistantName preserves the V2.12 first-person register.
func TestBuildWhatsAppSystemPrompt_LegacyFirstPerson(t *testing.T) {
	prompt := buildWhatsAppSystemPrompt("VOICE", DraftWhatsAppContext{})
	assert.Contains(t, prompt, "drafting a WhatsApp message on behalf of the user")
	assert.Contains(t, prompt, "no signature")
	assert.NotContains(t, prompt, "executive assistant")
}

// TestBuildWhatsAppSystemPrompt_AssistantPersona asserts the third-person
// EA register replaces the first-person preamble when persona is set.
func TestBuildWhatsAppSystemPrompt_AssistantPersona(t *testing.T) {
	c := DraftWhatsAppContext{
		AssistantName:     "Aria",
		UserName:          "Jamie",
		AssistantRegister: "(register block goes here)",
	}
	prompt := buildWhatsAppSystemPrompt("VOICE", c)
	assert.Contains(t, prompt, "Aria, Jamie's executive assistant")
	assert.Contains(t, prompt, "Third person about the principal")
	assert.Contains(t, prompt, "— Aria")
	assert.Contains(t, prompt, "(register block goes here)")
	assert.NotContains(t, prompt, "drafting a WhatsApp message on behalf of the user")
}

// TestBuildWhatsAppSystemPrompt_ToneSteerOrdering asserts tone is
// appended AFTER the canon block so the canon wins on conflict.
func TestBuildWhatsAppSystemPrompt_ToneSteerOrdering(t *testing.T) {
	c := DraftWhatsAppContext{
		AssistantName: "Aria",
		UserName:      "Jamie",
		AssistantTone: "warm but brisk",
	}
	prompt := buildWhatsAppSystemPrompt("VOICE", c)
	canonIdx := strings.Index(prompt, "Aria, Jamie's executive assistant")
	toneIdx := strings.Index(prompt, "warm but brisk")
	assert.Greater(t, canonIdx, -1)
	assert.Greater(t, toneIdx, canonIdx, "tone steer should appear AFTER canon so canon wins")
}

// TestFallbackWhatsAppBody_AssistantSignature asserts the deterministic
// fallback (used when LLM is unavailable) produces a third-person body
// with the `— Aria` sign-off in assistant mode.
func TestFallbackWhatsAppBody_AssistantSignature(t *testing.T) {
	body := FallbackWhatsAppBody(DraftWhatsAppContext{
		AssistantName: "Aria",
		UserName:      "Jamie",
		EventTitle:    "Dinner with Dana & Alex",
		EventStart:    time.Date(2026, 5, 10, 20, 0, 0, 0, time.UTC),
		EventLocation: "Avli",
	})
	assert.Contains(t, body, "Jamie asked me to confirm")
	assert.True(t, strings.HasSuffix(body, "— Aria"), "fallback should end with — Aria; got %q", body)
	assert.NotContains(t, body, "I'll fill you in shortly.", "first-person phrasing must not leak through")
}

// TestFallbackWhatsAppBody_LegacyFirstPerson preserves V2.12 behavior
// when persona is unset.
func TestFallbackWhatsAppBody_LegacyFirstPerson(t *testing.T) {
	body := FallbackWhatsAppBody(DraftWhatsAppContext{
		EventTitle: "Dinner",
		EventStart: time.Date(2026, 5, 10, 20, 0, 0, 0, time.UTC),
	})
	assert.Contains(t, body, "Heads-up: Dinner on")
	assert.NotContains(t, body, "— ")
}
