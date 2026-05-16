import { useState } from "react";
import clsx from "clsx";
import {
  Pencil,
  Check,
  X,
  Pause,
  Play,
  Square,
  GitMerge,
  GitBranch,
} from "lucide-react";

import type { Concern, RetrospectiveProgress } from "../types";
import { useConcernEdit } from "../api/useConcernEdit";

const NAME_MAX_LEN = 80;
const DESC_MAX_LEN = 600;

interface Props {
  concern: Concern;
  progress?: RetrospectiveProgress;
  onApprove?: (c: Concern) => void;
  onDismiss?: (c: Concern) => void;
  onPause?: (c: Concern) => void;
  onResume?: (c: Concern) => void;
  onEnd?: (c: Concern) => void;
  onMerge?: (c: Concern) => void;
  onSplit?: (c: Concern) => void;
}

// State source dot mirrors MemoryItem's confidence-dot vocabulary.
// User-declared concerns get the amber tone (the "you" badge),
// model-proposed get the neutral tone (the "learned" badge).
const SOURCE_DOT: Record<Concern["source"], string> = {
  user: "bg-amber",
  model: "bg-ink-5",
};

export function ConcernItem({
  concern,
  progress,
  onApprove,
  onDismiss,
  onPause,
  onResume,
  onEnd,
  onMerge,
  onSplit,
}: Props) {
  const [editing, setEditing] = useState(false);
  const [draftName, setDraftName] = useState(concern.name);
  const [draftDesc, setDraftDesc] = useState(concern.description);
  const edit = useConcernEdit();

  const isUser = concern.source === "user";
  const isProposed = concern.state === "proposed";
  const isActive = concern.state === "active";
  const isPaused = concern.state === "paused";
  const isArchived = concern.state === "ended" || concern.state === "merged";

  const nameTrimmed = draftName.trim();
  const descTrimmed = draftDesc.trim();
  const nameTooLong = draftName.length > NAME_MAX_LEN;
  const descTooLong = draftDesc.length > DESC_MAX_LEN;
  const nameUnchanged = nameTrimmed === concern.name.trim();
  const descUnchanged = descTrimmed === concern.description.trim();
  const blank = nameTrimmed === "" || descTrimmed === "";
  const saveDisabled =
    blank ||
    nameTooLong ||
    descTooLong ||
    (nameUnchanged && descUnchanged) ||
    edit.isPending;

  function startEdit() {
    setDraftName(concern.name);
    setDraftDesc(concern.description);
    setEditing(true);
  }

  function cancelEdit() {
    setDraftName(concern.name);
    setDraftDesc(concern.description);
    setEditing(false);
  }

  function saveEdit() {
    edit.mutate(
      {
        id: concern.id,
        name: nameUnchanged ? undefined : nameTrimmed,
        description: descUnchanged ? undefined : descTrimmed,
      },
      {
        onSuccess: () => setEditing(false),
      }
    );
  }

  return (
    <div className="flex items-start gap-3 py-2.5 group">
      <span
        title={`source: ${concern.source}`}
        className={clsx(
          "h-1.5 w-1.5 rounded-full mt-2 shrink-0",
          SOURCE_DOT[concern.source]
        )}
      />

      <div className="flex-1 min-w-0">
        {!editing && (
          <>
            <p className="text-[14px] font-[500] text-ink leading-snug">
              {concern.name}
            </p>
            <p className="text-[13px] text-ink-3 leading-snug mt-0.5">
              {concern.description}
            </p>
          </>
        )}
        {editing && (
          <div className="space-y-1.5">
            <input
              type="text"
              value={draftName}
              onChange={(e) => setDraftName(e.target.value)}
              maxLength={NAME_MAX_LEN + 16}
              className={clsx(
                "w-full bg-bg-elev rounded-z-sm border px-2 py-1 text-[14px] font-[500] text-ink focus:outline-none focus:ring-1",
                nameTooLong
                  ? "border-crit/40 focus:ring-crit/40"
                  : "border-line focus:ring-line"
              )}
            />
            <textarea
              value={draftDesc}
              onChange={(e) => setDraftDesc(e.target.value)}
              rows={3}
              maxLength={DESC_MAX_LEN + 40}
              className={clsx(
                "w-full bg-bg-elev rounded-z-sm border px-2 py-1.5 text-[13px] text-ink-3 leading-snug focus:outline-none focus:ring-1",
                descTooLong
                  ? "border-crit/40 focus:ring-crit/40"
                  : "border-line focus:ring-line"
              )}
            />
            <div className="flex items-center justify-between text-[10px] font-mono text-ink-5">
              <span>
                <span className={clsx(nameTooLong && "text-crit")}>
                  name {draftName.length}/{NAME_MAX_LEN}
                </span>
                <span className="mx-1.5">·</span>
                <span className={clsx(descTooLong && "text-crit")}>
                  description {draftDesc.length}/{DESC_MAX_LEN}
                </span>
              </span>
              {edit.isError && (
                <span className="text-crit">{edit.error.message}</span>
              )}
            </div>
          </div>
        )}

        {/* Subdued meta line: source badge + last-active + observation count */}
        <div className="flex items-center gap-2 mt-1.5 font-mono text-[10px] text-ink-5">
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
          <span>·</span>
          <span title={`last active: ${concern.last_active_at}`}>
            {formatRelative(concern.last_active_at)}
          </span>
          <span>·</span>
          <span>
            {concern.observation_count}{" "}
            {concern.observation_count === 1 ? "observation" : "observations"}
          </span>
        </div>

        {/* Live retrospective progress slot — text only, no progress bars. */}
        {progress && (
          <p
            role="status"
            aria-live="polite"
            className="mt-1.5 font-mono text-[10px] text-ink-4"
          >
            {progress.status === "running" &&
              `tagging history… ${progress.processed} of ~${progress.total}`}
            {progress.status === "completed" &&
              `tagging history… settled at ${progress.processed}`}
            {progress.status === "cancelled" &&
              `tagging history… stopped at ${progress.processed}`}
            {progress.status === "failed" &&
              `tagging history paused — ${progress.error ?? "will retry"}`}
          </p>
        )}

        {/* V2.5.0 Phase 5: ready-to-retire note. Calm copy, never a
            badge. The user retires by clicking End. */}
        {concern.ready_to_retire && (
          <p className="mt-1.5 font-mono text-[10px] text-ink-4 italic">
            quiet for a while — ready to retire?
          </p>
        )}
      </div>

      {/* Right-side actions */}
      <div className="flex items-center gap-1 opacity-60 group-hover:opacity-100 transition shrink-0">
        {!editing && (
          <>
            {isProposed && onApprove && (
              <ActionButton
                title="Approve"
                onClick={() => onApprove(concern)}
                tone="accent"
              >
                <Check className="h-3.5 w-3.5" />
              </ActionButton>
            )}
            {isProposed && onDismiss && (
              <ActionButton
                title="Dismiss"
                onClick={() => onDismiss(concern)}
                tone="crit"
              >
                <X className="h-3.5 w-3.5" />
              </ActionButton>
            )}
            {isActive && onPause && (
              <ActionButton title="Pause" onClick={() => onPause(concern)}>
                <Pause className="h-3.5 w-3.5" />
              </ActionButton>
            )}
            {isPaused && onResume && (
              <ActionButton title="Resume" onClick={() => onResume(concern)}>
                <Play className="h-3.5 w-3.5" />
              </ActionButton>
            )}
            {(isActive || isPaused) && onEnd && (
              <ActionButton
                title="End"
                onClick={() => onEnd(concern)}
                tone="crit"
              >
                <Square className="h-3.5 w-3.5" />
              </ActionButton>
            )}
            {(isActive || isPaused) && onMerge && (
              <ActionButton title="Merge" onClick={() => onMerge(concern)}>
                <GitMerge className="h-3.5 w-3.5" />
              </ActionButton>
            )}
            {(isActive || isPaused) && onSplit && (
              <ActionButton title="Split" onClick={() => onSplit(concern)}>
                <GitBranch className="h-3.5 w-3.5" />
              </ActionButton>
            )}
            {!isArchived && (
              <ActionButton title="Edit" onClick={startEdit}>
                <Pencil className="h-3.5 w-3.5" />
              </ActionButton>
            )}
          </>
        )}
        {editing && (
          <>
            <ActionButton title="Cancel" onClick={cancelEdit}>
              <X className="h-3.5 w-3.5" />
            </ActionButton>
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

interface ActionButtonProps {
  title: string;
  onClick: () => void;
  tone?: "accent" | "crit" | "neutral";
  children: React.ReactNode;
}

function ActionButton({ title, onClick, tone = "neutral", children }: ActionButtonProps) {
  const hoverClass =
    tone === "accent"
      ? "hover:text-accent"
      : tone === "crit"
        ? "hover:text-crit"
        : "hover:text-ink-3";
  return (
    <button
      type="button"
      title={title}
      aria-label={title}
      onClick={onClick}
      className={clsx(
        "h-7 w-7 rounded-z-sm flex items-center justify-center text-ink-4 hover:bg-bg-elev transition",
        hoverClass
      )}
    >
      {children}
    </button>
  );
}

function formatRelative(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const diffMs = Date.now() - t;
  const minute = 60 * 1000;
  const hour = 60 * minute;
  const day = 24 * hour;
  if (diffMs < minute) return "just now";
  if (diffMs < hour) return `${Math.floor(diffMs / minute)}m ago`;
  if (diffMs < day) return `${Math.floor(diffMs / hour)}h ago`;
  if (diffMs < 30 * day) return `${Math.floor(diffMs / day)}d ago`;
  return new Date(t).toISOString().slice(0, 10);
}
