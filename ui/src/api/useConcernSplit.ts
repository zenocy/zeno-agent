import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { ConcernSplitInput, ConcernSplitResponse } from "../types";

export interface ConcernSplitArgs {
  id: string;
  splits: ConcernSplitInput[];
}

export function useConcernSplit() {
  const qc = useQueryClient();
  return useMutation<ConcernSplitResponse, Error, ConcernSplitArgs>({
    mutationFn: async ({ id, splits }) => {
      const r = await fetch(`/api/concerns/${id}/split`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ splits }),
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `/api/concerns/${id}/split returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["concerns"] }),
  });
}
