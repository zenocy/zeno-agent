import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { ActionResult } from "../types";

export function useTaskDelete() {
  const qc = useQueryClient();
  return useMutation<ActionResult, Error, string>({
    mutationFn: async (uid) => {
      const r = await fetch(`/api/tasks/${encodeURIComponent(uid)}`, {
        method: "DELETE",
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error || `DELETE /api/tasks/${uid} returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["tasks"] });
    },
  });
}
