import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

// WhatsAppConfig mirrors internal/http/api/whatsapp.configDTO.
export interface WhatsAppConfig {
  mention_name: string;
  allowed_dms: string[];
  min_chat_interval_ms: number;
  max_concurrent_synth: number;
  per_chat_buffer: number;
}

// WhatsAppStatus mirrors internal/http/api/whatsapp.statusDTO.
export interface WhatsAppStatus {
  enabled: boolean;
  has_session: boolean;
  connected: boolean;
  logged_in: boolean;
  own_jid?: string;
  own_push_name?: string;
  last_error?: string;
  last_seen_at?: string;
  paired_at?: string;
  config: WhatsAppConfig;
}

export function useWhatsAppStatus() {
  return useQuery<WhatsAppStatus>({
    queryKey: ["whatsapp-status"],
    queryFn: async () => {
      const r = await fetch("/api/whatsapp/status");
      if (!r.ok) throw new Error(`/api/whatsapp/status returned ${r.status}`);
      return r.json();
    },
    // SSE-driven: useTodayStream forwards whatsapp.status_changed events
    // from the eventbus and writes them straight into this cache via
    // setQueryData. Initial fetch on mount remains for first paint;
    // afterwards the cache is event-driven, so polling is off and
    // staleness is treated as fresh until the next SSE update arrives.
    refetchInterval: false,
    staleTime: Infinity,
  });
}

export function useWhatsAppConfigUpdate() {
  const qc = useQueryClient();
  return useMutation<WhatsAppConfig, Error, WhatsAppConfig>({
    mutationFn: async (cfg) => {
      const r = await fetch("/api/whatsapp/config", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(cfg),
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `/api/whatsapp/config returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["whatsapp-status"] });
    },
  });
}

export function useWhatsAppUnlink() {
  const qc = useQueryClient();
  return useMutation<void, Error>({
    mutationFn: async () => {
      const r = await fetch("/api/whatsapp/unlink", { method: "POST" });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `/api/whatsapp/unlink returned ${r.status}`);
      }
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["whatsapp-status"] });
    },
  });
}

// PairFrame is one decoded SSE event from /api/whatsapp/pair.
export interface PairFrame {
  event: "code" | "success" | "timeout" | "error" | "closed" | string;
  code?: string;
  error?: string;
}

// startPair opens the SSE stream and invokes onFrame for every event
// it sees. Returns a cancel function the caller invokes on unmount or
// when the user dismisses the modal. The stream closes when the server
// emits a terminal frame; the AbortController lets the client abort
// early.
export function startPair(
  onFrame: (frame: PairFrame) => void,
  onError?: (err: Error) => void,
): () => void {
  const ctrl = new AbortController();
  (async () => {
    try {
      // The server pre-empts any in-flight pair when a new request
      // arrives, so concurrent calls (React StrictMode dev double-mount,
      // multiple browser tabs) all eventually receive their own stream
      // — no 409 retry dance needed on the client.
      const r = await fetch("/api/whatsapp/pair", {
        method: "POST",
        signal: ctrl.signal,
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `pair returned ${r.status}`);
      }
      if (!r.body) throw new Error("no response body for SSE");
      const reader = r.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let idx;
        while ((idx = buffer.indexOf("\n\n")) >= 0) {
          const block = buffer.slice(0, idx);
          buffer = buffer.slice(idx + 2);
          const frame = parseSSEBlock(block);
          if (frame) onFrame(frame);
        }
      }
    } catch (e) {
      if (ctrl.signal.aborted) return;
      onError?.(e instanceof Error ? e : new Error(String(e)));
    }
  })();
  return () => ctrl.abort();
}

function parseSSEBlock(block: string): PairFrame | null {
  let event = "";
  let data = "";
  for (const line of block.split("\n")) {
    if (line.startsWith("event:")) event = line.slice(6).trim();
    else if (line.startsWith("data:")) data = line.slice(5).trim();
  }
  if (!event) return null;
  let payload: Partial<PairFrame> = {};
  if (data) {
    try {
      payload = JSON.parse(data);
    } catch {
      // ignore malformed frame
    }
  }
  return { event, ...payload };
}
