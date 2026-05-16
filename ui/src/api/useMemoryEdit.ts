import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { MemoryFact } from "../types";

export interface MemoryEditArgs {
  id: string;
  fact?: string;
  category?: string;
}

export function useMemoryEdit() {
  const qc = useQueryClient();
  return useMutation<MemoryFact, Error, MemoryEditArgs>({
    mutationFn: async ({ id, ...patch }) => {
      const r = await fetch(`/api/memory/${id}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(patch),
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `/api/memory/${id} returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["memory"] }),
  });
}
