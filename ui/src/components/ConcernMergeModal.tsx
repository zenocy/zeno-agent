import { useEffect, useRef, useState } from "react";
import { X } from "lucide-react";
import clsx from "clsx";

import type { Concern } from "../types";
import { useConcernMerge } from "../api/useConcernMerge";

interface Props {
  source: Concern;
  candidates: Concern[];
  onClose: () => void;
}

export function ConcernMergeModal({ source, candidates, onClose }: Props) {
  const [targetId, setTargetId] = useState<string>(candidates[0]?.id ?? "");
  const merge = useConcernMerge();
  const closeRef = useRef<HTMLButtonElement | null>(null);

  // ESC closes; mount-time focus on the close button keeps keyboard
  // users sane until we fold in a focus-trap library.
  useEffect(() => {
    closeRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const target = candidates.find((c) => c.id === targetId) ?? null;
  const submitDisabled = !target || merge.isPending;

  function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!target) return;
    merge.mutate(
      { id: source.id, into_id: target.id },
      {
        onSuccess: () => onClose(),
      }
    );
  }

  const noCandidates = candidates.length === 0;

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="merge-modal-title"
      className="fixed inset-0 z-40 flex items-center justify-center bg-black/40 p-4"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <form
        onSubmit={submit}
        className="w-full max-w-md rounded-z-md border border-line bg-bg p-5 space-y-4 shadow-xl"
      >
        <div className="flex items-baseline justify-between">
          <h2
            id="merge-modal-title"
            className="font-display font-[500] text-[16px] text-ink"
          >
            Merge concern
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
          Merge <span className="font-[500] text-ink">"{source.name}"</span>{" "}
          into another thread. Observations will be re-tagged.
        </p>

        {noCandidates && (
          <p className="text-[12px] text-ink-5 italic">
            No other active or paused threads to merge into.
          </p>
        )}

        {!noCandidates && (
          <div>
            <label
              htmlFor="merge-target"
              className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1"
            >
              Merge into
            </label>
            <select
              id="merge-target"
              value={targetId}
              onChange={(e) => setTargetId(e.target.value)}
              className="w-full h-9 px-2 rounded-z-sm bg-bg-elev border border-line text-[13px] text-ink focus:outline-none focus:ring-1 focus:ring-line"
            >
              {candidates.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          </div>
        )}

        {merge.isError && (
          <p className="text-[11px] text-crit font-mono">
            {merge.error.message}
          </p>
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
            {merge.isPending ? "Merging…" : "Merge"}
          </button>
        </div>
      </form>
    </div>
  );
}
