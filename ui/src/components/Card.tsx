import { useState } from "react";
import clsx from "clsx";
import { Pin, Plus, X, Clock } from "lucide-react";
import type { Card as CardData, ActionResult, CardAction } from "../types";
import { Trace } from "./Trace";
import { SourceIcon } from "./SourceIcon";
import { useAction, type ActionArgs } from "../api/useAction";
import { useIntentModes, modeFor } from "../hooks/useIntentModes";
import { ActionConfirmModal } from "./ActionConfirmModal";
import { useToast } from "./Toast";
import { renderMarkdown } from "../lib/markdown";

interface Props {
  card: CardData;
  // V2.10: when set, clicking the card body (outside interactive
  // children) opens a CardFocus conversation modal. Followup cards
  // rendered inline inside this component intentionally don't get
  // wired so a conversation reply doesn't open another conversation.
  onOpen?: (card: CardData) => void;
}

const REL_DOT: Record<CardData["rel"], string> = {
  high: "bg-crit",
  med: "bg-amber",
  low: "bg-ink-5",
};

export function Card({ card, onOpen }: Props) {
  const [expanded, setExpanded] = useState(false);
  const [removing, setRemoving] = useState(false);
  const [pending, setPending] = useState<{
    args: ActionArgs;
    preview?: Record<string, unknown>;
  } | null>(null);
  const [followup, setFollowup] = useState<CardData | null>(null);
  const { mutateAsync: doAction, isPending: actionPending } = useAction();
  const { data: intents } = useIntentModes();
  const toast = useToast();

  if (removing) return null;

  const isPersonal = card.kind === "personal";
  // The "crit" treatment is a kind-level signal (incoming critical message),
  // not a relevance score. high-rel personal cards still read as personal.
  const isCritical = card.kind === "crit";
  // V2.13.0: assistant-mode reply-received cards get a distinct accent
  // (calm blue border) so the user can pick them out at a glance from
  // ordinary personal cards.
  const isReplyReceived = card.kind === "reply_received";
  const isInjected = card.origin === "inject";
  const hasExpand = card.expand && Object.keys(card.expand).length > 0;

  // V2.8.1: filter out actions whose intent is in the catalog with
  // wired:false. When the catalog hasn't loaded (intents undefined)
  // we render everything — the server-side drop pass is the durable
  // filter; this is purely a defense against pre-V2.8.1 stored cards.
  const wiredSet = intents
    ? new Set(intents.filter((i) => i.wired).map((i) => i.intent))
    : null;

  function resolveIntent(action: CardAction): string {
    if (action.intent && action.intent.length > 0) return action.intent;
    return action.label.toLowerCase();
  }

  // The X (and optional snooze Clock) live in their own column. Pull
  // dismiss + snooze out of the actions row so they don't render as
  // labelled buttons mixed with the primary affordances.
  const filteredActions = card.actions.filter((a) => {
    const intent = resolveIntent(a);
    return intent !== "dismiss" && intent !== "snooze";
  });
  const visibleActions = wiredSet
    ? filteredActions.filter((a) => wiredSet.has(resolveIntent(a)))
    : filteredActions;

  const dismissAction = card.actions.find((a) => resolveIntent(a) === "dismiss");
  const snoozeAction = card.actions.find((a) => resolveIntent(a) === "snooze");

  // V2.9: every card gets a persistent "+ Add to todo" affordance
  // when the deployment has the tasks sensor wired AND the card
  // doesn't already advertise add_task.
  const canAddTodo =
    !!wiredSet?.has("add_task") &&
    !card.actions.some((a) => resolveIntent(a) === "add_task");

  async function dispatch(action: CardAction) {
    const intent = resolveIntent(action);
    const target = (action.target ?? {}) as Record<string, unknown>;

    if (intent === "open_url") {
      const url = String(target.url ?? "");
      if (url) window.open(url, "_blank", "noopener,noreferrer");
      doAction({ id: card.id, intent, target }).catch(() => {});
      return;
    }

    const mode = modeFor(intents, intent);

    if (intent === "dismiss" || intent === "snooze") {
      setRemoving(true);
    }

    if (mode === "confirm") {
      setPending({ args: { id: card.id, intent, target, confirm: true } });
      return;
    }
    if (mode === "preflight") {
      try {
        const result = await doAction({ id: card.id, intent, target, confirm: false });
        if (!result.ok) {
          toast.push(result.toast || "Could not prepare preview.", "error");
          return;
        }
        if (result.needs_confirm && result.preview) {
          setPending({ args: { id: card.id, intent, target, confirm: true }, preview: result.preview });
          return;
        }
        handleResult(result);
      } catch (err) {
        toast.push(`Action failed: ${(err as Error).message}`, "error");
      }
      return;
    }

    try {
      const result = await doAction({ id: card.id, intent, target });
      handleResult(result);
    } catch (err) {
      toast.push(`Action failed: ${(err as Error).message}`, "error");
    }
  }

  function handleResult(result: ActionResult) {
    if (result.toast) toast.push(result.toast, result.ok ? "info" : "error");
    if (result.followup) setFollowup(result.followup);
  }

  async function commitPending() {
    if (!pending) return;
    try {
      const result = await doAction(pending.args);
      handleResult(result);
    } catch (err) {
      toast.push(`Action failed: ${(err as Error).message}`, "error");
    } finally {
      setPending(null);
    }
  }

  function dismissCard() {
    if (dismissAction) {
      dispatch(dismissAction);
    } else {
      // No explicit dismiss action — fall back to the canonical intent.
      dispatch({ label: "Dismiss", intent: "dismiss", target: {} });
    }
  }

  return (
    <>
      <div
        className={clsx(
          "grid grid-cols-[20px_1fr_auto] gap-[18px] items-start p-5 rounded-z-md border bg-bg-card transition-colors",
          isInjected ? "animate-inject-in" : "animate-fade-up",
          isCritical
            ? "border-crit/25 bg-crit-soft"
            : isReplyReceived
              ? "border-accent/30 bg-bg-elev"
              : isPersonal
                ? "border-amber/30 bg-amber-soft"
                : "border-line",
          onOpen && "group hover:border-accent/30",
        )}
      >
        {/* Source icon column */}
        <SourceIcon src={card.src} srcLabel={card.src_label} kind={card.kind} />

        {/* Body column — clickable when onOpen is provided. The handler
            skips clicks landing on interactive children so action
            buttons, dismiss X, and the expand toggle keep their own
            semantics. */}
        <div
          className={clsx("min-w-0", onOpen && "cursor-pointer")}
          onClick={
            onOpen
              ? (e) => {
                  if (
                    (e.target as HTMLElement).closest(
                      "button, a, input, textarea, select",
                    )
                  )
                    return;
                  onOpen(card);
                }
              : undefined
          }
        >
          {/* Title */}
          <h4 className="text-[15px] font-[500] leading-[1.45] tracking-[-0.005em] text-ink flex items-baseline gap-1">
            <span className="min-w-0">
              {renderMarkdown(card.title, { emClassName: "not-italic font-[600] text-ink" })}
            </span>
            {onOpen && (
              <span
                aria-hidden
                className="ml-2 font-mono text-[12px] text-ink-3 opacity-0 transition-all duration-150 group-hover:opacity-100 group-hover:translate-x-[2px] group-hover:-translate-y-[2px]"
              >
                ↗
              </span>
            )}
          </h4>

          {/* Sub */}
          {card.sub && (
            <p className="mt-1.5 text-[14px] text-ink-3 leading-[1.55] max-w-[60ch]">
              {renderMarkdown(card.sub, { emClassName: "not-italic font-[600] text-ink-2" })}
            </p>
          )}

          {/* Body — multi-paragraph elaboration on in-app ask cards. */}
          {card.body && (
            <div className="mt-3 text-[14px] text-ink-3 leading-[1.6] max-w-prose space-y-3">
              {card.body.split(/\n{2,}/).map((para, i) => (
                <p key={i}>
                  {renderMarkdown(para, { emClassName: "not-italic font-[600] text-ink-2" })}
                </p>
              ))}
            </div>
          )}

          {/* Meta row — pin + rel dot live here as quiet inline tokens.
              srcLabel leads (matches design) followed by any extra meta
              entries the synth produced. */}
          {(card.meta.length > 0 || card.pinned || card.src_label) && (
            <div className="mt-2 flex items-center gap-2.5 font-mono text-[11px] text-ink-4">
              {card.pinned && (
                <Pin className="h-3 w-3 text-accent fill-accent shrink-0" aria-label="Pinned" />
              )}
              <span className={clsx("h-[5px] w-[5px] rounded-full shrink-0", REL_DOT[card.rel])} aria-hidden />
              {card.src_label && <span>{card.src_label}</span>}
              {card.meta.map((m, i) => (
                <span key={i} className="flex items-center gap-2.5">
                  {(i > 0 || card.src_label) && (
                    <span className="h-[3px] w-[3px] rounded-full bg-ink-5" aria-hidden />
                  )}
                  <span>{m}</span>
                </span>
              ))}
            </div>
          )}

          {/* Actions row */}
          {(visibleActions.length > 0 || canAddTodo || hasExpand) && (
            <div className="mt-2.5 flex flex-wrap items-center gap-1.5">
              {visibleActions.map((a, i) => (
                <button
                  key={i}
                  type="button"
                  disabled={actionPending}
                  onClick={() => dispatch(a)}
                  className={clsx(
                    "h-[26px] px-3 rounded-z-sm text-[12px] font-[500] border transition disabled:opacity-50",
                    a.primary
                      ? "bg-ink text-bg border-transparent hover:bg-ink-2"
                      : "border-line text-ink-3 hover:bg-bg-elev",
                  )}
                >
                  {a.label}
                </button>
              ))}
              {canAddTodo && (
                <button
                  type="button"
                  disabled={actionPending}
                  onClick={() =>
                    dispatch({
                      label: "Add to todo",
                      intent: "add_task",
                      target: { title: card.title },
                    })
                  }
                  title="Track this card as a task"
                  className="h-[26px] inline-flex items-center gap-1 px-2.5 rounded-z-sm text-[12px] border border-dashed border-line text-ink-4 hover:bg-bg-elev hover:text-ink-2 transition disabled:opacity-50"
                >
                  <Plus className="h-3 w-3" />
                  Add to todo
                </button>
              )}
              {hasExpand && (
                <button
                  type="button"
                  onClick={() => setExpanded((v) => !v)}
                  className="h-[26px] px-3 rounded-z-sm text-[12px] border border-line text-ink-4 hover:bg-bg-elev hover:text-ink-3 transition"
                >
                  {expanded ? "Collapse" : "Why?"}
                </button>
              )}
            </div>
          )}

          {/* Expand content — spans body + dismiss columns */}
          {expanded && card.expand && (
            <div className="mt-3 pt-3 border-t border-line space-y-3 animate-fade-in">
              {Object.entries(card.expand).map(([k, v]) => (
                <div key={k}>
                  <h6 className="font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-1">{k}</h6>
                  <p className="text-[13px] text-ink-3 leading-relaxed">{v}</p>
                </div>
              ))}
            </div>
          )}

          {/* Trace */}
          {card.trace_id && <Trace traceId={card.trace_id} />}
        </div>

        {/* Right column: snooze (optional) stacked above dismiss X so the
            body column gets ~32px more width for the title and sub. */}
        <div className="flex flex-col items-center gap-1">
          {snoozeAction && (
            <button
              type="button"
              disabled={actionPending}
              onClick={() => dispatch(snoozeAction)}
              aria-label="Snooze"
              title="Snooze"
              className="h-[26px] w-[26px] rounded-[6px] grid place-items-center text-ink-4 border border-transparent hover:bg-bg-elev hover:text-ink hover:border-line transition disabled:opacity-50"
            >
              <Clock className="h-3 w-3" />
            </button>
          )}
          <button
            type="button"
            disabled={actionPending}
            onClick={dismissCard}
            aria-label="Dismiss"
            title="Dismiss"
            className="h-[26px] w-[26px] rounded-[6px] grid place-items-center text-ink-4 border border-transparent hover:bg-bg-elev hover:text-ink hover:border-line transition disabled:opacity-50"
          >
            <X className="h-3 w-3" />
          </button>
        </div>
      </div>

      {/* Inline ask_followup result. Renders below the source card so
          the user keeps the original card's context while reading the
          reactive answer. */}
      {followup && (
        <div className="mt-2 pl-4 border-l-2 border-accent/40">
          <Card card={followup} />
        </div>
      )}

      {pending && (
        <ActionConfirmModal
          intent={pending.args.intent}
          preview={pending.preview}
          pending={actionPending}
          onConfirm={commitPending}
          onCancel={() => setPending(null)}
        />
      )}
    </>
  );
}
