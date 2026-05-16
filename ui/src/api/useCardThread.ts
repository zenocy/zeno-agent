import { useQuery } from "@tanstack/react-query";
import type { ConversationThread } from "../types";

// useCardThread loads the persisted conversation for a card. Disabled
// when cardId is null so the query only fires while the CardFocus
// modal is open.
export function useCardThread(cardId: string | null) {
  return useQuery<ConversationThread>({
    queryKey: ["cardThread", cardId],
    queryFn: async () => {
      const r = await fetch(`/api/cards/${cardId}/thread`);
      if (!r.ok) throw new Error(`/api/cards/${cardId}/thread returned ${r.status}`);
      return r.json();
    },
    enabled: !!cardId,
    staleTime: 30 * 1000,
  });
}
