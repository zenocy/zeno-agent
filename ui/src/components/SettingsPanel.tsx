import { useEffect, useId, useMemo, useState } from "react";
import clsx from "clsx";
import { useSettings } from "../api/useSettings";
import { useSettingsUpdate } from "../api/useSettingsUpdate";
import {
  useWhatsAppStatus,
  useWhatsAppConfigUpdate,
  useWhatsAppUnlink,
  type WhatsAppConfig,
} from "../api/useWhatsApp";
import { WhatsAppPairModal } from "./WhatsAppPairModal";

// Resolve the IANA timezone list once. Modern evergreen browsers expose
// `Intl.supportedValuesOf("timeZone")`; fall back to a minimal curated
// list if not (older Safari, some embedded WebViews).
function listTimezones(): string[] {
  const intl = Intl as unknown as { supportedValuesOf?: (key: string) => string[] };
  if (typeof intl.supportedValuesOf === "function") {
    try {
      return intl.supportedValuesOf("timeZone");
    } catch {
      // fall through
    }
  }
  return [
    "UTC",
    "Europe/London",
    "Europe/Berlin",
    "Europe/Athens",
    "America/New_York",
    "America/Chicago",
    "America/Denver",
    "America/Los_Angeles",
    "Asia/Tokyo",
    "Asia/Singapore",
    "Australia/Sydney",
  ];
}

export function SettingsPanel() {
  const { data, isLoading, isError } = useSettings();
  const update = useSettingsUpdate();
  const tzListId = useId();

  const [timezone, setTimezone] = useState("");
  const [city, setCity] = useState("");
  const [country, setCountry] = useState("");
  const [stockTickersRaw, setStockTickersRaw] = useState("");
  const [stockThreshold, setStockThreshold] = useState(0);
  const [stockAlwaysPoll, setStockAlwaysPoll] = useState(false);
  const [worldClocksRaw, setWorldClocksRaw] = useState("");
  const [savedFlash, setSavedFlash] = useState(false);

  const allTimezones = useMemo(listTimezones, []);

  // Hydrate the form from the server response on first load (and after
  // any server-side change like the legacy YAML auto-seed).
  useEffect(() => {
    if (!data) return;
    setTimezone(data.timezone);
    setCity(data.city);
    setCountry(data.country);
    // Render the CSV one-per-line for editing; we serialize back on submit.
    setStockTickersRaw(
      data.stock_tickers
        ? data.stock_tickers.split(",").join("\n")
        : ""
    );
    setStockThreshold(data.stock_threshold_pct ?? 0);
    setStockAlwaysPoll(data.stock_always_poll ?? false);
    setWorldClocksRaw(
      data.world_clocks ? data.world_clocks.split(",").join("\n") : ""
    );
  }, [data]);

  // Normalize ticker textarea input back to a CSV (uppercase, dedupe,
  // drop empties) — the same shape the backend expects.
  const normalizedTickersCsv = useMemo(() => {
    const seen = new Set<string>();
    const out: string[] = [];
    for (const raw of stockTickersRaw.split(/[\s,]+/)) {
      const t = raw.trim().toUpperCase();
      if (!t || seen.has(t)) continue;
      seen.add(t);
      out.push(t);
    }
    return out.join(",");
  }, [stockTickersRaw]);

  // World clocks: CSV of IANA tz strings. Case-preserving (IANA is
  // case-sensitive); the backend additionally drops entries that don't
  // load with time.LoadLocation, so invalid input is benign.
  const normalizedClocksCsv = useMemo(() => {
    const seen = new Set<string>();
    const out: string[] = [];
    for (const raw of worldClocksRaw.split(/[\s,]+/)) {
      const t = raw.trim();
      if (!t || seen.has(t)) continue;
      seen.add(t);
      out.push(t);
    }
    return out.join(",");
  }, [worldClocksRaw]);

  const dirty =
    !!data &&
    (timezone !== data.timezone ||
      city !== data.city ||
      country !== data.country ||
      normalizedTickersCsv !== (data.stock_tickers ?? "") ||
      stockThreshold !== (data.stock_threshold_pct ?? 0) ||
      stockAlwaysPoll !== (data.stock_always_poll ?? false) ||
      normalizedClocksCsv !== (data.world_clocks ?? ""));

  function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!dirty) return;
    update.mutate(
      {
        timezone: timezone.trim(),
        city: city.trim(),
        country: country.trim(),
        stock_tickers: normalizedTickersCsv,
        stock_threshold_pct: stockThreshold,
        stock_always_poll: stockAlwaysPoll,
        world_clocks: normalizedClocksCsv,
        // Persona fields are owned by the Profile → Assistant tab; the
        // Settings panel passes them through unchanged so a save here
        // doesn't clobber a name configured elsewhere.
        user_name: data?.user_name ?? "",
        assistant_name: data?.assistant_name ?? "",
        assistant_tone: data?.assistant_tone ?? "",
      },
      {
        onSuccess: () => {
          setSavedFlash(true);
          window.setTimeout(() => setSavedFlash(false), 2000);
        },
      }
    );
  }

  const errorMessage = update.isError ? update.error.message : null;
  const geocodeError = update.data?.geocode_error;

  return (
    <div className="px-8 pt-8 pb-40 max-w-2xl mx-auto">
      <header className="flex items-baseline justify-between mb-6">
        <h1 className="font-display font-[500] text-[22px] text-ink">Settings</h1>
        {data?.set === false && (
          <span className="font-mono text-[10px] uppercase tracking-wide text-ink-4">
            First-time setup
          </span>
        )}
      </header>

      {isLoading && (
        <div className="space-y-3">
          {[...Array(3)].map((_, i) => (
            <div
              key={i}
              className="h-12 rounded-z-sm border border-line bg-bg-card opacity-50"
            />
          ))}
        </div>
      )}

      {isError && (
        <p className="font-mono text-[11px] text-crit py-4">
          Couldn't load settings. Try refreshing.
        </p>
      )}

      {!isLoading && data && (
        <form
          onSubmit={submit}
          className="rounded-z-md border border-line bg-bg-card p-5 space-y-4"
        >
          <section>
            <h2 className="font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-2">
              Location
            </h2>
            <p className="text-[12px] text-ink-5 mb-3">
              Used by the weather sensor and any feature that needs to know
              where you are.
            </p>
            <div className="flex gap-3">
              <div className="flex-1 min-w-0">
                <label
                  htmlFor="settings-city"
                  className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1"
                >
                  City
                </label>
                <input
                  id="settings-city"
                  type="text"
                  value={city}
                  onChange={(e) => setCity(e.target.value)}
                  placeholder="Athens"
                  autoComplete="address-level2"
                  className="w-full h-9 px-2 rounded-z-sm bg-bg-elev border border-line text-[13px] text-ink placeholder:text-ink-5 focus:outline-none focus:ring-1 focus:ring-line"
                />
              </div>
              <div className="flex-1 min-w-0">
                <label
                  htmlFor="settings-country"
                  className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1"
                >
                  Country
                </label>
                <input
                  id="settings-country"
                  type="text"
                  value={country}
                  onChange={(e) => setCountry(e.target.value)}
                  placeholder="Greece"
                  autoComplete="country-name"
                  className="w-full h-9 px-2 rounded-z-sm bg-bg-elev border border-line text-[13px] text-ink placeholder:text-ink-5 focus:outline-none focus:ring-1 focus:ring-line"
                />
              </div>
            </div>
            {data.set && (data.latitude !== 0 || data.longitude !== 0) && (
              <p className="font-mono text-[10px] text-ink-5 mt-2">
                Resolved to {data.latitude.toFixed(4)}, {data.longitude.toFixed(4)}
              </p>
            )}
          </section>

          <section>
            <h2 className="font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-2">
              Timezone
            </h2>
            <label
              htmlFor="settings-tz"
              className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1"
            >
              IANA timezone
            </label>
            <input
              id="settings-tz"
              type="text"
              list={tzListId}
              value={timezone}
              onChange={(e) => setTimezone(e.target.value)}
              placeholder="Europe/Athens"
              spellCheck={false}
              autoComplete="off"
              className="w-full h-9 px-2 rounded-z-sm bg-bg-elev border border-line font-mono text-[13px] text-ink placeholder:text-ink-5 focus:outline-none focus:ring-1 focus:ring-line"
            />
            <datalist id={tzListId}>
              {allTimezones.map((tz) => (
                <option key={tz} value={tz} />
              ))}
            </datalist>
          </section>

          <section>
            <h2 className="font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-2">
              Stocks
            </h2>
            <p className="text-[12px] text-ink-5 mb-3">
              Tickers to watch and a percent threshold for alert events.
              Leave the list empty to disable. Threshold of 0 disables
              alerting; the widget still shows prices.
            </p>
            <label
              htmlFor="settings-tickers"
              className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1"
            >
              Tickers (one per line or comma-separated)
            </label>
            <textarea
              id="settings-tickers"
              value={stockTickersRaw}
              onChange={(e) => setStockTickersRaw(e.target.value)}
              placeholder={"AAPL\nGOOGL\nMSFT"}
              rows={3}
              spellCheck={false}
              className="w-full px-2 py-2 rounded-z-sm bg-bg-elev border border-line font-mono text-[13px] text-ink placeholder:text-ink-5 focus:outline-none focus:ring-1 focus:ring-line"
            />
            <label
              htmlFor="settings-stock-threshold"
              className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mt-3 mb-1"
            >
              Alert threshold (% absolute day move)
            </label>
            <input
              id="settings-stock-threshold"
              type="number"
              min={0}
              max={50}
              step={0.1}
              value={stockThreshold}
              onChange={(e) =>
                setStockThreshold(parseFloat(e.target.value) || 0)
              }
              className="w-32 h-9 px-2 rounded-z-sm bg-bg-elev border border-line font-mono text-[13px] text-ink focus:outline-none focus:ring-1 focus:ring-line"
            />
            <label className="flex items-start gap-2 mt-3 text-[12px] text-ink-3 cursor-pointer">
              <input
                type="checkbox"
                checked={stockAlwaysPoll}
                onChange={(e) => setStockAlwaysPoll(e.target.checked)}
                className="mt-0.5 cursor-pointer"
              />
              <span>
                Always poll
                <span className="block font-mono text-[10px] text-ink-5">
                  Default off: skip syncs outside Mon–Fri 13:00–21:00 UTC
                  (US market hours). Turn on for non-US watchlists.
                </span>
              </span>
            </label>
          </section>

          <section>
            <h2 className="font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-2">
              World clocks
            </h2>
            <p className="text-[12px] text-ink-5 mb-3">
              IANA timezones rendered by the World clocks widget. One per
              line or comma-separated. Invalid entries are dropped on save.
            </p>
            <label
              htmlFor="settings-world-clocks"
              className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1"
            >
              Timezones (e.g. America/Los_Angeles)
            </label>
            <textarea
              id="settings-world-clocks"
              value={worldClocksRaw}
              onChange={(e) => setWorldClocksRaw(e.target.value)}
              placeholder={"America/Los_Angeles\nEurope/London\nAsia/Kolkata"}
              rows={3}
              spellCheck={false}
              autoComplete="off"
              className="w-full px-2 py-2 rounded-z-sm bg-bg-elev border border-line font-mono text-[13px] text-ink placeholder:text-ink-5 focus:outline-none focus:ring-1 focus:ring-line"
            />
          </section>

          <div className="flex items-center justify-between pt-2 border-t border-line">
            <div className="font-mono text-[11px]">
              {errorMessage && <span className="text-crit">{errorMessage}</span>}
              {!errorMessage && geocodeError && (
                <span className="text-warn">Saved, but {geocodeError}.</span>
              )}
              {!errorMessage && !geocodeError && savedFlash && (
                <span className="text-good">Saved.</span>
              )}
            </div>
            <button
              type="submit"
              disabled={!dirty || update.isPending}
              className={clsx(
                "h-[28px] px-3 rounded-z-sm text-[12px] font-[500] border transition",
                !dirty || update.isPending
                  ? "border-line text-ink-5 opacity-50 cursor-not-allowed"
                  : "bg-accent text-white border-transparent hover:opacity-90"
              )}
            >
              {update.isPending ? "Saving…" : "Save changes"}
            </button>
          </div>
        </form>
      )}

      <WhatsAppSection />
    </div>
  );
}

function WhatsAppSection() {
  const { data, isLoading } = useWhatsAppStatus();
  const update = useWhatsAppConfigUpdate();
  const unlink = useWhatsAppUnlink();
  const [showPair, setShowPair] = useState(false);

  const [mention, setMention] = useState("zeno");
  const [allowedDmsRaw, setAllowedDmsRaw] = useState("");
  const [interval, setIntervalMs] = useState(3000);
  const [savedFlash, setSavedFlash] = useState(false);

  useEffect(() => {
    if (!data?.config) return;
    setMention(data.config.mention_name);
    // The Go API serializes an empty slice as `null`, so guard the join.
    setAllowedDmsRaw((data.config.allowed_dms ?? []).join("\n"));
    setIntervalMs(data.config.min_chat_interval_ms);
  }, [data]);

  if (isLoading || !data?.config) return null;

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const cfg: WhatsAppConfig = {
      mention_name: mention.trim(),
      allowed_dms: allowedDmsRaw
        .split(/\s+/)
        .map((s) => s.trim())
        .filter(Boolean),
      min_chat_interval_ms: interval,
      max_concurrent_synth: data.config.max_concurrent_synth,
      per_chat_buffer: data.config.per_chat_buffer,
    };
    update.mutate(cfg, {
      onSuccess: () => {
        setSavedFlash(true);
        window.setTimeout(() => setSavedFlash(false), 2000);
      },
    });
  };

  const errorMessage = update.isError ? update.error.message : null;
  const sessionLabel = data.has_session
    ? data.own_push_name
      ? `${data.own_push_name} (${data.own_jid})`
      : data.own_jid ?? "linked"
    : "not linked";

  return (
    <section className="mt-6 rounded-z-md border border-line bg-bg-card p-5 space-y-4">
      <header>
        <h2 className="font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-1">
          WhatsApp
        </h2>
        <p className="text-[12px] text-ink-5">
          Reach Zeno from your phone. DMs from your number land here; in groups
          Zeno responds when @-mentioned.
        </p>
      </header>

      {!data.enabled && (
        <p className="font-mono text-[11px] text-warn">
          Disabled in config. Set <code>sensors.whatsapp.enabled: true</code> in
          your config.yaml and restart to enable.
        </p>
      )}

      <div className="font-mono text-[11px] flex items-center gap-2">
        <span
          className={clsx(
            "inline-block w-2 h-2 rounded-full",
            data.connected ? "bg-good" : data.has_session ? "bg-warn" : "bg-ink-5"
          )}
        />
        <span className="text-ink">{sessionLabel}</span>
        {data.last_error && (
          <span className="text-crit">· {data.last_error}</span>
        )}
      </div>

      {data.enabled && !data.has_session && (
        <button
          type="button"
          onClick={() => setShowPair(true)}
          className="h-[28px] px-3 rounded-z-sm text-[12px] font-[500] bg-accent text-white border-transparent hover:opacity-90"
        >
          Link WhatsApp
        </button>
      )}

      {data.has_session && (
        <form onSubmit={submit} className="space-y-3 pt-3 border-t border-line">
          <div>
            <label className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1">
              Mention name
            </label>
            <input
              type="text"
              value={mention}
              onChange={(e) => setMention(e.target.value)}
              placeholder="zeno"
              className="w-full h-9 px-2 rounded-z-sm bg-bg-elev border border-line font-mono text-[13px] text-ink placeholder:text-ink-5 focus:outline-none focus:ring-1 focus:ring-line"
            />
            <p className="font-mono text-[10px] text-ink-5 mt-1">
              In a group, Zeno responds when someone writes @{mention || "zeno"}.
            </p>
          </div>

          <div>
            <label className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1">
              Allowed DM senders (one JID per line)
            </label>
            <textarea
              value={allowedDmsRaw}
              onChange={(e) => setAllowedDmsRaw(e.target.value)}
              placeholder={"e.g. " + (data.own_jid ?? "12345@s.whatsapp.net")}
              rows={3}
              className="w-full px-2 py-2 rounded-z-sm bg-bg-elev border border-line font-mono text-[12px] text-ink placeholder:text-ink-5 focus:outline-none focus:ring-1 focus:ring-line"
            />
            <p className="font-mono text-[10px] text-ink-5 mt-1">
              DMs from senders not on this list are dropped silently. Group
              mentions bypass this list.
            </p>
          </div>

          <div>
            <label className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1">
              Min reply gap per chat (ms)
            </label>
            <input
              type="number"
              min={1000}
              step={500}
              value={interval}
              onChange={(e) => setIntervalMs(parseInt(e.target.value, 10) || 3000)}
              className="w-32 h-9 px-2 rounded-z-sm bg-bg-elev border border-line font-mono text-[13px] text-ink focus:outline-none focus:ring-1 focus:ring-line"
            />
          </div>

          <div className="flex items-center justify-between pt-2 border-t border-line">
            <div className="font-mono text-[11px]">
              {errorMessage && <span className="text-crit">{errorMessage}</span>}
              {!errorMessage && savedFlash && (
                <span className="text-good">Saved.</span>
              )}
            </div>
            <div className="flex gap-2">
              <button
                type="button"
                onClick={() => unlink.mutate()}
                disabled={unlink.isPending}
                className="h-[28px] px-3 rounded-z-sm text-[12px] font-[500] border border-line text-ink-5 hover:text-crit"
              >
                Unlink
              </button>
              <button
                type="submit"
                disabled={update.isPending}
                className={clsx(
                  "h-[28px] px-3 rounded-z-sm text-[12px] font-[500] border transition",
                  update.isPending
                    ? "border-line text-ink-5 opacity-50 cursor-not-allowed"
                    : "bg-accent text-white border-transparent hover:opacity-90"
                )}
              >
                {update.isPending ? "Saving…" : "Save"}
              </button>
            </div>
          </div>
        </form>
      )}

      {showPair && (
        <WhatsAppPairModal
          onClose={() => setShowPair(false)}
          onPaired={() => setShowPair(false)}
        />
      )}
    </section>
  );
}
