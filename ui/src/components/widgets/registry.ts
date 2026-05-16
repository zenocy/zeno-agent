import type { ComponentType } from "react";
import type { WidgetId } from "../../hooks/usePinnedWidgets";
import { WeatherWidget } from "./WeatherWidget";
import { StockWidget } from "./StockWidget";
import { WorldClockWidget } from "./WorldClockWidget";

export interface WidgetEntry {
  id: WidgetId;
  label: string;
  Component: ComponentType<{ onUnpin?: () => void }>;
}

export const WIDGET_CATALOG: WidgetEntry[] = [
  { id: "weather", label: "Weather", Component: WeatherWidget },
  { id: "stock", label: "Markets", Component: StockWidget },
  { id: "world-clock", label: "World clocks", Component: WorldClockWidget },
];

export function findWidget(id: WidgetId): WidgetEntry | undefined {
  return WIDGET_CATALOG.find((w) => w.id === id);
}
