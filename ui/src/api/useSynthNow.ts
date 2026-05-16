import { useMutation, useQueryClient } from "@tanstack/react-query";

interface SynthNowResponse {
  ok: boolean;
  duration_ms: number;
  error?: string;
}

// Triggers a manual morning synth (cards + briefing). On success the briefing
// and cards queries are invalidated so the UI refetches the new content.
//
// 409 means another synth (cron or manual) is already mid-flight; the caller
// should surface this as a non-error "already running" state.
export function useSynthNow() {
  const qc = useQueryClient();
  return useMutation<SynthNowResponse, Error, void>({
    mutationFn: async () => {
      const r = await fetch("/api/synth/now", { method: "POST" });
      const body = (await r.json().catch(() => ({}))) as SynthNowResponse;
      if (r.status === 409) {
        const err = new Error(body.error || "synth already in flight");
        (err as Error & { conflict?: boolean }).conflict = true;
        throw err;
      }
      if (!r.ok) {
        throw new Error(body.error || `/api/synth/now ${r.status}`);
      }
      return body;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["briefing"] });
      qc.invalidateQueries({ queryKey: ["cards"] });
    },
  });
}
