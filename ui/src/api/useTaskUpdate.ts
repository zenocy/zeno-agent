import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { ActionResult } from "../types";

export interface TaskUpdateArgs {
  uid: string;
  title?: string;
  // Pass an empty string to clear the due_date column. `undefined`
  // leaves the column untouched.
  due_date?: string;
}

export function useTaskUpdate() {
  const qc = useQueryClient();
  return useMutation<ActionResult, Error, TaskUpdateArgs>({
    mutationFn: async ({ uid, title, due_date }) => {
      const body: Record<string, unknown> = {};
      if (title !== undefined) body.title = title;
      if (due_date !== undefined) body.due_date = due_date;
      const r = await fetch(`/api/tasks/${uid}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error || `/api/tasks/${uid} returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["tasks"] });
    },
  });
}
