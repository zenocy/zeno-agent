import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { ActionResult } from "../types";

export interface ActionArgs {
  id: string;
  intent: string;
  target?: Record<string, unknown>;
  confirm?: boolean;
}

// useAction posts the V2.8 action request shape and returns the parsed
// ActionResult. The pre-V2.8 wire (`{action: <label>}`) still works as
// a server-side legacy fallback; new callers should pass `intent`.
//
// The mutation does not auto-invalidate the cards query — Card.tsx
// optimistically hides the card on dismiss/snooze before the request
// fires, and other intents (add_concern, add_memory, ask_followup) do
// not change the cards list. Cards is invalidated only when the result
// indicates a state change worth re-fetching.
export function useAction() {
  const qc = useQueryClient();
  return useMutation<ActionResult, Error, ActionArgs>({
    mutationFn: async ({ id, intent, target, confirm }) => {
      const r = await fetch(`/api/cards/${id}/action`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ intent, target, confirm }),
      });
      if (!r.ok) throw new Error(`/api/cards/${id}/action ${r.status}`);
      // Tolerate the legacy 204 No Content response in case a stale
      // backend is on the other end of a hot reload.
      if (r.status === 204) return { ok: true } as ActionResult;
      const body = (await r.json()) as ActionResult;
      return body;
    },
    onSuccess: (result) => {
      if (result?.hide) {
        // Cards mutated server-side (dismiss/snooze) — refetch the list
        // so the optimistic hide aligns with the source of truth.
        qc.invalidateQueries({ queryKey: ["cards"] });
      }
    },
  });
}
