import { useEffect, useMemo, useRef, useState } from "react";
import clsx from "clsx";
import { Plus, X } from "lucide-react";
import { useMemory } from "../api/useMemory";
import { useMemoryAdd, MemoryConflictError } from "../api/useMemoryAdd";
import { useMemoryDelete } from "../api/useMemoryDelete";
import { useQueryClient } from "@tanstack/react-query";
import type { MemoryFact } from "../types";
import { MemoryItem } from "./MemoryItem";

const CATEGORIES: { value: string; label: string }[] = [
  { value: "identity", label: "Identity" },
  { value: "relationship", label: "Relationship" },
  { value: "preference", label: "Preference" },
  { value: "routine", label: "Routine" },
  { value: "context", label: "Context" },
];

const CATEGORY_ORDER = ["identity", "relationship", "preference", "routine", "context", "misc"];

const SUBJECT_MAX = 64;
const FACT_MAX = 280;
const UNDO_MS = 5000;

interface PendingDelete {
  fact: MemoryFact;
  expiresAt: number;
  timer: ReturnType<typeof setTimeout>;
}

export function MemoryPanel() {
  const { data, isLoading, isError } = useMemory();
  const facts = useMemo(() => data?.facts ?? [], [data]);

  const [showAdd, setShowAdd] = useState(false);
  const [pending, setPending] = useState<PendingDelete | null>(null);
  const del = useMemoryDelete();
  const qc = useQueryClient();

  // On unmount: drop any pending delete timer so an in-flight tab switch
  // doesn't fire DELETE from a now-unmounted component. The pending delete
  // is local-only until the timer expires; navigating away cancels it.
  useEffect(() => {
    return () => {
      if (pending) clearTimeout(pending.timer);
    };
  }, [pending]);

  // Group + sort facts by category. Within a category, hide rows that are
  // currently in pending-delete state so the optimistic UX feels clean.
  const grouped = useMemo(() => {
    const visibleFacts = facts.filter((f) => f.id !== pending?.fact.id);
    const map = new Map<string, MemoryFact[]>();
    for (const f of visibleFacts) {
      const key = CATEGORY_ORDER.includes(f.category) ? f.category : "misc";
      const list = map.get(key) ?? [];
      list.push(f);
      map.set(key, list);
    }
    return CATEGORY_ORDER.flatMap((cat) => {
      const list = map.get(cat);
      if (!list || list.length === 0) return [];
      return [{ category: cat, facts: list }];
    });
  }, [facts, pending]);

  const visibleCount = facts.length - (pending ? 1 : 0);

  function startDelete(fact: MemoryFact) {
    if (pending) {
      clearTimeout(pending.timer);
      // Previously-pending row was about to be deleted. Fire that delete
      // now so we don't drop it; the new one starts its own timer.
      del.mutate({ id: pending.fact.id });
    }
    const timer = setTimeout(() => {
      del.mutate({ id: fact.id });
      setPending(null);
    }, UNDO_MS);
    setPending({ fact, expiresAt: Date.now() + UNDO_MS, timer });
  }

  function undoDelete() {
    if (!pending) return;
    clearTimeout(pending.timer);
    setPending(null);
    // Refetch so the cache reconciles with server state, in case the timer
    // had already fired (race-safe).
    qc.invalidateQueries({ queryKey: ["memory"] });
  }

  return (
    <div className="px-8 pt-8 pb-40 max-w-2xl mx-auto">
      <header className="flex items-baseline justify-between mb-6">
        <h1 className="font-display font-[500] text-[22px] text-ink">
          What I think I know about you
        </h1>
        <span className="font-mono text-[11px] text-ink-4">
          {isLoading ? "…" : `${visibleCount}`}
        </span>
      </header>

      {!showAdd && (
        <button
          type="button"
          onClick={() => setShowAdd(true)}
          className="w-full flex items-center gap-2 px-3 py-2.5 mb-5 rounded-z-sm border border-dashed border-line text-ink-4 text-[13px] hover:border-line hover:text-ink-3 hover:bg-bg-card transition"
        >
          <Plus className="h-4 w-4" />
          Add a fact
        </button>
      )}

      {showAdd && (
        <AddFactForm
          onCancel={() => setShowAdd(false)}
          onAdded={() => setShowAdd(false)}
        />
      )}

      {isError && (
        <p className="font-mono text-[11px] text-crit py-4">
          Couldn't load memory. Try refreshing.
        </p>
      )}

      {isLoading && (
        <div className="space-y-2">
          {[...Array(3)].map((_, i) => (
            <div
              key={i}
              className="h-12 rounded-z-sm border border-line bg-bg-card opacity-50"
            />
          ))}
        </div>
      )}

      {!isLoading && visibleCount === 0 && (
        <div className="rounded-z-md border border-line bg-bg-card p-6 text-center">
          <p className="text-[14px] text-ink-3 mb-1">No facts yet.</p>
          <p className="text-[12px] text-ink-5">
            Zeno will learn as you use it. You can also add facts above.
          </p>
        </div>
      )}

      {!isLoading && grouped.length > 0 && (
        <div className="space-y-6">
          {grouped.map(({ category, facts: list }) => (
            <section key={category}>
              <h2 className="font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-2">
                {CATEGORIES.find((c) => c.value === category)?.label ?? category}
              </h2>
              <div className="rounded-z-md border border-line bg-bg-card divide-y divide-line">
                {list.map((fact) => (
                  <div key={fact.id} className="px-3">
                    <MemoryItem fact={fact} onDelete={startDelete} />
                  </div>
                ))}
              </div>
            </section>
          ))}
        </div>
      )}

      {pending && <UndoToast pending={pending} onUndo={undoDelete} />}
    </div>
  );
}

interface AddFactFormProps {
  onCancel: () => void;
  onAdded: () => void;
}

function AddFactForm({ onCancel, onAdded }: AddFactFormProps) {
  const [subject, setSubject] = useState("");
  const [fact, setFact] = useState("");
  const [category, setCategory] = useState("relationship");
  const add = useMemoryAdd();
  const subjectRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    subjectRef.current?.focus();
  }, []);

  function normalizeSubject(raw: string): string {
    return raw.trim().toLowerCase().replace(/\s+/g, "-").slice(0, SUBJECT_MAX);
  }

  const cleanSubject = normalizeSubject(subject);
  const factTooLong = fact.length > FACT_MAX;
  const submitDisabled =
    cleanSubject === "" || fact.trim() === "" || factTooLong || add.isPending;

  function submit(e: React.FormEvent) {
    e.preventDefault();
    if (submitDisabled) return;
    add.mutate(
      { subject: cleanSubject, fact: fact.trim(), category },
      {
        onSuccess: () => {
          setSubject("");
          setFact("");
          onAdded();
        },
      }
    );
  }

  const errorMessage = add.isError
    ? add.error instanceof MemoryConflictError
      ? `'${add.error.conflictSubject}' already exists — try editing it instead.`
      : add.error.message
    : null;

  return (
    <form
      onSubmit={submit}
      className="rounded-z-md border border-line bg-bg-card p-4 mb-5 space-y-3 animate-fade-up"
    >
      <div className="flex items-center justify-between">
        <h3 className="font-mono text-[11px] uppercase tracking-wide text-ink-4">
          New fact
        </h3>
        <button
          type="button"
          onClick={onCancel}
          aria-label="Close add form"
          className="h-7 w-7 rounded-z-sm flex items-center justify-center text-ink-4 hover:text-ink-3 hover:bg-bg-elev"
        >
          <X className="h-3.5 w-3.5" />
        </button>
      </div>

      <div className="flex gap-3">
        <div className="flex-1 min-w-0">
          <label
            htmlFor="memory-add-subject"
            className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1"
          >
            Subject
          </label>
          <input
            id="memory-add-subject"
            ref={subjectRef}
            type="text"
            value={subject}
            onChange={(e) => setSubject(e.target.value)}
            onBlur={(e) => setSubject(normalizeSubject(e.target.value))}
            placeholder="partner"
            spellCheck={false}
            autoComplete="off"
            maxLength={SUBJECT_MAX + 16}
            className="w-full h-9 px-2 rounded-z-sm bg-bg-elev border border-line font-mono text-[13px] text-ink placeholder:text-ink-5 focus:outline-none focus:ring-1 focus:ring-line"
          />
        </div>
        <div className="w-44">
          <label
            htmlFor="memory-add-category"
            className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1"
          >
            Category
          </label>
          <select
            id="memory-add-category"
            value={category}
            onChange={(e) => setCategory(e.target.value)}
            className="w-full h-9 px-2 rounded-z-sm bg-bg-elev border border-line text-[13px] text-ink focus:outline-none focus:ring-1 focus:ring-line"
          >
            {CATEGORIES.map((c) => (
              <option key={c.value} value={c.value}>
                {c.label}
              </option>
            ))}
          </select>
        </div>
      </div>

      <div>
        <label
          htmlFor="memory-add-fact"
          className="block font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-1"
        >
          Fact
        </label>
        <textarea
          id="memory-add-fact"
          value={fact}
          onChange={(e) => setFact(e.target.value)}
          rows={2}
          placeholder="Sam handles school pickup."
          className={clsx(
            "w-full px-2 py-1.5 rounded-z-sm bg-bg-elev border text-[14px] text-ink placeholder:text-ink-5 focus:outline-none focus:ring-1",
            factTooLong
              ? "border-crit/40 focus:ring-crit/40"
              : "border-line focus:ring-line"
          )}
        />
        <div className="flex items-center justify-between mt-1 font-mono text-[10px] text-ink-5">
          <span className={clsx(factTooLong && "text-crit")}>
            {fact.length}/{FACT_MAX}
          </span>
          {errorMessage && <span className="text-crit">{errorMessage}</span>}
        </div>
      </div>

      <div className="flex items-center justify-end gap-2 pt-1">
        <button
          type="button"
          onClick={onCancel}
          className="h-[26px] px-3 rounded-z-sm text-[12px] border border-line text-ink-3 hover:bg-bg-elev"
        >
          Cancel
        </button>
        <button
          type="submit"
          disabled={submitDisabled}
          className={clsx(
            "h-[26px] px-3 rounded-z-sm text-[12px] font-[500] border transition",
            submitDisabled
              ? "border-line text-ink-5 opacity-50 cursor-not-allowed"
              : "bg-accent text-white border-transparent hover:opacity-90"
          )}
        >
          {add.isPending ? "Saving…" : "Add fact"}
        </button>
      </div>
    </form>
  );
}

function UndoToast({
  pending,
  onUndo,
}: {
  pending: PendingDelete;
  onUndo: () => void;
}) {
  const [secondsLeft, setSecondsLeft] = useState(
    Math.max(0, Math.round((pending.expiresAt - Date.now()) / 1000))
  );
  useEffect(() => {
    const id = setInterval(() => {
      setSecondsLeft(Math.max(0, Math.round((pending.expiresAt - Date.now()) / 1000)));
    }, 250);
    return () => clearInterval(id);
  }, [pending]);

  const truncated =
    pending.fact.fact.length > 40
      ? pending.fact.fact.slice(0, 40) + "…"
      : pending.fact.fact;

  return (
    <div className="fixed bottom-6 left-1/2 -translate-x-1/2 z-30">
      <div className="flex items-center gap-3 rounded-z-md bg-ink text-white px-4 py-2.5 shadow-lg font-sans text-[13px]">
        <span className="opacity-90">Deleted "{truncated}"</span>
        <button
          type="button"
          onClick={onUndo}
          className="font-[500] underline underline-offset-2 hover:opacity-90"
        >
          Undo ({secondsLeft}s)
        </button>
      </div>
    </div>
  );
}
