// @vitest-environment node
/**
 * What a node's own labels say, and the reason there are two of them for one
 * fact.
 *
 * What these protect: a unit's lead reads as an ANSWER on the surface that
 * asks for it, which is the name alone under a column headed "Lead" and the
 * word with the name on a chart node's pill, where nothing above it says what
 * the name is; and a lead is always an answer now — the lead a unit inherits
 * is read off the draft by the engine's own cascade, so there is no state in
 * which it waits for a check.
 */

import { describe, expect, test } from "vitest";
import type { LeadView, UnitView } from "./chartModel.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { leadChipLabel, leadLabel, leadSentence } from "./nodeActions.tsx";

/** A unit with the lead answer under test and nothing else that matters here. */
function unit(lead: LeadView | null): UnitView {
  return {
    type: "unit",
    key: "unit:engineering" as NodeKey,
    address: "engineering",
    name: "Engineering",
    unitType: "Department",
    lead,
    inheritable: null,
    danglingLead: null,
    danglingNote: undefined,
    seats: [],
    units: [],
    parent: COMPANY_KEY,
  };
}

const led = (name: string, inherited = false): LeadView => ({ handle: "vp", name, inherited });

describe("a unit's lead, as a table cell writes it", () => {
  /*
   * THE NAME ALONE, because the cell sits under a column already headed
   * "Lead": the prefix the chart's pill carries would write the word twice on
   * every row of the outline.
   */
  test("says the lead, and whether it was inherited rather than declared", () => {
    expect(leadLabel(unit(led("VP Engineering")))).toBe("VP Engineering");
    expect(leadLabel(unit(led("Ada", true)))).toBe("Ada (inherited)");
    expect(leadLabel(unit(null))).toBe("No lead");
  });
});

describe("a unit's lead, as a chart node's pill writes it", () => {
  /*
   * THE WORD AND THE NAME, which is what the console chart draws in this pill:
   * a pill along the bottom edge of a card has nothing above it saying what
   * the name IS.
   */
  test("says the word with the name, inherited or not", () => {
    expect(leadChipLabel(unit(led("VP Engineering")))).toBe("Lead: VP Engineering");
    expect(leadChipLabel(unit(led("Ada", true)))).toBe("Lead: Ada (inherited)");
  });

  /*
   * AN EMPTY PILL READS "Lead", that chart's own word for a unit with none:
   * the pill is drawn as an outline in that state, so the word invites the
   * reader to set one rather than stating an absence.
   */
  test("a unit with no lead reads as that chart's empty pill", () => {
    expect(leadChipLabel(unit(null))).toBe("Lead");
    // Control: it is never the name of somebody.
    expect(leadChipLabel(unit(null))).not.toBe(leadChipLabel(unit(led("Lead"))));
  });
});

describe("a unit's lead, as a screen reader hears it", () => {
  /*
   * ONE SENTENCE, and it is the only reading: the pill itself is drawn inside
   * an `aria-hidden` element, because pressing it opens the menu that SETS the
   * lead rather than stating it.
   */
  test("states the lead as a sentence", () => {
    expect(leadSentence(unit(led("VP Engineering")))).toBe("Lead: VP Engineering.");
    expect(leadSentence(unit(led("Ada", true)))).toBe("Lead: Ada (inherited).");
    expect(leadSentence(unit(null))).toBe("No lead.");
  });
});
