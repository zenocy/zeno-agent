import { usePinnedWidgets } from "../hooks/usePinnedWidgets";
import { AddWidgetButton } from "./widgets/AddWidgetButton";
import { findWidget } from "./widgets/registry";
import { AttentionStream } from "./AttentionStream";

interface RightRailProps {
  hidden?: boolean;
}

export function RightRail({ hidden = false }: RightRailProps) {
  const { pinned, pin, unpin } = usePinnedWidgets();

  if (hidden) return null;

  return (
    <aside className="hidden lg:flex flex-col border-l border-line py-2 overflow-y-auto">
      <div className="px-5 pt-1 pb-2.5 flex justify-between items-baseline">
        <h3 className="m-0 text-[11px] font-mono text-ink-3 uppercase tracking-[0.08em] font-medium">
          Pinned
        </h3>
        <AddWidgetButton pinned={pinned} onPin={pin} />
      </div>
      <div className="px-4 pb-3.5 flex flex-col gap-2">
        {pinned.length === 0 && (
          <p className="px-1 text-[11px] text-ink-4 font-mono">no widgets pinned</p>
        )}
        {pinned.map((id) => {
          const entry = findWidget(id);
          if (!entry) return null;
          const Widget = entry.Component;
          return <Widget key={id} onUnpin={() => unpin(id)} />;
        })}
      </div>

      {/* Attention stream — replaces the old today timeline + open tasks split.
          Open tasks moved out of the rail entirely (the Tasks page is the
          canonical surface for those). */}
      <div className="border-t border-line">
        <AttentionStream />
      </div>
    </aside>
  );
}
