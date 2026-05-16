import { useState } from "react";
import clsx from "clsx";
import { Pencil, Check, X, Trash2 } from "lucide-react";
import type { MemoryFact } from "../types";
import { useMemoryEdit } from "../api/useMemoryEdit";

// Confidence dot mirrors REL_DOT in Card.tsx — same visual vocabulary so
// the user reads the same meaning across surfaces. high → filled (crit),
// med → amber, low → empty (ink-5).
const CONF_DOT: Record<MemoryFact["confidence"], string> = {
  high: "bg-crit",
  med: "bg-amber",
  low: "bg-ink-5",
};

interface Props {
  fact: MemoryFact;
  onDelete: (fact: MemoryFact) => void;
}

const FACT_MAX_LEN = 280;

export function MemoryItem({ fact, onDelete }: Props) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(fact.fact);
  const edit = useMemoryEdit();

  const isUser = fact.source === "user";
  const draftTooLong = draft.length > FACT_MAX_LEN;
  const draftBlank = draft.trim() === "";
  const draftUnchanged = draft.trim() === fact.fact.trim();
  const saveDisabled = draftBlank || draftTooLong || draftUnchanged || edit.isPending;

  function startEdit() {
    setDraft(fact.fact);
    setEditing(true);
  }

  function cancelEdit() {
    setDraft(fact.fact);
    setEditing(false);
  }

  function saveEdit() {
    edit.mutate(
      { id: fact.id, fact: draft.trim() },
      {
        onSuccess: () => setEditing(false),
      }
    );
  }

  return (
    <div className="flex items-start gap-3 py-2 group">
      <span
        title={`confidence: ${fact.confidence}`}
        className={clsx("h-1.5 w-1.5 rounded-full mt-2 shrink-0", CONF_DOT[fact.confidence])}
      />

      <div className="flex-1 min-w-0">
        {!editing && (
          <p className="text-[14px] text-ink leading-snug">
            {fact.fact}
          </p>
        )}
        {editing && (
          <div className="space-y-1.5">
            <textarea
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              rows={2}
              maxLength={FACT_MAX_LEN + 40 /* let the validator catch overrun, but stop runaway typing */}
              className={clsx(
                "w-full bg-bg-elev rounded-z-sm border px-2 py-1.5 text-[14px] text-ink leading-snug focus:outline-none focus:ring-1",
                draftTooLong
                  ? "border-crit/40 focus:ring-crit/40"
                  : "border-line focus:ring-line"
              )}
            />
            <div className="flex items-center justify-between text-[10px] font-mono text-ink-5">
              <span className={clsx(draftTooLong && "text-crit")}>
                {draft.length}/{FACT_MAX_LEN}
              </span>
              {edit.isError && (
                <span className="text-crit">{edit.error.message}</span>
              )}
            </div>
          </div>
        )}

        {/* Subject + source + evidence (subdued meta line) */}
        <div className="flex items-center gap-2 mt-1 font-mono text-[10px] text-ink-5">
          <span className="uppercase tracking-wide">{fact.subject}</span>
          <span>·</span>
          <span
            className={clsx(
              "px-1.5 py-0.5 rounded-full border",
              isUser
                ? "text-amber border-amber/30 bg-amber-soft"
                : "text-ink-4 border-line"
            )}
          >
            {isUser ? "you" : "learned"}
          </span>
          <span
            title="evidence count"
            className="opacity-0 group-hover:opacity-100 transition"
          >
            ×{fact.evidence_count}
          </span>
        </div>
      </div>

      {/* Right-side actions */}
      <div className="flex items-center gap-1 opacity-60 group-hover:opacity-100 transition shrink-0">
        {!editing && (
          <>
            <button
              type="button"
              title="Edit"
              aria-label="Edit fact"
              onClick={startEdit}
              className="h-7 w-7 rounded-z-sm flex items-center justify-center text-ink-4 hover:text-ink-3 hover:bg-bg-elev"
            >
              <Pencil className="h-3.5 w-3.5" />
            </button>
            <button
              type="button"
              title="Delete"
              aria-label="Delete fact"
              onClick={() => onDelete(fact)}
              className="h-7 w-7 rounded-z-sm flex items-center justify-center text-ink-4 hover:text-crit hover:bg-bg-elev"
            >
              <Trash2 className="h-3.5 w-3.5" />
            </button>
          </>
        )}
        {editing && (
          <>
            <button
              type="button"
              title="Cancel"
              aria-label="Cancel edit"
              onClick={cancelEdit}
              className="h-7 w-7 rounded-z-sm flex items-center justify-center text-ink-4 hover:text-ink-3 hover:bg-bg-elev"
            >
              <X className="h-3.5 w-3.5" />
            </button>
            <button
              type="button"
              title="Save"
              aria-label="Save edit"
              disabled={saveDisabled}
              onClick={saveEdit}
              className={clsx(
                "h-7 w-7 rounded-z-sm flex items-center justify-center transition",
                saveDisabled
                  ? "text-ink-5 opacity-50"
                  : "text-accent hover:bg-bg-elev"
              )}
            >
              <Check className="h-3.5 w-3.5" />
            </button>
          </>
        )}
      </div>
    </div>
  );
}
