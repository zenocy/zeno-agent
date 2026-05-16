import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { Settings } from "./useSettings";

export interface SettingsUpdateArgs {
  timezone: string;
  city: string;
  country: string;
  stock_tickers: string;
  stock_threshold_pct: number;
  stock_always_poll: boolean;
  world_clocks: string;
  user_name: string;
  assistant_name: string;
  assistant_tone: string;
}

export function useSettingsUpdate() {
  const qc = useQueryClient();
  return useMutation<Settings, Error, SettingsUpdateArgs>({
    mutationFn: async (args) => {
      const r = await fetch("/api/settings", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(args),
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.error ?? `/api/settings returned ${r.status}`);
      }
      return r.json();
    },
    onSuccess: (resp) => {
      qc.setQueryData(["settings"], resp);
      qc.invalidateQueries({ queryKey: ["settings"] });
      // The backend's AfterSave hook kicks off a background SyncAll so
      // dependent widgets reflect the new settings. Hitting upstreams
      // takes ~1s; refetch the dependent projections once after a small
      // delay so the UI lands on fresh data without flicker.
      window.setTimeout(() => {
        qc.invalidateQueries({ queryKey: ["weather-snapshot"] });
        qc.invalidateQueries({ queryKey: ["run-window"] });
        qc.invalidateQueries({ queryKey: ["stock-snapshot"] });
      }, 2500);
    },
  });
}
