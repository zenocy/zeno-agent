import { useQuery } from "@tanstack/react-query";

export interface ThreadPreview {
  subject: string;
  from: string;
  date: string;
  body: string;
}

// useThreadPreview fetches the verbatim body preview of the most recent
// email thread whose subject contains `hint`. Used by the SubCard
// `document` kind's "view original" toggle. Disabled by default — the
// caller flips `enabled` on when the user actually opens the toggle, so
// the request fires only on demand.
export function useThreadPreview(hint: string | undefined, enabled: boolean) {
  return useQuery<ThreadPreview>({
    queryKey: ["threadPreview", hint],
    queryFn: async () => {
      const r = await fetch(`/api/threads/preview?hint=${encodeURIComponent(hint ?? "")}`);
      if (!r.ok) throw new Error(`/api/threads/preview returned ${r.status}`);
      return r.json();
    },
    enabled: enabled && !!hint,
    staleTime: 5 * 60 * 1000,
    retry: false,
  });
}
