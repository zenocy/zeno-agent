package gemini

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// TestResolveServiceTier pins the precedence chain: per-call > ctx
// CallProfile + config map > "". Mirrors the equivalent test for the
// OpenAI client so both providers share an observable resolution
// signature.
func TestResolveServiceTier(t *testing.T) {
	cases := []struct {
		name    string
		profile llm.CallProfile
		bgTier  string
		intTier string
		perCall string
		want    string
	}{
		{name: "all empty", want: ""},
		{name: "per-call only", perCall: "priority", want: "priority"},
		{name: "background profile maps to config", profile: llm.CallProfileBackground, bgTier: "flex", want: "flex"},
		{name: "interactive profile maps to config", profile: llm.CallProfileInteractive, intTier: "priority", want: "priority"},
		{name: "per-call beats profile", profile: llm.CallProfileBackground, bgTier: "flex", perCall: "standard", want: "standard"},
		{name: "profile with empty config falls through", profile: llm.CallProfileBackground, want: ""},
		{name: "wrong profile direction does not bleed", profile: llm.CallProfileInteractive, bgTier: "flex", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.profile != "" {
				ctx = llm.ContextWithCallProfile(ctx, tc.profile)
			}
			c := &Client{cfg: llm.GeminiClientConfig{
				ServiceTierBackground:  tc.bgTier,
				ServiceTierInteractive: tc.intTier,
			}}
			require.Equal(t, tc.want, c.resolveServiceTier(ctx, tc.perCall))
		})
	}
}

// TestResolveThinkingLevel mirrors the service-tier resolver test
// against the thinking-level mapping the Gemini client uses for the
// thinkingConfig knob.
func TestResolveThinkingLevel(t *testing.T) {
	cases := []struct {
		name    string
		profile llm.CallProfile
		bgLvl   string
		intLvl  string
		perCall string
		want    string
	}{
		{name: "all empty", want: ""},
		{name: "per-call only", perCall: "high", want: "high"},
		{name: "background profile maps to config", profile: llm.CallProfileBackground, bgLvl: "high", want: "high"},
		{name: "interactive profile maps to config", profile: llm.CallProfileInteractive, intLvl: "low", want: "low"},
		{name: "per-call beats profile", profile: llm.CallProfileBackground, bgLvl: "high", perCall: "minimal", want: "minimal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.profile != "" {
				ctx = llm.ContextWithCallProfile(ctx, tc.profile)
			}
			c := &Client{cfg: llm.GeminiClientConfig{
				ThinkingLevelBackground:  tc.bgLvl,
				ThinkingLevelInteractive: tc.intLvl,
			}}
			require.Equal(t, tc.want, c.resolveThinkingLevel(ctx, tc.perCall))
		})
	}
}
