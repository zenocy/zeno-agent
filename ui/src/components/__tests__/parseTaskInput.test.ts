import { describe, it, expect } from "vitest";
import dayjs from "dayjs";
import { parseTaskInput } from "../parseTaskInput";

// Anchor every test at a known wall-time so day/time math is deterministic.
// 2026-05-09 is a Saturday; from there, Mon=2026-05-11, Thu=2026-05-14.
const NOW = dayjs("2026-05-09T08:00:00");

describe("parseTaskInput", () => {
  it("extracts a remind time without a day, anchored to today", () => {
    const got = parseTaskInput("remind me at 17:00 to call Lin", NOW);
    expect(got.title).toBe("call Lin");
    expect(got.due_date).toBe("2026-05-09");
    expect(got.fire_at).toMatch(/^2026-05-09T17:00:00/);
  });

  it("extracts due day + time", () => {
    const got = parseTaskInput("review deck by Thu 9am", NOW);
    expect(got.title).toBe("review deck");
    expect(got.due_date).toBe("2026-05-14"); // next Thursday from Sat 5/9
    expect(got.fire_at).toMatch(/^2026-05-14T09:00:00/);
  });

  it("extracts a bare trailing day word", () => {
    const got = parseTaskInput("call mom tomorrow", NOW);
    expect(got.title).toBe("call mom");
    expect(got.due_date).toBe("2026-05-10");
    expect(got.fire_at).toBeUndefined();
  });

  it("falls through with the title intact when no markers match", () => {
    const got = parseTaskInput("Renew passport", NOW);
    expect(got.title).toBe("Renew passport");
    expect(got.due_date).toBeUndefined();
    expect(got.fire_at).toBeUndefined();
  });

  it("handles a 'today' marker explicitly", () => {
    const got = parseTaskInput("call Lin today", NOW);
    expect(got.title).toBe("call Lin");
    expect(got.due_date).toBe("2026-05-09");
  });

  it("pushes a remind time that's already passed today to tomorrow", () => {
    const lateNow = dayjs("2026-05-09T20:00:00");
    const got = parseTaskInput("remind me at 17:00 to call Lin", lateNow);
    expect(got.title).toBe("call Lin");
    expect(got.due_date).toBe("2026-05-10");
    expect(got.fire_at).toMatch(/^2026-05-10T17:00:00/);
  });

  it("strips 'remember to' leader", () => {
    const got = parseTaskInput("remember to send tax doc", NOW);
    expect(got.title).toBe("send tax doc");
  });

  it("returns empty title for empty input", () => {
    expect(parseTaskInput("", NOW)).toEqual({ title: "" });
  });

  it("trims surrounding whitespace and collapses inner whitespace", () => {
    const got = parseTaskInput("  ship   the    PR  by Fri  ", NOW);
    expect(got.title).toBe("ship the PR");
    expect(got.due_date).toBe("2026-05-15"); // next Friday
  });
});
