import { describe, it, expect } from "vitest";
import { splitMarkdownSegments } from "../../lib/markdown";

describe("splitMarkdownSegments", () => {
  it("returns a single text segment for a plain string", () => {
    expect(splitMarkdownSegments("Hello world")).toEqual([{ text: "Hello world" }]);
  });

  it("wraps a single *…* pair as em", () => {
    expect(splitMarkdownSegments("A *calm* start")).toEqual([
      { text: "A " },
      { em: "calm" },
      { text: " start" },
    ]);
  });

  it("handles multiple em segments", () => {
    expect(splitMarkdownSegments("*One* and *two*")).toEqual([
      { em: "One" },
      { text: " and " },
      { em: "two" },
    ]);
  });

  it("handles a leading em with no preceding text", () => {
    const result = splitMarkdownSegments("*Bold* opener");
    expect(result[0]).toEqual({ em: "Bold" });
    expect(result[1]).toEqual({ text: " opener" });
  });

  it("handles a trailing em with no following text", () => {
    const result = splitMarkdownSegments("End *here*");
    expect(result[0]).toEqual({ text: "End " });
    expect(result[1]).toEqual({ em: "here" });
  });

  it("returns a single text segment when there are no asterisks", () => {
    expect(splitMarkdownSegments("No markup at all")).toEqual([
      { text: "No markup at all" },
    ]);
  });
});
