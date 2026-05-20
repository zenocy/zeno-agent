.PHONY: dev test lint ship ship-strict eval eval-baseline eval-compare eval-state eval-gemini help verify-no-pm-language

VERSION    ?= dev
GATE_MODEL ?= gemma3:4b
# EVAL_CONFIG is the source of truth for endpoint/model/api_key/json_schema_mode/no_think.
# The eval binary loads this YAML at startup and uses cfg.LLM as defaults; the
# EVAL_* env knobs below override the config when explicitly set in the make
# invocation. Default points at the same config.yaml the daemon runs against
# so `make eval` evaluates the deployed configuration by default.
EVAL_CONFIG ?= config.yaml
EVAL_MODEL ?=
EVAL_ENDPOINT ?=
EVAL_FIXTURES ?= eval/corpus
EVAL_REPORT ?= eval-report.html
EVAL_REPORT_STATE ?= eval-report-state.html
EVAL_STATE_FIXTURES ?= eval/corpus/morning_calm.json,eval/corpus/pre_meeting.json,eval/corpus/deep_work.json,eval/corpus/inject.json,eval/corpus/end_of_day.json
# Phase 0 probe (2026-04-26) confirmed the deployment endpoint requires
# json_schema and rejects json_object. Empty here = inherit from config.yaml.
# Set EVAL_JSON_SCHEMA_MODE=off to override per-run.
EVAL_JSON_SCHEMA_MODE ?=

help:
	@echo "Targets:"
	@echo "  dev          Run Go (air hot-reload) + Vite dev server concurrently."
	@echo "  test         Go ./... + Vitest."
	@echo "  lint         go vet + gofmt -l + tsc + vitest --run."
	@echo "  ship         lint + test + docker build (-t zeno:\$$(VERSION))."
	@echo "               VERSION=v0.1.0 make ship"
	@echo "  ship-strict  ship + verify-no-pm-language + four risk gates (requires LLM endpoint)."
	@echo "               GATE_MODEL=gemma3:4b make ship-strict"
	@echo "  verify-no-pm-language"
	@echo "               V2.5.0 word-ban gate. Greps source/UI/prompts for PM-shaped"
	@echo "               tokens and exits non-zero on any hit. Tag intentional refs"
	@echo "               with '// allow-pm-language' (or HTML comment in templates)."
	@echo "  eval         V2.1 evals harness — runs every fixture in eval/corpus"
	@echo "               against the deployment model and emits an HTML report."
	@echo "               EVAL_MODEL=$(EVAL_MODEL) EVAL_ENDPOINT=$(EVAL_ENDPOINT) make eval"
	@echo "  eval-state   V2.3.0 scoped eval — five state fixtures only."
	@echo "               Faster cycle for state-aware voice iteration."

# Start Go backend (air hot-reload) + Vite dev server concurrently.
# Requires: air (go install github.com/air-verse/air@latest)
# Config:   copy deploy/config.example.yaml -> config.yaml and fill in values.
dev:
	@trap 'kill 0' EXIT; \
	air & \
	cd ui && npm run dev & \
	wait

# Run all tests: Go (./...) + UI (vitest).
test:
	go test ./...
	cd ui && npm test -- --run

# Static checks. Fails on any unformatted Go file or any TS error.
lint:
	go vet ./...
	@unformatted=$$(gofmt -l . | grep -v '^ui/' || true); \
	if [ -n "$$unformatted" ]; then \
	  echo "gofmt would change:"; echo "$$unformatted"; exit 1; \
	fi
	cd ui && npx tsc -b
	cd ui && npm test -- --run

# Build a tagged docker image. Pass VERSION=v0.1.0 to tag releases.
ship: lint test
	docker build \
	  -f deploy/Dockerfile \
	  --build-arg VERSION=$(VERSION) \
	  -t zeno:$(VERSION) .
	@echo ""
	@echo "==> built and tagged zeno:$(VERSION)"
	@echo "    next: docker run / push, or run 'make ship-strict' to also exercise risk gates."

# V2.5.0 Phase 4 — word-ban gate. The Concerns surface is named so the
# product never reads like a project tracker; this target enforces that
# at the source level. Banned tokens: project (word-boundary), kanban,
# milestone, deliverable, assignee, in progress, on hold, action item.
# Lines tagged with `allow-pm-language` are exempt — use sparingly for
# comments that *define* the ban itself.
verify-no-pm-language:
	@echo "==> verify-no-pm-language: scanning for banned PM tokens"
	@hits=$$(grep -rEHIin \
	  --include='*.go' --include='*.ts' --include='*.tsx' --include='*.tmpl' \
	  --exclude='*_test.go' --exclude='*.test.ts' --exclude='*.test.tsx' \
	  --exclude-dir='__tests__' --exclude-dir='node_modules' --exclude-dir='corpus' \
	  '\bproject\b|\bkanban\b|\bmilestone\b|\bdeliverable\b|\bassignee\b|in progress|on hold|action item' \
	  internal ui/src \
	  | grep -v 'allow-pm-language' || true); \
	if [ -n "$$hits" ]; then \
	  echo ""; echo "$$hits"; echo ""; \
	  echo "==> banned PM language found. Remove or tag the line with 'allow-pm-language'."; \
	  exit 1; \
	fi
	@echo "==> verify-no-pm-language: clean"

# ship + risk gates. Requires a reachable LLM endpoint; for the frontier
# control set OPENROUTER_API_KEY in your env. Gate 1 is human-judged after
# the run — read benches/REPORT.md.
ship-strict: ship verify-no-pm-language
	@echo ""
	@echo "==> running risk gates against $(GATE_MODEL)"
	go run ./benches/voice    -model=$(GATE_MODEL) -label="ship-$(VERSION)"
	go run ./benches/toolchain -model=$(GATE_MODEL) -label="ship-$(VERSION)" -runs=20
	go run ./benches/schema    -model=$(GATE_MODEL) -label="ship-$(VERSION)" -runs=20
	go build -o /tmp/zeno-ship ./cmd/zeno
	go run ./benches/coldstart -binary=/tmp/zeno-ship -config=$$PWD/config.yaml -label="ship-$(VERSION)"
	@echo ""
	@echo "==> all gates exited 0; gate 1 (voice) still needs a human read of benches/REPORT.md."

# V2.1 evals harness. Runs every *.json fixture in EVAL_FIXTURES against the
# LLM endpoint defined in EVAL_CONFIG (default: config.yaml), scores each,
# and writes an HTML report.
#
# Default behavior reads endpoint/model/api_key/json_schema_mode/no_think
# from EVAL_CONFIG so `make eval` evaluates the deployed configuration. Set
# any of the EVAL_* env vars below to override per-run; an empty value
# means "inherit from config.yaml":
#   EVAL_CONFIG=config.yaml         # path to zeno config; '-' to skip loading
#   EVAL_MODEL=qwen3.6-35b-a3b      # override config llm.model
#   EVAL_ENDPOINT=http://localhost:11434/v1
#   EVAL_JSON_SCHEMA_MODE=on        # override config llm.json_schema_mode
#   EVAL_FIXTURES=eval/corpus
#   EVAL_REPORT=eval-report.html
EVAL_FLAGS = -config=$(EVAL_CONFIG) \
  -fixtures=$(EVAL_FIXTURES) \
  $(if $(EVAL_ENDPOINT),-endpoint=$(EVAL_ENDPOINT)) \
  $(if $(EVAL_MODEL),-model=$(EVAL_MODEL)) \
  $(if $(EVAL_JSON_SCHEMA_MODE),-json-schema-mode=$(EVAL_JSON_SCHEMA_MODE))

eval:
	@echo "==> eval harness — config=$(EVAL_CONFIG)$(if $(EVAL_MODEL), model=$(EVAL_MODEL))$(if $(EVAL_ENDPOINT), endpoint=$(EVAL_ENDPOINT))$(if $(EVAL_JSON_SCHEMA_MODE), json_schema=$(EVAL_JSON_SCHEMA_MODE))"
	go run ./eval/cmd/eval $(EVAL_FLAGS) -report=$(EVAL_REPORT)
	@echo "==> report → $(EVAL_REPORT)"

# Freeze the current eval output as the v2.1.0 golden corpus snapshot.
# Writes per-fixture {input,output,scores,meta}.json into eval/corpus/golden/
# Re-baselining is intentional and committed; never silent. Set EVAL_FORCE=1
# to overwrite an existing golden snapshot.
eval-baseline:
	@echo "==> freezing golden corpus — config=$(EVAL_CONFIG)"
	@if [ -d eval/corpus/golden ] && [ -z "$(EVAL_FORCE)" ]; then \
	  echo "eval/corpus/golden/ exists; pass EVAL_FORCE=1 to overwrite"; exit 1; \
	fi
	go run ./eval/cmd/eval $(EVAL_FLAGS) \
	  -report=/tmp/eval-baseline-report.html \
	  -freeze-golden=eval/corpus/golden
	@echo "==> golden corpus written under eval/corpus/golden/"
	@echo "    Review with 'git diff eval/corpus/golden/' and commit if intentional."

# V2.3.0 P5: scoped eval — five state fixtures only. Mirrors `eval` but
# skips the reactive + memory-grounding fixtures so a state-aware voice
# iteration cycle runs in a fraction of the time.
eval-state:
	@echo "==> eval-state — state fixtures only (config=$(EVAL_CONFIG))"
	go run ./eval/cmd/eval -config=$(EVAL_CONFIG) \
	  -fixtures=$(EVAL_STATE_FIXTURES) \
	  -report=$(EVAL_REPORT_STATE) \
	  $(if $(EVAL_ENDPOINT),-endpoint=$(EVAL_ENDPOINT)) \
	  $(if $(EVAL_MODEL),-model=$(EVAL_MODEL)) \
	  $(if $(EVAL_JSON_SCHEMA_MODE),-json-schema-mode=$(EVAL_JSON_SCHEMA_MODE))
	@echo "==> state report → $(EVAL_REPORT_STATE)"

# V2.x: run the eval harness against the native Gemini provider. Uses
# eval/eval-config.gemini.yaml so a single command exercises Gemini
# end-to-end with the same fixtures the OpenAI-side eval uses; useful
# for catching regressions when the provider abstraction changes shape.
# Requires GEMINI_API_KEY in env (or set llm.gemini.api_key in the YAML).
eval-gemini:
	@echo "==> eval-gemini — config=eval/eval-config.gemini.yaml"
	go run ./eval/cmd/eval -config=eval/eval-config.gemini.yaml \
	  -fixtures=$(EVAL_FIXTURES) \
	  -report=eval-report-gemini.html
	@echo "==> gemini eval report → eval-report-gemini.html"

# Diff the current run against the committed golden corpus. Exits non-zero
# on regression (any deterministic dimension drops by ≥1 point on ≥1 fixture,
# OR must_cards/reactive_expect drops on any fixture, OR latency p50 grows
# ≥30%).
eval-compare:
	@echo "==> eval-compare against eval/corpus/golden/"
	go run ./eval/cmd/eval $(EVAL_FLAGS) \
	  -report=/tmp/eval-compare-report.html \
	  -compare-golden=eval/corpus/golden
	@echo "==> compare report → /tmp/eval-compare-report.html"
