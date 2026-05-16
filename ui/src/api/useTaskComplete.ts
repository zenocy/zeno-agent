import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { ActionResult } from "../types";

export function useTaskComplete() {
  const qc = useQueryClient();
  return useMutation<ActionResult, Error, string>({
    mutationFn: async (uid) => {
      const r = await fetch(`/api/tasks/${encodeURIComponent(uid)}/complete`, {
        method: "POST",
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error || `/api/tasks/${uid}/complete returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["tasks"] });
    },
  });
}
