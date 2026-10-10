// @vitest-environment node
/**
 * What a node's own labels say, and the reason there are two of them for one
 * fact.
 *
 * What these protect: a unit's lead reads as an ANSWER on the surface that
 * asks for it, which is the name alone under a column headed "Lead" and the
 * word with the name on a chart node's pill, where nothing above it says what
 * the name is; and the state the console chart has no idea of, a check of this
 * draft that has not answered yet, is never collapsed into the state that says
 * a unit declares no lead.
 */

import { describe, expect, test } from "vitest";
import type { BuilderApi } from "./BuilderContext.tsx";
import {
  chartInputs,
  structure,
  type LeadView,
  type SeatView,
  type UnitView,
} from "./chartModel.ts";
import { COMPANY_KEY, seatKey, seatPathKey, type NodeKey } from "./model/keys.ts";
import { INITIAL_BUILDER, type BuilderState } from "./model/reducer.ts";
import { fixtureCompany } from "./model/testkit.ts";
import { leadChipLabel, leadLabel, leadSentence, nodeMenu } from "./nodeActions.tsx";
import { recheck, run } from "./testState.ts";

/** A unit with the lead answer under test and nothing else that matters here. */
function unit(lead: LeadView | null | undefined): UnitView {
  return {
    type: "unit",
    key: "unit:engineering" as NodeKey,
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

const led = (name: string, inherited = false): LeadView => ({ name, inherited });

describe("a unit's lead, as a table cell writes it", () => {
  /*
   * THE NAME ALONE, because the cell sits under a column already headed
   * "Lead": the prefix the chart's pill carries would write the word twice on
   * every row of the outline.
   */
  test("says the lead, and whether it was inherited rather than declared", () => {
    expect(leadLabel(unit(led("VP Engineering")))).toBe("VP Engineering");
    expect(leadLabel(unit(led("Ada", true)))).toBe("Ada (inherited)");
  });

  test("says which of the two nothings it is", () => {
    expect(leadLabel(unit(null))).toBe("No lead");
    expect(leadLabel(unit(undefined))).toBe("Lead after the check");
  });
});

describe("a unit's lead, as a chart node's pill writes it", () => {
  /*
   * THE WORD AND THE NAME, which is what the console chart draws in this pill.
   * A chart node's pill stands on its own along the bottom edge of a card with
   * nothing above it saying what the name IS, so the name alone read as a
   * second caption rather than as the unit's lead.
   */
  test("says the word with the name, inherited or not", () => {
    expect(leadChipLabel(unit(led("VP Engineering")))).toBe("Lead: VP Engineering");
    expect(leadChipLabel(unit(led("Ada", true)))).toBe("Lead: Ada (inherited)");
  });

  /*
   * AND AN EMPTY PILL READS "Lead", that chart's own word for a unit with none:
   * the pill is drawn as an outline in that state, so the word is what invites
   * the reader to set one rather than a sentence about its absence.
   */
  test("a unit that declares no lead reads as that chart's empty pill", () => {
    expect(leadChipLabel(unit(null))).toBe("Lead");
  });

  /*
   * THE THIRD ANSWER IS THIS BUILDER'S OWN. That chart has two states and this
   * one has three, because the lead a unit inherits is derived by the engine
   * and no check of the current draft has answered yet just after an edit.
   * Written as "Lead" too, the pill would hide a check still out behind a unit
   * that declares nothing, which are different facts about the organization.
   */
  test("a check that has not answered is never the same as declaring none", () => {
    expect(leadChipLabel(unit(undefined))).toBe("Lead after the check");
    expect(leadChipLabel(unit(undefined))).not.toBe(leadChipLabel(unit(null)));
  });
});

describe("a unit's lead, as a screen reader hears it", () => {
  /*
   * ONE SENTENCE, and it is the only reading: the pill itself is drawn inside
   * an `aria-hidden` element, because pressing it opens the menu that SETS the
   * lead rather than stating it. So the sentence is not a second telling of
   * what the pill says, and the two may differ in wording without a reader
   * hearing the same fact twice.
   */
  test("states the lead as a sentence, with the two nothings kept apart", () => {
    expect(leadSentence(unit(led("VP Engineering")))).toBe("Lead: VP Engineering.");
    expect(leadSentence(unit(null))).toBe("No lead.");
    expect(leadSentence(unit(undefined))).toBe("Lead after the check.");
  });
});

describe("a saved seat's screen", () => {
  /** Where the node's "Open seat" goes, or `undefined` where none is offered. */
  const screenOf = (state: BuilderState, key: NodeKey): string[] | undefined => {
    const view = structure(chartInputs(state)).nodes.get(key) as SeatView;
    let opened: string[] | undefined;
    const entries = nodeMenu({ readOnly: false } as BuilderApi, view, (path) => {
      opened = path;
    });
    const open = entries.find((e) => e.key === "open") as { onSelect?: () => void } | undefined;
    open?.onSelect?.();
    return opened;
  };

  /*
   * BY THE HANDLE ALONE, which is all a seat's page resolves. Before the
   * engine has described the company, a seat declaring no handle is keyed by
   * its path and its handle is unknown — and it was offered a screen spelled
   * with its NAME, which the page answers "No seat called". The first check
   * names the handle, and the screen is offered by it.
   */
  test("is offered by the handle the engine derived, and not before it has", () => {
    const loaded = run(INITIAL_BUILDER, {
      type: "load",
      mode: "edit",
      document: fixtureCompany(),
      revision: "rev-1",
    });
    expect(screenOf(loaded, seatPathKey("roles[0]"))).toBeUndefined();
    expect(screenOf(recheck(loaded), seatKey("ceo"))).toEqual(["agents", "seats", "ceo"]);
  });
});
