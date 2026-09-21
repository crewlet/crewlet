/**
 * The grammar a room's name has to meet, checked while somebody types.
 *
 * TWO PROPERTIES, and they pull against each other. The form must refuse what
 * the engine refuses — before a round trip, so a person is told what is wrong
 * while the caret is still in the field — and it must refuse NOTHING ELSE,
 * because a name this client rejects is a name that cannot be created from
 * this dashboard at all, with no server answer anywhere to say why. So these
 * cases are the engine's own pattern (`^[a-z0-9][a-z0-9-]{0,63}$` over the
 * normalised form) read from both directions.
 */

import { describe, expect, test } from "vitest";

import { MAX_CHANNEL_NAME, nameRefusal, normalizeName, startable, validName } from "./name.ts";

describe("what the field accepts", () => {
  test("a plain name, a hyphenated one and a digit are all addresses", () => {
    for (const name of ["launch", "launch-2026", "q3", "a", "9"]) {
      expect(nameRefusal(name)).toBe("");
      expect(startable(name)).toBe(true);
    }
  });

  test("what somebody types is normalised rather than refused", () => {
    // A name is an ADDRESS: `#Launch` and `#launch ` mean one room. Refusing
    // the capital would be this client inventing a rule the engine does not
    // have — it lower-cases and trims before it arbitrates.
    expect(normalizeName("  Launch  ")).toBe("launch");
    expect(startable("  Launch  ")).toBe(true);
    expect(nameRefusal("Launch")).toBe("");
  });

  test("exactly sixty-four characters is a name, and sixty-five is not", () => {
    expect(validName("a".repeat(MAX_CHANNEL_NAME))).toBe(true);
    expect(startable("a".repeat(MAX_CHANNEL_NAME))).toBe(true);
    expect(startable("a".repeat(MAX_CHANNEL_NAME + 1))).toBe(false);
    expect(nameRefusal("a".repeat(MAX_CHANNEL_NAME + 1))).toMatch(/65 characters/);
  });
});

describe("what it refuses, and what it says", () => {
  test("a space is named as a space, with the hyphen that replaces it", () => {
    // "invalid name" tells somebody they are wrong without telling them what
    // right looks like. The first illegal character is the one thing the field
    // can point at.
    const said = nameRefusal("product launch");
    expect(said).toMatch(/space/i);
    expect(said).toMatch(/hyphen/i);
    expect(startable("product launch")).toBe(false);
  });

  test("any other character it will not take is quoted back", () => {
    expect(nameRefusal("launch!")).toContain("“!”");
    expect(nameRefusal("launch_2026")).toContain("“_”");
    expect(nameRefusal("lançar")).toContain("“ç”");
  });

  test("a leading hyphen is refused although every character is legal", () => {
    // The pattern's first class is narrower than its rest, and a name that
    // passed a character check alone would be refused by the engine with
    // nothing on the form having said so.
    expect(validName("-launch")).toBe(false);
    expect(nameRefusal("-launch")).toMatch(/starts with a letter or a digit/i);
  });

  test("an empty field is not a refusal, and is not a name either", () => {
    // Nothing is wrong with a form nobody has filled in yet: a dialog that
    // opens shouting at a person has refused them before they typed. What
    // stops the write is that there is nothing to send.
    expect(nameRefusal("")).toBe("");
    expect(nameRefusal("   ")).toBe("");
    expect(startable("")).toBe(false);
    expect(startable("   ")).toBe(false);
  });
});
