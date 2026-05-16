// useLiveSynth — derives the live-trace panel's render state from the
// broker's event stream. Subscribes once on mount; cleans up on unmount.
//
// State machine:
//   idle (active=false, dissolving=false)
//     └ synth.started → active=true, runId set
//   active (streaming)
//     ├ trace.step → append to steps
//     ├ synth.delta → append to body
//     └ synth.completed → active=false, dissolving=true
//   dissolving (panel fading out)
//     └ after 600ms → reset to idle
//
// Stale events (different run_id than the current one) are dropped on
// the floor — the panel shows whichever run is most recent.

import { useEffect, useRef, useState } from "react";

import type { TraceStep } from "../types";
import { subscribeLive, type LiveEvent } from "./liveBroker";

export interface LiveSynthState {
  active: boolean;
  dissolving: boolean;
  runId: string | null;
  stage: string | null;
  steps: TraceStep[];
  body: string;
}

const initialState: LiveSynthState = {
  active: false,
  dissolving: false,
  runId: null,
  stage: null,
  steps: [],
  body: "",
};

// DissolveDelayMs matches the prototype's settle delay and the
// LiveSynthPanel.css opacity transition. Pulled out so tests can read
// the same constant.
export const DissolveDelayMs = 600;

export function useLiveSynth(): LiveSynthState {
  const [state, setState] = useState<LiveSynthState>(initialState);

  // Latest runId is mirrored into a ref so the listener (which closes
  // over the initial render's state) can filter stale events without
  // re-subscribing on every render.
  const runIdRef = useRef<string | null>(null);
  const dissolveTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    const cancelTimer = () => {
      if (dissolveTimerRef.current !== null) {
        clearTimeout(dissolveTimerRef.current);
        dissolveTimerRef.current = null;
      }
    };

    const unsub = subscribeLive((ev: LiveEvent) => {
      switch (ev.kind) {
        case "synth.started": {
          // A new run resets everything — including any in-flight
          // dissolve timer from a previous run.
          cancelTimer();
          runIdRef.current = ev.run_id;
          setState({
            active: true,
            dissolving: false,
            runId: ev.run_id,
            stage: ev.stage,
            steps: [],
            body: "",
          });
          return;
        }
        case "trace.step": {
          if (ev.run_id !== runIdRef.current) return;
          setState((s) => ({ ...s, steps: [...s.steps, ev.step] }));
          return;
        }
        case "synth.delta": {
          if (ev.run_id !== runIdRef.current) return;
          setState((s) => ({ ...s, body: s.body + ev.delta }));
          return;
        }
        case "synth.completed": {
          if (ev.run_id !== runIdRef.current) return;
          // Move to dissolving state immediately; schedule a hard
          // reset after the dissolve animation completes.
          setState((s) => ({ ...s, active: false, dissolving: true }));
          cancelTimer();
          dissolveTimerRef.current = setTimeout(() => {
            // Only reset if we're still on the same run (a new
            // synth.started during the dissolve already reset us).
            if (runIdRef.current === ev.run_id) {
              runIdRef.current = null;
              setState(initialState);
            }
            dissolveTimerRef.current = null;
          }, DissolveDelayMs);
          return;
        }
      }
    });

    return () => {
      unsub();
      cancelTimer();
    };
  }, []);

  return state;
}
