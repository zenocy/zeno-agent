import { useMutation, useQueryClient } from "@tanstack/react-query";

export interface MemoryDeleteArgs {
  id: string;
}

// useMemoryDelete fires DELETE /api/memory/:id and invalidates the cache.
// The undo affordance (delete-with-5s-grace) is owned by the panel/component
// — the panel keeps the row hidden locally, then calls this hook only when
// the timer expires. Cancelling the timer never reaches the server.
export function useMemoryDelete() {
  const qc = useQueryClient();
  return useMutation<void, Error, MemoryDeleteArgs>({
    mutationFn: async ({ id }) => {
      const r = await fetch(`/api/memory/${id}`, { method: "DELETE" });
      // Server is idempotent: 204 even if already deleted. Treat 4xx as
      // user-correctable but don't crash the panel — the cache invalidation
      // refetches and reconciles.
      if (!r.ok && r.status !== 404) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `/api/memory/${id} returned ${r.status}`);
      }
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["memory"] }),
  });
}
