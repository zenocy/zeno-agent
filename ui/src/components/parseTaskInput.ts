import dayjs from "dayjs";

// parseTaskInput extracts a title plus optional due_date / fire_at from
// freeform text. Ported from the V2 reference design at
// `Zeno V2/zeno-tasks.jsx`, adapted to the backend shape (no time-of-day
// on due_date — time-of-day is carried by fire_at).
//
// Recognized patterns:
//   "remind me at 17:00 to call Lin"      → title: "call Lin", fire_at: today 17:00
//   "review deck by Thu 9am"              → title: "review deck", due_date: <next Thu>, fire_at: <Thu 09:00>
//   "call mom tomorrow"                   → title: "call mom", due_date: tomorrow
//   "Renew passport"                      → title: "Renew passport"

export interface ParsedTaskInput {
  title: string;
  due_date?: string; // YYYY-MM-DD
  fire_at?: string;  // RFC3339, in local TZ
}

const DAY_NAMES: Record<string, number> = {
  sun: 0, sunday: 0,
  mon: 1, monday: 1,
  tue: 2, tuesday: 2,
  wed: 3, wednesday: 3,
  thu: 4, thursday: 4,
  fri: 5, friday: 5,
  sat: 6, saturday: 6,
};

// Resolve a day token ("today" | "tomorrow" | "mon".."sun"...) to a
// YYYY-MM-DD string, anchored at `from`. Picks the next occurrence
// (today if today, else strictly forward up to 7 days).
function resolveDay(token: string, from = dayjs()): string | null {
  const t = token.toLowerCase();
  if (t === "today") return from.format("YYYY-MM-DD");
  if (t === "tomorrow") return from.add(1, "day").format("YYYY-MM-DD");
  if (t in DAY_NAMES) {
    const target = DAY_NAMES[t];
    const cur = from.day();
    let delta = target - cur;
    if (delta < 0) delta += 7;
    if (delta === 0) delta = 0; // today if same day
    return from.add(delta, "day").format("YYYY-MM-DD");
  }
  return null;
}

// Normalize a time token ("17", "17:30", "9am", "9:30pm") to "HH:MM"
// 24-hour. Returns null if unrecognized.
function normalizeTime(raw: string): string | null {
  const t = raw.toLowerCase().trim();
  const m = t.match(/^([0-9]{1,2})(?::([0-9]{2}))?\s*(am|pm)?$/);
  if (!m) return null;
  let h = parseInt(m[1], 10);
  const min = m[2] ? parseInt(m[2], 10) : 0;
  const ampm = m[3];
  if (h < 0 || h > 23 || min < 0 || min > 59) return null;
  if (ampm === "am" && h === 12) h = 0;
  if (ampm === "pm" && h < 12) h += 12;
  return `${String(h).padStart(2, "0")}:${String(min).padStart(2, "0")}`;
}

// Combine a YYYY-MM-DD date and HH:MM time-of-day into a local-TZ
// RFC3339 string (e.g., "2026-05-09T17:00:00+02:00").
function combineDateTime(date: string, hhmm: string): string {
  return dayjs(`${date}T${hhmm}`).format();
}

const TIME_RE = "([0-9]{1,2}(?::[0-9]{2})?(?:am|pm)?)";
const DAY_RE = "(today|tomorrow|monday|tuesday|wednesday|thursday|friday|saturday|sunday|mon|tue|wed|thu|fri|sat|sun)";

export function parseTaskInput(text: string, now: dayjs.Dayjs = dayjs()): ParsedTaskInput {
  let title = text.trim();
  if (!title) return { title: "" };

  let due_date: string | undefined;
  let fire_at: string | undefined;
  let remindTime: string | null = null;

  // Pattern: "remind me at HH:MM to ..." — extracts the reminder time.
  const remindMatch = title.match(new RegExp(`\\bremind me (?:at|by) ${TIME_RE}\\b`, "i"));
  if (remindMatch) {
    remindTime = normalizeTime(remindMatch[1]);
    title = title.replace(remindMatch[0], "").trim();
  }

  // Pattern: "by/due/at <day>? <time>?" — captures whichever fragments are present.
  const dueMatch = title.match(
    new RegExp(`\\b(?:by|due|at)\\s+(?:${DAY_RE})?\\s*${TIME_RE}?\\b`, "i"),
  );
  let dueDay: string | null = null;
  let dueTime: string | null = null;
  if (dueMatch && (dueMatch[1] || dueMatch[2])) {
    dueDay = dueMatch[1] ? resolveDay(dueMatch[1], now) : null;
    dueTime = dueMatch[2] ? normalizeTime(dueMatch[2]) : null;
    title = title.replace(dueMatch[0], "").trim();
  }

  // Pattern: a bare leading/trailing day word ("call mom tomorrow", "tomorrow review deck").
  if (!dueDay) {
    const bareDay = title.match(new RegExp(`(?:^|\\s)${DAY_RE}(?:\\s|$)`, "i"));
    if (bareDay) {
      dueDay = resolveDay(bareDay[1], now);
      title = (title.slice(0, bareDay.index ?? 0) + title.slice((bareDay.index ?? 0) + bareDay[0].length)).trim();
    }
  }

  // Cleanup: strip leading verbs that survive the above strips.
  title = title.replace(/^(remind me|remember to)\s+/i, "");
  title = title.replace(/^to\s+/i, "");
  title = title.replace(/\s+/g, " ").trim();
  if (!title) title = text.trim();

  // Compose due_date and fire_at from the fragments we found.
  if (dueDay) due_date = dueDay;
  if (dueTime) {
    const day = dueDay ?? now.format("YYYY-MM-DD");
    fire_at = combineDateTime(day, dueTime);
    if (!due_date) due_date = day;
  }
  if (remindTime) {
    // "remind me at 17:00" sets fire_at; if no due day was given,
    // anchor to today (or tomorrow if the time has already passed).
    const day = dueDay ?? now.format("YYYY-MM-DD");
    let candidate = combineDateTime(day, remindTime);
    if (dayjs(candidate).isBefore(now) && !dueDay) {
      candidate = combineDateTime(now.add(1, "day").format("YYYY-MM-DD"), remindTime);
      if (!due_date) due_date = now.add(1, "day").format("YYYY-MM-DD");
    } else if (!due_date) {
      due_date = day;
    }
    fire_at = candidate;
  }

  return { title, due_date, fire_at };
}
