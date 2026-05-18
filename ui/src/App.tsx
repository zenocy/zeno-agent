import { useEffect, useMemo, useState } from "react";
import dayjs from "dayjs";
import {
  LayoutDashboard,
  Inbox,
  CalendarDays,
  CheckSquare,
  FileText,
  User,
  Settings,
  Activity,
  Archive,
  LogOut,
} from "lucide-react";
import { useBriefing } from "./api/useBriefing";
import { useCards } from "./api/useCards";
import { useAsk } from "./api/useAsk";
import { useTodayCalendar } from "./api/useTodayCalendar";
import { useTodayStream } from "./api/useTodayStream";
import { Briefing } from "./components/Briefing";
import { Card } from "./components/Card";
import { CardFocus } from "./components/CardFocus";
import { Topbar } from "./components/Topbar";
import { RightRail } from "./components/RightRail";
import { InputBar } from "./components/InputBar";
import { ProfilePanel } from "./components/ProfilePanel";
import { SettingsPanel } from "./components/SettingsPanel";
import { StatsPanel } from "./components/StatsPanel";
import { ArchivePanel } from "./components/ArchivePanel";
import type { Card as CardData, CalendarEvent } from "./types";

type Page = "briefing" | "archive" | "profile" | "settings" | "stats";

// Calendar and Tasks aren't pages in the design — they open the focus
// modal as anchor cards (Zeno V2/zeno-app.jsx:71–78). These synthetic
// cards stamp the kind the modal switches on.
const CALENDAR_ANCHOR: CardData = {
  id: "calendar_day",
  date: "",
  src: "calendar",
  src_label: "Calendar · today",
  rel: "med",
  kind: "calendar_day",
  title: "Today's calendar",
  sub: "",
  meta: [],
  actions: [],
};

const TASKS_ANCHOR: CardData = {
  id: "tasks_view",
  date: "",
  src: "tasks",
  src_label: "Tasks · all",
  rel: "med",
  kind: "tasks_view",
  title: "Tasks",
  sub: "",
  meta: [],
  actions: [],
};

function GridBackground() {
  return (
    <div
      aria-hidden
      className="fixed inset-0 z-0 pointer-events-none opacity-[0.35]"
      style={{
        backgroundImage:
          "linear-gradient(to right, var(--line) 1px, transparent 1px), linear-gradient(to bottom, var(--line) 1px, transparent 1px)",
        backgroundSize: "64px 64px",
        backgroundPosition: "-1px -1px",
        WebkitMaskImage: "radial-gradient(ellipse 80% 80% at 50% 40%, #000 30%, transparent 80%)",
        maskImage: "radial-gradient(ellipse 80% 80% at 50% 40%, #000 30%, transparent 80%)",
      }}
    />
  );
}

interface LeftNavProps {
  currentPage: Page;
  onNavigate: (page: Page) => void;
  onOpenAnchor: (card: CardData) => void;
}

function LeftNav({ currentPage, onNavigate, onOpenAnchor }: LeftNavProps) {
  // Items map to a Page when navigation is wired; placeholder items have
  // no binding and stay visually inert. Calendar and Tasks open the
  // focus modal as anchors (design intent — see Zeno V2/zeno-app.jsx
  // :71–78), they're not standalone pages.
  type NavItem = {
    icon: typeof LayoutDashboard;
    label: string;
    page?: Page;
    anchor?: CardData;
  };
  const navItems: NavItem[] = [
    { icon: LayoutDashboard, label: "Briefing", page: "briefing" },
    { icon: Inbox, label: "Inbox" },
    { icon: CalendarDays, label: "Calendar", anchor: CALENDAR_ANCHOR },
    { icon: CheckSquare, label: "Tasks", anchor: TASKS_ANCHOR },
    { icon: FileText, label: "Documents" },
    { icon: Archive, label: "Archive", page: "archive" },
    { icon: User, label: "Profile", page: "profile" },
    { icon: Activity, label: "Stats", page: "stats" },
  ];

  const navBtn = (active: boolean) =>
    `relative h-9 w-9 rounded-z-sm flex items-center justify-center transition ${
      active ? "text-ink" : "text-ink-4 hover:text-ink-3 hover:bg-bg-elev"
    }`;

  return (
    <aside className="flex flex-col items-center gap-1 py-4 border-r border-line z-10">
      {/* Logo */}
      <div className="h-8 w-8 rounded-[8px] flex items-center justify-center font-display font-[700] text-[17px] bg-ink text-bg mb-2">
        Z
      </div>

      {navItems.map(({ icon: Icon, label, page, anchor }) => {
        const active = page !== undefined && currentPage === page;
        const onClick = page
          ? () => onNavigate(page)
          : anchor
            ? () => onOpenAnchor(anchor)
            : undefined;
        return (
          <button
            key={label}
            type="button"
            title={label}
            aria-label={label}
            onClick={onClick}
            className={navBtn(active)}
          >
            {active && (
              <span
                aria-hidden
                className="absolute left-0 top-1.5 bottom-1.5 w-[2px] rounded-r-[2px] bg-accent"
              />
            )}
            <Icon className="h-4 w-4" />
          </button>
        );
      })}

      <div className="flex-1" />

      <button
        type="button"
        title="Settings"
        aria-label="Settings"
        onClick={() => onNavigate("settings")}
        className={navBtn(currentPage === "settings")}
      >
        {currentPage === "settings" && (
          <span
            aria-hidden
            className="absolute left-0 top-1.5 bottom-1.5 w-[2px] rounded-r-[2px] bg-accent"
          />
        )}
        <Settings className="h-4 w-4" />
      </button>

      <button
        type="button"
        title="Sign out"
        aria-label="Sign out"
        onClick={async () => {
          try {
            await fetch("/api/auth/logout", {
              method: "POST",
              credentials: "same-origin",
            });
          } finally {
            window.location.reload();
          }
        }}
        className={navBtn(false)}
      >
        <LogOut className="h-4 w-4" />
      </button>

      {/* Avatar */}
      <div className="h-8 w-8 rounded-full border border-line flex items-center justify-center font-mono text-[10px] text-ink-4 mt-1">
        Z
      </div>
    </aside>
  );
}

export default function App() {
  const { data: briefing, isLoading: briefingLoading } = useBriefing();
  const { data: cardsData, isLoading: cardsLoading } = useCards();
  const { data: todayEvents = [] } = useTodayCalendar();
  const ask = useAsk();
  useTodayStream();
  const [currentPage, setCurrentPage] = useState<Page>("briefing");
  const [rightRailHidden, setRightRailHidden] = useState(false);
  const [focusedCard, setFocusedCard] = useState<CardData | null>(null);

  // Pre-meeting pinned event: when the briefing is in pre_meeting state,
  // surface the next today's-calendar event whose start is within 15
  // minutes (or just started up to 5 minutes ago, so the card stays
  // visible right at the moment the user is walking into the room).
  const nextEvent = useMemo<CalendarEvent | null>(() => {
    if (briefing?.state !== "pre_meeting") return null;
    if (!todayEvents.length) return null;
    const now = dayjs();
    const upcoming = todayEvents
      .map((ev) => ({ ev, start: dayjs(ev.start) }))
      .filter(({ start }) => start.diff(now, "minute") <= 15)
      .filter(({ start }) => start.diff(now, "minute") >= -5)
      .sort((a, b) => a.start.valueOf() - b.start.valueOf());
    return upcoming[0]?.ev ?? null;
  }, [briefing?.state, todayEvents]);

  const allCards = cardsData?.cards ?? [];
  // V2.4 P3: ask cards arrive via SSE with `origin: "ask"` and live in
  // the same cards cache as morning/inject. Slice them apart for the
  // Generated section so the user's "I just asked something" surface
  // doesn't get mixed with the morning context cards.
  const askCards = allCards.filter((c) => c.origin === "ask");
  const contextCards = allCards.filter((c) => c.origin !== "ask");
  const briefingState = briefing?.state;

  // deep_work hides the side panel by default. Other states leave it visible.
  // The Topbar exposes a manual restore button while it is hidden.
  useEffect(() => {
    setRightRailHidden(briefingState === "deep_work");
  }, [briefingState]);

  function handleSubmit(query: string) {
    // V2.4 P3: no optimistic prepend. The InputBar clears on submit
    // (its own local state) and the LiveSynthPanel is the user's
    // feedback surface during the round-trip; the card itself lands
    // via SSE → cards cache → Generated section below.
    ask.mutate(query);
  }

  return (
    <div
      className={`h-screen grid bg-bg text-ink font-sans overflow-hidden ${
        rightRailHidden
          ? "grid-cols-[var(--left)_1fr_0px]"
          : "grid-cols-[var(--left)_1fr_var(--rail)]"
      }`}
    >
      <GridBackground />

      <LeftNav
        currentPage={currentPage}
        onNavigate={setCurrentPage}
        onOpenAnchor={setFocusedCard}
      />

      {/* Center */}
      <main className="flex flex-col overflow-hidden relative z-10">
        <Topbar
          state={briefingState}
          tension={briefing?.tension}
          rightRailHidden={rightRailHidden}
          onShowRightRail={() => setRightRailHidden(false)}
        />

        {/* Scrollable stage */}
        <div className="flex-1 overflow-y-auto">
          {currentPage === "briefing" && (
            <div className="px-12 pt-9 pb-40 max-w-[880px] mx-auto">
              <Briefing
                data={briefing}
                isLoading={briefingLoading}
                nextEvent={nextEvent}
                onBrief={(ev) =>
                  ask.mutate(
                    `Brief me on the ${ev.title} meeting at ${dayjs(ev.start).format("HH:mm")}.`,
                  )
                }
              />

              {/* Reactive cards (delivered via SSE after the user
                  submits an Ask). The pending placeholder shows during
                  the SSE round-trip; askCards renders once
                  card.appended fires. */}
              {(ask.isPending || askCards.length > 0) && (
                <div className="mb-6">
                  <h3 className="font-mono text-[11px] uppercase tracking-wide text-ink-4 mb-3">
                    Generated
                  </h3>
                  {ask.isPending && (
                    <div className="rounded-z-md border border-line bg-bg-card p-4 animate-pulse mb-3">
                      <span className="font-mono text-[10px] text-ink-5">Generated · just now</span>
                    </div>
                  )}
                  {askCards.length > 0 && (
                    <div className="space-y-3">
                      {askCards.map((card) => (
                        <Card key={card.id} card={card} onOpen={setFocusedCard} />
                      ))}
                    </div>
                  )}
                </div>
              )}

              {/* Card stack */}
              <div className="mb-4">
                <div className="flex items-center justify-between mb-3">
                  <h3 className="font-mono text-[11px] uppercase tracking-wide text-ink-4">
                    Context cards
                  </h3>
                  <div className="flex items-center gap-1.5 font-mono text-[10px] text-ink-5">
                    <span>sorted by priority</span>
                    {!cardsLoading && (
                      <>
                        <span>·</span>
                        <span>{contextCards.length} {contextCards.length === 1 ? "card" : "cards"}</span>
                      </>
                    )}
                  </div>
                </div>

                {cardsLoading && (
                  <div className="space-y-3">
                    {[...Array(3)].map((_, i) => (
                      <div key={i} className="h-24 rounded-z-md border border-line bg-bg-card opacity-50" />
                    ))}
                  </div>
                )}

                {!cardsLoading && contextCards.length === 0 && (
                  <p className="font-mono text-[11px] text-ink-5 py-4">No cards for today.</p>
                )}

                {!cardsLoading && contextCards.length > 0 && (
                  <div className="space-y-3">
                    {contextCards.map((card) => (
                      <Card key={card.id} card={card} onOpen={setFocusedCard} />
                    ))}
                  </div>
                )}
              </div>
            </div>
          )}

          {currentPage === "archive" && <ArchivePanel />}

          {currentPage === "profile" && <ProfilePanel />}

          {currentPage === "stats" && <StatsPanel />}

          {currentPage === "settings" && <SettingsPanel />}
        </div>

        {/* Floating InputBar (Briefing page only — memory has no input bar).
            The fade-mask sits behind the bar so cards scrolling underneath
            dissolve into the page background. */}
        {currentPage === "briefing" && (
          <>
            <div
              aria-hidden
              className="absolute bottom-0 left-0 right-0 pointer-events-none z-10"
              style={{
                height: 180,
                background:
                  "linear-gradient(to bottom, transparent 0%, var(--bg) 70%)",
              }}
            />

            <div className="absolute left-1/2 -translate-x-1/2 bottom-[18px] w-[min(880px,calc(100%-96px))] z-20">
              <InputBar
                onSubmit={handleSubmit}
                loading={ask.isPending}
                suggestion={briefing?.suggested_followup}
                state={briefingState}
              />
            </div>
          </>
        )}
      </main>

      <RightRail hidden={rightRailHidden} />

      {focusedCard && (
        <CardFocus
          card={focusedCard}
          onClose={() => setFocusedCard(null)}
        />
      )}
    </div>
  );
}
