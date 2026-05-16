import { useEffect, useMemo, useRef, useState } from "react";
import { X } from "lucide-react";
import clsx from "clsx";

import type { Concern } from "../types";
import { useConcernSplit } from "../api/useConcernSplit";
import { useConcernObservations } from "../api/useConcernObservations";

interface Props {
  source: Concern;
  onClose: () => void;
}

type Side = "A" | "B" | "skip";

const NAME_MAX = 80;
const DESC_MAX = 600;

// ConcernSplitModal — two-way split. The backend supports N-way; the UI
// caps at two for V2.5 because the row-by-row routing UX gets unwieldy
// past two columns. Multi-way split is a V2.5.x stretch.
export function ConcernSplitModal({ source, onClose }: Props) {
  const obs = useConcernObservations(source.id);
  const split = useConcernSplit();
  const closeRef = useRef<HTMLButtonElement | null>(null);

  const [aName, setANameRaw] = useState("");
  const [aDesc, setADescRaw] = useState("");
  const [bName, setBNameRaw] = useState("");
  const [bDesc, setBDescRaw] = useState("");

  const observations = useMemo(() => obs.data?.observations ?? [], [obs.data]);
  const [routing, setRouting] = useState<Record<string, Side>>({});

  useEffect(() => {
    closeRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  // Default every observation to A on first load, so an "everything to
  // A" outcome (the user only renames the source) becomes a one-click
  // confirm.
  useEffect(() => {
    if (observations.length === 0) return;
    setRouting((prev) => {
      if (Object.keys(prev).length > 0) return prev;
      const next: Record<string, Side> = {};
      for (const o of observations) next[o.event_id] = "A";
      return next;
    });
  }, [observations]);

  const aIds = observations
    .filter((o) => routing[o.event_id] === "A")
    .map((o) => o.event_id);
  const bIds = observations
    .filter((o) => routing[o.event_id] === "B")
    .map((o) => o.event_id);

  const namesValid =
    aName.trim() !== "" &&
    bName.trim() !== "" &&
    aName.length <= NAME_MAX &&
    bName.length <= NAME_MAX;
  const descsValid = aDesc.length <= DESC_MAX && bDesc.length <= DESC_MAX;
  const sidesValid = aIds.length > 0 && bIds.length > 0;
  const submitDisabled = !namesValid || !descsValid || !sidesValid || split.isPending;

  function setSide(eventID: string, side: Side) {
    setRouting((m) => ({ ...m, [eventID]: side }));
  }

  function submit(e: React.FormEvent) {
    e.preventDefault();
    if (submitDisabled) return;
    split.mutate(
      {
        id: source.id,
        splits: [
          { name: aName.trim(), description: aDesc.trim(), observation_ids: aIds },
          { name: bName.trim(), description: bDesc.trim(), observation_ids: bIds },
        ],
      },
      {
        onSuccess: () => onClose(),
      }
    );
  }

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="split-modal-title"
      className="fixed inset-0 z-40 flex items-center justify-center bg-black/40 p-4"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <form
        onSubmit={submit}
        className="w-full max-w-2xl max-h-[90vh] overflow-y-auto rounded-z-md border border-line bg-bg p-5 space-y-4 shadow-xl"
      >
        <div className="flex items-baseline justify-between">
          <h2
            id="split-modal-title"
            className="font-display font-[500] text-[16px] text-ink"
          >
            Split concern
          </h2>
          <button
            ref={closeRef}
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="h-7 w-7 rounded-z-sm flex items-center justify-center text-ink-4 hover:text-ink-3 hover:bg-bg-elev"
          >
            <X className="h-3.5 w-3.5" />
          </button>
        </div>

        <p className="text-[13px] text-ink-3 leading-snug">
          Split <span className="font-[500] text-ink">"{source.name}"</span>{" "}
          into two threads. Route each observation to one side or skip it.
        </p>

        <div className="grid grid-cols-2 gap-4">
          <SidePane
            label="A"
            name={aName}
            desc={aDesc}
            onName={setANameRaw}
            onDesc={setADescRaw}
            count={aIds.length}
          />
          <SidePane
            label="B"
            name={bName}
            desc={bDesc}
            onName={setBNameRaw}
            onDesc={setBDescRaw}
            count={bIds.length}
          />
        </div>

        <div>
          <h3 className="font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-2">
            Observations
          </h3>
          {obs.isLoading && (
            <p className="text-[12px] text-ink-5 font-mono">loading…</p>
          )}
          {!obs.isLoading && observations.length === 0 && (
            <p className="text-[12px] text-ink-5 italic">
              This thread has no tagged observations yet.
            </p>
          )}
          {observations.length > 0 && (
            <div className="rounded-z-sm border border-line divide-y divide-line max-h-72 overflow-y-auto">
              {observations.map((o) => {
                const side = routing[o.event_id] ?? "A";
                return (
                  <div
                    key={o.event_id}
                    className="flex items-center justify-between px-3 py-2"
                  >
                    <div className="min-w-0">
                      <p className="font-mono text-[11px] text-ink-3 truncate">
                        {o.event_id}
                      </p>
                      <p className="font-mono text-[10px] text-ink-5">
                        {o.source} · {new Date(o.tagged_at).toISOString().slice(0, 10)}
                      </p>
                    </div>
                    <div className="flex items-center gap-1 shrink-0">
                      {(["A", "B", "skip"] as Side[]).map((s) => (
                        <button
                          key={s}
                          type="button"
                          onClick={() => setSide(o.event_id, s)}
                          className={clsx(
                            "h-6 px-2 rounded-z-sm text-[11px] font-mono border transition",
                            side === s
                              ? "bg-accent text-white border-transparent"
                              : "border-line text-ink-4 hover:text-ink-3 hover:bg-bg-elev"
                          )}
                        >
                          {s}
                        </button>
                      ))}
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>

        {!sidesValid && observations.length > 0 && (
          <p className="text-[11px] text-ink-5 font-mono">
            Each side needs at least one observation, or use Cancel.
          </p>
        )}
        {split.isError && (
          <p className="text-[11px] text-crit font-mono">{split.error.message}</p>
        )}

        <div className="flex items-center justify-end gap-2 pt-1">
          <button
            type="button"
            onClick={onClose}
            className="h-[28px] px-3 rounded-z-sm text-[12px] border border-line text-ink-3 hover:bg-bg-elev"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={submitDisabled}
            className={clsx(
              "h-[28px] px-3 rounded-z-sm text-[12px] font-[500] border transition",
              submitDisabled
                ? "border-line text-ink-5 opacity-50 cursor-not-allowed"
                : "bg-accent text-white border-transparent hover:opacity-90"
            )}
          >
            {split.isPending ? "Splitting…" : "Split"}
          </button>
        </div>
      </form>
    </div>
  );
}

interface SidePaneProps {
  label: string;
  name: string;
  desc: string;
  count: number;
  onName: (s: string) => void;
  onDesc: (s: string) => void;
}

function SidePane({ label, name, desc, count, onName, onDesc }: SidePaneProps) {
  const nameTooLong = name.length > NAME_MAX;
  const descTooLong = desc.length > DESC_MAX;
  return (
    <div className="space-y-2">
      <h3 className="font-mono text-[10px] uppercase tracking-wide text-ink-5">
        Side {label} · {count}
      </h3>
      <input
        type="text"
        value={name}
        onChange={(e) => onName(e.target.value)}
        maxLength={NAME_MAX + 16}
        placeholder="name"
        className={clsx(
          "w-full h-9 px-2 rounded-z-sm bg-bg-elev border text-[13px] font-[500] text-ink focus:outline-none focus:ring-1",
          nameTooLong
            ? "border-crit/40 focus:ring-crit/40"
            : "border-line focus:ring-line"
        )}
      />
      <textarea
        value={desc}
        onChange={(e) => onDesc(e.target.value)}
        rows={3}
        maxLength={DESC_MAX + 40}
        placeholder="description (optional)"
        className={clsx(
          "w-full px-2 py-1.5 rounded-z-sm bg-bg-elev border text-[13px] text-ink-3 leading-snug focus:outline-none focus:ring-1",
          descTooLong
            ? "border-crit/40 focus:ring-crit/40"
            : "border-line focus:ring-line"
        )}
      />
    </div>
  );
}
