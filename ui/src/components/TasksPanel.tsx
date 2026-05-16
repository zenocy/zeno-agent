import { useEffect, useMemo, useRef, useState } from "react";
import clsx from "clsx";
import dayjs from "dayjs";
import { X, Bell, Clock, Trash2, ChevronRight } from "lucide-react";

import { useTasks } from "../api/useTasks";
import { useTaskCreate } from "../api/useTaskCreate";
import { useTaskComplete } from "../api/useTaskComplete";
import { useTaskDelete } from "../api/useTaskDelete";
import { useTaskReminder } from "../api/useTaskReminder";
import { useTaskUpdate } from "../api/useTaskUpdate";
import type { OpenTask, TaskPriority } from "../types";
import { parseTaskInput } from "./parseTaskInput";

type Group = "today" | "tomorrow" | "this_week" | "sometime";

const GROUPS: { key: Group; label: string }[] = [
  { key: "today", label: "Today" },
  { key: "tomorrow", label: "Tomorrow" },
  { key: "this_week", label: "This week" },
  { key: "sometime", label: "Sometime" },
];

function bucket(t: OpenTask, today: string, tomorrow: string): Group {
  if (!t.due_date) return "sometime";
  if (t.due_date <= today) return "today"; // overdue collapses into today
  if (t.due_date === tomorrow) return "tomorrow";
  if (dayjs(t.due_date).diff(dayjs(today), "day") <= 7) return "this_week";
  return "sometime";
}

// dayLabel renders a YYYY-MM-DD date in the design's tiered style:
// "today" / "tomorrow" / "Wed" (within a week) / "May 7" (further out).
function dayLabel(day: string, today: string, tomorrow: string): string {
  if (day === today) return "today";
  if (day === tomorrow) return "tomorrow";
  if (dayjs(day).diff(dayjs(today), "day") <= 6) return dayjs(day).format("ddd");
  return dayjs(day).format("MMM D");
}

// displayDue renders the meta-line due token in the reference style:
// "today 17:00" / "tomorrow" / "Wed 17:00" / "May 7". HH:MM is appended
// from fire_at only when fire_at falls on the same calendar date as
// due_date (or as today for tasks with no due_date but a fire_at).
function displayDue(t: OpenTask, today: string, tomorrow: string): string {
  const day = t.due_date;
  if (!day) return "";
  let label = dayLabel(day, today, tomorrow);
  if (t.fire_at && dayjs(t.fire_at).format("YYYY-MM-DD") === day) {
    label += ` ${dayjs(t.fire_at).format("HH:mm")}`;
  }
  return label;
}

// formatFireAt renders a fire_at timestamp as the design's clock-time
// string ("10:30" today, "Wed 09:00" later this week, "May 7 12:00"
// further out). The reference UI never shows a relative ("in 2 hours")
// reminder — see Zeno V2/zeno-tasks.jsx:227.
function formatFireAt(fireAt: string, today: string, tomorrow: string): string {
  const d = dayjs(fireAt);
  const day = d.format("YYYY-MM-DD");
  const time = d.format("HH:mm");
  if (day === today) return time;
  return `${dayLabel(day, today, tomorrow)} ${time}`;
}

export function TasksPanel() {
  const { data, isLoading, isError } = useTasks();
  const tasks = useMemo(() => data ?? [], [data]);
  const today = dayjs().format("YYYY-MM-DD");
  const tomorrow = dayjs().add(1, "day").format("YYYY-MM-DD");

  const [showCreate, setShowCreate] = useState(false);
  const [reminderFor, setReminderFor] = useState<OpenTask | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editingDueId, setEditingDueId] = useState<string | null>(null);
  const [doneOpen, setDoneOpen] = useState(false);
  const [adding, setAdding] = useState("");

  const create = useTaskCreate();
  const complete = useTaskComplete();
  const remove = useTaskDelete();
  const remind = useTaskReminder();
  const update = useTaskUpdate();

  const open = useMemo(() => tasks.filter((t) => !t.completed), [tasks]);
  const doneToday = useMemo(
    () => tasks.filter((t) => t.completed && t.done_date === today),
    [tasks, today],
  );

  const grouped: Record<Group, OpenTask[]> = useMemo(() => {
    const acc: Record<Group, OpenTask[]> = { today: [], tomorrow: [], this_week: [], sometime: [] };
    for (const t of open) {
      acc[bucket(t, today, tomorrow)].push(t);
    }
    for (const g of Object.keys(acc) as Group[]) {
      acc[g].sort((a, b) => {
        const ad = a.due_date || "9999-12-31";
        const bd = b.due_date || "9999-12-31";
        return ad.localeCompare(bd);
      });
    }
    return acc;
  }, [open, today, tomorrow]);

  const dueToday = grouped.today.length;

  function submitCapture() {
    const text = adding.trim();
    if (!text) return;
    const parsed = parseTaskInput(text);
    create.mutate(
      {
        title: parsed.title,
        due: parsed.due_date,
        fire_at: parsed.fire_at,
      },
      {
        onSuccess: () => setAdding(""),
      },
    );
  }

  return (
    <div className="flex flex-col gap-[22px]">
      {/* Header — matches .focus-anchor-title (22px Fraunces 400) +
          .focus-anchor-sub (14px sans, ink-2). */}
      <header>
        <h2 className="font-display text-[22px] font-[400] leading-[1.25] tracking-[-0.005em] text-ink m-0 mb-1.5">
          Tasks
        </h2>
        <p className="text-[14px] leading-[1.55] text-ink-2 m-0">
          {isLoading
            ? "loading…"
            : `${open.length} open · ${doneToday.length} done today · ${dueToday} due today.`}
        </p>
      </header>

      {/* Capture row — bordered rounded container per .tasks-add (Zeno.html
          line 1207). Accent border on focus, accent + mark, solid ink "add"
          button. */}
      <div>
        <div className="flex items-center gap-2.5 border border-line rounded-[8px] px-3.5 py-2.5 bg-bg focus-within:border-accent transition-colors">
          <span className="font-mono text-accent font-medium text-[14px] leading-none select-none">+</span>
          <input
            type="text"
            value={adding}
            onChange={(e) => setAdding(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                submitCapture();
              }
            }}
            placeholder="Capture a task — try 'remind me at 17:00 to call Lin'"
            className="flex-1 bg-transparent text-[13.5px] text-ink placeholder:text-ink-4 focus:outline-none"
            aria-label="Capture a task"
          />
          {adding.trim() && (
            <button
              type="button"
              onClick={submitCapture}
              disabled={create.isPending}
              className="font-mono text-[11px] tracking-[0.04em] bg-ink text-bg rounded-[4px] px-2.5 py-1 hover:opacity-90 disabled:opacity-50"
            >
              add ↵
            </button>
          )}
        </div>
        <div className="flex justify-end mt-1.5">
          <button
            type="button"
            onClick={() => setShowCreate(true)}
            className="font-mono text-[10px] text-ink-5 hover:text-ink-3 transition"
          >
            Advanced…
          </button>
        </div>
        {create.isError && (
          <p className="font-mono text-[11px] text-amber mt-2">{create.error?.message}</p>
        )}
      </div>

      {isError && (
        <p className="font-mono text-[11px] text-amber py-4">Could not load tasks.</p>
      )}

      {!isLoading && !isError && tasks.length === 0 && (
        <p className="text-[13px] text-ink-4 py-2">
          No tasks yet — try the capture row above.
        </p>
      )}

      {/* Groups — head has bottom border (.tasks-group-head, line 1230). */}
      {GROUPS.map((g) => {
        const items = grouped[g.key];
        if (items.length === 0) return null;
        return (
          <section key={g.key} className="flex flex-col">
            <div className="flex items-baseline gap-2 pb-1.5 mb-0.5 border-b border-line font-mono text-[11px] uppercase tracking-[0.06em] text-ink-3">
              <span>{g.label}</span>
              <span className="text-ink-4">{items.length}</span>
            </div>
            <div className="flex flex-col">
              {items.map((t) => (
                <TaskRow
                  key={t.uid}
                  task={t}
                  today={today}
                  tomorrow={tomorrow}
                  isEditingTitle={editingId === t.uid}
                  isEditingDue={editingDueId === t.uid}
                  setEditingTitle={(b) => setEditingId(b ? t.uid : null)}
                  setEditingDue={(b) => setEditingDueId(b ? t.uid : null)}
                  onComplete={() => complete.mutate(t.uid)}
                  onDelete={() => remove.mutate(t.uid)}
                  onReminder={() => setReminderFor(t)}
                  onEditTitle={(title) => update.mutate({ uid: t.uid, title })}
                  onEditDue={(due_date) => update.mutate({ uid: t.uid, due_date })}
                  onQuickDue={() =>
                    update.mutate({ uid: t.uid, due_date: today })
                  }
                />
              ))}
            </div>
          </section>
        );
      })}

      {/* Done — today (.tasks-done has dashed group-head). */}
      {doneToday.length > 0 && (
        <section className="flex flex-col mt-4">
          <button
            type="button"
            onClick={() => setDoneOpen((v) => !v)}
            className="flex items-baseline gap-2 pb-1.5 mb-0.5 border-b border-dashed border-line font-mono text-[11px] uppercase tracking-[0.06em] text-ink-3 hover:text-ink-2 transition group"
          >
            <ChevronRight
              className={clsx(
                "h-3 w-3 text-ink-4 transition-transform",
                doneOpen && "rotate-90",
              )}
            />
            <span>Done · today</span>
            <span className="text-ink-4">{doneToday.length}</span>
          </button>
          {doneOpen && (
            <div className="flex flex-col">
              {doneToday.map((t) => (
                <TaskRow
                  key={t.uid}
                  task={t}
                  today={today}
                  tomorrow={tomorrow}
                  isEditingTitle={false}
                  isEditingDue={false}
                  setEditingTitle={() => {}}
                  setEditingDue={() => {}}
                  onComplete={() => complete.mutate(t.uid)}
                  onDelete={() => remove.mutate(t.uid)}
                  onReminder={() => setReminderFor(t)}
                  onEditTitle={() => {}}
                  onEditDue={() => {}}
                  onQuickDue={() => {}}
                />
              ))}
            </div>
          )}
        </section>
      )}

      {showCreate && (
        <CreateTaskModal
          onClose={() => setShowCreate(false)}
          onSubmit={(args) => {
            create.mutate(args, {
              onSuccess: () => setShowCreate(false),
            });
          }}
          submitting={create.isPending}
          error={create.error?.message}
        />
      )}

      {reminderFor && (
        <SetReminderModal
          task={reminderFor}
          onClose={() => setReminderFor(null)}
          onSubmit={(when) => {
            remind.mutate(
              { uid: reminderFor.uid, when },
              { onSuccess: () => setReminderFor(null) },
            );
          }}
          submitting={remind.isPending}
          error={remind.error?.message}
        />
      )}
    </div>
  );
}

interface TaskRowProps {
  task: OpenTask;
  today: string;
  tomorrow: string;
  isEditingTitle: boolean;
  isEditingDue: boolean;
  setEditingTitle: (b: boolean) => void;
  setEditingDue: (b: boolean) => void;
  onComplete: () => void;
  onDelete: () => void;
  onReminder: () => void;
  onEditTitle: (title: string) => void;
  onEditDue: (due_date: string) => void;
  onQuickDue: () => void;
}

function TaskRow({
  task,
  today,
  tomorrow,
  isEditingTitle,
  isEditingDue,
  setEditingTitle,
  setEditingDue,
  onComplete,
  onDelete,
  onReminder,
  onEditTitle,
  onEditDue,
  onQuickDue,
}: TaskRowProps) {
  const overdue = !task.completed && task.due_date && task.due_date < today;
  const hasMeta = !!task.due_date || (!!task.fire_at && !task.fired_at);

  // Reference: .task in Zeno.html (line 1239). 22px / 1fr / auto grid,
  // 10px y-padding, dashed bottom-border between rows.
  return (
    <div
      className={clsx(
        "group grid grid-cols-[22px_1fr_auto] gap-2.5 items-start py-2.5",
        "border-b border-dashed border-line last:border-b-0",
        task.completed && "opacity-70",
      )}
    >
      {/* Checkbox — 16px square per .task-check */}
      <button
        type="button"
        onClick={onComplete}
        disabled={task.completed}
        className={clsx(
          "mt-0.5 h-4 w-4 shrink-0 rounded-[4px] border flex items-center justify-center transition",
          task.completed
            ? "border-ink bg-ink text-bg"
            : "border-ink-3 bg-bg hover:border-accent",
        )}
        title={task.completed ? "Done" : "Mark complete"}
        aria-label={task.completed ? "Completed" : "Mark complete"}
      >
        {task.completed && <span className="text-[10px] leading-none">✓</span>}
      </button>

      {/* Main: title + meta */}
      <div className="flex flex-col gap-1 min-w-0">
        {isEditingTitle ? (
          <InlineEdit
            initial={task.title}
            placeholder="Task title"
            className="text-[14px]"
            onCommit={(v) => {
              const trimmed = v.trim();
              if (trimmed && trimmed !== task.title) onEditTitle(trimmed);
              setEditingTitle(false);
            }}
            onCancel={() => setEditingTitle(false)}
          />
        ) : (
          <button
            type="button"
            onClick={() => !task.completed && setEditingTitle(true)}
            className={clsx(
              "block text-left text-[14px] leading-[1.4] truncate w-full border-b border-transparent transition",
              overdue && "text-amber",
              !overdue && !task.completed && "text-ink hover:border-line",
              task.completed && "text-ink-3 line-through decoration-ink-4",
            )}
            title={task.title}
          >
            {task.title}
          </button>
        )}

        {hasMeta && (
          <div className="flex flex-wrap items-center gap-2.5 text-[11px] text-ink-3 tracking-[0.02em]">
            {task.due_date && (
              isEditingDue ? (
                <InlineEdit
                  initial={displayDue(task, today, tomorrow)}
                  placeholder="today, Mon, May 12"
                  className="text-[11px] font-mono w-[110px]"
                  onCommit={(v) => {
                    const parsed = parseTaskInput(v);
                    if (parsed.due_date !== undefined) {
                      onEditDue(parsed.due_date);
                    } else if (v.trim() === "") {
                      onEditDue("");
                    }
                    setEditingDue(false);
                  }}
                  onCancel={() => setEditingDue(false)}
                />
              ) : (
                <button
                  type="button"
                  onClick={() => !task.completed && setEditingDue(true)}
                  className={clsx(
                    "font-mono whitespace-nowrap border-b border-transparent transition hover:text-ink hover:border-line",
                    overdue && "text-amber",
                  )}
                >
                  {overdue ? "overdue" : displayDue(task, today, tomorrow)}
                </button>
              )
            )}
            {task.fire_at && !task.fired_at && (
              <span className="inline-flex items-center gap-1 text-accent whitespace-nowrap">
                <Bell className="h-3 w-3" strokeWidth={1.4} />
                {formatFireAt(task.fire_at, today, tomorrow)}
              </span>
            )}
          </div>
        )}
      </div>

      {/* Hover actions — 22px square buttons, design uses .task-icon-btn. */}
      <div className="opacity-0 group-hover:opacity-100 focus-within:opacity-100 transition flex items-center gap-0.5 shrink-0">
        {!task.completed && (
          <button
            type="button"
            onClick={() => (task.due_date ? setEditingDue(true) : onQuickDue())}
            className="h-[22px] w-[22px] rounded-[4px] flex items-center justify-center text-ink-4 hover:text-ink hover:bg-bg-elev transition"
            title={task.due_date ? "Edit due" : "Set due today"}
            aria-label={task.due_date ? "Edit due" : "Set due today"}
          >
            <Clock className="h-3 w-3" />
          </button>
        )}
        {!task.completed && !task.fire_at && (
          <button
            type="button"
            onClick={onReminder}
            className="h-[22px] w-[22px] rounded-[4px] flex items-center justify-center text-ink-4 hover:text-ink hover:bg-bg-elev transition"
            title="Set reminder"
            aria-label="Set reminder"
          >
            <Bell className="h-3 w-3" />
          </button>
        )}
        <button
          type="button"
          onClick={onDelete}
          className="h-[22px] w-[22px] rounded-[4px] flex items-center justify-center text-ink-4 hover:text-crit hover:bg-bg-elev transition"
          title="Delete"
          aria-label="Delete task"
        >
          <Trash2 className="h-3 w-3" />
        </button>
      </div>
    </div>
  );
}

interface InlineEditProps {
  initial: string;
  placeholder?: string;
  className?: string;
  onCommit: (value: string) => void;
  onCancel: () => void;
}

function InlineEdit({ initial, placeholder, className, onCommit, onCancel }: InlineEditProps) {
  const [val, setVal] = useState(initial);
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    ref.current?.focus();
    ref.current?.select();
  }, []);

  return (
    <input
      ref={ref}
      type="text"
      value={val}
      placeholder={placeholder}
      onChange={(e) => setVal(e.target.value)}
      onBlur={() => onCommit(val)}
      onKeyDown={(e) => {
        if (e.key === "Enter") {
          e.preventDefault();
          onCommit(val);
        } else if (e.key === "Escape") {
          e.preventDefault();
          onCancel();
        }
      }}
      className={clsx(
        "w-full bg-transparent border-b border-accent text-ink focus:outline-none text-[13px] leading-tight",
        className,
      )}
    />
  );
}

interface CreateTaskModalProps {
  onClose: () => void;
  onSubmit: (args: {
    title: string;
    due?: string;
    priority?: TaskPriority;
    tags?: string[];
  }) => void;
  submitting: boolean;
  error?: string;
}

function CreateTaskModal({ onClose, onSubmit, submitting, error }: CreateTaskModalProps) {
  const [title, setTitle] = useState("");
  const [due, setDue] = useState("");
  const [priority, setPriority] = useState<TaskPriority>("med");
  const [tags, setTags] = useState("");

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!title.trim()) return;
    onSubmit({
      title: title.trim(),
      due: due || undefined,
      priority,
      tags: tags
        .split(/[, ]+/)
        .map((t) => t.replace(/^#/, "").trim())
        .filter(Boolean),
    });
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-bg/80 backdrop-blur-sm" onClick={onClose}>
      <form
        onClick={(e) => e.stopPropagation()}
        onSubmit={handleSubmit}
        className="w-full max-w-md rounded-z-md border border-line bg-bg-card p-6 shadow-2xl"
      >
        <div className="flex items-center justify-between mb-4">
          <h3 className="font-display text-lg text-ink">New task</h3>
          <button
            type="button"
            onClick={onClose}
            className="h-7 w-7 rounded-sm flex items-center justify-center text-ink-4 hover:text-ink-2"
            aria-label="Close"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        <label className="block mb-3">
          <span className="block font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-1">Title</span>
          <input
            type="text"
            autoFocus
            required
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            placeholder="What needs doing?"
            className="w-full rounded-sm border border-line bg-bg px-3 py-2 text-[13px] text-ink focus:outline-none focus:border-accent"
          />
        </label>

        <div className="grid grid-cols-2 gap-3 mb-3">
          <label className="block">
            <span className="block font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-1">Due</span>
            <input
              type="date"
              value={due}
              onChange={(e) => setDue(e.target.value)}
              className="w-full rounded-sm border border-line bg-bg px-3 py-2 text-[13px] text-ink focus:outline-none focus:border-accent"
            />
          </label>
          <label className="block">
            <span className="block font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-1">Priority</span>
            <select
              value={priority}
              onChange={(e) => setPriority(e.target.value as TaskPriority)}
              className="w-full rounded-sm border border-line bg-bg px-3 py-2 text-[13px] text-ink focus:outline-none focus:border-accent"
            >
              <option value="low">Low</option>
              <option value="med">Medium</option>
              <option value="high">High</option>
            </select>
          </label>
        </div>

        <label className="block mb-4">
          <span className="block font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-1">Tags</span>
          <input
            type="text"
            value={tags}
            onChange={(e) => setTags(e.target.value)}
            placeholder="comma or space separated, e.g. work, deep"
            className="w-full rounded-sm border border-line bg-bg px-3 py-2 text-[13px] text-ink focus:outline-none focus:border-accent"
          />
        </label>

        {error && <p className="text-[12px] text-amber mb-3">{error}</p>}

        <div className="flex justify-end gap-2">
          <button
            type="button"
            onClick={onClose}
            className="rounded-sm border border-line px-3 py-1.5 text-[12px] text-ink-3 hover:text-ink hover:border-ink-4"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={submitting || !title.trim()}
            className="rounded-sm bg-accent text-bg px-3 py-1.5 text-[12px] font-medium disabled:opacity-50"
          >
            {submitting ? "Adding…" : "Add task"}
          </button>
        </div>
      </form>
    </div>
  );
}

interface SetReminderModalProps {
  task: OpenTask;
  onClose: () => void;
  onSubmit: (when: string) => void;
  submitting: boolean;
  error?: string;
}

const RELATIVE_PRESETS: { label: string; value: string }[] = [
  { label: "In 15 min", value: "+15m" },
  { label: "In 1 hour", value: "+1h" },
  { label: "In 4 hours", value: "+4h" },
  { label: "Tomorrow", value: "+1d" },
];

function SetReminderModal({ task, onClose, onSubmit, submitting, error }: SetReminderModalProps) {
  const [mode, setMode] = useState<"relative" | "absolute">("relative");
  const [relative, setRelative] = useState("+1h");
  const [absolute, setAbsolute] = useState("");

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (mode === "relative") {
      onSubmit(relative);
    } else if (absolute) {
      const local = new Date(absolute);
      onSubmit(local.toISOString());
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-bg/80 backdrop-blur-sm" onClick={onClose}>
      <form
        onClick={(e) => e.stopPropagation()}
        onSubmit={handleSubmit}
        className="w-full max-w-md rounded-z-md border border-line bg-bg-card p-6 shadow-2xl"
      >
        <div className="flex items-center justify-between mb-2">
          <h3 className="font-display text-lg text-ink">Set reminder</h3>
          <button
            type="button"
            onClick={onClose}
            className="h-7 w-7 rounded-sm flex items-center justify-center text-ink-4 hover:text-ink-2"
            aria-label="Close"
          >
            <X className="h-4 w-4" />
          </button>
        </div>
        <p className="font-mono text-[11px] text-ink-4 mb-4 truncate" title={task.title}>
          {task.title}
        </p>

        <div className="flex gap-1 mb-4">
          <button
            type="button"
            onClick={() => setMode("relative")}
            className={clsx(
              "flex-1 rounded-sm border px-3 py-1.5 text-[12px] transition",
              mode === "relative"
                ? "border-accent text-ink bg-bg-elev"
                : "border-line text-ink-4 hover:text-ink-2",
            )}
          >
            Relative
          </button>
          <button
            type="button"
            onClick={() => setMode("absolute")}
            className={clsx(
              "flex-1 rounded-sm border px-3 py-1.5 text-[12px] transition",
              mode === "absolute"
                ? "border-accent text-ink bg-bg-elev"
                : "border-line text-ink-4 hover:text-ink-2",
            )}
          >
            Specific time
          </button>
        </div>

        {mode === "relative" ? (
          <div className="grid grid-cols-2 gap-2 mb-4">
            {RELATIVE_PRESETS.map((p) => (
              <button
                type="button"
                key={p.value}
                onClick={() => setRelative(p.value)}
                className={clsx(
                  "rounded-sm border px-3 py-2 text-[12px] transition",
                  relative === p.value
                    ? "border-accent text-ink bg-bg-elev"
                    : "border-line text-ink-3 hover:text-ink hover:border-ink-4",
                )}
              >
                {p.label}
              </button>
            ))}
          </div>
        ) : (
          <label className="block mb-4">
            <span className="block font-mono text-[10px] uppercase tracking-wide text-ink-4 mb-1">When</span>
            <input
              type="datetime-local"
              value={absolute}
              onChange={(e) => setAbsolute(e.target.value)}
              className="w-full rounded-sm border border-line bg-bg px-3 py-2 text-[13px] text-ink focus:outline-none focus:border-accent"
            />
          </label>
        )}

        {error && <p className="text-[12px] text-amber mb-3">{error}</p>}

        <div className="flex justify-end gap-2">
          <button
            type="button"
            onClick={onClose}
            className="rounded-sm border border-line px-3 py-1.5 text-[12px] text-ink-3 hover:text-ink hover:border-ink-4"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={submitting || (mode === "absolute" && !absolute)}
            className="rounded-sm bg-accent text-bg px-3 py-1.5 text-[12px] font-medium disabled:opacity-50"
          >
            {submitting ? "Setting…" : "Set reminder"}
          </button>
        </div>
      </form>
    </div>
  );
}
