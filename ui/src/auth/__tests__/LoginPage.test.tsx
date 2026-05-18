import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import LoginPage from "../LoginPage";

function fakeResponse(status: number, body: unknown = {}) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

describe("LoginPage", () => {
  let reloadSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    reloadSpy = vi.fn();
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...window.location, reload: reloadSpy },
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("posts credentials and reloads on 204", async () => {
    const fetchMock = vi.fn().mockResolvedValue(fakeResponse(204));
    vi.stubGlobal("fetch", fetchMock);

    render(<LoginPage />);
    await userEvent.type(screen.getByLabelText(/username/i), "alice");
    await userEvent.type(screen.getByLabelText(/password/i), "s3cret");
    await userEvent.click(screen.getByRole("button", { name: /sign in/i }));

    await waitFor(() => {
      expect(reloadSpy).toHaveBeenCalled();
    });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/auth/login",
      expect.objectContaining({
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: "alice", password: "s3cret" }),
      })
    );
  });

  it("renders an inline error on 401 and clears only the password", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(fakeResponse(401, { error: "invalid credentials" }));
    vi.stubGlobal("fetch", fetchMock);

    render(<LoginPage />);
    const username = screen.getByLabelText(/username/i) as HTMLInputElement;
    const password = screen.getByLabelText(/password/i) as HTMLInputElement;
    await userEvent.type(username, "alice");
    await userEvent.type(password, "wrong");
    await userEvent.click(screen.getByRole("button", { name: /sign in/i }));

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent(/invalid username or password/i);
    });
    expect(username.value).toBe("alice");
    expect(password.value).toBe("");
  });

  it("disables the submit button while the request is in flight", async () => {
    let resolveFetch: (r: Response) => void = () => {};
    const fetchMock = vi.fn().mockImplementation(
      () =>
        new Promise<Response>((resolve) => {
          resolveFetch = resolve;
        })
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<LoginPage />);
    await userEvent.type(screen.getByLabelText(/username/i), "alice");
    await userEvent.type(screen.getByLabelText(/password/i), "s3cret");

    const button = screen.getByRole("button", { name: /sign in/i });
    await userEvent.click(button);

    expect(screen.getByRole("button", { name: /signing in/i })).toBeDisabled();

    resolveFetch(fakeResponse(204));
  });
});
