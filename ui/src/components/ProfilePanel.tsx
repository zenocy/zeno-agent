import { useState } from "react";
import clsx from "clsx";

import { MemoryPanel } from "./MemoryPanel";
import { ConcernsPanel } from "./ConcernsPanel";
import { ContactsPanel } from "./ContactsPanel";
import { AssistantPanel } from "./AssistantPanel";
import { SendsPanel } from "./SendsPanel";

type Tab = "memory" | "concerns" | "contacts" | "assistant" | "sends";

// ProfilePanel wraps MemoryPanel and ConcernsPanel under one Profile
// umbrella. Both surfaces are derived knowledge about the user — facts
// in Memory, threads in Concerns — so they share an icon and a header.
// Tab state is local; navigation off the Profile route resets to
// memory on return, which is fine for V2.5.
export function ProfilePanel() {
  const [tab, setTab] = useState<Tab>("memory");

  return (
    <div className="flex flex-col h-full">
      <div className="border-b border-line bg-bg sticky top-0 z-10">
        <div className="px-8 pt-6 pb-0 max-w-2xl mx-auto">
          <h1 className="font-mono text-[10px] uppercase tracking-wide text-ink-5 mb-3">
            Profile
          </h1>
          <div className="flex items-center gap-1">
            <TabButton
              label="Memory"
              active={tab === "memory"}
              onClick={() => setTab("memory")}
            />
            <TabButton
              label="Concerns"
              active={tab === "concerns"}
              onClick={() => setTab("concerns")}
            />
            <TabButton
              label="Contacts"
              active={tab === "contacts"}
              onClick={() => setTab("contacts")}
            />
            <TabButton
              label="Assistant"
              active={tab === "assistant"}
              onClick={() => setTab("assistant")}
            />
            <TabButton
              label="Sends"
              active={tab === "sends"}
              onClick={() => setTab("sends")}
            />
          </div>
        </div>
      </div>

      <div className="flex-1 overflow-y-auto">
        {tab === "memory" && <MemoryPanel />}
        {tab === "concerns" && <ConcernsPanel />}
        {tab === "contacts" && <ContactsPanel />}
        {tab === "assistant" && <AssistantPanel />}
        {tab === "sends" && <SendsPanel />}
      </div>
    </div>
  );
}

interface TabButtonProps {
  label: string;
  active: boolean;
  onClick: () => void;
}

function TabButton({ label, active, onClick }: TabButtonProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={clsx(
        "h-8 px-3 -mb-px text-[12px] font-mono uppercase tracking-wide border-b-2 transition",
        active
          ? "text-ink border-accent"
          : "text-ink-4 border-transparent hover:text-ink-3"
      )}
    >
      {label}
    </button>
  );
}
