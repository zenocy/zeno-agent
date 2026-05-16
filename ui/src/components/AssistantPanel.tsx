import { useEffect, useState } from "react";
import clsx from "clsx";

import { useSettings } from "../api/useSettings";
import { useSettingsUpdate } from "../api/useSettingsUpdate";

const NAME_MAX = 32;
const TONE_MAX = 80;

// AssistantPanel lets the user configure Zeno's assistant persona — the
// named EA Zeno speaks as in proactive WhatsApp drafts. Empty assistant
// name disables the feature; drafts then revert to the legacy
// first-person register ("on behalf of the user").
export function AssistantPanel() {
  const { data, isLoading, isError } = useSettings();
  const update = useSettingsUpdate();

  const [userName, setUserName] = useState("");
  const [assistantName, setAssistantName] = useState("");
  const [assistantTone, setAssistantTone] = useState("");
  const [savedFlash, setSavedFlash] = useState(false);

  useEffect(() => {
    if (!data) return;
    setUserName(data.user_name ?? "");
    setAssistantName(data.assistant_name ?? "");
    setAssistantTone(data.assistant_tone ?? "");
  }, [data]);

  const dirty =
    !!data &&
    (userName !== (data.user_name ?? "") ||
      assistantName !== (data.assistant_name ?? "") ||
      assistantTone !== (data.assistant_tone ?? ""));

  function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!dirty || !data) return;
    update.mutate(
      {
        timezone: data.timezone,
        city: data.city,
        country: data.country,
        stock_tickers: data.stock_tickers,
        stock_threshold_pct: data.stock_threshold_pct,
        stock_always_poll: data.stock_always_poll,
        world_clocks: data.world_clocks,
        user_name: userName.trim(),
        assistant_name: assistantName.trim(),
        assistant_tone: assistantTone.trim(),
      },
      {
        onSuccess: () => {
          setSavedFlash(true);
          window.setTimeout(() => setSavedFlash(false), 1500);
        },
      }
    );
  }

  if (isLoading) {
    return (
      <div className="px-8 py-6 max-w-2xl mx-auto text-ink-4 text-sm">Loading…</div>
    );
  }
  if (isError) {
    return (
      <div className="px-8 py-6 max-w-2xl mx-auto text-rose-400 text-sm">
        Could not load settings.
      </div>
    );
  }

  const enabled = assistantName.trim() !== "";

  return (
    <form onSubmit={submit} className="px-8 py-6 max-w-2xl mx-auto space-y-6">
      <header>
        <h2 className="text-ink text-base font-medium">Assistant</h2>
        <p className="text-ink-4 text-[13px] mt-1 leading-relaxed">
          When Zeno drafts a proactive WhatsApp message — confirming a dinner,
          declining an invite — it can speak as a named assistant on your
          behalf, in third person. Leave the name blank to keep drafts in your
          first-person voice.
        </p>
      </header>

      <Field label="Your name" hint="The principal Zeno is the assistant for.">
        <input
          type="text"
          value={userName}
          onChange={(e) => setUserName(e.target.value.slice(0, NAME_MAX))}
          placeholder="e.g. Jamie"
          maxLength={NAME_MAX}
          className="zeno-input"
        />
      </Field>

      <Field
        label="Assistant name"
        hint='The name the assistant introduces themselves as. Empty disables assistant mode.'
      >
        <input
          type="text"
          value={assistantName}
          onChange={(e) => setAssistantName(e.target.value.slice(0, NAME_MAX))}
          placeholder="e.g. Aria"
          maxLength={NAME_MAX}
          className="zeno-input"
        />
      </Field>

      <Field
        label="Tone (optional)"
        hint="One short line; refines voice but the canon (no exclamations, calm-literary) wins on conflict."
      >
        <input
          type="text"
          value={assistantTone}
          onChange={(e) => setAssistantTone(e.target.value.slice(0, TONE_MAX))}
          placeholder="warm but brisk"
          maxLength={TONE_MAX}
          className="zeno-input"
        />
      </Field>

      <div
        className={clsx(
          "rounded-md border px-4 py-3 text-[13px]",
          enabled
            ? "border-line bg-bg-2 text-ink-3"
            : "border-line/60 bg-bg-2 text-ink-4"
        )}
      >
        {enabled ? (
          <>
            Drafts will open with{" "}
            <em className="text-ink">
              "Hi — {userName.trim() || "the principal"} asked me to…"
            </em>{" "}
            and sign with <em className="text-ink">— {assistantName.trim()}</em>{" "}
            on the first message of a thread.
          </>
        ) : (
          <>
            Assistant mode is off — drafts will be in your first-person voice.
          </>
        )}
      </div>

      <div className="flex items-center gap-3 pt-2">
        <button
          type="submit"
          disabled={!dirty || update.isPending}
          className={clsx(
            "h-9 px-4 rounded-md text-[13px] font-mono uppercase tracking-wide transition",
            dirty && !update.isPending
              ? "bg-accent text-bg hover:opacity-90"
              : "bg-bg-2 text-ink-4 cursor-not-allowed"
          )}
        >
          {update.isPending ? "Saving…" : "Save"}
        </button>
        {savedFlash && (
          <span className="text-ink-4 text-[12px] font-mono uppercase tracking-wide">
            Saved
          </span>
        )}
        {update.isError && (
          <span className="text-rose-400 text-[12px]">
            {update.error?.message ?? "Save failed"}
          </span>
        )}
      </div>
    </form>
  );
}

interface FieldProps {
  label: string;
  hint?: string;
  children: React.ReactNode;
}

function Field({ label, hint, children }: FieldProps) {
  return (
    <label className="block">
      <span className="block text-ink text-[13px] font-medium mb-1">
        {label}
      </span>
      {children}
      {hint && (
        <span className="block text-ink-4 text-[12px] mt-1 leading-relaxed">
          {hint}
        </span>
      )}
    </label>
  );
}
