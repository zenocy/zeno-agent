// Command eval runs the V2.1 evals harness against the configured LLM
// endpoint. Usage:
//
//	make eval                                      # uses eval/eval-config.yaml
//	go run ./eval/cmd/eval -fixtures eval/corpus
//	go run ./eval/cmd/eval -fixtures eval/corpus/morning_calm.json -model qwen3.6-35b-a3b
//
// Default behavior: walks the -fixtures path (file or directory) for
// *.json fixtures, runs each against the LLM endpoint, scores them, and
// writes an HTML report at -report (default: eval-report.html in CWD).
//
// Fixture kind is auto-detected from the JSON's `kind` field. Morning
// fixtures (kind="" or "morning") run synth.Runner; reactive fixtures
// (kind="reactive") invoke synth.Ask with the fixture's `query` and
// score against `reactive_expect`.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/zenocy/zeno-v2/eval"
	"github.com/zenocy/zeno-v2/internal/config"
	"github.com/zenocy/zeno-v2/internal/llm"
	"github.com/zenocy/zeno-v2/internal/synth"
)

func main() {
	// Sentinel zero-values on the LLM-shaping flags. After flag.Parse we
	// fold in defaults from config.yaml (best-effort) for any flag the
	// user didn't explicitly set, then fall back to the historical
	// hardcoded values if no config is reachable. CLI flags always win
	// when explicitly given — flag.Visit is the source of truth on what
	// the user typed.
	var (
		fixturesPath    = flag.String("fixtures", "eval/corpus", "path to a fixture file or a directory of fixtures")
		reportPath      = flag.String("report", "eval-report.html", "where to write the HTML report")
		configPath      = flag.String("config", "config.yaml", "path to zeno config (provides LLM defaults)")
		endpoint        = flag.String("endpoint", "", "OpenAI-compatible endpoint (default: config llm.endpoint, then http://localhost:11434/v1)")
		apiKey          = flag.String("api-key", os.Getenv("ZENO_LLM_API_KEY"), "API key (default: ZENO_LLM_API_KEY env, then config llm.api_key)")
		model           = flag.String("model", "", "model name (default: config llm.model, then qwen3.6-35b-a3b)")
		promptsDir      = flag.String("prompts", "", "prompts dir (default: embedded)")
		schemaMode      = flag.String("json-schema-mode", "", "off | auto | on (default: config llm.json_schema_mode, then off)")
		cardsTimeout    = flag.Duration("cards-timeout", 300*time.Second, "cards loop deadline (also used as reactive deadline)")
		brTimeout       = flag.Duration("briefing-timeout", 180*time.Second, "briefing call deadline")
		toolTimeout     = flag.Duration("tool-timeout", 10*time.Second, "per-tool execution timeout")
		reactiveTimeout = flag.Duration("reactive-timeout", 180*time.Second, "deadline for synth.Ask on reactive fixtures (separate from cards-timeout)")
		noThink         = flag.Bool("no-think", false, "prepend /no_think to briefing system prompt on Qwen3 models (default: config llm.no_think)")
		freezeGolden    = flag.String("freeze-golden", "", "if set, write per-fixture {input,output,scores,meta}.json into this directory")
		compareGolden   = flag.String("compare-golden", "", "if set, diff each fixture's scores against the golden snapshot at this directory; exits non-zero on any regression")
	)
	flag.Parse()

	// Detect which flags the user typed explicitly. Anything in `userSet`
	// wins over the config; everything else inherits the config value
	// (or the historical hardcoded default if config is unreachable).
	userSet := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { userSet[f.Name] = true })

	cfg, cfgErr := config.Load(*configPath)
	if cfgErr != nil {
		// Missing config is non-fatal — keep the hardcoded fallbacks. Log
		// quietly so the operator can debug a typo, but don't bail.
		fmt.Fprintf(os.Stderr, "[eval] config %s not loaded: %v (using defaults)\n", *configPath, cfgErr)
	}

	applyLLMDefault(endpoint, "endpoint", userSet, cfgLLMEndpoint(cfg), "http://localhost:11434/v1")
	applyLLMDefault(model, "model", userSet, cfgLLMModel(cfg), "qwen3.6-35b-a3b")
	applyLLMDefault(schemaMode, "json-schema-mode", userSet, cfgLLMJSONSchemaMode(cfg), "off")
	// API key: the env-var-derived default counts as "user-supplied" for the
	// purposes of override; only fall through to config when both flag and
	// env are empty.
	if !userSet["api-key"] && *apiKey == "" {
		if k := cfgLLMAPIKey(cfg); k != "" {
			*apiKey = k
		}
	}
	if !userSet["no-think"] && cfg != nil && cfg.LLM.NoThink {
		*noThink = true
	}

	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel) // quiet by default; per-fixture summary printed below
	entry := logrus.NewEntry(logger)

	prompts, err := synth.LoadPrompts(*promptsDir)
	if err != nil {
		fail("load prompts: %v", err)
	}

	client := llm.NewClient(llm.ClientConfig{
		Endpoint:       *endpoint,
		APIKey:         *apiKey,
		Model:          *model,
		Timeout:        *brTimeout + 30*time.Second, // generous HTTP cap; loop deadlines do real work
		JSONSchemaMode: *schemaMode,
		MaxTokens:      cfgLLMMaxTokens(cfg),
		NoThink:        *noThink,
	})

	fixtures, err := collectFixtures(*fixturesPath)
	if err != nil {
		fail("collect fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		fail("no fixtures found at %s", *fixturesPath)
	}

	fmt.Printf("[eval] %d fixtures · model=%s · endpoint=%s\n", len(fixtures), *model, *endpoint)

	morningResults := make([]*eval.RunResult, 0, len(fixtures))
	reactiveResults := make([]*eval.ReactiveResult, 0)

	opts := eval.RunOpts{
		LLM:             client,
		Prompts:         prompts,
		CardsTimeout:    *cardsTimeout,
		BriefingTimeout: *brTimeout,
		ToolTimeout:     *toolTimeout,
		ReactiveTimeout: *reactiveTimeout,
		Logger:          entry,
	}

	for _, fp := range fixtures {
		f, err := eval.LoadFixture(fp)
		if err != nil {
			fmt.Printf("[eval]   %s · LOAD FAILED: %v\n", filepath.Base(fp), err)
			continue
		}
		fmt.Printf("[eval] running %s (%s) ...\n", filepath.Base(fp), kindLabel(f.Kind))

		// V2.3.0 P3: route fixtures with an inject_signal block + a
		// message_inject expected_state through the inject harness, which
		// bypasses the morning Runner and calls SynthesizeInject directly.
		// All other morning-style fixtures keep going through RunFixture.
		isInject := f.InjectSignal != nil && f.ExpectedState == "message_inject"

		switch {
		case f.Kind == "reactive":
			ctx, cancel := context.WithTimeout(context.Background(), *reactiveTimeout+30*time.Second)
			res, err := eval.RunReactiveFixture(ctx, fp, opts)
			cancel()
			if err != nil {
				fmt.Printf("[eval]   FAILED: %v\n", err)
				continue
			}
			fmt.Printf("[eval]   %s\n", eval.SummaryLineReactive(res))
			reactiveResults = append(reactiveResults, res)
			if *freezeGolden != "" {
				if dir, err := eval.FreezeReactive(*freezeGolden, fp, *model, *endpoint, *schemaMode == "on", res); err != nil {
					fmt.Printf("[eval]   FREEZE FAILED: %v\n", err)
				} else {
					fmt.Printf("[eval]   froze → %s\n", dir)
				}
			}
		case isInject:
			ctx, cancel := context.WithTimeout(context.Background(), *cardsTimeout+*brTimeout+30*time.Second)
			res, err := eval.RunInjectFixture(ctx, fp, opts)
			cancel()
			if err != nil {
				fmt.Printf("[eval]   FAILED: %v\n", err)
				continue
			}
			fmt.Printf("[eval]   %s\n", eval.SummaryLine(res))
			morningResults = append(morningResults, res)
			if *freezeGolden != "" {
				if dir, err := eval.FreezeMorning(*freezeGolden, fp, *model, *endpoint, *schemaMode == "on", res); err != nil {
					fmt.Printf("[eval]   FREEZE FAILED: %v\n", err)
				} else {
					fmt.Printf("[eval]   froze → %s\n", dir)
				}
			}
		default: // "" or "morning"
			ctx, cancel := context.WithTimeout(context.Background(), *cardsTimeout+*brTimeout+30*time.Second)
			res, err := eval.RunFixture(ctx, fp, opts)
			cancel()
			if err != nil {
				fmt.Printf("[eval]   FAILED: %v\n", err)
				continue
			}
			fmt.Printf("[eval]   %s\n", eval.SummaryLine(res))
			morningResults = append(morningResults, res)
			if *freezeGolden != "" {
				if dir, err := eval.FreezeMorning(*freezeGolden, fp, *model, *endpoint, *schemaMode == "on", res); err != nil {
					fmt.Printf("[eval]   FREEZE FAILED: %v\n", err)
				} else {
					fmt.Printf("[eval]   froze → %s\n", dir)
				}
			}
		}
	}

	// Compare against golden if requested. Exits non-zero if any regression.
	regressed := false
	if *compareGolden != "" {
		fmt.Println("[eval] comparing against golden corpus at", *compareGolden)
		for _, r := range morningResults {
			rep, err := eval.CompareMorning(*compareGolden, r)
			if err != nil {
				fmt.Printf("[eval]   compare %s · ERROR: %v\n", r.FixtureName, err)
				regressed = true
				continue
			}
			if len(rep.Regressions) > 0 {
				regressed = true
				fmt.Printf("[eval]   compare %s · REGRESSION: %v\n", r.FixtureName, rep.Regressions)
			} else {
				fmt.Printf("[eval]   compare %s · ok\n", r.FixtureName)
			}
		}
		for _, r := range reactiveResults {
			rep, err := eval.CompareReactive(*compareGolden, r)
			if err != nil {
				fmt.Printf("[eval]   compare %s · ERROR: %v\n", r.FixtureName, err)
				regressed = true
				continue
			}
			if len(rep.Regressions) > 0 {
				regressed = true
				fmt.Printf("[eval]   compare %s · REGRESSION: %v\n", r.FixtureName, rep.Regressions)
			} else {
				fmt.Printf("[eval]   compare %s · ok\n", r.FixtureName)
			}
		}
	}

	if err := writeReport(*reportPath, *model, *endpoint, morningResults, reactiveResults); err != nil {
		fail("write report: %v", err)
	}
	fmt.Printf("[eval] report → %s\n", *reportPath)

	if regressed {
		fmt.Fprintln(os.Stderr, "[eval] one or more fixtures regressed against the golden corpus")
		os.Exit(1)
	}
}

// kindLabel returns "morning" for empty kind, the kind itself otherwise.
// V2.3.0 P3 callers may want a distinct label for inject; that's surfaced
// inline at the dispatch site rather than complicating this helper.
func kindLabel(kind string) string {
	if kind == "" {
		return "morning"
	}
	return kind
}

// collectFixtures returns *.json files under path. Path may be a single
// file, a directory, or a comma-separated list of files/directories — the
// V2.3.0 P5 `make eval-state` target leans on the comma form to scope a
// run to the five state fixtures without restructuring the corpus.
// Duplicate entries are de-duplicated; results sorted for determinism.
func collectFixtures(path string) ([]string, error) {
	parts := strings.Split(path, ",")
	seen := make(map[string]struct{}, len(parts))
	out := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		paths, err := collectOneFixturePath(p)
		if err != nil {
			return nil, err
		}
		for _, fp := range paths {
			if _, dup := seen[fp]; dup {
				continue
			}
			seen[fp] = struct{}{}
			out = append(out, fp)
		}
	}
	sort.Strings(out)
	return out, nil
}

func collectOneFixturePath(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		out = append(out, filepath.Join(path, name))
	}
	return out, nil
}

func writeReport(path, model, endpoint string, morning []*eval.RunResult, reactive []*eval.ReactiveResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return eval.WriteHTML(f, eval.ReportData{
		GeneratedAt:     time.Now(),
		Model:           model,
		Endpoint:        endpoint,
		Results:         morning,
		ReactiveResults: reactive,
	})
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[eval] "+format+"\n", args...)
	os.Exit(1)
}

// applyLLMDefault folds the config-provided value into a flag pointer
// only when the user didn't explicitly set the flag. Empty config
// values fall through to the hardcoded fallback so a config that omits
// a key still produces a sensible run.
func applyLLMDefault(target *string, name string, userSet map[string]bool, fromCfg, fallback string) {
	if userSet[name] {
		return
	}
	if fromCfg != "" {
		*target = fromCfg
		return
	}
	*target = fallback
}

// Tiny accessors so the call sites read like English. Each tolerates a
// nil config (the load may have failed) and returns a zero string.
func cfgLLMEndpoint(c *config.Config) string {
	if c == nil {
		return ""
	}
	return c.LLM.Endpoint
}

func cfgLLMModel(c *config.Config) string {
	if c == nil {
		return ""
	}
	return c.LLM.Model
}

func cfgLLMAPIKey(c *config.Config) string {
	if c == nil {
		return ""
	}
	return c.LLM.APIKey
}

func cfgLLMJSONSchemaMode(c *config.Config) string {
	if c == nil {
		return ""
	}
	return c.LLM.JSONSchemaMode
}

func cfgLLMMaxTokens(c *config.Config) int {
	if c == nil {
		return 0
	}
	return c.LLM.MaxTokens
}
