import { useQuery } from "@tanstack/react-query";
import type { IntentEntry, IntentModesResponse, ActionMode } from "../types";

// useIntentModes fetches the V2.8 action catalog (/api/actions/modes)
// once at app boot. The response is small (16 entries, fixed at
// compile time on the server) and stable for the lifetime of the
// process, so staleTime is Infinity — only a remount triggers a refetch.
export function useIntentModes() {
  return useQuery<IntentEntry[]>({
    queryKey: ["intent-modes"],
    queryFn: async () => {
      const r = await fetch("/api/actions/modes");
      if (!r.ok) throw new Error(`/api/actions/modes ${r.status}`);
      const body = (await r.json()) as IntentModesResponse;
      return body.intents;
    },
    staleTime: Infinity,
    gcTime: Infinity,
  });
}

// modeFor returns the policy mode for an intent. Falls back to
// "one_click" when the catalog hasn't loaded yet so a click before
// hydration still gets a sensible default — the server makes the
// final policy decision regardless.
export function modeFor(intents: IntentEntry[] | undefined, intent: string): ActionMode {
  if (!intents) return "one_click";
  const found = intents.find((i) => i.intent === intent);
  return found?.mode ?? "one_click";
}
