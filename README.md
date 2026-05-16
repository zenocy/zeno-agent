# Zeno V2

Single-user, self-hosted ambient cognitive surface. One Go binary, embedded React UI, SQLite. Reads your mail/calendar/weather and surfaces what matters in a calm, opinionated, literary voice.

**Status:** MVP. Phases 0–5 complete. V2.1.0 voice & reliability hardening shipped (see [`doc/v2.1/`](doc/v2.1/)). V2.2.0 derived-memory layer shipped (see [`doc/v2.2/`](doc/v2.2/)). V2.3.0 adaptive states shipping (see [`doc/v2.3/`](doc/v2.3/)). For roadmap and ship details see [`doc/TODO.md`](doc/TODO.md) and [`doc/Phase5.md`](doc/Phase5.md).

## Try it (production)

```bash
cp deploy/config.example.yaml deploy/config.yaml   # then edit
docker compose -f deploy/docker-compose.yml up -d
open http://localhost:7777
```

Full install path — provider notes (Gmail / Fastmail / iCloud), CalDAV URL discovery, LLM endpoint setup, LAN exposure, troubleshooting — is in **[`docs/README.md`](docs/README.md)**.

## Web tools (optional)

Reactive Ask, the morning briefing, and the inject pipeline can call out
to the public web via two LLM tools — `search_web` and `read_url` —
backed by [Jina AI](https://jina.ai/). Disabled until you provide a key.

```bash
# Get a key (10M free tokens, no card) at https://jina.ai/?sui=apikey
export ZENO_WEB_JINA_API_KEY=jina_xxxxxxxxxxxxxx
```

…or in `config.yaml`:

```yaml
web:
  jina:
    api_key: jina_xxxxxxxxxxxxxx
    # Optional scheduled saved searches — refreshed every SyncCron tick,
    # gated by the 6h cache TTL so each query hits Jina at most every 6h.
    # Each refresh appends one web.search.result observation to the log
    # so the inject subscriber can promote a high-signal result to a card.
    saved_searches:
      - name: golang-news
        query: "Go 1.24 release notes"
        site: go.dev
```

Boot logs `jina: web tools enabled` once the key is picked up. Responses
are cached in SQLite (`jina_cache` table; 6h for search, 24h for read,
5min for sign-in pages). Empty `api_key` with non-empty `saved_searches`
is a fatal config error. Full reference: [`docs/web.md`](docs/web.md).

## Develop

```bash
# Backend
cp deploy/config.example.yaml config.yaml          # edit credentials
go run ./cmd/zeno                                  # serve on :7777

# Frontend (separate terminal)
cd ui && npm install && npm run dev                # vite on :5173, proxies /api

# Or both at once with hot reload
make dev                                           # requires `air`
```

## Test, lint, ship

```bash
make test          # go test ./... + vitest
make lint          # vet + gofmt + tsc + vitest
make eval          # V2.1 harness — every fixture in eval/corpus against the LLM
make eval-state    # V2.3.0 scoped harness — five state fixtures only (faster cycle)
make eval-compare  # diff live run vs eval/corpus/golden/, non-zero on regression
make ship VERSION=v0.1.0
                   # lint + test + docker build → zeno:v0.1.0
make ship-strict GATE_MODEL=gemma3:4b
                   # ship + the four risk gates (needs LLM endpoint)
```

> Touching `prompts/*.tmpl` or `prompts/_voice*.md` requires running
> `make eval` and committing a new baseline (`make eval-baseline
> EVAL_FORCE=1`) if the change is intentional. See
> [`docs/voice.md`](docs/voice.md) for the rubric.
>
> Memory-aware fixtures (V2.2.0, `eval/corpus/*_with_memory.json`)
> seed `memory_facts` rows before the synth run; the
> `memory_grounding` rubric dimension scores opener-tells,
> fact-density, and multi-word verbatim leaks. Re-baselining is
> required when prompts in the memory section change.

## Documentation

| Where | What |
|---|---|
| [`docs/README.md`](docs/README.md) | Install from cold (Docker, providers, LAN, troubleshooting) |
| [`docs/voice.md`](docs/voice.md) | Voice rules — public mirror of `prompts/_voice.md`, with harness-enforcement section |
| [`docs/web.md`](docs/web.md) | Web tools (Jina) — setup, caching, saved searches, costs |
| [`docs/benches.md`](docs/benches.md) | Risk-gate harnesses (voice, tool-call, cold-start, schema) |
| [`doc/TODO.md`](doc/TODO.md) | Phase index, architectural decisions, open questions |
| [`doc/PhaseN.md`](doc/) | Self-contained per-phase briefs (Phase 0 → 5) |
| [`doc/v2.1/`](doc/v2.1/) | V2.1.0 voice + reliability hardening — phases, handoff, release notes |
| [`doc/v2.2/`](doc/v2.2/) | V2.2.0 derived-memory layer — phases, handoff, release notes |
| [`doc/v2.3/`](doc/v2.3/) | V2.3.0 adaptive states — phases, handoff, release notes |
| [`doc/v2.8/`](doc/v2.8/) | V2.8.0 action surface — phases, handoff, release notes |
| [`prompts/_voice.md`](prompts/_voice.md) | Source of truth for the voice |

## Architecture in one paragraph

Five components inside one Go process. **Sensors** (IMAP / CalDAV / Open-Meteo, plus an optional Jina web sensor for scheduled saved searches) write to an append-only **observation log** (SQLite). **Projections** fold the log into typed views (today's calendar, open email threads, run window, derived-memory facts). The **synthesizer** runs twice a day: a tool-using cards loop reads projections via a small read-only tool registry — `read_thread`, `read_event`, `read_weather_window`, plus optional `search_web` / `read_url` when Jina is configured — emits `remember: <subject>: <predicate>` lines for durable user facts, and then a single literary call writes the briefing seeded by the cards. Between cards and briefing, a deterministic detector picks an **adaptive state** (`morning_calm`, `pre_meeting`, `deep_work`, `message_inject`, `end_of_day`) that overlays per-state voice and cards-bias rules onto the existing prompts; `message_inject` is its own mid-day single-card pipeline that delivers via SSE without reshuffling the morning grid. A **deterministic consolidator** folds new candidates into a separate `memory_facts` table — dedup by subject, evidence-counting, confidence promotion, eviction at cap. **Cards/briefings/traces/memory** persist in their own tables. The **HTTP API** serves projections, briefings, cards, memory CRUD, a reactive `Ask` endpoint, and a `/api/today/stream` SSE feed to the **embedded React UI** (which exposes a Profile panel for users to see, add, edit, and delete memory, and a state-aware Topbar pill). Cron drives `sync_all` (every 10 min) and `morning_synth` (configurable); message-inject fires reactively from sensor observations on the in-process event bus, not on a cron tick. No microservices, no DI container — modularity comes from typed interfaces between the components.

## License

Licensed under the [Apache License, Version 2.0](LICENSE). See [`NOTICE`](NOTICE) for attribution.

Copyright 2026 Andreas Louca
