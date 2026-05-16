package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/zenocy/zeno-v2/internal/synth"
)

// GoldenMeta records the model + endpoint + freeze timestamp for one
// frozen fixture. Stored alongside input/output/scores so a future
// `make eval-compare` knows what model produced the golden.
type GoldenMeta struct {
	Model        string    `json:"model"`
	Endpoint     string    `json:"endpoint"`
	FrozenAt     time.Time `json:"frozen_at"`
	JSONSchemaOn bool      `json:"json_schema_on"`
	FixtureName  string    `json:"fixture_name"`
	FixtureKind  string    `json:"fixture_kind"` // "morning" | "reactive"
}

// FreezeMorning writes one morning fixture's input + output + scores + meta
// into goldenDir/<fixture_name>/. When the fixture seeded any memory facts,
// a sibling memory.json is also written so the golden directory is self-
// describing without forcing future readers to scan the full input.json.
// Returns the directory written.
func FreezeMorning(goldenDir, fixturePath, model, endpoint string, jsonSchemaOn bool, res *RunResult) (string, error) {
	dir := filepath.Join(goldenDir, res.FixtureName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}

	if err := copyFile(fixturePath, filepath.Join(dir, "input.json")); err != nil {
		return "", fmt.Errorf("copy input: %w", err)
	}

	if err := writeMemorySidecar(fixturePath, dir); err != nil {
		return "", fmt.Errorf("write memory sidecar: %w", err)
	}

	if err := writeMustCardsStateSidecar(fixturePath, dir); err != nil {
		return "", fmt.Errorf("write must_cards_state sidecar: %w", err)
	}

	if err := writeConcernsSidecar(fixturePath, dir); err != nil {
		return "", fmt.Errorf("write concerns sidecar: %w", err)
	}

	output := map[string]any{
		"briefing": res.Briefing,
		"cards":    res.Cards,
	}
	if err := writeJSON(filepath.Join(dir, "output.json"), output); err != nil {
		return "", fmt.Errorf("write output: %w", err)
	}
	if err := writeJSON(filepath.Join(dir, "scores.json"), res.Scoreboard); err != nil {
		return "", fmt.Errorf("write scores: %w", err)
	}
	meta := GoldenMeta{
		Model:        model,
		Endpoint:     endpoint,
		FrozenAt:     time.Now().UTC(),
		JSONSchemaOn: jsonSchemaOn,
		FixtureName:  res.FixtureName,
		FixtureKind:  "morning",
	}
	if err := writeJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
		return "", fmt.Errorf("write meta: %w", err)
	}
	return dir, nil
}

// FreezeReactive writes one reactive fixture's frozen artifacts. Memory
// sidecar is written when the fixture seeded any facts (V2.2.0).
func FreezeReactive(goldenDir, fixturePath, model, endpoint string, jsonSchemaOn bool, res *ReactiveResult) (string, error) {
	dir := filepath.Join(goldenDir, res.FixtureName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}

	if err := copyFile(fixturePath, filepath.Join(dir, "input.json")); err != nil {
		return "", fmt.Errorf("copy input: %w", err)
	}

	if err := writeMemorySidecar(fixturePath, dir); err != nil {
		return "", fmt.Errorf("write memory sidecar: %w", err)
	}

	if err := writeConcernsSidecar(fixturePath, dir); err != nil {
		return "", fmt.Errorf("write concerns sidecar: %w", err)
	}

	output := map[string]any{
		"query":       res.Query,
		"card":        res.Card,
		"degraded":    res.Degraded,
		"expect_hits": res.ExpectHits,
	}
	if err := writeJSON(filepath.Join(dir, "output.json"), output); err != nil {
		return "", fmt.Errorf("write output: %w", err)
	}
	if err := writeJSON(filepath.Join(dir, "scores.json"), res.Scoreboard); err != nil {
		return "", fmt.Errorf("write scores: %w", err)
	}
	meta := GoldenMeta{
		Model:        model,
		Endpoint:     endpoint,
		FrozenAt:     time.Now().UTC(),
		JSONSchemaOn: jsonSchemaOn,
		FixtureName:  res.FixtureName,
		FixtureKind:  "reactive",
	}
	if err := writeJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
		return "", fmt.Errorf("write meta: %w", err)
	}
	return dir, nil
}

// CompareReport summarizes a single fixture's diff against its golden.
type CompareReport struct {
	FixtureName     string   `json:"fixture_name"`
	Regressions     []string `json:"regressions,omitempty"`
	LatencyDeltaPct float64  `json:"latency_delta_pct"`
}

// CompareMorning loads golden scores for the named fixture and reports any
// regressions vs the live result. Regression rules: any deterministic
// dimension drops by ≥1 point; AllMustCardsPresent flips false→true was
// false (i.e. any must-card pass count drops); latency p50 grows ≥30%.
func CompareMorning(goldenDir string, res *RunResult) (CompareReport, error) {
	dir := filepath.Join(goldenDir, res.FixtureName)
	out := CompareReport{FixtureName: res.FixtureName}

	var goldenSb Scoreboard
	if err := readJSON(filepath.Join(dir, "scores.json"), &goldenSb); err != nil {
		return out, fmt.Errorf("read golden scores: %w", err)
	}

	if res.Scoreboard.Calmness.Value < goldenSb.Calmness.Value {
		out.Regressions = append(out.Regressions, fmt.Sprintf("calmness %d→%d", goldenSb.Calmness.Value, res.Scoreboard.Calmness.Value))
	}
	if res.Scoreboard.Decisiveness.Value < goldenSb.Decisiveness.Value {
		out.Regressions = append(out.Regressions, fmt.Sprintf("decisiveness %d→%d", goldenSb.Decisiveness.Value, res.Scoreboard.Decisiveness.Value))
	}
	if res.Scoreboard.Concreteness.Value < goldenSb.Concreteness.Value {
		out.Regressions = append(out.Regressions, fmt.Sprintf("concreteness %d→%d", goldenSb.Concreteness.Value, res.Scoreboard.Concreteness.Value))
	}
	if goldenSb.BriefingSchema && !res.Scoreboard.BriefingSchema {
		out.Regressions = append(out.Regressions, "briefing schema regressed (was OK, now failing)")
	}
	if goldenSb.CardsSchema && !res.Scoreboard.CardsSchema {
		out.Regressions = append(out.Regressions, "cards schema regressed")
	}
	if goldenSb.ItalicBalanced && !res.Scoreboard.ItalicBalanced {
		out.Regressions = append(out.Regressions, "italic balance regressed")
	}
	goldenPass := mustCardsPassCount(goldenSb)
	livePass := mustCardsPassCount(res.Scoreboard)
	if livePass < goldenPass {
		out.Regressions = append(out.Regressions, fmt.Sprintf("must_cards %d→%d", goldenPass, livePass))
	}
	return out, nil
}

// CompareReactive loads golden state for a reactive fixture and reports
// any regressions vs the live result.
func CompareReactive(goldenDir string, res *ReactiveResult) (CompareReport, error) {
	dir := filepath.Join(goldenDir, res.FixtureName)
	out := CompareReport{FixtureName: res.FixtureName}

	var goldenSb Scoreboard
	if err := readJSON(filepath.Join(dir, "scores.json"), &goldenSb); err != nil {
		return out, fmt.Errorf("read golden scores: %w", err)
	}

	if res.Scoreboard.Calmness.Value < goldenSb.Calmness.Value {
		out.Regressions = append(out.Regressions, fmt.Sprintf("calmness %d→%d", goldenSb.Calmness.Value, res.Scoreboard.Calmness.Value))
	}
	if res.Scoreboard.Decisiveness.Value < goldenSb.Decisiveness.Value {
		out.Regressions = append(out.Regressions, fmt.Sprintf("decisiveness %d→%d", goldenSb.Decisiveness.Value, res.Scoreboard.Decisiveness.Value))
	}
	if res.Scoreboard.Concreteness.Value < goldenSb.Concreteness.Value {
		out.Regressions = append(out.Regressions, fmt.Sprintf("concreteness %d→%d", goldenSb.Concreteness.Value, res.Scoreboard.Concreteness.Value))
	}
	if goldenSb.CardsSchema && !res.Scoreboard.CardsSchema {
		out.Regressions = append(out.Regressions, "card schema regressed")
	}
	// Reactive expect-hits: load from golden output.json and compare.
	var goldenOut struct {
		ExpectHits ReactiveHits `json:"expect_hits"`
	}
	_ = readJSON(filepath.Join(dir, "output.json"), &goldenOut)
	goldenPass := reactiveHitsPassCount(goldenOut.ExpectHits)
	livePass := reactiveHitsPassCount(res.ExpectHits)
	if livePass < goldenPass {
		out.Regressions = append(out.Regressions, fmt.Sprintf("expect_hits %d→%d", goldenPass, livePass))
	}
	return out, nil
}

func mustCardsPassCount(s Scoreboard) int {
	n := 0
	for _, v := range s.MustCards {
		if v {
			n++
		}
	}
	return n
}

func reactiveHitsPassCount(h ReactiveHits) int {
	n := 0
	if h.Title {
		n++
	}
	if h.Sub {
		n++
	}
	if h.Src {
		n++
	}
	if h.Rel {
		n++
	}
	if h.NotDeg {
		n++
	}
	return n
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}

// writeMemorySidecar projects the fixture's inline `memory:` block into a
// standalone memory.json next to the frozen input.json. The sidecar is a
// redundant projection — the source of truth stays inside input.json — but
// it makes the golden directory self-describing: a future reader can see
// the seeded memory state without scanning the full event-log slice. When
// the fixture has no seeded memory, no file is written (fixtures without
// memory don't grow a stub memory.json with `null` or `[]`).
func writeMemorySidecar(fixturePath, dir string) error {
	f, err := LoadFixture(fixturePath)
	if err != nil {
		return err
	}
	if len(f.Memory) == 0 {
		return nil
	}
	return writeJSON(filepath.Join(dir, "memory.json"), f.Memory)
}

// writeConcernsSidecar projects the fixture's `seed_concerns` block into
// a standalone concerns.json next to the frozen input.json. Mirrors
// writeMemorySidecar — same self-describing-golden-directory rationale.
// When the fixture has no seeded concerns, no file is written.
func writeConcernsSidecar(fixturePath, dir string) error {
	f, err := LoadFixture(fixturePath)
	if err != nil {
		return err
	}
	if len(f.SeedConcerns) == 0 {
		return nil
	}
	return writeJSON(filepath.Join(dir, "concerns.json"), f.SeedConcerns)
}

// mustCardsStateSidecar is the on-disk shape of must_cards_state.json. The
// State field records which expected_state the rules apply to so a future
// auditor can read the file standalone without cross-referencing the input.
type mustCardsStateSidecar struct {
	State     string     `json:"state"`
	MustCards []MustCard `json:"must_cards"`
}

// writeMustCardsStateSidecar projects the fixture's per-state must_cards
// rules — the ones that apply only when expected_state matches — into a
// standalone must_cards_state.json next to the frozen input.json. The
// merged pass/fail counts already live in scores.json; this sidecar exposes
// the rule definitions themselves so the golden directory is auditable
// without re-parsing input.json. Skipped when the fixture has no
// expected_state or no per-state rules for that state (morning_calm
// fixtures with only flat must_cards do not grow a stub).
func writeMustCardsStateSidecar(fixturePath, dir string) error {
	f, err := LoadFixture(fixturePath)
	if err != nil {
		return err
	}
	if f.ExpectedState == "" {
		return nil
	}
	rules := f.PerStateMustCards(synth.State(f.ExpectedState))
	if len(rules) == 0 {
		return nil
	}
	return writeJSON(filepath.Join(dir, "must_cards_state.json"), mustCardsStateSidecar{
		State:     f.ExpectedState,
		MustCards: rules,
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
