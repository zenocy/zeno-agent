import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { SettingsPanel } from "../SettingsPanel";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

interface SettingsBody {
  timezone: string;
  city: string;
  country: string;
  latitude: number;
  longitude: number;
  set: boolean;
  geocode_error?: string;
}

function emptySettings(): SettingsBody {
  return {
    timezone: "",
    city: "",
    country: "",
    latitude: 0,
    longitude: 0,
    set: false,
  };
}

function populatedSettings(): SettingsBody {
  return {
    timezone: "Europe/Athens",
    city: "Athens",
    country: "Greece",
    latitude: 37.9838,
    longitude: 23.7275,
    set: true,
  };
}

describe("SettingsPanel", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  function mockGet(body: SettingsBody) {
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/api/settings" && (!init || init.method === undefined)) {
        return Promise.resolve({
          ok: true, status: 200, json: async () => body,
        } as Response);
      }
      return Promise.resolve({ ok: true, status: 200, json: async () => body } as Response);
    });
  }

  it("renders the first-time-setup label when no settings have been saved", async () => {
    mockGet(emptySettings());
    render(<SettingsPanel />, { wrapper });
    await waitFor(() =>
      expect(screen.getByText(/First-time setup/i)).toBeInTheDocument()
    );
    expect((screen.getByLabelText(/^City$/) as HTMLInputElement).value).toBe("");
    expect((screen.getByLabelText(/^Country$/) as HTMLInputElement).value).toBe("");
  });

  it("hydrates the form with current values when settings exist", async () => {
    mockGet(populatedSettings());
    render(<SettingsPanel />, { wrapper });
    await waitFor(() =>
      expect((screen.getByLabelText(/^City$/) as HTMLInputElement).value).toBe("Athens")
    );
    expect((screen.getByLabelText(/^Country$/) as HTMLInputElement).value).toBe("Greece");
    expect((screen.getByLabelText(/IANA timezone/i) as HTMLInputElement).value).toBe(
      "Europe/Athens"
    );
    // Coordinate readout is shown for transparency.
    expect(screen.getByText(/Resolved to 37\.9838/)).toBeInTheDocument();
  });

  it("Save is disabled until a field changes, then submits via PUT", async () => {
    mockGet(populatedSettings());
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/api/settings" && init?.method === "PUT") {
        return Promise.resolve({
          ok: true, status: 200,
          json: async () => ({ ...populatedSettings(), timezone: "America/New_York" }),
        } as Response);
      }
      return Promise.resolve({
        ok: true, status: 200, json: async () => populatedSettings(),
      } as Response);
    });

    const user = userEvent.setup();
    render(<SettingsPanel />, { wrapper });
    await waitFor(() =>
      expect((screen.getByLabelText(/IANA timezone/i) as HTMLInputElement).value).toBe(
        "Europe/Athens"
      )
    );
    const submit = screen.getByRole("button", { name: /Save changes/i });
    expect(submit).toBeDisabled();

    const tzInput = screen.getByLabelText(/IANA timezone/i);
    await user.clear(tzInput);
    await user.type(tzInput, "America/New_York");
    expect(submit).not.toBeDisabled();

    await user.click(submit);

    await waitFor(() => {
      const putCall = fetchMock.mock.calls.find((call) => {
        const [u, i] = call as [string, RequestInit | undefined];
        return u === "/api/settings" && i?.method === "PUT";
      });
      expect(putCall).toBeDefined();
      if (putCall) {
        const init = putCall[1] as RequestInit;
        expect(JSON.parse(init.body as string)).toMatchObject({
          timezone: "America/New_York",
          city: "Athens",
          country: "Greece",
        });
      }
    });
  });

  // When the backend saved settings but the geocoder failed, the response
  // includes geocode_error. The UI must surface it inline so the user
  // knows lat/lon weren't refreshed — without throwing or rolling back.
  it("surfaces geocode_error from a successful PUT response", async () => {
    mockGet(emptySettings());
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/api/settings" && init?.method === "PUT") {
        return Promise.resolve({
          ok: true, status: 200,
          json: async () => ({
            timezone: "UTC", city: "Atlantis", country: "Mu",
            latitude: 0, longitude: 0, set: true,
            geocode_error: "could not find that city/country",
          }),
        } as Response);
      }
      return Promise.resolve({
        ok: true, status: 200, json: async () => emptySettings(),
      } as Response);
    });

    const user = userEvent.setup();
    render(<SettingsPanel />, { wrapper });
    await screen.findByLabelText(/^City$/);
    await user.type(screen.getByLabelText(/^City$/), "Atlantis");
    await user.type(screen.getByLabelText(/^Country$/), "Mu");
    await user.type(screen.getByLabelText(/IANA timezone/i), "UTC");
    await user.click(screen.getByRole("button", { name: /Save changes/i }));

    await waitFor(() =>
      expect(screen.getByText(/could not find that city\/country/i)).toBeInTheDocument()
    );
  });

  // Regression: the Go API serializes an empty AllowedDMs slice as `null`
  // (not `[]`), which used to make the WhatsApp section's useEffect throw
  // when calling `.join("\n")` on null — and an uncaught effect error was
  // blanking the entire Settings page. Guard with `?? []`.
  it("renders even when /api/whatsapp/status returns allowed_dms: null", async () => {
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/api/settings" && (!init || init.method === undefined)) {
        return Promise.resolve({
          ok: true, status: 200, json: async () => populatedSettings(),
        } as Response);
      }
      if (url === "/api/whatsapp/status") {
        return Promise.resolve({
          ok: true, status: 200,
          json: async () => ({
            enabled: false, has_session: false, connected: false,
            logged_in: false, last_seen_at: "", paired_at: "",
            config: {
              mention_name: "zeno",
              allowed_dms: null,
              min_chat_interval_ms: 3000,
              max_concurrent_synth: 4,
              per_chat_buffer: 4,
            },
          }),
        } as Response);
      }
      return Promise.resolve({ ok: true, status: 200, json: async () => ({}) } as Response);
    });

    render(<SettingsPanel />, { wrapper });
    // The Settings header is unique to the panel — confirms the page
    // mounted and didn't blank out from an uncaught useEffect throw.
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /^Settings$/ })).toBeInTheDocument()
    );
  });

  it("hydrates and submits the world_clocks list", async () => {
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/api/settings" && init?.method === "PUT") {
        return Promise.resolve({
          ok: true, status: 200,
          json: async () => ({
            ...populatedSettings(),
            world_clocks: "America/Los_Angeles,Europe/London,Asia/Kolkata",
          }),
        } as Response);
      }
      if (url === "/api/settings") {
        return Promise.resolve({
          ok: true, status: 200,
          json: async () => ({
            ...populatedSettings(),
            world_clocks: "America/Los_Angeles,Europe/London",
          }),
        } as Response);
      }
      return Promise.resolve({ ok: true, status: 200, json: async () => ({}) } as Response);
    });

    const user = userEvent.setup();
    render(<SettingsPanel />, { wrapper });

    const textarea = (await screen.findByLabelText(
      /Timezones/i
    )) as HTMLTextAreaElement;
    // Hydrated as one IANA per line.
    expect(textarea.value).toBe("America/Los_Angeles\nEurope/London");

    await user.click(textarea);
    await user.keyboard("{End}\nAsia/Kolkata");
    await user.click(screen.getByRole("button", { name: /Save changes/i }));

    await waitFor(() => {
      const putCall = fetchMock.mock.calls.find((call) => {
        const [u, i] = call as [string, RequestInit | undefined];
        return u === "/api/settings" && i?.method === "PUT";
      });
      expect(putCall).toBeDefined();
      if (putCall) {
        const init = putCall[1] as RequestInit;
        expect(JSON.parse(init.body as string)).toMatchObject({
          world_clocks: "America/Los_Angeles,Europe/London,Asia/Kolkata",
        });
      }
    });
  });

  it("surfaces a network/server error from a failed PUT", async () => {
    mockGet(populatedSettings());
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/api/settings" && init?.method === "PUT") {
        return Promise.resolve({
          ok: false, status: 400,
          json: async () => ({ error: "invalid timezone" }),
        } as Response);
      }
      return Promise.resolve({
        ok: true, status: 200, json: async () => populatedSettings(),
      } as Response);
    });

    const user = userEvent.setup();
    render(<SettingsPanel />, { wrapper });
    await waitFor(() =>
      expect((screen.getByLabelText(/IANA timezone/i) as HTMLInputElement).value).toBe(
        "Europe/Athens"
      )
    );
    const tzInput = screen.getByLabelText(/IANA timezone/i);
    await user.clear(tzInput);
    await user.type(tzInput, "Not/A/Real/Zone");
    await user.click(screen.getByRole("button", { name: /Save changes/i }));

    await waitFor(() =>
      expect(screen.getByText(/invalid timezone/i)).toBeInTheDocument()
    );
  });
});
