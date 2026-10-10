/**
 * A seat's avatar, read one way.
 *
 * The contract's lists are held to the engine by `internal/config`; this holds
 * them to the design system this build installs, so every character the
 * engine admits is one the kit can draw and every colour one it has a plate
 * for, in the same order the kit's own picker offers them.
 */

import { describe, expect, test } from "vitest";
import { CREWLET_CHARACTERS } from "@crewlethq/icons/characters";
import { NODE_HUES } from "@crewlethq/ui";

import { AVATAR_CHARACTERS, AVATAR_COLORS } from "../contract/avatar.ts";
import { DEFAULT_AVATAR, avatarOf } from "./avatar.ts";

describe("the avatar contract", () => {
  test("names exactly the characters the installed kit draws, in its order", () => {
    expect([...AVATAR_CHARACTERS]).toEqual([...CREWLET_CHARACTERS]);
  });

  test("names exactly the colours the installed kit has plates for, in its order", () => {
    expect([...AVATAR_COLORS]).toEqual([...NODE_HUES]);
  });
});

describe("avatarOf", () => {
  test("takes each part as written", () => {
    expect(avatarOf({ character: "hexlet", color: "cyan" })).toEqual({
      character: "hexlet",
      color: "cyan",
    });
  });

  test("draws a seat that chose nothing as the original Crewlet, in purple", () => {
    expect(DEFAULT_AVATAR).toEqual({ character: "crewlet", color: "purple" });
    expect(avatarOf(undefined)).toEqual(DEFAULT_AVATAR);
    expect(avatarOf({})).toEqual(DEFAULT_AVATAR);
  });

  test("keeps the default for the part a seat left out", () => {
    expect(avatarOf({ color: "rose" })).toEqual({ character: "crewlet", color: "rose" });
    expect(avatarOf({ character: "foxlet" })).toEqual({ character: "foxlet", color: "purple" });
  });

  test("draws a part this build cannot draw as the default part, never as nothing", () => {
    expect(avatarOf({ character: "starfish", color: "teal" })).toEqual(DEFAULT_AVATAR);
    expect(avatarOf({ character: 7, color: null })).toEqual(DEFAULT_AVATAR);
  });
});
