import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { Concern } from "../types";

export interface ConcernEditArgs {
  id: string;
  name?: string;
  description?: string;
}

export function useConcernEdit() {
  const qc = useQueryClient();
  return useMutation<Concern, Error, ConcernEditArgs>({
    mutationFn: async ({ id, ...patch }) => {
      const r = await fetch(`/api/concerns/${id}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(patch),
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `/api/concerns/${id} returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["concerns"] }),
  });
}
