// @vitest-environment node
/**
 * The builder reducer.
 *
 * What these protect: a template can only start a company from nothing, never
 * replace one that exists; a reader the chart did not show the runtime half
 * cannot write one; the draft is always its base with the log replayed,
 * whatever sequence of actions produced it; every change moves the generation
 * and a check for another generation is ignored; the base is keyed by address
 * the moment it is read, and a derivation is kept only while it describes that
 * chart; a refused credential stops the log being kept; and an update or a
 * restore adopts a log only once every conflict is resolved.
 */

import { describe, expect, test } from "vitest";
import type { ChartRead, CompanyDocument } from "~/protocol/index.ts";
import { COMPANY_KEY } from "./keys.ts";
import { allSeats, locate } from "./draft.ts";
import { replay } from "./history.ts";
import { OPERATIONS_VERSION, type Intent } from "./operations.ts";
import type { CheckOutcome } from "./scheduler.ts";
import { templateIntent } from "./templates.ts";
import {
  builderReducer,
  checkTrigger,
  hasChanges,
  INITIAL_BUILDER,
  recordIntent,
  type BuilderAction,
  type BuilderState,
} from "./reducer.ts";
import type { KeptDraft } from "./persistence.ts";
import {
  chartOf,
  countingKeys,
  fixtureChart,
  fixtureDerived,
  fixtureSettings,
  strippedChart,
} from "./testkit.ts";

/** mulberry32: a seeded PRNG, so a failing property names the run that found it. */
function prng(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (s + 0x6d2b79f5) >>> 0;
    let t = s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const run = (state: BuilderState, ...actions: BuilderAction[]) =>
  actions.reduce(builderReducer, state);

function edit(
  chart: ChartRead = fixtureChart(),
  settings: CompanyDocument = fixtureSettings(),
  revision = "rev-1",
): BuilderState {
  return run(INITIAL_BUILDER, { type: "load", mode: "edit", settings, revision, chart });
}

const create = () =>
  run(INITIAL_BUILDER, {
    type: "load",
    mode: "create",
    settings: null,
    revision: null,
    chart: null,
  });

/** The answer to the check of the state's own generation. */
function checked(state: BuilderState, outcome: CheckOutcome): BuilderAction {
  return {
    type: "checked",
    settled: {
      generation: state.generation,
      baseRevision: state.base.revision,
      basePrint: state.base.print,
      outcome,
    },
  };
}

const template = (): Intent => {
  const built = templateIntent(
    { template: "new_company", charter: { name: "Acme" } },
    countingKeys(),
  );
  if (!built.ok) throw new Error(built.message);
  return built.intent;
};

describe("mode guards", () => {
  test("a template is refused in edit mode, and the company is left untouched", () => {
    const state = edit();
    const next = run(state, { type: "record", intent: template() });
    expect(next.refusal).toMatchObject({ reason: "mode" });
    expect(next.draft).toBe(state.draft);
    expect(next.generation).toBe(state.generation);

    // The dangerous case: a company whose chart is empty, where nothing but the
    // mode stands between a template and replacing its charter.
    const bare = edit(chartOf({}), { name: "Existing", mission: "Keep me" });
    const refused = run(bare, { type: "record", intent: template() });
    expect(refused.refusal).toMatchObject({ reason: "mode" });
    expect(refused.draft.company).toEqual({ name: "Existing", mission: "Keep me" });
  });

  test("a template starts a company in create mode, once", () => {
    const created = run(create(), { type: "record", intent: template() });
    expect(created.refusal).toBeNull();
    expect(created.log.ops.map((op) => op.type)).toEqual(["applyTemplate"]);
    expect(created.draft.company.name).toBe("Acme");
    expect(run(created, { type: "record", intent: template() }).refusal).toMatchObject({
      reason: "not_empty",
    });
  });

  test("before anything is loaded, a template is refused: nobody knows yet whether a company exists", () => {
    expect(run(INITIAL_BUILDER, { type: "record", intent: template() }).refusal).toMatchObject({
      reason: "mode",
    });
  });

  test("create mode stands on nothing, whatever the chart read found", () => {
    const state = run(INITIAL_BUILDER, {
      type: "load",
      mode: "create",
      settings: null,
      revision: null,
      chart: fixtureChart(),
    });
    expect(state.baseDraft.roles).toEqual([]);
    expect(state.baseDraft.units).toEqual([]);
  });
});

describe("a reader the chart did not show the runtime half", () => {
  const stripped = () => edit(fixtureChart(), fixtureSettings()).base.runtimeVisible;

  test("may edit the prose and the relations, but nothing that writes the runtime half", () => {
    expect(stripped()).toBe(true);
    const reader = edit(strippedChart(fixtureChart()));
    expect(reader.base.runtimeVisible).toBe(false);
    const refusedIntents: Intent[] = [
      { type: "changeKind", target: "seat:dev", kind: "human" },
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["runtime", "llm"], value: "x" }] },
      {
        type: "setScheduleEnabled",
        target: "unit:engineering",
        schedule: "standup",
        enabled: false,
      },
      {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: COMPANY_KEY },
        data: { handle: "qa", name: "QA", runtime: { llm: "fast" } },
      },
    ];
    for (const intent of refusedIntents) {
      expect(recordIntent(reader, intent), intent.type).toMatchObject({
        ok: false,
        refusal: "runtime_hidden",
      });
    }
    // Control: prose, a relation and an add with no runtime record.
    const allowed: Intent[] = [
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["project"], value: "ENG" }] },
      {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: COMPANY_KEY },
        data: { handle: "qa", name: "QA" },
      },
    ];
    for (const intent of allowed) expect(recordIntent(reader, intent).ok, intent.type).toBe(true);
    // And the same writes record for a reader who was shown it.
    expect(recordIntent(edit(), refusedIntents[0]!).ok).toBe(true);
  });
});

describe("loading and keying the base", () => {
  test("the base is keyed by address the moment it is read", () => {
    const state = edit();
    expect(state.draft).toBe(state.baseDraft);
    expect([...allSeats(state.draft)].map(({ seat }) => seat.key)).toContain("seat:vp-engineering");
    expect(locate(state.draft, "unit:platform")?.parent).toBe("unit:engineering");
    // An operation records against it at once.
    expect(
      run(state, { type: "record", intent: { type: "remove", target: "seat:sre" } }).refusal,
    ).toBeNull();
  });

  test("a derivation is kept only while it describes the chart the draft was read from", () => {
    const chart = fixtureChart();
    const state = edit(chart);
    const derived = fixtureDerived(chart);
    const described = run(state, { type: "derived", derived });
    expect(described.base.derived).toBe(derived);
    // A derivation of a chart that has since gained a seat is not placed.
    const other = chartOf({
      units: chart.units,
      seats: [...chart.seats, { handle: "qa", name: "QA" }],
    });
    const stale = run(state, { type: "derived", derived: fixtureDerived(other) });
    expect(stale.base.derived).toBeNull();
    expect(stale.orgDerived).not.toBeNull();
    // A new derivation moves neither the generation nor the base's identity.
    expect(described.generation).toBe(state.generation);
    expect(checkTrigger(state, described)).toBeNull();
  });

  test("a load carries the latest derivation onto its base when it describes it", () => {
    const chart = fixtureChart();
    const derived = run(INITIAL_BUILDER, { type: "derived", derived: fixtureDerived(chart) });
    const loaded = run(derived, {
      type: "load",
      mode: "edit",
      settings: fixtureSettings(),
      revision: "rev-1",
      chart,
    });
    expect(loaded.base.derived).not.toBeNull();
  });

  test("a check of another generation is ignored", () => {
    const state = edit();
    const stale: BuilderAction = {
      type: "checked",
      settled: {
        generation: state.generation - 1,
        baseRevision: "rev-1",
        basePrint: state.base.print,
        outcome: { status: "guarded", grants: [] },
      },
    };
    expect(run(state, stale)).toBe(state);
    // Control: the same answer for its own generation is taken.
    expect(run(state, checked(state, { status: "guarded", grants: [] })).check.outcome).toEqual({
      status: "guarded",
      grants: [],
    });
  });

  test("a check's findings are indexed onto the nodes they are about", () => {
    const state = edit();
    const next = run(
      state,
      checked(state, {
        status: "problems",
        findings: [
          {
            severity: "problem",
            kind: "invalid",
            message: "bad handle",
            node: "seat:dev",
            field: ["handle"],
            link: null,
            source: { path: "", segments: null, kind: "invalid", message: "bad handle" },
          },
        ],
        code: "",
        hint: "",
      }),
    );
    expect(next.check.problems.byNode.get("seat:dev")?.[0]?.message).toBe("bad handle");
  });
});

describe("editing", () => {
  test("recording moves the generation, logs the operation with its report, and says what to announce and focus", () => {
    const state = edit();
    const next = run(state, {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:sales" },
        data: { handle: "qa", name: "QA" },
      },
    });
    expect(next.generation).toBe(state.generation + 1);
    expect(next.log.ops).toHaveLength(1);
    expect(next.reports).toHaveLength(1);
    expect(next.last).toMatchObject({
      kind: "applied",
      description: "Added agent seat QA to Sales.",
      focus: "new:qa",
    });
    expect(hasChanges(next)).toBe(true);
    expect(hasChanges(state)).toBe(false);
  });

  test("operations that cancel out leave nothing to save", () => {
    const state = edit();
    const edited = run(state, {
      type: "record",
      intent: { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
    });
    expect(hasChanges(edited)).toBe(true);
    const reverted = run(edited, {
      type: "record",
      intent: { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Build" }] },
    });
    expect(reverted.log.ops).toHaveLength(2);
    expect(hasChanges(reverted)).toBe(false);

    // In create mode the settings are what make the company exist.
    expect(hasChanges(create())).toBe(false);
    const built = templateIntent({ template: "empty", charter: { name: "Acme" } }, countingKeys());
    if (!built.ok) throw new Error(built.message);
    expect(hasChanges(run(create(), { type: "record", intent: built.intent }))).toBe(true);
  });

  test("a refused intent changes nothing but the refusal", () => {
    const state = edit();
    const next = run(state, {
      type: "record",
      intent: { type: "renameUnit", target: "unit:nope", name: "X" },
    });
    expect(next.refusal).toMatchObject({ reason: "missing_target" });
    expect({ ...next, refusal: null }).toEqual(state);
  });

  test("removing a node focuses its previous sibling, or else its parent", () => {
    const state = edit();
    expect(
      run(state, { type: "record", intent: { type: "remove", target: "seat:vp-engineering" } }).last
        ?.focus,
    ).toBe("seat:dev");
    expect(
      run(state, { type: "record", intent: { type: "remove", target: "seat:dev" } }).last?.focus,
    ).toBe("unit:engineering");
    expect(
      run(state, { type: "record", intent: { type: "remove", target: "unit:engineering" } }).last
        ?.focus,
    ).toBe(COMPANY_KEY);
  });

  test("undo and redo move the generation and focus what the operation touched", () => {
    const state = edit();
    const edited = run(state, {
      type: "record",
      intent: { type: "move", target: "seat:dev", to: { parent: "unit:sales" } },
    });
    const undone = run(edited, { type: "undo" });
    expect(undone.draft).toEqual(state.draft);
    expect(undone.last).toMatchObject({ kind: "undone", focus: "seat:dev" });
    expect(undone.generation).toBe(edited.generation + 1);
    const redone = run(undone, { type: "redo" });
    expect(redone.draft).toEqual(edited.draft);
    expect(redone.log).toEqual(edited.log);
    expect(run(state, { type: "undo" })).toBe(state);
  });

  test("the draft is always the base with the log replayed, and the reports stay aligned", () => {
    const failures: string[] = [];
    for (let seed = 1; seed <= 60; seed++) {
      const rand = prng(seed);
      let state = edit();
      let minted = 0;
      for (let step = 0; step < 25; step++) {
        const seats = [...allSeats(state.draft)].map(({ seat }) => seat);
        const seat = seats[Math.floor(rand() * seats.length)];
        const roll = rand();
        const action: BuilderAction =
          roll < 0.15
            ? { type: "undo" }
            : roll < 0.25
              ? { type: "redo" }
              : roll < 0.28
                ? { type: "discard" }
                : roll < 0.45 || !seat
                  ? {
                      type: "record",
                      intent: {
                        type: "addSeat",
                        key: `new:r${seed}x${++minted}`,
                        placement: { parent: COMPANY_KEY },
                        data: { handle: `s${seed}-${minted}`, name: `S${minted}` },
                      },
                    }
                  : roll < 0.6
                    ? { type: "record", intent: { type: "remove", target: seat.key } }
                    : roll < 0.75
                      ? {
                          type: "record",
                          intent: {
                            type: "updateSeat",
                            target: seat.key,
                            set: [{ path: ["goal"], value: `g${step}` }],
                          },
                        }
                      : roll < 0.9
                        ? {
                            type: "record",
                            intent: {
                              type: "updateSeat",
                              target: seat.key,
                              set: [{ path: ["handle"], value: `h${seed}-${step}` }],
                            },
                          }
                        : {
                            type: "record",
                            intent: {
                              type: "renameSeat",
                              target: seat.key,
                              name: `${seat.data.name} ${step}`,
                            },
                          };
        const before = state.generation;
        const next = builderReducer(state, action);
        const changed = next.draft !== state.draft || next.log !== state.log;
        if (changed && next.generation === before)
          failures.push(
            `seed ${seed} step ${step}: ${action.type} changed the draft without moving the generation`,
          );
        state = next;
        if (
          JSON.stringify(replay(state.baseDraft, state.log.ops).draft) !==
          JSON.stringify(state.draft)
        ) {
          failures.push(
            `seed ${seed} step ${step}: the draft is not the replayed log after ${action.type}`,
          );
          break;
        }
        if (state.reports.length !== state.log.ops.length) {
          failures.push(
            `seed ${seed} step ${step}: ${state.reports.length} reports for ${state.log.ops.length} operations`,
          );
          break;
        }
      }
    }
    expect(failures).toEqual([]);
  });

  test("discard returns to the base", () => {
    const state = edit();
    const edited = run(state, { type: "record", intent: { type: "remove", target: "unit:sales" } });
    const discarded = run(edited, { type: "discard" });
    expect(discarded.draft).toBe(state.baseDraft);
    expect(discarded.log.ops).toEqual([]);
    expect(discarded.generation).toBe(edited.generation + 1);
  });
});

describe("an editor's edit", () => {
  const change: Intent = {
    type: "edit",
    target: "seat:dev",
    intents: [
      { type: "renameSeat", target: "seat:dev", name: "Developer" },
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
    ],
  };

  test("is one step: one operation logged, announced once, and undone at once", () => {
    const state = edit();
    const edited = run(state, { type: "record", intent: change });
    expect(edited.log.ops).toHaveLength(1);
    expect(edited.last).toMatchObject({
      description: "Edited Dev: renamed to Developer, goal.",
      focus: "seat:dev",
    });
    const undone = run(edited, { type: "undo" });
    expect(undone.draft).toEqual(state.draft);
    expect(run(undone, { type: "redo" }).draft).toEqual(edited.draft);
  });

  test("a dialog asking recordIntent hears exactly what the reducer would do", () => {
    const state = edit();
    const answer = recordIntent(state, change);
    const next = run(state, { type: "record", intent: change });
    expect(answer.ok && answer.op).toEqual(next.log.ops[0]);
    expect(recordIntent(state, change)).toEqual(answer);
    const reader = edit(strippedChart(fixtureChart()));
    const kind: Intent = { type: "changeKind", target: "seat:dev", kind: "human" };
    expect(recordIntent(reader, kind).ok).toBe(false);
    expect(run(reader, { type: "record", intent: kind }).refusal?.reason).toBe("runtime_hidden");
  });
});

describe("checkTrigger", () => {
  test("a new base resets the check, a moved draft changes it, and a derivation does neither", () => {
    const loaded = edit();
    expect(checkTrigger(INITIAL_BUILDER, loaded)).toBe("reset");
    const edited = run(loaded, { type: "record", intent: { type: "remove", target: "seat:dev" } });
    expect(checkTrigger(loaded, edited)).toBe("changed");
    expect(checkTrigger(edited, run(edited, { type: "undo" }))).toBe("changed");
    expect(
      checkTrigger(
        edited,
        run(edited, {
          type: "saved",
          settings: fixtureSettings(),
          revision: "rev-1",
          chart: chartOf({ seats: [{ handle: "ceo", name: "CEO" }] }),
        }),
      ),
    ).toBe("reset");
    expect(checkTrigger(edited, run(edited, { type: "tokenChanged" }))).toBeNull();
    expect(
      checkTrigger(
        edited,
        run(edited, { type: "derived", derived: fixtureDerived(fixtureChart()) }),
      ),
    ).toBeNull();
  });
});

describe("keeping the log", () => {
  test("a refused credential or a change of reader stops keeping it, and the next operation resumes", () => {
    const state = edit();
    const edited = run(state, { type: "record", intent: { type: "remove", target: "unit:sales" } });
    const refused = run(edited, checked(edited, { status: "guarded", grants: [] }));
    expect(refused.keep).toBe(false);
    expect(refused.log).toBe(edited.log);
    expect(run(edited, { type: "tokenChanged" }).keep).toBe(false);
    expect(
      run(refused, { type: "record", intent: { type: "remove", target: "seat:dev" } }).keep,
    ).toBe(true);
  });
});

describe("a save", () => {
  test("makes the company read back the base, keyed by address, and moves every key it created", () => {
    const created = run(create(), { type: "record", intent: template() });
    const chief = [...allSeats(created.draft)].find(
      ({ seat }) => seat.data.name === "Chief Executive",
    )!;
    expect(chief.seat.key.startsWith("new:")).toBe(true);
    const chart = chartOf({
      seats: [{ handle: "chief-executive", name: "Chief Executive", goal: "Lead" }],
    });
    const saved = run(created, {
      type: "saved",
      settings: { name: "Acme" },
      revision: "rev-9",
      chart,
    });
    expect(saved).toMatchObject({
      mode: "edit",
      base: { revision: "rev-9" },
      log: { ops: [], undone: [] },
    });
    expect(saved.generation).toBe(created.generation + 1);
    expect(saved.draft).toBe(saved.baseDraft);
    // A surface still holding the minted key reads the node through this.
    expect(saved.rekeyed.get(chief.seat.key)).toBe("seat:chief-executive");
    // A load starts the list again.
    expect(
      run(saved, { type: "load", mode: "edit", settings: {}, revision: "rev-9", chart }).rekeyed
        .size,
    ).toBe(0);
  });
});

describe("updating onto a newer company", () => {
  function conflicted() {
    const state = run(edit(), {
      type: "record",
      intent: { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Mine" }] },
    });
    const theirs = fixtureChart();
    const dev = theirs.seats.find((s) => s.handle === "dev")!;
    (dev as { goal?: string }).goal = "Theirs";
    const withLegal = chartOf({
      units: [...theirs.units, { key: "legal", name: "Legal" }],
      seats: theirs.seats,
      manages: theirs.manages,
    });
    return run(state, {
      type: "updateBegin",
      settings: fixtureSettings(),
      revision: "rev-1",
      chart: withLegal,
    });
  }

  test("holds the draft until every conflict is resolved, then adopts the rebased log", () => {
    const pending = conflicted();
    expect(pending.update?.result.pending).toBe(1);
    expect(locate(pending.draft, "unit:legal")).toBeUndefined();
    expect(run(pending, { type: "updateConfirm" })).toBe(pending);

    const chosen = run(pending, { type: "updateChoose", index: 0, choice: "mine" });
    expect(chosen.update?.result.pending).toBe(0);
    const adopted = run(chosen, { type: "updateConfirm" });
    expect(adopted.update).toBeNull();
    expect(adopted.generation).toBe(pending.generation + 1);
    expect(locate(adopted.draft, "unit:legal")?.kind).toBe("unit");
    const dev = locate(adopted.draft, "seat:dev");
    expect(dev?.kind === "seat" && dev.node.data.goal).toBe("Mine");
    expect(JSON.stringify(replay(adopted.baseDraft, adopted.log.ops).draft)).toBe(
      JSON.stringify(adopted.draft),
    );
    // The base it moved onto is the one a check now compares against.
    expect(adopted.base.print).not.toBe(pending.base.print);
  });

  test("cancelling leaves the draft as it was", () => {
    const pending = conflicted();
    const cancelled = run(pending, { type: "updateCancel" });
    expect(cancelled.update).toBeNull();
    expect(cancelled.draft).toBe(pending.draft);
  });

  test("a save that stopped part way carries on from what landed at once, its creations resolved", () => {
    const state = run(
      edit(),
      {
        type: "record",
        intent: {
          type: "addSeat",
          key: "new:qa",
          placement: { parent: "unit:sales" },
          data: { handle: "qa", name: "QA", goal: "Test" },
        },
      },
      {
        type: "record",
        intent: {
          type: "updateSeat",
          target: "new:qa",
          set: [{ path: ["backstory"], value: "New." }],
        },
      },
    );
    // The batch created the seat; its content write did not land.
    const chart = fixtureChart();
    const landed = chartOf({
      units: chart.units,
      seats: [...chart.seats, { handle: "qa", name: "", unit: "sales" }],
      manages: chart.manages,
    });
    const next = run(state, {
      type: "updateBegin",
      settings: fixtureSettings(),
      revision: "rev-1",
      chart: landed,
      landed: [{ key: "new:qa", kind: "seat", address: "qa" }],
    });
    expect(next.update).toBeNull();
    const qa = locate(next.draft, "seat:qa");
    expect(qa?.kind === "seat" && qa.node.data).toMatchObject({
      name: "QA",
      goal: "Test",
      backstory: "New.",
    });
    expect(next.rekeyed.get("new:qa")).toBe("seat:qa");
    // What is left is still to save.
    expect(hasChanges(next)).toBe(true);
  });
});

describe("restoring a kept draft", () => {
  const kept = (state: BuilderState, base: BuilderState = edit()): KeptDraft => ({
    v: OPERATIONS_VERSION,
    mode: "edit",
    baseRevision: base.base.revision,
    basePrint: base.base.print,
    ops: state.log.ops,
    undone: state.log.undone,
    savedAt: 0,
    reader: "p-1",
  });

  test("on the same base, with every operation applying, is the draft the operator left, redo stack included", () => {
    const left = run(
      edit(),
      { type: "record", intent: { type: "remove", target: "seat:dev" } },
      { type: "record", intent: { type: "renameUnit", target: "unit:sales", name: "Revenue" } },
      { type: "undo" },
    );
    const restored = run(edit(), { type: "restore", kept: kept(left) });
    expect(restored.draft).toEqual(left.draft);
    expect(restored.log).toEqual(left.log);
    expect(run(restored, { type: "redo" }).log.ops).toHaveLength(2);
  });

  test("on a moved chart is an update to review, leaving the draft on screen untouched", () => {
    const left = run(edit(), { type: "record", intent: { type: "remove", target: "seat:dev" } });
    const theirs = fixtureChart();
    (theirs.seats.find((s) => s.handle === "dev") as { goal?: string }).goal = "Changed upstream";
    const loaded = edit(theirs);
    const restoring = run(loaded, { type: "restore", kept: kept(left) });
    expect(restoring.update).toMatchObject({ restoring: true, result: { pending: 1 } });
    expect(restoring.draft).toBe(loaded.draft);
    expect(restoring.log.ops).toEqual([]);
    const theirsKept = run(
      restoring,
      { type: "updateChoose", index: 0, choice: "theirs" },
      { type: "updateConfirm" },
    );
    expect(locate(theirsKept.draft, "seat:dev")?.kind).toBe("seat");
    expect(theirsKept.log.ops).toEqual([]);
  });

  test("never replaces work in progress, which the operator discards first", () => {
    const keptDraft = kept(
      run(edit(), { type: "record", intent: { type: "remove", target: "seat:dev" } }),
    );
    const editing = run(edit(), {
      type: "record",
      intent: { type: "renameUnit", target: "unit:sales", name: "Revenue" },
    });
    const refused = run(editing, { type: "restore", kept: keptDraft });
    expect(refused.refusal).toMatchObject({ reason: "has_changes" });
    expect(refused.log).toBe(editing.log);
    // An undo still leaves work to redo, and that is work in progress too.
    const undone = run(editing, { type: "undo" });
    expect(run(undone, { type: "restore", kept: keptDraft }).refusal).toMatchObject({
      reason: "has_changes",
    });
    const discarded = run(editing, { type: "discard" });
    expect(run(discarded, { type: "restore", kept: keptDraft }).log.ops).toHaveLength(1);
  });

  test("drops a kept redo stack that does not redo, rather than leaving a redo that throws", () => {
    const left = run(
      edit(),
      { type: "record", intent: { type: "remove", target: "seat:dev" } },
      { type: "record", intent: { type: "renameUnit", target: "unit:sales", name: "Revenue" } },
      { type: "undo" },
    );
    // An undone operation that names the removed seat, which no draft this log still holds.
    const stale = run(edit(), {
      type: "record",
      intent: { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "x" }] },
    }).log.ops[0]!;
    const restored = run(edit(), { type: "restore", kept: { ...kept(left), undone: [stale] } });
    expect(restored.log.ops).toEqual(left.log.ops);
    expect(restored.log.undone).toEqual([]);
    expect(() => run(restored, { type: "redo" })).not.toThrow();
  });

  test("made for another mode is refused, and a kept edit log carrying a template is refused", () => {
    expect(run(create(), { type: "restore", kept: kept(edit()) }).refusal).toMatchObject({
      reason: "mode",
    });
    const op = run(create(), { type: "record", intent: template() }).log.ops[0]!;
    const next = run(edit(), { type: "restore", kept: { ...kept(edit()), ops: [op] } });
    expect(next.refusal).toMatchObject({ reason: "mode" });
    expect(next.log.ops).toEqual([]);
  });

  test("a create draft whose save was sent is carried onto the company it made, as an edit", () => {
    const made = run(create(), { type: "record", intent: template() });
    const keptCreate: KeptDraft = {
      v: OPERATIONS_VERSION,
      mode: "create",
      baseRevision: null,
      basePrint: made.base.print,
      ops: made.log.ops,
      undone: [],
      savedAt: 0,
      reader: "p-1",
      write: "w1",
      creates: [],
    };
    // The save wrote the settings and nothing of the chart.
    const company = edit(chartOf({}), { name: "Acme" });
    const restored = run(company, { type: "restore", kept: keptCreate });
    const ops = restored.update?.ops ?? restored.log.ops;
    expect(ops.some((op) => op.type === "applyTemplate")).toBe(false);
    expect(ops.filter((op) => op.type === "addSeat").length).toBeGreaterThan(0);
    // Control: without a write the same log is another mode's, and refused.
    const { write: _w, ...unsent } = keptCreate;
    expect(run(company, { type: "restore", kept: unsent }).refusal).toMatchObject({
      reason: "mode",
    });
  });
});
