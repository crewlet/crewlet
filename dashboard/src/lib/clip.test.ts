// @vitest-environment node

/**
 * A text held to a cap in bytes is cut at a whole character and marked, never
 * mid-character: the engine reads what is sent as UTF-8, and a cut through a
 * character's bytes, or between an emoji's two halves, is text it refuses.
 * Mutation: count the cap in characters, or leave the mark outside it, and a
 * cut text is past the cap.
 */

import { describe, expect, test } from "vitest";
import { bytesOf, clipUtf8 } from "./clip.ts";

describe("a text held to a cap in bytes", () => {
  test("is whole where it fits", () => {
    expect(clipUtf8("Applied to Brand.", 256)).toBe("Applied to Brand.");
    expect(clipUtf8("x".repeat(256), 256)).toBe("x".repeat(256));
  });

  test("is cut at a whole character, its mark within the cap", () => {
    const cut = clipUtf8("x".repeat(257), 256);
    expect(cut).toBe(`${"x".repeat(253)}…`);
    expect(bytesOf(cut)).toBe(256);
    // THREE BYTES A CHARACTER: 84 of them and the mark are 255 bytes, and an
    // 85th would be 258.
    const japanese = clipUtf8("設".repeat(100), 256);
    expect(japanese).toBe(`${"設".repeat(84)}…`);
    expect(bytesOf(japanese)).toBe(255);
  });

  test("never splits an emoji's two halves", () => {
    const cut = clipUtf8("😀".repeat(100), 256);
    expect(cut).toBe(`${"😀".repeat(63)}…`);
    expect([...cut].every((ch) => ch === "😀" || ch === "…")).toBe(true);
  });

  test("is nothing where not even the mark fits", () => {
    expect(clipUtf8("abcdef", 2)).toBe("");
  });
});
