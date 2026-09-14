// @vitest-environment node
/**
 * The charts' reading of a draft.
 *
 * What these protect: a root seat is drawn in a unit only where the engine
 * placed it and the draft still names that unit, and a reference the engine
 * resolved to nothing is drawn at the root and marked; an inherited lead is
 * the engine's, and is never shown after the unit declares or clears its own;
 * a seat's handle, running identity and Datadog fallback are read from the
 * right source; and the reporting chart is the model's forest with every
 * seat under its current name, keyed by node, with the cycle group last.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { fromDocument, toDocument } from "./model/document.ts";
import type { Draft } from "./model/draft.ts";
import { COMPANY_KEY, seatKey, unitKey } from "./model/keys.ts";
import { INITIAL_BUILDER, type BuilderState } from "./model/reducer.ts";
import { fixtureCompany, fixtureDerived } from "./model/testkit.ts";
import { answered, checkedEdit, PLACED, record, run } from "./stateTestkit.ts";
import {
  chartInputs,
  CYCLE_GROUP,
  reporting,
  structure,
  type SeatView,
  type UnitView,
} from "./chartModel.ts";

const seatOf = (state: BuilderState, key: string) =>
  structure(chartInputs(state)).nodes.get(key) as SeatView;
const unitOf = (state: BuilderState, key: string) =>
  structure(chartInputs(state)).nodes.get(key) as UnitView;

describe("structure", () => {
  test("a root seat placed by its unit reference is drawn in that unit, after the unit's own seats", () => {
    const state = checkedEdit(fixtureCompany());
    const chart = structure(chartInputs(state));
    const company = chart.nodes.get(COMPANY_KEY);
    expect(company).toMatchObject({ type: "company", name: "Acme", seats: [seatKey("ceo")] });
    expect(unitOf(state, unitKey("Platform")).seats).toEqual([seatKey("sre"), seatKey("designer")]);
    expect(seatOf(state, seatKey("designer"))).toMatchObject({
      placedByRef: true,
      danglingUnitRef: null,
      parent: unitKey("Platform"),
    });
    // The tree the views navigate holds the same placement.
    const platform = chart.tree[0]!.children![1]!.children![2]!;
    expect(platform.id).toBe(unitKey("Platform"));
    expect(platform.children!.map((c) => c.id)).toEqual([seatKey("sre"), seatKey("designer")]);
  });

  test("a unit rename the reference followed keeps the seat in the unit", () => {
    const state = checkedEdit(fixtureCompany());
    const renamed = record(state, {
      type: "renameUnit",
      target: unitKey("Platform"),
      name: "Infrastructure",
    });
    expect(seatOf(renamed, seatKey("designer")).parent).toBe(unitKey("Platform"));
  });

  test("a reference the draft no longer holds is not drawn where the engine last placed it", () => {
    const state = checkedEdit(fixtureCompany());
    const moved = record(state, {
      type: "move",
      target: seatKey("designer"),
      to: { parent: COMPANY_KEY, after: seatKey("ceo") },
    });
    const designer = seatOf(moved, seatKey("designer"));
    expect(designer).toMatchObject({ parent: COMPANY_KEY, placedByRef: false });
    expect(unitOf(moved, unitKey("Platform")).seats).toEqual([seatKey("sre")]);
  });

  test("a reference the engine resolved to no unit is drawn at the root and marked", () => {
    const doc = fixtureCompany();
    doc.roles![1]!.unit = "Ghost";
    const state = checkedEdit(doc, {});
    expect(seatOf(state, seatKey("designer"))).toMatchObject({
      parent: COMPANY_KEY,
      placedByRef: false,
      danglingUnitRef: "Ghost",
    });
    // Without a derivation the chart claims nothing either way.
    const unchecked = run(INITIAL_BUILDER, {
      type: "load",
      mode: "edit",
      document: doc,
      revision: "rev-1",
    });
    const first = structure(chartInputs(unchecked));
    const designer = [...first.nodes.values()].find(
      (n): n is SeatView => n.type === "seat" && n.name === "Designer",
    )!;
    expect(designer).toMatchObject({ parent: COMPANY_KEY, danglingUnitRef: null });
  });

  test("an inherited lead is the engine's, and goes when the unit declares its own", () => {
    const doc = fixtureCompany();
    const state = checkedEdit(doc, {
      ...PLACED,
      units: { "units[0].children[0]": { lead: "vp-engineering", lead_inherited: true } },
    });
    expect(unitOf(state, unitKey("Platform"))).toMatchObject({
      lead: { name: "VP Engineering", inherited: true },
      inheritable: { name: "VP Engineering", inherited: true },
    });
    expect(unitOf(state, unitKey("Engineering"))).toMatchObject({
      lead: { name: "VP Engineering", inherited: false },
      inheritable: null,
    });

    const declared = record(state, { type: "setLead", target: unitKey("Platform"), lead: "SRE" });
    expect(unitOf(declared, unitKey("Platform"))).toMatchObject({
      lead: { name: "SRE", inherited: false },
      // With a lead of its own it would inherit its parent's again.
      inheritable: { name: "VP Engineering", inherited: true },
    });
  });

  test("a lead the check did not see is unknown rather than shown stale, down the whole chain", () => {
    const state = checkedEdit(fixtureCompany(), {
      ...PLACED,
      units: { "units[0].children[0]": { lead: "vp-engineering", lead_inherited: true } },
    });
    const cleared = record(state, { type: "setLead", target: unitKey("Engineering") });
    // The check saw a declared lead, so it says nothing about what the unit
    // inherits now, and Platform no longer inherits what the check saw.
    expect(unitOf(cleared, unitKey("Engineering")).lead).toBeUndefined();
    expect(unitOf(cleared, unitKey("Platform"))).toMatchObject({
      lead: undefined,
      inheritable: null,
    });
    const changed = record(state, { type: "setLead", target: unitKey("Engineering"), lead: "Dev" });
    expect(unitOf(changed, unitKey("Platform")).lead).toBeUndefined();
    // A unit the check never saw has no known inherited lead either.
    const added = record(state, {
      type: "addUnit",
      key: "new:u1",
      placement: { parent: unitKey("Sales"), after: null },
      data: { name: "Partners" },
    });
    expect(unitOf(added, "new:u1").lead).toBeUndefined();
    // A unit the check saw with no lead at all, and nothing above it, has none.
    expect(unitOf(state, unitKey("Sales")).lead).toBeNull();
  });

  test("a unit moved under another parent inherits from it, so the checked lead is unknown", () => {
    const state = checkedEdit(fixtureCompany(), {
      ...PLACED,
      units: { "units[0].children[0]": { lead: "vp-engineering", lead_inherited: true } },
    });
    // Every lead on Platform's new chain is the one the check saw (Sales and
    // Platform declare none, as before); only where Platform sits changed.
    const moved = record(state, {
      type: "move",
      target: unitKey("Platform"),
      to: { parent: unitKey("Sales"), after: null },
    });
    expect(unitOf(moved, unitKey("Platform"))).toMatchObject({
      lead: undefined,
      inheritable: null,
    });
    // A reorder among the same siblings moves no unit to another parent.
    const reordered = record(state, {
      type: "reorder",
      target: unitKey("Sales"),
      to: { parent: COMPANY_KEY, after: null },
    });
    expect(unitOf(reordered, unitKey("Platform")).lead).toEqual({
      name: "VP Engineering",
      inherited: true,
    });
  });

  test("a derived placement or dangling reference holds only while the seat still writes what was checked", () => {
    const doc = fixtureCompany();
    doc.roles!.push({ name: "Scout", unit: "Ghost" });
    const derived = fixtureDerived(doc, PLACED);
    const checkedDraft = fromDocument(doc, derived);
    const sent = toDocument(checkedDraft);
    const inputs = (draft: Draft) => ({ draft, baseDraft: checkedDraft, sent, derived });
    const rewrite = (data: Partial<Record<string, string>>) => ({
      ...checkedDraft,
      roles: checkedDraft.roles.map((s) =>
        s.key === seatKey("designer")
          ? { ...s, data: { ...s.data, unit: data.designer! } }
          : s.key === seatKey("scout")
            ? { ...s, data: { ...s.data, unit: data.scout! } }
            : s,
      ),
    });

    const asChecked = structure(inputs(checkedDraft)).nodes;
    expect(asChecked.get(seatKey("designer"))).toMatchObject({ parent: unitKey("Platform") });
    expect(asChecked.get(seatKey("scout"))).toMatchObject({ danglingUnitRef: "Ghost" });

    // Both references rewritten since the check (as a rebase onto somebody
    // else's edit can): the engine's verdicts were about other values.
    const since = structure(inputs(rewrite({ designer: "Sales", scout: "Platform" }))).nodes;
    expect(since.get(seatKey("designer"))).toMatchObject({
      parent: COMPANY_KEY,
      placedByRef: false,
      danglingUnitRef: null,
    });
    expect(since.get(seatKey("scout"))).toMatchObject({
      parent: COMPANY_KEY,
      danglingUnitRef: null,
    });
  });

  test("a seat's handle, running identity and Datadog fallback come from the right place", () => {
    const state = checkedEdit(fixtureCompany());
    expect(seatOf(state, seatKey("sre"))).toMatchObject({
      handle: "sre",
      saved: { handle: "sre", name: "SRE" },
      running: true,
      datadogFallback: true,
    });
    expect(seatOf(state, seatKey("dev")).datadogFallback).toBe(false);

    // Renamed in the draft, the seat still RUNS under its saved name.
    const renamed = record(state, { type: "renameSeat", target: seatKey("dev"), name: "Builder" });
    expect(seatOf(renamed, seatKey("dev"))).toMatchObject({
      name: "Builder",
      saved: { handle: "dev", name: "Dev" },
      running: true,
    });

    // Becoming human, it will not run at all.
    const human = record(state, {
      type: "changeKind",
      target: seatKey("dev"),
      kind: "human",
      contact: { slack: "U0DEV" },
    });
    expect(seatOf(human, seatKey("dev"))).toMatchObject({
      kind: "human",
      saved: { handle: "dev", name: "Dev" },
      running: false,
    });

    // A new seat has no saved identity, and no handle until a check reports one.
    const added = record(state, {
      type: "addSeat",
      key: "new:a1",
      placement: { parent: unitKey("Sales"), after: null },
      data: { name: "Closer" },
    });
    expect(seatOf(added, "new:a1")).toMatchObject({
      saved: null,
      running: false,
      handle: undefined,
    });
  });

  test("a unit's type is the one it writes, else the engine's while the check saw none written", () => {
    const doc = fixtureCompany();
    const state = checkedEdit(doc);
    expect(unitOf(state, unitKey("Engineering")).unitType).toBe("department");
    // Sales writes none, so the engine's effective type stands in.
    expect(unitOf(state, unitKey("Sales")).unitType).toBe("unit");
    // Cleared since the check: the check reported the type it wrote.
    const cleared = record(state, {
      type: "updateUnit",
      target: unitKey("Engineering"),
      set: [{ path: ["type"] }],
    });
    expect(unitOf(cleared, unitKey("Engineering")).unitType).toBe("");
  });

  test("a created seat's checked handle holds only while it is still called what the check saw", () => {
    const added = record(checkedEdit(fixtureCompany()), {
      type: "addSeat",
      key: "new:a1",
      placement: { parent: unitKey("Sales"), after: null },
      data: { name: "Closer" },
    });
    const checked = answered(added, {
      status: "clean",
      warnings: [],
      derived: fixtureDerived(toDocument(added.draft).document, PLACED),
    });
    expect(seatOf(checked, "new:a1").handle).toBe("closer");
    // The engine derives an undeclared handle from the name, so after a
    // rename the handle is the next check's to report.
    const renamed = record(checked, { type: "renameSeat", target: "new:a1", name: "Deal Closer" });
    expect(seatOf(renamed, "new:a1").handle).toBeUndefined();
    // The reporting chart, still drawn from that check, reads it the same way.
    expect(reporting(chartInputs(checked)).items.get("new:a1")?.handle).toBe("closer");
    expect(reporting(chartInputs(renamed)).items.get("new:a1")).toMatchObject({
      name: "Deal Closer",
      handle: undefined,
    });
    // A seat of the saved company keeps the handle its key carries, even
    // when the last check could not describe the draft at all.
    const saved = record(checked, { type: "renameSeat", target: seatKey("dev"), name: "Builder" });
    expect(seatOf(saved, seatKey("dev")).handle).toBe("dev");
    const unreachable = answered(checked, { status: "unreachable", detail: "offline" });
    expect(seatOf(unreachable, seatKey("sre")).handle).toBe("sre");
  });

  test("a manager is named as the draft names that seat now", () => {
    const doc = fixtureCompany();
    const state = checkedEdit(doc, {
      seats: { ...PLACED.seats, "units[0].roles[1]": { manager: "vp-engineering" } },
    });
    expect(seatOf(state, seatKey("dev")).manager).toBe("VP Engineering");
    expect(seatOf(state, seatKey("sre")).manager).toBeNull();
    const renamed = record(state, {
      type: "renameSeat",
      target: seatKey("vp-engineering"),
      name: "Head of Engineering",
    });
    expect(seatOf(renamed, seatKey("dev")).manager).toBe("Head of Engineering");
  });
});

describe("reporting", () => {
  test("is unknown until a check has described the draft", () => {
    const loaded = run(INITIAL_BUILDER, {
      type: "load",
      mode: "edit",
      document: fixtureCompany(),
      revision: "rev-1",
    });
    expect(reporting(chartInputs(loaded))).toMatchObject({ known: false, tree: [] });
  });

  test("is the forest by node key, with current names, and the cycle group last", () => {
    const doc: CompanyDocument = {
      name: "Loop",
      roles: [
        { name: "Chief", manages: ["Ops"] },
        { name: "Ops" },
        { name: "A", manages: ["B"] },
        { name: "B", manages: ["A"] },
      ],
    };
    const state = checkedEdit(doc, {
      seats: {
        "roles[1]": { manager: "chief" },
        "roles[2]": { manager: "b" },
        "roles[3]": { manager: "a" },
      },
    });
    const renamed = record(state, {
      type: "renameSeat",
      target: seatKey("ops"),
      name: "Operations",
    });
    const chart = reporting(chartInputs(renamed));
    expect(chart.known).toBe(true);
    // Its handle is read as the structure chart reads it: pinned by its key.
    expect(chart.items.get(seatKey("ops"))).toMatchObject({ name: "Operations", handle: "ops" });
    expect(chart.roots.map((r) => [r.id, r.root, r.reports.map((c) => c.name)])).toEqual([
      [seatKey("chief"), true, ["Operations"]],
    ]);
    expect(chart.cycles).toHaveLength(1);
    expect(chart.cycles[0]).toMatchObject({ key: seatKey("a"), cycleSize: 2 });
    expect(chart.tree.map((t) => t.id)).toEqual([seatKey("chief"), CYCLE_GROUP]);
    expect(chart.tree[1]!.children!.map((c) => c.id)).toEqual([seatKey("a")]);
    expect(chart.items.get(seatKey("b"))).toMatchObject({ cycleSize: 2, root: false });
  });
});
