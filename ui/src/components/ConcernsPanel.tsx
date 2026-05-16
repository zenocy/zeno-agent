import { useEffect, useMemo, useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";

import type { Concern } from "../types";
import { useConcerns } from "../api/useConcerns";
import { useConcernAction } from "../api/useConcernAction";
import { useConcernRetrospectiveProgress } from "../api/useConcernRetrospectiveProgress";
import { ConcernItem } from "./ConcernItem";
import { ConcernMergeModal } from "./ConcernMergeModal";
import { ConcernSplitModal } from "./ConcernSplitModal";

const UNDO_MS = 5000;
const ARCHIVED_DEFAULT_LIMIT = 20;

type DeferredAction = "dismiss" | "end";

interface PendingAction {
  concern: Concern;
  action: DeferredAction;
  expiresAt: number;
  timer: ReturnType<typeof setTimeout>;
}

export function ConcernsPanel() {
  const { data, isLoading, isError } = useConcerns();
  const concerns = useMemo(() => data?.concerns ?? [], [data]);
  const action = useConcernAction();
  const qc = useQueryClient();
  const progress = useConcernRetrospectiveProgress();

  const [pending, setPending] = useState<PendingAction | null>(null);
  const [pausedExpanded, setPausedExpanded] = useState(false);
  const [archivedExpanded, setArchivedExpanded] = useState(false);
  const [mergeFor, setMergeFor] = useState<Concern | null>(null);
  const [splitFor, setSplitFor] = useState<Concern | null>(null);

  // On unmount, drop any pending timer so an unmount during the grace
  // window doesn't fire the deferred action against a possibly-already
  // -gone concern.
  useEffect(() => {
    return () => {
      if (pending) clearTimeout(pending.timer);
    };
  }, [pending]);

  // Group concerns by lifecycle bucket. The pending row is hidden while
  // the undo timer is in flight; cancelling restores it via cache
  // invalidation.
  const grouped = useMemo(() => {
    const buckets: Record<
      "proposed" | "active" | "paused" | "archived",
      Concern[]
    > = {
      proposed: [],
      active: [],
      paused: [],
      archived: [],
    };
    for (const c of concerns) {
      if (pending && pending.concern.id === c.id) continue;
      switch (c.state) {
        case "proposed":
          buckets.proposed.push(c);
          break;
        case "active":
          buckets.active.push(c);
          break;
        case "paused":
          buckets.paused.push(c);
          break;
        case "ended":
        case "merged":
          buckets.archived.push(c);
          break;
      }
    }
    return buckets;
  }, [concerns, pending]);

  function startDeferred(concern: Concern, deferredAction: DeferredAction) {
    if (pending) {
      clearTimeout(pending.timer);
      // The previously-pending action was about to fire. Fire it now so
      // we don't drop it; the new one starts its own timer.
      action.mutate({ id: pending.concern.id, action: pending.action });
    }
    const timer = setTimeout(() => {
      action.mutate({ id: concern.id, action: deferredAction });
      setPending(null);
    }, UNDO_MS);
    setPending({
      concern,
      action: deferredAction,
      expiresAt: Date.now() + UNDO_MS,
      timer,
    });
  }

  function undoDeferred() {
    if (!pending) return;
    clearTimeout(pending.timer);
    setPending(null);
    qc.invalidateQueries({ queryKey: ["concerns"] });
  }

  function fireImmediate(concern: Concern, a: "approve" | "pause" | "resume") {
    action.mutate({ id: concern.id, action: a });
  }

  const visibleCount =
    concerns.length - (pending ? 1 : 0);

  return (
    <div className="px-8 pt-8 pb-40 max-w-2xl mx-auto">
      <header className="flex items-baseline justify-between mb-6">
        <h1 className="font-display font-[500] text-[22px] text-ink">
          What I'm holding for you
        </h1>
        <span className="font-mono text-[11px] text-ink-4">
          {isLoading ? "…" : `${visibleCount}`}
        </span>
      </header>

      {isError && (
        <p className="font-mono text-[11px] text-crit py-4">
          Couldn't load concerns. Try refreshing.
        </p>
      )}

      {isLoading && (
        <div className="space-y-2">
          {[...Array(3)].map((_, i) => (
            <div
              key={i}
              className="h-16 rounded-z-sm border border-line bg-bg-card opacity-50"
            />
          ))}
        </div>
      )}

      {!isLoading && visibleCount === 0 && (
        <div className="rounded-z-md border border-line bg-bg-card p-6 text-center">
          <p className="text-[14px] text-ink-3 mb-1">Nothing yet.</p>
          <p className="text-[12px] text-ink-5">
            Zeno will surface threads as patterns emerge.
          </p>
        </div>
      )}

      {!isLoading && (
        <div className="space-y-6">
          {grouped.proposed.length > 0 && (
            <Section title="Pending review">
              <Bucket
                concerns={grouped.proposed}
                progress={progress}
                onApprove={(c) => fireImmediate(c, "approve")}
                onDismiss={(c) => startDeferred(c, "dismiss")}
              />
            </Section>
          )}

          {grouped.active.length > 0 && (
            <Section title="Active">
              <Bucket
                concerns={grouped.active}
                progress={progress}
                onPause={(c) => fireImmediate(c, "pause")}
                onEnd={(c) => startDeferred(c, "end")}
                onMerge={(c) => setMergeFor(c)}
                onSplit={(c) => setSplitFor(c)}
              />
            </Section>
          )}

          {grouped.paused.length > 0 && (
            <CollapsibleSection
              title="Paused"
              count={grouped.paused.length}
              expanded={pausedExpanded}
              onToggle={() => setPausedExpanded((v) => !v)}
            >
              <Bucket
                concerns={grouped.paused}
                progress={progress}
                onResume={(c) => fireImmediate(c, "resume")}
                onEnd={(c) => startDeferred(c, "end")}
                onMerge={(c) => setMergeFor(c)}
                onSplit={(c) => setSplitFor(c)}
              />
            </CollapsibleSection>
          )}

          {grouped.archived.length > 0 && (
            <CollapsibleSection
              title="Archived"
              count={grouped.archived.length}
              expanded={archivedExpanded}
              onToggle={() => setArchivedExpanded((v) => !v)}
            >
              <Bucket
                concerns={grouped.archived.slice(0, ARCHIVED_DEFAULT_LIMIT)}
                progress={progress}
              />
              {grouped.archived.length > ARCHIVED_DEFAULT_LIMIT && (
                <p className="font-mono text-[10px] text-ink-5 px-3 py-2 italic">
                  showing {ARCHIVED_DEFAULT_LIMIT} of {grouped.archived.length}
                </p>
              )}
            </CollapsibleSection>
          )}
        </div>
      )}

      {pending && <UndoToast pending={pending} onUndo={undoDeferred} />}

      {mergeFor && (
        <ConcernMergeModal
          source={mergeFor}
          candidates={[...grouped.active, ...grouped.paused].filter(
            (c) => c.id !== mergeFor.id
          )}
          onClose={() => setMergeFor(null)}
        />
      )}

      {splitFor && (
        <ConcernSplitModal
          source={splitFor}
          onClose={() => setSplitFor(null)}
        />
      )}
    </div>
  );
}

interface SectionProps {
  title: string;
  children: React.ReactNode;
}

function Section({ title, children }: SectionProps) {
  return (
    <section>
      <h2 className="font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-2">
        {title}
      </h2>
      <div className="rounded-z-md border border-line bg-bg-card divide-y divide-line">
        {children}
      </div>
    </section>
  );
}

interface CollapsibleSectionProps extends SectionProps {
  count: number;
  expanded: boolean;
  onToggle: () => void;
}

function CollapsibleSection({
  title,
  count,
  expanded,
  onToggle,
  children,
}: CollapsibleSectionProps) {
  return (
    <section>
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={expanded}
        className="flex items-center gap-1.5 mb-2 font-mono text-[10px] uppercase tracking-wide text-ink-4 hover:text-ink-3 transition"
      >
        {expanded ? (
          <ChevronDown className="h-3 w-3" />
        ) : (
          <ChevronRight className="h-3 w-3" />
        )}
        <span>{title}</span>
        <span className="text-ink-5">·</span>
        <span className="text-ink-5 normal-case tracking-normal">{count}</span>
      </button>
      {expanded && (
        <div className="rounded-z-md border border-line bg-bg-card divide-y divide-line">
          {children}
        </div>
      )}
    </section>
  );
}

interface BucketProps {
  concerns: Concern[];
  progress: Record<string, import("../types").RetrospectiveProgress>;
  onApprove?: (c: Concern) => void;
  onDismiss?: (c: Concern) => void;
  onPause?: (c: Concern) => void;
  onResume?: (c: Concern) => void;
  onEnd?: (c: Concern) => void;
  onMerge?: (c: Concern) => void;
  onSplit?: (c: Concern) => void;
}

function Bucket(props: BucketProps) {
  const { concerns, progress, ...handlers } = props;
  return (
    <>
      {concerns.map((c) => (
        <div key={c.id} className="px-3">
          <ConcernItem concern={c} progress={progress[c.id]} {...handlers} />
        </div>
      ))}
    </>
  );
}

function UndoToast({
  pending,
  onUndo,
}: {
  pending: PendingAction;
  onUndo: () => void;
}) {
  const [secondsLeft, setSecondsLeft] = useState(
    Math.max(0, Math.round((pending.expiresAt - Date.now()) / 1000))
  );
  useEffect(() => {
    const id = setInterval(() => {
      setSecondsLeft(
        Math.max(0, Math.round((pending.expiresAt - Date.now()) / 1000))
      );
    }, 250);
    return () => clearInterval(id);
  }, [pending]);

  const verb = pending.action === "dismiss" ? "Dismissed" : "Ended";

  return (
    <div className="fixed bottom-6 left-1/2 -translate-x-1/2 z-30">
      <div className="flex items-center gap-3 rounded-z-md bg-ink text-white px-4 py-2.5 shadow-lg font-sans text-[13px]">
        <span className="opacity-90">
          {verb} "{truncate(pending.concern.name, 40)}"
        </span>
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

function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n) + "…" : s;
}
