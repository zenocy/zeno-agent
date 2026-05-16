# Voice gate rubric

Read the briefings emitted by `benches/voice/main.go` against this rubric. Each is scored 0–3; total of 12.

| Dimension | 0 (fails) | 1 (weak) | 2 (passes) | 3 (excels) |
|---|---|---|---|---|
| **Calmness** | Exclamation marks, urgency markers, "Important:", emoji | Slightly chatty, pleasantries | Even tone throughout | The reader exhales reading it |
| **Decisiveness** | Asks the reader what they want | Hedges ("maybe", "you might want to") | States the day's pivot in the first sentence | Names the *one* thing without ceremony |
| **Concreteness** | Vague ("today", "soon", "recent") | Mostly vague with one concrete detail | Times, counts, names mostly present | Reader can act on the briefing alone |
| **Voice match** | Reads like ChatGPT | Reads generic-AI | Sounds like the `zeno-data.jsx` samples | Indistinguishable from a hand-written sample |

Threshold for gate-1 pass: **9/12 on at least one local model.** If only the frontier control passes, document that local needs more work and consider the `llm.briefing_endpoint` override.

Sample target — `zeno-data.jsx` `default` briefing — would score 12/12.
