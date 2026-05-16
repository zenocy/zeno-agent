import {
  Mail,
  User,
  CalendarDays,
  CheckSquare,
  MessagesSquare,
  FileText,
  Users,
  Sun,
  type LucideIcon,
} from "lucide-react";

const ICON_BY_KEYWORD: Array<[RegExp, LucideIcon]> = [
  [/mail|email|gmail|inbox/i, Mail],
  [/family|personal|home/i, User],
  [/calendar|meeting|event/i, CalendarDays],
  [/task|todo/i, CheckSquare],
  [/slack|chat|message/i, MessagesSquare],
  [/crm|contact|people/i, Users],
  [/weather|forecast/i, Sun],
  [/doc|file|drive|note/i, FileText],
];

function pickIcon(...candidates: Array<string | undefined>): LucideIcon {
  for (const c of candidates) {
    if (!c) continue;
    for (const [re, Icon] of ICON_BY_KEYWORD) {
      if (re.test(c)) return Icon;
    }
  }
  return FileText;
}

interface Props {
  src?: string;
  srcLabel?: string;
  kind?: string;
}

export function SourceIcon({ src, srcLabel, kind }: Props) {
  const Icon = pickIcon(kind, src, srcLabel);
  return (
    <span
      className="h-[18px] w-[18px] rounded-[5px] border border-line bg-bg-elev grid place-items-center text-ink-3 shrink-0"
      aria-hidden
    >
      <Icon className="h-3 w-3" strokeWidth={1.5} />
    </span>
  );
}
