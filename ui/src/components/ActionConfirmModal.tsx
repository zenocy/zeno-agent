import { useEffect, useRef } from "react";
import { X, Loader2 } from "lucide-react";

interface Props {
  intent: string;
  preview: Record<string, unknown> | undefined;
  pending: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

// ActionConfirmModal renders the V2.8 preview-then-commit UX. The
// server's preflight branch builds the artifact (draft body, calendar
// event details, ...) without writing; this modal shows it and asks
// the user to confirm before the second POST runs the commit branch.
export function ActionConfirmModal({ intent, preview, pending, onConfirm, onCancel }: Props) {
  const cancelRef = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    cancelRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
      if (e.key === "Enter" && !pending) onConfirm();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onCancel, onConfirm, pending]);

  const heading = headingFor(intent);
  const isSend = intent === "send_reply" || intent === "forward" || intent === "send_whatsapp";

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="action-confirm-title"
      className="fixed inset-0 z-40 flex items-center justify-center bg-black/40 p-4"
      onClick={(e) => {
        if (e.target === e.currentTarget) onCancel();
      }}
    >
      <div className="w-full max-w-lg rounded-z-md border border-line bg-bg p-5 space-y-4 shadow-xl">
        <div className="flex items-baseline justify-between">
          <h2
            id="action-confirm-title"
            className="font-display font-[500] text-[16px] text-ink"
          >
            {heading}
          </h2>
          <button
            ref={cancelRef}
            type="button"
            onClick={onCancel}
            aria-label="Close"
            className="h-7 w-7 rounded-z-sm flex items-center justify-center text-ink-4 hover:text-ink-3 hover:bg-bg-elev"
          >
            <X className="h-3.5 w-3.5" />
          </button>
        </div>

        <PreviewBody intent={intent} preview={preview} />

        {isSend && (
          <p className="text-[12px] text-crit leading-snug">
            This will send to{" "}
            <span className="font-[500]">
              {previewRecipients(preview).join(", ") || "the recipient"}
            </span>
            . Once sent it cannot be recalled.
          </p>
        )}

        <div className="flex justify-end gap-2 pt-2">
          <button
            type="button"
            onClick={onCancel}
            className="h-[28px] px-3 rounded-z-sm text-[12px] font-[500] border border-line text-ink-3 hover:bg-bg-elev"
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={onConfirm}
            disabled={pending}
            className="h-[28px] px-3 rounded-z-sm text-[12px] font-[500] bg-accent text-white hover:opacity-90 disabled:opacity-50 inline-flex items-center gap-1.5"
          >
            {pending && <Loader2 className="h-3 w-3 animate-spin" />}
            {confirmLabel(intent)}
          </button>
        </div>
      </div>
    </div>
  );
}

function headingFor(intent: string): string {
  switch (intent) {
    case "draft_reply":
      return "Save reply to Drafts";
    case "send_reply":
      return "Send reply";
    case "forward":
      return "Forward message";
    case "add_event":
      return "Add calendar event";
    case "reschedule_event":
      return "Reschedule event";
    case "cancel_event":
      return "Cancel event";
    case "send_whatsapp":
      return "Send WhatsApp message";
    default:
      return "Confirm action";
  }
}

function confirmLabel(intent: string): string {
  switch (intent) {
    case "draft_reply":
      return "Save draft";
    case "send_reply":
      return "Send";
    case "forward":
      return "Save forward";
    case "add_event":
      return "Add";
    case "reschedule_event":
      return "Reschedule";
    case "cancel_event":
      return "Cancel event";
    case "send_whatsapp":
      return "Send";
    default:
      return "Confirm";
  }
}

function PreviewBody({ intent, preview }: { intent: string; preview: Record<string, unknown> | undefined }) {
  // ModeConfirm intents (cancel_event today) don't carry a server-side
  // preview — render a confirmation copy instead.
  if (intent === "cancel_event") {
    return (
      <p className="text-[13px] text-ink-2 leading-relaxed">
        Cancel this event? The CalDAV record will be deleted; attendees may
        receive a cancellation notification depending on your server's policy.
      </p>
    );
  }
  if (!preview) return null;
  if (intent === "reschedule_event") {
    return (
      <div className="space-y-2 text-[13px] text-ink-2">
        <KV label="Title" value={String(preview.title ?? "")} />
        <KV label="New start" value={fmtTime(preview.new_start)} />
        <KV label="New end" value={fmtTime(preview.new_end)} />
      </div>
    );
  }
  if (intent === "add_event") {
    return (
      <div className="space-y-2 text-[13px] text-ink-2">
        <KV label="Title" value={String(preview.title ?? "")} />
        <KV label="Start" value={fmtTime(preview.start)} />
        <KV label="End" value={fmtTime(preview.end)} />
        {String(preview.location ?? "") !== "" && (
          <KV label="Where" value={String(preview.location)} />
        )}
        {String(preview.description ?? "") !== "" && (
          <KV label="Notes" value={String(preview.description)} />
        )}
      </div>
    );
  }
  if (intent === "send_whatsapp") {
    const isGroup = Boolean(preview.is_group);
    const asAssistant = Boolean(preview.as_assistant);
    const assistantName = String(preview.assistant_name ?? "");
    return (
      <div className="space-y-2 text-[13px]">
        {asAssistant && assistantName !== "" && (
          <div className="rounded-z-sm border border-accent/40 bg-accent/5 px-3 py-1.5 text-[12px] text-ink-2">
            From <span className="text-ink font-medium">{assistantName}</span> (your assistant)
          </div>
        )}
        <KV
          label={isGroup ? "Group" : "To"}
          value={String(preview.to_name ?? "")}
        />
        <pre className="whitespace-pre-wrap rounded-z-sm border border-line bg-bg-elev p-3 text-[12px] text-ink-2 max-h-72 overflow-y-auto">
          {String(preview.body ?? "")}
        </pre>
      </div>
    );
  }
  // mail-flavor intents
  return (
    <div className="space-y-2 text-[13px]">
      <KV label="To" value={listOrString(preview.to)} />
      <KV label="Subject" value={String(preview.subject ?? "")} />
      <pre className="whitespace-pre-wrap rounded-z-sm border border-line bg-bg-elev p-3 text-[12px] text-ink-2 max-h-72 overflow-y-auto">
        {String(preview.body ?? "")}
      </pre>
    </div>
  );
}

function KV({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex gap-2">
      <span className="w-16 shrink-0 text-ink-4 font-[500]">{label}</span>
      <span className="text-ink">{value}</span>
    </div>
  );
}

function listOrString(v: unknown): string {
  if (Array.isArray(v)) return v.map(String).join(", ");
  return String(v ?? "");
}

function previewRecipients(preview: Record<string, unknown> | undefined): string[] {
  if (!preview) return [];
  // V2.12 send_whatsapp surfaces the canonical contact name (no JID).
  if (typeof preview.to_name === "string" && preview.to_name.length > 0) {
    return [preview.to_name];
  }
  const to = preview.to;
  if (Array.isArray(to)) return to.map(String);
  if (typeof to === "string" && to.length > 0) return [to];
  return [];
}

function fmtTime(v: unknown): string {
  if (!v) return "";
  const s = String(v);
  // RFC3339 → "Thu, May 7, 17:00"
  const d = new Date(s);
  if (isNaN(d.getTime())) return s;
  return d.toLocaleString(undefined, {
    weekday: "short",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}
