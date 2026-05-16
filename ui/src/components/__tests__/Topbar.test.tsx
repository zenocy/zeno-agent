import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { Topbar } from "../Topbar";
import {
  publishLive,
  resetLiveBroker,
  type LiveEvent,
} from "../../api/liveBroker";

// jsdom (in this project's setup) doesn't expose window.matchMedia or a
// usable localStorage. The theme provider reads both during mount. Stub
// them so the Topbar (and its useTheme call) can render under test.
function stubBrowserAPIs() {
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  });
  const store = new Map<string, string>();
  Object.defineProperty(window, "localStorage", {
    writable: true,
    value: {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => void store.set(k, v),
      removeItem: (k: string) => void store.delete(k),
      clear: () => store.clear(),
      length: 0,
      key: () => null,
    },
  });
}

function withQuery(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

const startMorning: LiveEvent = {
  kind: "synth.started",
  run_id: "topbar-run",
  stage: "morning",
  date: "2026-04-29",
};

describe("Topbar with LiveSynthPanel", () => {
  beforeEach(() => {
    resetLiveBroker();
    stubBrowserAPIs();
  });

  afterEach(() => {
    resetLiveBroker();
    vi.unstubAllGlobals();
  });

  it("does not render the live panel when no synth is active", () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<Topbar />, { wrapper: withQuery(qc) });
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("mounts the LiveSynthPanel inside its own wrapper when synth.started fires", () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { container } = render(<Topbar />, { wrapper: withQuery(qc) });

    act(() => {
      publishLive(startMorning);
    });

    const panel = container.querySelector(".live-synth-panel");
    expect(panel).not.toBeNull();
    // The panel must be a descendant of the Topbar wrapper, not a sibling
    // attached elsewhere — pin the DOM hierarchy so layout stays correct.
    expect(container.firstChild?.contains(panel)).toBe(true);
    // The panel is rendered AFTER the row containing the time chip, so
    // the dissolve animation pushes downward.
    const eyebrowRow = panel?.querySelector(".trace-eb");
    expect(eyebrowRow?.textContent).toBe("working");
  });
});
