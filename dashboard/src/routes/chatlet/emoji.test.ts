/**
 * A REACTION HAS ONE SPELLING, the engine's shortcode, and is drawn as its
 * glyph. Sent as the glyph, one emoji counted twice, once per spelling, and an
 * agent's reaction drew as ":+1:".
 */

import { EMOJI_GROUPS } from "@crewlethq/ui";
import { describe, expect, test } from "vitest";

import {
  emojiInWords,
  glyphOf,
  reactionName,
  reactionShown,
  shortcodeOf,
  SPELLED_GLYPHS,
} from "./emoji.ts";

describe("a reaction's spelling", () => {
  // A KIT BUMP THAT OFFERS A NEW EMOJI fails here until it has a shortcode,
  // rather than sending that one emoji as a glyph.
  test("every emoji the picker offers has a shortcode, each its own", () => {
    const offered = EMOJI_GROUPS.flatMap((group) => group.items.map((item) => item.emoji));
    expect([...new Set(offered)].sort()).toEqual([...SPELLED_GLYPHS].sort());
    const codes = SPELLED_GLYPHS.map(shortcodeOf);
    expect(codes.every((code) => /^:[^:\s]+:$/.test(code))).toBe(true);
    expect(new Set(codes).size).toBe(codes.length);
  });

  test("a stored shortcode is drawn as its glyph and read by the picker's name", () => {
    expect(shortcodeOf("👀")).toBe(":eyes:");
    expect(glyphOf(":+1:")).toBe("👍");
    expect(reactionShown(":+1:")).toBe("👍");
    expect(reactionName(":+1:")).toBe("thumbs up");
  });

  test("a shortcode the table does not know is drawn by its name, never its colons", () => {
    expect(glyphOf(":sparkles:")).toBeNull();
    expect(reactionShown(":sparkles:")).toBe("sparkles");
    expect(reactionName(":white_flower:")).toBe("white flower");
  });

  test("a glyph stored as such is drawn as itself", () => {
    expect(glyphOf("👀")).toBe("👀");
  });
});

test("a shortcode written in a message's words is drawn as its emoji, and an unknown one stays", () => {
  expect(emojiInWords("Shipped :tada: and :Rocket:")).toBe("Shipped 🎉 and 🚀");
  expect(emojiInWords("React with :one: for three")).toBe("React with :one: for three");
  expect(emojiInWords("at 10:30: done")).toBe("at 10:30: done");
});
