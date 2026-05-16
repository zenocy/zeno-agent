import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { ConversationThread, ConversationTurn } from "../types";

// useCardConverse posts a new query to a card's conversation thread.
// On success the returned turn is appended to the cached thread so the
// modal renders the new SubCard without a refetch round-trip.
export function useCardConverse(cardId: string) {
  const qc = useQueryClient();
  return useMutation<ConversationTurn, Error, string>({
    mutationFn: async (query) => {
      const r = await fetch(`/api/cards/${cardId}/converse`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ query }),
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `/api/cards/${cardId}/converse returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: (turn) => {
      qc.setQueryData<ConversationThread>(["cardThread", cardId], (old) => {
        if (!old) return old;
        return { ...old, turns: [...old.turns, turn] };
      });
    },
  });
}
