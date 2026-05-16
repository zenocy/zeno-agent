import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { MemoryFact } from "../types";

export interface MemoryAddArgs {
  subject: string;
  fact: string;
  category: string;
}

export class MemoryConflictError extends Error {
  constructor(public conflictId: string, public conflictSubject: string) {
    super(`subject already exists`);
    this.name = "MemoryConflictError";
  }
}

export function useMemoryAdd() {
  const qc = useQueryClient();
  return useMutation<MemoryFact, Error, MemoryAddArgs>({
    mutationFn: async (args) => {
      const r = await fetch("/api/memory", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(args),
      });
      if (r.status === 409) {
        const body = await r.json().catch(() => ({}));
        throw new MemoryConflictError(body.id ?? "", body.subject ?? args.subject);
      }
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `/api/memory returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["memory"] }),
  });
}
