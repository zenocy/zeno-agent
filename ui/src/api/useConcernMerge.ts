import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { Concern } from "../types";

export interface ConcernMergeArgs {
  id: string;
  into_id: string;
}

export function useConcernMerge() {
  const qc = useQueryClient();
  return useMutation<Concern, Error, ConcernMergeArgs>({
    mutationFn: async ({ id, into_id }) => {
      const r = await fetch(`/api/concerns/${id}/merge`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ into_id }),
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `/api/concerns/${id}/merge returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["concerns"] }),
  });
}
