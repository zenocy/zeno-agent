import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { Concern, ConcernApproveResponse } from "../types";

export type ConcernAction = "approve" | "dismiss" | "pause" | "resume" | "end";

export interface ConcernActionArgs {
  id: string;
  action: ConcernAction;
}

export class ConcernStateError extends Error {
  constructor(message: string, public status: number) {
    super(message);
    this.name = "ConcernStateError";
  }
}

// useConcernAction multiplexes the lifecycle mutations onto one hook so
// callers can render a row of action buttons without juggling five
// separate mutation states. Approve returns the dispatcher hint
// (retrospective_dispatched) which the caller surfaces to the user;
// every other action returns nothing useful — invalidation is the
// reconciler.
export function useConcernAction() {
  const qc = useQueryClient();
  return useMutation<Concern | ConcernApproveResponse | null, Error, ConcernActionArgs>({
    mutationFn: async ({ id, action }) => {
      let url: string;
      let body: string | null = null;
      switch (action) {
        case "approve":
          url = `/api/concerns/${id}/approve`;
          break;
        case "dismiss":
          url = `/api/concerns/${id}/dismiss`;
          break;
        case "pause":
          url = `/api/concerns/${id}/state`;
          body = JSON.stringify({ state: "paused" });
          break;
        case "resume":
          url = `/api/concerns/${id}/state`;
          body = JSON.stringify({ state: "active" });
          break;
        case "end":
          url = `/api/concerns/${id}/state`;
          body = JSON.stringify({ state: "ended" });
          break;
      }
      const r = await fetch(url, {
        method: "POST",
        headers: body ? { "Content-Type": "application/json" } : undefined,
        body: body ?? undefined,
      });
      if (r.status === 409) {
        const errBody = await r.json().catch(() => ({}));
        throw new ConcernStateError(
          errBody.error ?? "lifecycle precondition failed; refresh and retry",
          409
        );
      }
      if (!r.ok) {
        const errBody = await r.json().catch(() => ({}));
        throw new Error(errBody.error ?? `${url} returned ${r.status}`);
      }
      // Dismiss returns { dismissed, id, name } not a Concern; everything
      // else returns either Concern or { concern, retrospective_dispatched }.
      // Caller cares about the response only for approve.
      if (action === "dismiss") {
        return null;
      }
      return r.json();
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["concerns"] }),
  });
}
