# Risk-gate report

Auto-appended by the four bench harnesses (`benches/{voice,toolchain,coldstart,schema}`). Each run adds a section. Compare side-by-side; act on the threshold breaches called out below.

## Thresholds

- **voice (gate 1):** human-judged via `benches/voice/RUBRIC.md`. Pass = ≥9/12 on at least one local model.
- **toolchain (gate 2):** ≥80% success over 20 runs.
- **coldstart (gate 3):** ≤10s container-up to first `/api/health` response.
- **schema (gate 4):** ≥80% clean-parse rate over 20 runs.

If any gate fails, stop and re-plan the affected phase before writing code in it.

---

## voice · qwen/qwen3.6-35b-a3b · 2026-04-25T18:09:27+03:00

_duration: 1m54.591s, prompt tokens: 2635, completion tokens: 7698_

```
Eyebrow: this morning · 4 things worth knowing
Title: *One* thing wants you before noon. Series B review at 11:00.
Summary: Walked the redline with Lin. Saru left two questions on the table — option pool, and the 1× non-participating preferred; you will answer before the 11:00. Ashby confirmed three of four panelists, and the hold expires at 17:00. Mara’s 14:00 meets your 12:30–13:30 run window at 16° under clear skies and light wind, while Sam takes Lia at 15:30. The afternoon breathes until rain arrives at 19:00.
Tension: 70
```

## voice · google/gemma-4-e2b · 2026-04-25T18:10:19+03:00

_duration: 9.492s, prompt tokens: 2659, completion tokens: 877_

```
Eyebrow: *the narrative pivot · 3 key windows*
Title: The Series B review is set for 11:00. Otherwise the day breathes — your run window is open at noon.
Summary: Acuity meets at 11:00 with Saru Patel and Lin Vega; the discussion centers on the *narrative*. Saru replied to your email regarding the redline, leaving two questions pending. The forecast holds clear conditions until 13:30, making the run window available. Lia's pickup is scheduled for Sam at 15:30.
Tension: 65
```

## voice · gpt-oss-120b · 2026-04-25T18:11:59+03:00

_duration: 27.759s, prompt tokens: 2426, completion tokens: 987_

```
Eyebrow: this morning · 4 things worth knowing  
Title: Series B narrative review at 11 sets the day's tone. *One* clear window for a run follows.  
Summary: Series B narrative review with Saru Patel, Lin Vega and Park Choi starts at 11:00. Your 1:1 with Mara follows at 14:00, and Lia's pickup is on Sam at 15:30. Walked the redline with Lin — two questions remain, option pool and the 1× non‑participating preferred. The panel hold expires today at 17:00, and a *run* window opens 12:30–13:30 under 16° sky.  
Tension: 72
```

## voice · qwen/qwen3.6-27b · 2026-04-25T18:19:44+03:00

_duration: 4m13.914s, prompt tokens: 2635, completion tokens: 3850_

```
Eyebrow: this morning · series b narrative, panel hold, run window
Title: *One* thing wants you before noon. Series B review at 11.
Summary: Saru Patel walked the redline with Lin. Two *questions* remain — option pool, and the 1× non-participating preferred. Acuity review at 11:00 with Lin and Park. Ashby confirmed three of four panelists for Owen Reid. Hold expires today at 17:00. Run window 12:30–13:30. Clear, 16°. Lia's pickup is on Sam at 15:30.
Tension: 65
```

## toolchain · qwen/qwen3.6-35b-a3b · 2026-04-25T18:37:11+03:00

_runs=20, success=20 (100.0%), avg=9.012s_

## schema · qwen/qwen3.6-35b-a3b · 2026-04-25T18:54:46+03:00

_runs=20, clean=20 (100.0%), repaired=0, failed=0_
