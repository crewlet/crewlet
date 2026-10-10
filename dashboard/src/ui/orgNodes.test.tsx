/**
 * What the shared drawing of an organization's nodes says, on both charts of
 * one company: the words under a node's name and the hue a seat is toned in.
 */

import { describe, expect, test } from "vitest";
import { render } from "@testing-library/react";
import { chartKindOf, seatKindLabel, seatMark, seatTone, unitTypeLabel } from "./orgNodes.tsx";

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

describe("how a seat is toned and marked", () => {
  /*
   * AN AGENT IS ITS OWN COLOUR on the chart, the colour its author chose and
   * its avatar wears everywhere else; one that chose none is purple, as every
   * agent was before a seat could choose. A person keeps the neutral surface
   * whatever is written for them.
   */
  test("an agent seat takes its own colour, purple where it names none, and a person none", () => {
    expect(seatTone("agent", { character: "hexlet", color: "cyan" })).toBe("cyan");
    expect(seatTone("agent", null)).toBe("purple");
    expect(seatTone(undefined, undefined)).toBe("purple");
    expect(seatTone("human", { character: "hexlet", color: "cyan" })).toBeUndefined();
  });

  test("an agent seat leads with its own character, and the original where it names none", () => {
    const drawn = (avatar: Parameters<typeof seatMark>[1]) =>
      render(<>{seatMark("agent", avatar).icon}</>)
        .container.querySelector("svg")
        ?.getAttribute("data-character");
    expect(drawn({ character: "foxlet", color: "rose" })).toBe("foxlet");
    expect(drawn(null)).toBe("crewlet");
    const person = render(<>{seatMark("human", { character: "foxlet", color: "rose" }).icon}</>);
    expect(person.container.querySelector("svg[data-character]")).toBeNull();
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
