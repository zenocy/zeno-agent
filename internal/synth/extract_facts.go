package synth

import (
	"context"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/internal/llm"
)

// ExtractFactsTimeout is the fallback cap when the caller passes 0. Matches
// the reactive Ask deadline default so the extractor shares the same budget
// as the main answer loop. The caller is expected to pass its actual loop
// deadline so the two stay aligned across config changes.
const ExtractFactsTimeout = 45 * time.Second

// extractFactsSystem is the focused prompt for the dedicated extraction call.
// Single, narrow task: emit `remember:` lines or `none`. No card schema, no
// voice rules, no tools — local 35B models follow narrow instructions much
// more reliably than the multiplexed answer-+-memory contract that lives in
// the main reactive prompt.
const extractFactsSystem = `You are a memory extractor. Look at the user's message and decide whether they STATED a durable personal fact about themselves or someone in their life.

A durable fact is something that stays true over weeks or months: relationships (wife, partner, manager, dentist), contact details (phone, email, address), recurring routines (runs Tue/Thu mornings), stable preferences (vegetarian, prefers tea, hates Mondays).

NOT durable: today's plans, one-off events, questions, requests for information, ephemeral mood.

If the user stated one or more durable facts, output one line per fact:

remember: <subject>: <predicate>

- subject is a single lowercase word (wife, partner, manager, dentist, runs, commute, breakfast, etc.)
- predicate is the fact, ≤280 characters. Pack related details into ONE line (name + phone + email together).

If no durable fact was stated, output the single word: none

Output nothing else. No prose, no JSON, no explanation.

Examples:

User: What's the weather today?
Output: none

User: My wife is Pat Morgan, +447700900222, pat.morgan@example.com
Output: remember: wife: Pat Morgan, +447700900222, pat.morgan@example.com

User: I run Tuesdays and Thursdays at 7am
Output: remember: runs: Tuesdays and Thursdays at 7am

User: Schedule lunch with Saru tomorrow
Output: none

User: My manager's name is Lin Park, she prefers Slack over email
Output: remember: manager: Lin Park, prefers Slack over email

User: Remind me to call mom
Output: none

User: I'm vegetarian and my partner Sam is too
Output: remember: diet: vegetarian
remember: partner: Sam, vegetarian`

// ExtractFacts runs a focused single-call LLM extraction over the user's
// query, returning any durable facts shaped as MemoryCandidates. Designed to
// be called in parallel with the main reactive Ask loop so total latency is
// max(answer, extract) rather than sum.
//
// `timeout` caps the call. Pass the same deadline as the main loop so the
// two share a single budget (configured via llm.reactive_deadline_sec); 0
// falls back to ExtractFactsTimeout. Caller-side cancellation (e.g. when the
// main loop returns) still bounds the call below the timeout.
//
// On any failure (timeout, network, validation) returns nil — extraction is
// best-effort decoration on top of the answer card. The caller never fails a
// response because extraction failed.
func ExtractFacts(ctx context.Context, client *llm.Client, query string, timeout time.Duration, logger *logrus.Entry) []llm.MemoryCandidate {
	if client == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	if timeout <= 0 {
		timeout = ExtractFactsTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	msgs := []llm.Message{
		{Role: "system", Content: extractFactsSystem},
		{Role: "user", Content: query},
	}
	cr, err := client.ChatCompletion(ctx, msgs, nil)
	if err != nil {
		logger.WithError(err).Warn("extract: chat completion failed (best-effort)")
		return nil
	}
	mems := parseExtractionOutput(cr.Content)
	if len(mems) > 0 {
		logger.WithField("count", len(mems)).Info("extract: facts captured")
	}
	return mems
}

// parseExtractionOutput parses the extractor's response. Accepts:
//   - "none" (case-insensitive, with optional surrounding whitespace) → no
//     candidates
//   - One or more lines starting with "remember:" → parsed via the existing
//     remember-line parser (subject:predicate)
//   - Anything else → no candidates (treated as model misbehavior)
//
// Caps at MaxMemoryCandidatesPerResponse to match the cards path's invariant.
func parseExtractionOutput(content string) []llm.MemoryCandidate {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil
	}
	// Some local models wrap the answer in ```; strip a leading/trailing fence.
	trimmed = stripCodeFences(trimmed)
	if trimmed == "" {
		return nil
	}
	if strings.EqualFold(trimmed, "none") {
		return nil
	}

	var out []llm.MemoryCandidate
	for _, line := range strings.Split(trimmed, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.EqualFold(t, "none") {
			continue
		}
		lower := strings.ToLower(t)
		if !strings.HasPrefix(lower, "remember:") {
			continue
		}
		rest := strings.TrimSpace(t[len("remember:"):])
		idx := strings.IndexByte(rest, ':')
		if idx < 0 {
			continue
		}
		subject := strings.ToLower(strings.TrimSpace(rest[:idx]))
		predicate := strings.TrimSpace(rest[idx+1:])
		if subject == "" || predicate == "" {
			continue
		}
		out = append(out, llm.MemoryCandidate{
			Subject:   subject,
			Predicate: predicate,
			Raw:       t,
		})
		if len(out) >= llm.MaxMemoryCandidatesPerResponse {
			break
		}
	}
	return out
}
