// @vitest-environment node
/**
 * The structure and reporting charts, as data.
 *
 * What these protect: a seat is drawn where the draft holds it, which is where
 * the chart's rows put it; a unit's lead and channel are the ones the engine's
 * cascade resolves — declared, else the parent's resolved — and a lead naming
 * no seat is marked on the unit that writes it; the engine's derivation is the
 * SAVED chart's, placed on the base, and a fact that follows from the whole
 * organization (a primary manager) is said only while the draft's chart is the
 * saved one; and the reporting forest is keyed by node with current names.
 */

import { describe, expect, test } from "vitest";
import type { ChartRead } from "~/protocol/index.ts";
import { COMPANY_KEY } from "./model/keys.ts";
import type { BuilderState } from "./model/reducer.ts";
import { chartOf, fixtureChart } from "./model/testkit.ts";
import {
  CYCLE_GROUP,
  chartInputs,
  datadogFallback,
  effectiveLeads,
  keyOfHandle,
  reporting,
  structure,
  type SeatView,
  type UnitView,
} from "./chartModel.ts";
import { checkedEdit, loadedState, record, withDerivation } from "./testState.ts";

const charts = (state: BuilderState) => {
  const inputs = chartInputs(state);
  return { structure: structure(inputs), reporting: reporting(inputs) };
};
const unitView = (state: BuilderState, key: string) =>
  charts(state).structure.nodes.get(key) as UnitView;
const seatView = (state: BuilderState, key: string) =>
  charts(state).structure.nodes.get(key) as SeatView;

describe("structure", () => {
  test("every seat is drawn where the chart's rows put it, the company first", () => {
    const { structure: s } = charts(checkedEdit());
    const tree = s.tree[0]!;
    expect(tree.id).toBe(COMPANY_KEY);
    expect(tree.children!.map((c) => c.id)).toEqual(["seat:ceo", "unit:engineering", "unit:sales"]);
    expect((s.nodes.get("unit:engineering") as UnitView).seats).toEqual([
      "seat:dev",
      "seat:vp-engineering",
    ]);
    expect(seatView(checkedEdit(), "seat:sre").parent).toBe("unit:platform");
    // A move is drawn at once, before anything is saved.
    const moved = record(checkedEdit(), {
      type: "move",
      target: "seat:sre",
      to: { parent: "unit:sales" },
    });
    expect(seatView(moved, "seat:sre").parent).toBe("unit:sales");
  });

  test("a unit's lead is the one it declares, else the one its parent resolved to, down every chain", () => {
    const state = checkedEdit();
    expect(unitView(state, "unit:engineering").lead).toEqual({
      handle: "vp-engineering",
      name: "VP Engineering",
      inherited: false,
    });
    // Platform declares none, so it takes Engineering's.
    expect(unitView(state, "unit:platform").lead).toEqual({
      handle: "vp-engineering",
      name: "VP Engineering",
      inherited: true,
    });
    expect(unitView(state, "unit:sales").lead).toBeNull();
    // Declaring one replaces it, and what it would inherit without it is kept for the editor.
    const own = record(state, { type: "setLead", target: "unit:platform", lead: "sre" });
    expect(unitView(own, "unit:platform").lead).toMatchObject({ handle: "sre", inherited: false });
    expect(unitView(own, "unit:platform").inheritable).toMatchObject({
      handle: "vp-engineering",
      inherited: true,
    });
    // Moved under another parent, it inherits from that one.
    const moved = record(state, {
      type: "move",
      target: "unit:platform",
      to: { parent: "unit:sales" },
    });
    expect(unitView(moved, "unit:platform").lead).toBeNull();
  });

  test("the cascade restates the engine's: a channel inherits the same way, through units that name none", () => {
    const deep = chartOf({
      units: [
        { key: "a", name: "A", lead: "x", channel: "a-chan" },
        { key: "b", name: "B", parent: "a" },
        { key: "c", name: "C", parent: "b", channel: "c-chan" },
      ],
      seats: [{ handle: "x", name: "X", unit: "a" }],
    });
    const leads = effectiveLeads(loadedState(deep).draft);
    expect(leads.get("unit:b")).toEqual({
      lead: "x",
      leadInherited: true,
      channel: "a-chan",
      channelInherited: true,
    });
    expect(leads.get("unit:c")).toEqual({
      lead: "x",
      leadInherited: true,
      channel: "c-chan",
      channelInherited: false,
    });
  });

  test("a lead naming no seat is marked on the unit that writes it, and inherited by nobody", () => {
    const chart: ChartRead = chartOf({
      units: [
        { key: "a", name: "A", lead: "ghost" },
        { key: "b", name: "B", parent: "a" },
      ],
    });
    const state = checkedEdit(chart);
    expect(unitView(state, "unit:a")).toMatchObject({
      danglingLead: "ghost",
      danglingNote: "The lead ghost names no seat in this draft.",
      lead: { handle: "ghost", name: "ghost", inherited: false },
    });
    // The engine resolves an inherited lead that names nobody to nobody.
    expect(unitView(state, "unit:b").lead).toBeNull();
    // Control: once a seat takes that handle, the mark goes.
    const taken = record(state, {
      type: "addSeat",
      key: "new:g",
      placement: { parent: "unit:a" },
      data: { handle: "ghost", name: "Ghost" },
    });
    expect(unitView(taken, "unit:a").danglingLead).toBeNull();
  });

  test("a seat's saved handle, its running identity and the Datadog fallback come from the right place", () => {
    const state = record(checkedEdit(), {
      type: "updateSeat",
      target: "seat:sre",
      set: [{ path: ["handle"], value: "oncall" }],
    });
    // Its own screen and its live state are still found under the saved handle.
    expect(seatView(state, "seat:sre")).toMatchObject({
      handle: "oncall",
      saved: { handle: "sre", name: "SRE" },
      running: true,
      // The fallback followed the rename, so it is still this seat.
      datadogFallback: true,
    });
    const created = record(state, {
      type: "addSeat",
      key: "new:q",
      placement: { parent: COMPANY_KEY },
      data: { handle: "qa", name: "QA" },
    });
    expect(seatView(created, "new:q")).toMatchObject({ saved: null, running: false });
    const human = record(state, { type: "changeKind", target: "seat:dev", kind: "human" });
    expect(seatView(human, "seat:dev").running).toBe(false);
  });

  test("a unit's type is the one it writes, else the engine's while the saved unit writes none either", () => {
    const chart = chartOf({ units: [{ key: "u", name: "U" }] });
    const state = checkedEdit(chart, { units: { u: { type: "unit" } } });
    expect(unitView(state, "unit:u").unitType).toBe("unit");
    const typed = record(state, {
      type: "updateUnit",
      target: "unit:u",
      set: [{ path: ["type"], value: "team" }],
    });
    expect(unitView(typed, "unit:u").unitType).toBe("team");
  });

  test("a primary manager is the saved chart's, said only while the draft's chart is still the saved one", () => {
    const state = checkedEdit(fixtureChart(), {
      seats: { dev: { manager: "vp-engineering" }, "vp-engineering": { manager: "ceo" } },
    });
    expect(seatView(state, "seat:dev").manager).toBe("VP Engineering");
    expect(seatView(state, "seat:ceo").manager).toBeNull();
    // ANY EDIT makes the draft a chart no derivation describes, even one that
    // cannot move a manager: telling the two apart would be the engine's
    // rules written again, so the fact is said to be not derived yet.
    const renamed = record(state, {
      type: "renameSeat",
      target: "seat:vp-engineering",
      name: "Head of Engineering",
    });
    expect(seatView(renamed, "seat:dev").manager).toBeUndefined();
    // Control: without any derivation it is unknown too.
    expect(seatView(loadedState(), "seat:dev").manager).toBeUndefined();
  });

  test("a derivation that does not describe the saved chart is not placed at all", () => {
    const state = withDerivation(loadedState(), {});
    expect(chartInputs(state).known).toBe(true);
    const other = chartOf({ seats: [{ handle: "someone", name: "Someone" }] });
    const stale = { ...state, base: { ...state.base, derived: null } };
    expect(chartInputs(stale).known).toBe(false);
    expect(chartInputs(loadedState(other)).known).toBe(false);
  });
});

describe("reporting", () => {
  test("is unknown until the engine has described the saved chart", () => {
    const { reporting: r } = charts(loadedState());
    expect(r).toMatchObject({ known: false, roots: [], tree: [] });
  });

  test("is the forest by node key with current names, the cycle group last, and says whose lines it draws", () => {
    const state = checkedEdit(fixtureChart(), {
      seats: {
        dev: { manager: "vp-engineering" },
        "vp-engineering": { manager: "ceo" },
        designer: { manager: "sre" },
        sre: { manager: "designer" },
      },
    });
    const { reporting: r } = charts(state);
    expect(r).toMatchObject({ known: true, current: true });
    const ceo = r.roots.find((item) => item.key === "seat:ceo")!;
    expect(ceo.reports.map((item) => item.key)).toEqual(["seat:vp-engineering"]);
    expect(r.tree.at(-1)!.id).toBe(CYCLE_GROUP);
    expect(r.cycles.map((c) => c.cycleSize)).toEqual([2]);

    // A name changed since is shown current; the lines are the saved company's.
    const renamed = record(state, { type: "renameSeat", target: "seat:dev", name: "Developer" });
    const after = charts(renamed).reporting;
    expect(after.current).toBe(false);
    expect(after.items.get("seat:dev")?.name).toBe("Developer");
  });
});

describe("what every surface reads about a node", () => {
  test("the Datadog fallback is the engine's: only while Datadog is on, and trimmed", () => {
    expect(
      datadogFallback({ integrations: { datadog: { enabled: true, route_to: " sre " } } }),
    ).toBe("sre");
    expect(
      datadogFallback({ integrations: { datadog: { enabled: false, route_to: "sre" } } }),
    ).toBe(undefined);
    expect(datadogFallback({ integrations: { datadog: { enabled: true, route_to: " " } } })).toBe(
      undefined,
    );
  });

  test("a derived handle is read as the seat of the draft it names, through a rename since", () => {
    const state = checkedEdit();
    expect(keyOfHandle(state, "dev")).toBe("seat:dev");
    const renamed = record(state, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["handle"], value: "zed" }],
    });
    expect(keyOfHandle(renamed, "dev")).toBe("seat:dev");
    const removed = record(state, { type: "remove", target: "seat:dev" });
    expect(keyOfHandle(removed, "dev")).toBeUndefined();
  });
});
