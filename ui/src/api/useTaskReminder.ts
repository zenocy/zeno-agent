import { useMutation } from "@tanstack/react-query";
import type { ActionResult } from "../types";

export interface TaskReminderArgs {
  uid: string;
  when: string; // RFC3339 OR +Nm/+Nh/+Nd
  body?: string;
}

export function useTaskReminder() {
  return useMutation<ActionResult, Error, TaskReminderArgs>({
    mutationFn: async ({ uid, when, body }) => {
      const r = await fetch(`/api/tasks/${encodeURIComponent(uid)}/reminder`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ when, body }),
      });
      if (!r.ok) {
        const errBody = await r.json().catch(() => ({}));
        throw new Error(errBody.error || `/api/tasks/${uid}/reminder returned ${r.status}`);
      }
      return r.json();
    },
  });
}
