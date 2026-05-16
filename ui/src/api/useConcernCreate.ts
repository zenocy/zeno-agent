import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { Concern, ConcernSource } from "../types";

export interface ConcernCreateArgs {
  name: string;
  description: string;
  source?: ConcernSource;
}

export class ConcernConflictError extends Error {
  constructor(public conflictId: string, public conflictName: string) {
    super(`name already exists`);
    this.name = "ConcernConflictError";
  }
}

export function useConcernCreate() {
  const qc = useQueryClient();
  return useMutation<Concern, Error, ConcernCreateArgs>({
    mutationFn: async (args) => {
      const r = await fetch("/api/concerns", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ source: "user", ...args }),
      });
      if (r.status === 409) {
        const body = await r.json().catch(() => ({}));
        throw new ConcernConflictError(body.id ?? "", body.name ?? args.name);
      }
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `/api/concerns returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["concerns"] }),
  });
}
