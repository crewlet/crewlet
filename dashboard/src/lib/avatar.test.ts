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
import { avatarOf, lookOf } from "./avatar.ts";

describe("the avatar contract", () => {
  test("names exactly the characters the installed kit draws, in its order", () => {
    expect([...AVATAR_CHARACTERS]).toEqual([...CREWLET_CHARACTERS]);
  });

  test("names exactly the colours the installed kit has plates for, in its order", () => {
    expect([...AVATAR_COLORS]).toEqual([...NODE_HUES]);
  });
});

// THE LOOK IS THE ENGINE'S: the dashboard draws what it is served and picks nothing.
describe("avatarOf", () => {
  test("takes a served look whose parts this build draws", () => {
    expect(avatarOf({ character: "hexlet", color: "cyan" })).toEqual({
      character: "hexlet",
      color: "cyan",
    });
  });

  test("is no look where a part is missing or one this build cannot draw", () => {
    expect(avatarOf(undefined)).toBeNull();
    expect(avatarOf({ color: "rose" })).toBeNull();
    expect(avatarOf({ character: "starfish", color: "teal" })).toBeNull();
  });
});

describe("lookOf", () => {
  const served = { character: "flasklet", color: "amber" } as const;

  test("lays the parts an author wrote over the served look", () => {
    expect(lookOf({ color: "rose" }, served)).toEqual({ character: "flasklet", color: "rose" });
    expect(lookOf({ character: "foxlet" }, served)).toEqual({
      character: "foxlet",
      color: "amber",
    });
    expect(lookOf(undefined, served)).toEqual(served);
  });

  test("is no look where neither the author nor the running seat says", () => {
    expect(lookOf({ color: "rose" }, undefined)).toBeNull();
    expect(lookOf(undefined, null)).toBeNull();
  });
});
