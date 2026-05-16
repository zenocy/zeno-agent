import { useMutation } from "@tanstack/react-query";
import type { Card } from "../types";

interface AskResponse {
  card: Card;
  trace_id: string;
}

// useAsk fires POST /api/ask. The response body still carries the
// generated card for back-compat, but V2.4 P3 ignores it client-side:
// the card lands in the cards cache via SSE (`card.appended` with
// `origin: "ask"`) routed by `useTodayStream`. That single delivery
// path keeps the InputBar's "submit then live-trace then card"
// sequence coherent.
export function useAsk() {
  return useMutation<AskResponse, Error, string>({
    mutationFn: async (query) => {
      const r = await fetch("/api/ask", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ query }),
      });
      if (!r.ok) throw new Error(`/api/ask ${r.status}`);
      return r.json();
    },
  });
}
