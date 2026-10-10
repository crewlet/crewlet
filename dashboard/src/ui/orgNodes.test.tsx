/**
 * What the shared drawing of an organization's nodes says, on both charts of
 * one company: the words under a node's name and the hue a seat is toned in.
 */

import { describe, expect, test } from "vitest";
import { AGENT_TONE, chartKindOf, seatKindLabel, seatTone, unitTypeLabel } from "./orgNodes.tsx";

describe("what a node is called", () => {
  test("a seat says which of the two kinds it is", () => {
    expect(seatKindLabel({ kind: "agent" })).toBe("Agent seat");
    expect(seatKindLabel({ kind: "human" })).toBe("Human seat");
  });

  /*
   * A UNIT'S TYPE IS THE FOUNDER'S OWN WORD, capitalised in ONE place: written
   * out on both surfaces it drifted, and one draft read "Team" on the card and
   * "team" on the row beside it.
   */
  test("a unit's type is the founder's word, capitalised, or Unit for none", () => {
    expect(unitTypeLabel({ unitType: "team" })).toBe("Team");
    expect(unitTypeLabel({ unitType: "Guild" })).toBe("Guild");
    expect(unitTypeLabel({ unitType: "  " })).toBe("Unit");
    expect(unitTypeLabel({ unitType: "" })).toBe("Unit");
  });
});

describe("how a seat is toned", () => {
  test("an agent seat takes the agent hue and a person keeps the neutral surface", () => {
    expect(seatTone("agent")).toBe(AGENT_TONE);
    expect(seatTone(undefined)).toBe(AGENT_TONE);
    expect(seatTone("human")).toBeUndefined();
  });
});

describe("which chart a section param names", () => {
  test("reporting is named, and anything else is the structure", () => {
    expect(chartKindOf("reporting")).toBe("reporting");
    expect(chartKindOf("structure")).toBe("structure");
    expect(chartKindOf("")).toBe("structure");
    expect(chartKindOf("tree")).toBe("structure");
  });
});
