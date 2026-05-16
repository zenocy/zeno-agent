import { useQuery } from "@tanstack/react-query";

// Send is one row in the V2.13.2 Sends panel — the assistant-mode
// outbound activity log. Mirrors api.SendDTO.
export interface Send {
  id: string;
  sent_at: string;
  recipient_name: string;
  status: "awaiting_reply" | "replied" | "expired";
  event_title?: string;
  event_uid?: string;
  draft_body: string;
  resolved_at?: string;
  reply_body?: string;
}

interface SendsResponse {
  sends: Send[];
}

// useSends fetches /api/sends. Pass cardId to scope to a single card's
// anchored event (used by the CardFocus inline banner).
export function useSends(opts?: { cardId?: string }) {
  const cardId = opts?.cardId;
  return useQuery<Send[]>({
    queryKey: ["sends", cardId ?? "all"],
    queryFn: async () => {
      const url = cardId
        ? `/api/sends?card_id=${encodeURIComponent(cardId)}`
        : "/api/sends";
      const r = await fetch(url);
      if (!r.ok) throw new Error(`/api/sends returned ${r.status}`);
      const body = (await r.json()) as SendsResponse;
      return body.sends ?? [];
    },
    // Refresh every 60s so an open "awaiting" send flips to "replied"
    // on the panel without a manual reload after the inbound lands.
    refetchInterval: 60_000,
    staleTime: 30_000,
  });
}
