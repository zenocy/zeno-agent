# Zeno Agent

A single-user, self-hosted ambient agent. It reads your mail, calendar,
weather, and the threads you care about; surfaces what actually matters
in a calm, opinionated, literary voice; and runs entirely on your own
machine against a local (or any OpenAI-compatible) LLM.

One Go binary, embedded React UI, SQLite. No cloud, no account, no
telemetry.

## What it does

- **Morning briefing.** Twice a day on cron, a synthesizer pass reads
  your inbox, today's calendar, weather, tracked concerns, and
  (optionally) the web; emits a small set of cards ranked by what needs
  your attention; and writes a short briefing in a literary voice.
- **Ambient cards.** Throughout the day, sensors push new context onto
  an append-only observation log. A deterministic state detector picks
  one of five registers (`morning_calm`, `pre_meeting`, `deep_work`,
  `message_inject`, `end_of_day`) that subtly changes the agent's voice
  and what it surfaces.
- **Reactive Ask.** One input box; the agent routes through a small
  tool registry (`read_thread`, `read_event`, `read_weather_window`,
  optional `search_web` / `read_url`) and answers with sources.
- **Derived memory.** Durable facts about you — relationships,
  routines, preferences — are extracted by a focused single-call pass
  and consolidated into a `memory_facts` table. Edit and curate them in
  the Profile panel.
- **Outbound actions.** Send a reply over SMTP, draft and send a
  WhatsApp message (optional pairing), or have your named EA reply on
  your behalf — with a "reply received" card when the other side
  responds.

## Sensors

Sensors are the only thing that writes to the observation log. Each one
polls its source on a cadence and appends typed events; everything
downstream (projections, cards, briefings) reads from the log, never
from the source directly.

| Sensor | What it reads | Required | Default cadence |
|---|---|---|---|
| **IMAP** | Inbox messages and threads (Fastmail, iCloud, Gmail with app password, …) | yes | every 10 min |
| **CalDAV** | Today's events and the run-window outlook | yes | every 10 min |
| **Weather** | Open-Meteo forecast for one lat/lon | yes | every 10 min |
| **CardDAV** | Address book — resolves "my wife" → vCard → phone → JID for WhatsApp send | no | 5 min |
| **Stock** | Yahoo quotes for a small ticker list; surfaces moves past a threshold | no | market hours only |
| **Jina** | Saved web searches (Reader + Search APIs) refreshed on each cron tick | no | gated by per-query TTL (6h default) |
| **WhatsApp** | Persistent `whatsmeow` socket as a linked device; inbound messages land as observations | no | event-driven |
| **SMTP** | Outbound only — the action surface for `send_reply` / `forward` | no | — |

Two reactive paths sit alongside the cron loop:

- **Inject bus.** After every successful log append, the sensor
  publishes on an in-process event bus. An `inject_detector` decides
  whether the event is worth interrupting the day for; if so, a
  single-card `message_inject` briefing fires immediately and streams
  to the UI over SSE — no cron tick, no full reshuffle of the morning
  grid.
- **Concerns.** Long-running situations the user is tracking (a
  Frankfurt trip, a hire, the house construction) bias card selection
  and weave into prose as scaffolding, never as status reports.

## Voice

Voice is the product. If a Zeno briefing reads like ChatGPT, the
product is dead — so every prompt in `prompts/` includes a voice rules
preamble (`prompts/_voice.md`). The shape:

- **Calm.** No exclamation marks, no urgency markers, no emoji.
- **Opinionated.** Tells, doesn't ask. *"Reply to Saru first — the
  option-pool answer feeds the deck."*
- **Literary.** Briefings read like a paragraph from a novel — concrete
  nouns, plain verbs, em-dash for asides, period for finality, one
  italicized word per beat.
- **Concrete.** Times, days, counts, names. *"6 days since your last
  run." "Hold expires today at 17:00."* Not "recently" or "soon".
- **Personal and work in one stream.** *"Sam's making dinner; Lia's
  bedtime story is your turn"* sits next to a Series B briefing.
- **Decisive close.** No "let me know what you think", no "happy to
  adjust". The briefing is a statement, not an offer.

The five adaptive states each pull the voice into a different register
— `pre_meeting` braces the user for one charged event in the next two
hours; `deep_work` protects a long block and trims cards; `end_of_day`
closes today and gestures at tomorrow's first thing. State-specific
rules and cards-bias overlays live in `prompts/_voice.md`; the
[Zeno V2 spec](Zeno%20V2/zeno-data.jsx) is the canonical exemplar.

## Quick start

```bash
cp deploy/config.example.yaml deploy/config.yaml   # then edit
docker compose -f deploy/docker-compose.yml run --rm zeno /app/zeno hash-password
                                                   # paste the hash into auth.password_hash
docker compose -f deploy/docker-compose.yml up -d
open http://localhost:7777
```

You'll need:

- An **IMAP** account (Fastmail, iCloud, Gmail with app password, …).
- A **CalDAV** URL (same provider, usually).
- An **OpenAI-compatible LLM endpoint**. The default config points at
  Ollama on the host (`http://host.docker.internal:11434/v1`); LM
  Studio, vLLM, or any other compatible server work too. A ~30B local
  model is the sweet spot — small enough to run on a workstation, large
  enough to hold the literary voice.
- Optional: a [Jina AI](https://jina.ai/) key to enable the
  `search_web` and `read_url` tools. Set
  `ZENO_WEB_JINA_API_KEY=jina_…` or fill in the `web.jina` block in
  `config.yaml`.

Everything else — WhatsApp pairing, CardDAV contacts, reminder
dispatch, LAN exposure, tuning knobs — is documented inline in
[`deploy/config.example.yaml`](deploy/config.example.yaml).

## Authentication

The UI and the `/api/*` surface are gated by a single-user cookie
login. Credentials live in `config.yaml` under the `auth:` block:

```yaml
auth:
  enabled: true
  username: you
  password_hash: "$2a$10$…"   # produced by `zeno hash-password`
  session_ttl: 720h            # 30 days
  cookie_secure: false         # set true when serving over HTTPS
```

Generate the hash interactively, then paste it into
`auth.password_hash`:

```bash
# In a running container:
docker compose -f deploy/docker-compose.yml exec zeno /app/zeno hash-password

# Or locally before first boot:
go run ./cmd/zeno hash-password
```

Sessions are persisted in the same SQLite database as everything else,
so a `docker compose restart` keeps you signed in. The cookie itself is
signed with `auth.session_secret`; leave it empty and the server
auto-generates one into `data/session.key` on first boot.

`server.lan_token` still works alongside the cookie — useful for the
docker `HEALTHCHECK` and any LAN scripts that hit `/api/*` with
`Authorization: Bearer <token>`. To disable the login entirely (e.g. on
a trusted loopback-only box, or as an emergency rollback), set
`auth.enabled: false`.

## Develop

```bash
# Backend (serves on :7777)
cp deploy/config.example.yaml config.yaml          # edit credentials
go run ./cmd/zeno

# Frontend (separate terminal — Vite on :5173, proxies /api)
cd ui && npm install && npm run dev

# Or both at once with hot reload
make dev                                           # requires `air`
```

## Test, lint, eval

```bash
make test          # go test ./... + vitest
make lint          # vet + gofmt + tsc + vitest
make eval          # run every fixture in eval/corpus through a real LLM
make eval-compare  # diff a fresh run vs. eval/corpus/golden/ (non-zero on regression)
make ship VERSION=v0.1.0
                   # lint + test + docker build → zeno:v0.1.0
```

The eval harness is the closest thing to a regression suite for the
agent's voice and tool-use. It expects a running LLM endpoint (set
`EVAL_CONFIG=config.yaml`) and is slow (~minutes against a local 30B);
the goldens in `eval/corpus/golden/` capture a frozen-good run so
prompt edits can be measured rather than guessed at.

## How it's built

Five components inside one Go process:

- **Sensors** (`internal/sensor/…`) — IMAP, CalDAV, Open-Meteo,
  optionally CardDAV, stock, Jina, and WhatsApp. They write to the
  append-only observation log (SQLite) and publish on the in-process
  event bus immediately after each append.
- **Projections** (`internal/projection`) — fold the log into typed
  views consumed by the synthesizer and the UI: today's calendar, open
  email threads, run window, derived-memory facts, stock snapshot,
  weather, WhatsApp activity.
- **Synthesizer** (`internal/synth`) — twice-daily cron runs a
  tool-using cards loop over projections, emits
  `remember: <subject>: <predicate>` lines for durable facts, and a
  single literary call writes the briefing seeded by the cards.
  `message_inject` is its own one-card pipeline that delivers via SSE
  without reshuffling the morning grid.
- **State + memory** — a deterministic detector picks the adaptive
  state that overlays per-state voice and card-bias rules onto the
  existing prompts. A deterministic consolidator folds extracted
  candidates into `memory_facts`, with dedup-by-subject, evidence
  counting, and eviction at a cap.
- **HTTP + UI** (`internal/http`, `ui/`) — serves projections,
  briefings, cards, memory CRUD, a reactive `Ask` endpoint, and a
  `/api/today/stream` SSE feed to the embedded React UI.

Cron drives `sync_all` (every 10 min) and `morning_synth`
(configurable). `message_inject` fires reactively from sensor
observations on the event bus, not on a cron tick. No microservices,
no DI container — modularity comes from typed interfaces between the
components.

## License

Licensed under the [Apache License, Version 2.0](LICENSE). See
[`NOTICE`](NOTICE) for attribution.

Copyright 2026 Andreas Louca
