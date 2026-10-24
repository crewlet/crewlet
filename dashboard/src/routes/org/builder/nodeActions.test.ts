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

import { describe, expect, test, vi } from "vitest";
import type { MenuEntry, MenuItem } from "@crewlethq/ui";
import type { BuilderApi, CompanyEdit } from "./BuilderContext.tsx";
import type { CompanyView, LeadView, UnitView } from "./chartModel.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { fixtureCompany } from "./model/testkit.ts";
import { editLabel, leadChipLabel, leadLabel, leadSentence, nodeMenu } from "./nodeActions.tsx";
import { keyedState, scopedState } from "./testState.ts";

/** A unit with the lead answer under test and nothing else that matters here. */
function unit(lead: LeadView | null | undefined): UnitView {
  return {
    type: "unit",
    key: "unit:engineering" as NodeKey,
    id: "engineering",
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

const led = (name: string, inherited = false): LeadView => ({ name, handle: "lead", inherited });

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

/*
 * THE COMPANY'S EDIT IS ITS PRINCIPLES. Its mission, vision and standing
 * policies are written on Company › Principles alone, so once the company
 * exists its Edit goes there, named for them, and the builder's one company
 * field, the name, is an entry of its own.
 */
describe("the company's menu", () => {
  const company: CompanyView = {
    type: "company",
    key: COMPANY_KEY,
    name: "Acme",
    seats: [],
    units: [],
  };

  function api(principles: CompanyEdit, state = keyedState(fixtureCompany())): BuilderApi {
    return {
      state,
      readOnly: false,
      principles,
      openEditor: vi.fn(),
      openCompanyRename: vi.fn(),
      openAdd: vi.fn(),
    } as unknown as BuilderApi;
  }

  const items = (menu: MenuEntry[]): MenuItem[] =>
    menu.filter((e): e is MenuItem => e.kind !== "separator");
  const entries = (menu: MenuEntry[]) => items(menu).map((e) => [e.label, e.disabled === true]);
  const item = (menu: MenuEntry[], label: string) => items(menu).find((e) => e.label === label)!;

  test("a reader who may write them is offered Edit principles and Rename company", () => {
    const at = api("write");
    const menu = nodeMenu(at, company, vi.fn());
    expect(entries(menu)).toEqual([
      ["Add unit", false],
      ["Add agent seat", false],
      ["Add human seat", false],
      ["Edit principles", false],
      ["Rename company", false],
    ]);
    item(menu, "Edit principles").onSelect();
    expect(at.openEditor).toHaveBeenCalledWith(COMPANY_KEY);
    item(menu, "Rename company").onSelect();
    expect(at.openCompanyRename).toHaveBeenCalledTimes(1);
  });

  test("a reader who may only read them is offered to open them, not to edit them", () => {
    expect(editLabel(api("read"), company)).toBe("Open principles");
    expect(entries(nodeMenu(api("read"), company, vi.fn()))).toContainEqual([
      "Open principles",
      false,
    ]);
  });

  // A LEAD'S DRAFT may open the principles to read them, and is refused the
  // rename with the reason every company change gets.
  test("a lead's draft refuses the rename, with the reason", () => {
    const menu = nodeMenu(
      api("read", scopedState(fixtureCompany(), "engineering")),
      company,
      vi.fn(),
    );
    expect(item(menu, "Rename company")).toMatchObject({
      disabled: true,
      description:
        "The company's name and settings are not part of Engineering: changing them takes Admin access.",
    });
  });

  // A COMPANY STILL BEING CREATED has no Principles page yet: its Edit is the
  // name form, and it has no separate rename.
  test("a company this draft is creating is edited like any other node", () => {
    const at = api(null);
    expect(editLabel(at, company)).toBe("Edit Acme");
    const labels = entries(nodeMenu(at, company, vi.fn())).map(([label]) => label);
    expect(labels).toContain("Edit");
    expect(labels).not.toContain("Rename company");
  });
});
