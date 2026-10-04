// @vitest-environment node
/**
 * The builder reducer.
 *
 * What these protect: a template can only start a company from nothing, never
 * replace one that exists; the draft is always its base with the log
 * replayed, whatever sequence of actions produced it; every change moves the
 * generation and a check for another generation is ignored; a loaded base is
 * keyed by the identities its document carries, with nothing to wait for; a
 * refused credential stops the log being kept; and an update or a restore
 * adopts a log only once every conflict is resolved.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { COMPANY_KEY } from "./keys.ts";
import { allSeats, locate } from "./draft.ts";
import { toDocument } from "./document.ts";
import { replay } from "./history.ts";
import type { Intent } from "./operations.ts";
import { OPERATIONS_VERSION } from "./operations.ts";
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
import { countingKeys, fixtureCompany, fixtureDerived } from "./testkit.ts";

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

function loadedEdit(doc: CompanyDocument = fixtureCompany()): BuilderState {
  return run(INITIAL_BUILDER, { type: "load", mode: "edit", document: doc, revision: "rev-1" });
}

/** The answer to the check of the state's own generation. */
function checked(
  state: BuilderState,
  outcome: CheckOutcome,
  baseRevision = state.base.revision,
): BuilderAction {
  return {
    type: "checked",
    settled: { generation: state.generation, sent: toDocument(state.draft), baseRevision, outcome },
  };
}

/** A loaded edit-mode builder whose base the first check has described. */
function keyedEdit(doc: CompanyDocument = fixtureCompany()): BuilderState {
  const loaded = loadedEdit(doc);
  return run(
    loaded,
    checked(loaded, { status: "clean", warnings: [], derived: fixtureDerived(doc) }),
  );
}

describe("mode guards", () => {
  const template = (): Intent => {
    const built = templateIntent(
      { template: "new_company", charter: { name: "Acme" } },
      countingKeys(),
    );
    if (!built.ok) throw new Error(built.message);
    return built.intent;
  };

  test("a template is refused in edit mode, and the company is left untouched", () => {
    const state = keyedEdit();
    const next = run(state, { type: "record", intent: template() });
    expect(next.refusal).toMatchObject({ reason: "mode" });
    expect(next.draft).toBe(state.draft);
    expect(next.generation).toBe(state.generation);
    expect(next.log.ops).toEqual([]);

    // The dangerous case: a company with no seats or units yet, where nothing
    // but the mode stands between a template and replacing the company's
    // charter and roster in one merge patch.
    const bare = keyedEdit({ name: "Existing", mission: "Keep me" });
    const refused = run(bare, { type: "record", intent: template() });
    expect(refused.refusal).toMatchObject({ reason: "mode" });
    expect(toDocument(refused.draft).document).toEqual({ name: "Existing", mission: "Keep me" });
  });

  test("a template starts a company in create mode, once", () => {
    const state = run(INITIAL_BUILDER, {
      type: "load",
      mode: "create",
      document: null,
      revision: null,
    });
    const created = run(state, { type: "record", intent: template() });
    expect(created.refusal).toBeNull();
    expect(created.log.ops.map((op) => op.type)).toEqual(["applyTemplate"]);
    expect(toDocument(created.draft).document.name).toBe("Acme");
    expect(run(created, { type: "record", intent: template() }).refusal).toMatchObject({
      reason: "not_empty",
    });
  });

  test("a kept edit-mode log carrying a template is refused on restore", () => {
    const create = run(INITIAL_BUILDER, {
      type: "load",
      mode: "create",
      document: null,
      revision: null,
    });
    const op = run(create, { type: "record", intent: template() }).log.ops[0]!;
    const next = run(keyedEdit(), {
      type: "restore",
      kept: {
        v: OPERATIONS_VERSION,
        mode: "edit",
        baseRevision: "rev-1",
        ops: [op],
        undone: [],
        savedAt: 0,
      },
    });
    expect(next.refusal).toMatchObject({ reason: "mode" });
    expect(next.log.ops).toEqual([]);
  });
});

describe("loading the base", () => {
  // EVERY SEAT THE ENGINE STORES DECLARES ITS HANDLE, every unit its key, so
  // the base is keyed as it loads and an edit is taken at once. What the first
  // check of the base adds is the engine's description of it, kept as the
  // base's own for the review to compare the draft against.
  test("a loaded base is keyed at once, and its first check is kept as the base's derivation", () => {
    const loaded = loadedEdit();
    expect(loaded.draft.roles[0]!.key).toBe("seat:ceo");
    expect(loaded.base.derived).toBeNull();
    const problem = {
      path: "roles[0].goal",
      segments: ["roles", 0, "goal"],
      kind: "invalid",
      message: "bad goal",
    };
    const derived = fixtureDerived(fixtureCompany());
    const described = run(
      loaded,
      checked(loaded, {
        status: "problems",
        problems: [problem],
        derived,
        code: "validation_error",
        hint: "",
      }),
    );
    expect(described.base.derived).toBe(derived);
    expect(described.draft).toBe(loaded.draft);
    expect(described.check.problems.byNode.get("seat:ceo")?.[0]?.message).toBe("bad goal");
    expect(described.generation).toBe(loaded.generation);
    expect(described.rekeyed.size).toBe(0);
  });

  test("an edit is recorded as soon as the base loads", () => {
    const removed = run(loadedEdit(), {
      type: "record",
      intent: { type: "remove", target: "seat:sre" },
    });
    expect(removed.refusal).toBeNull();
    expect(removed.log.ops[0]).toMatchObject({
      target: "seat:sre",
      accessLevels: [{ handle: "sre", before: "maintainer" }],
    });
  });

  // A dialog or a selection still holding a created node's minted key reads
  // its node through this list in the very render the keys change, rather
  // than finding no node there and drawing it as gone for that render.
  test("a save lists every key it moved to an identity, and a load starts the list again", () => {
    const added = run(keyedEdit(), {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: COMPANY_KEY, after: null },
        data: { name: "Quality Lead", handle: "quality-lead" },
      },
    });
    const saved = run(added, {
      type: "saved",
      revisionId: "rev-2",
      derived: fixtureDerived(toDocument(added.draft).document),
    });
    expect(saved.rekeyed.get("new:qa")).toBe("seat:quality-lead");
    expect(saved.rekeyed.has("seat:ceo")).toBe(false);
    expect(
      run(saved, { type: "load", mode: "edit", document: fixtureCompany(), revision: "rev-3" })
        .rekeyed.size,
    ).toBe(0);
  });

  test("before anything is loaded, neither an edit nor a template is recorded", () => {
    const template = templateIntent(
      { template: "new_company", charter: { name: "Acme" } },
      countingKeys(),
    );
    if (!template.ok) throw new Error(template.message);
    expect(run(INITIAL_BUILDER, { type: "record", intent: template.intent }).refusal).toMatchObject(
      { reason: "mode" },
    );
    const added = run(INITIAL_BUILDER, {
      type: "record",
      intent: {
        type: "addUnit",
        key: "new:u",
        placement: { parent: COMPANY_KEY, after: null },
        data: { name: "Ops", id: "ops" },
      },
    });
    expect(added.refusal).toMatchObject({ reason: "not_loaded" });
    expect(added.log.ops).toEqual([]);
  });

  test("a check of another generation is ignored", () => {
    const state = keyedEdit();
    const stale: BuilderAction = {
      type: "checked",
      settled: {
        generation: state.generation - 1,
        sent: toDocument(state.draft),
        baseRevision: "rev-1",
        outcome: { status: "guarded", code: "unauthorized" },
      },
    };
    expect(run(state, stale)).toBe(state);
  });
});

describe("editing", () => {
  test("recording moves the generation, logs the operation with its report, and says what to announce and focus", () => {
    const state = keyedEdit();
    const next = run(state, {
      type: "record",
      intent: {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:sales", after: null },
        data: { name: "QA", handle: "qa" },
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
  });

  test("operations that cancel out leave nothing to save in edit mode, where an empty patch would still activate a revision", () => {
    const state = keyedEdit();
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

    // In create mode the base is nothing: a draft holding a company has changes.
    const create = run(INITIAL_BUILDER, {
      type: "load",
      mode: "create",
      document: null,
      revision: null,
    });
    expect(hasChanges(create)).toBe(false);
    const built = templateIntent({ template: "empty", charter: { name: "Acme" } }, countingKeys());
    if (!built.ok) throw new Error(built.message);
    expect(hasChanges(run(create, { type: "record", intent: built.intent }))).toBe(true);
  });

  test("a refused intent changes nothing but the refusal", () => {
    const state = keyedEdit();
    const next = run(state, {
      type: "record",
      intent: { type: "renameUnit", target: "unit:nope", name: "X" },
    });
    expect(next.refusal).toMatchObject({ reason: "missing_target" });
    expect({ ...next, refusal: null }).toEqual(state);
  });

  test("removing a node focuses its previous sibling, or else its parent", () => {
    const state = keyedEdit();
    expect(
      run(state, { type: "record", intent: { type: "remove", target: "seat:dev" } }).last?.focus,
    ).toBe("seat:vp-engineering");
    expect(
      run(state, { type: "record", intent: { type: "remove", target: "seat:vp-engineering" } }).last
        ?.focus,
    ).toBe("unit:engineering");
    expect(
      run(state, { type: "record", intent: { type: "remove", target: "unit:engineering" } }).last
        ?.focus,
    ).toBe(COMPANY_KEY);
  });

  test("undo and redo move the generation and focus what the operation touched", () => {
    const state = keyedEdit();
    const edited = run(state, {
      type: "record",
      intent: { type: "move", target: "seat:dev", to: { parent: "unit:sales", after: null } },
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
      let state = keyedEdit();
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
                        placement: { parent: COMPANY_KEY, after: null },
                        data: { name: `S${minted}`, handle: `s${seed}x${minted}` },
                      },
                    }
                  : roll < 0.6
                    ? { type: "record", intent: { type: "remove", target: seat.key } }
                    : roll < 0.8
                      ? {
                          type: "record",
                          intent: {
                            type: "updateSeat",
                            target: seat.key,
                            set: [{ path: ["goal"], value: `g${step}` }],
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
    const state = keyedEdit();
    const edited = run(state, { type: "record", intent: { type: "remove", target: "unit:sales" } });
    const discarded = run(edited, { type: "discard" });
    expect(discarded.draft).toBe(state.baseDraft);
    expect(discarded.log.ops).toEqual([]);
    expect(discarded.generation).toBe(edited.generation + 1);
  });
});

describe("an editor's edit", () => {
  const edit: Intent = {
    type: "edit",
    target: "seat:dev",
    intents: [
      { type: "renameSeat", target: "seat:dev", name: "Developer" },
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
    ],
  };

  test("is one step: one operation logged, announced once, and undone at once", () => {
    const state = keyedEdit();
    const edited = run(state, { type: "record", intent: edit });
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
    const refusedEarly = recordIntent(INITIAL_BUILDER, edit);
    expect(refusedEarly).toMatchObject({ ok: false, refusal: "not_loaded" });
    expect(run(INITIAL_BUILDER, { type: "record", intent: edit }).refusal?.reason).toBe(
      "not_loaded",
    );

    const state = keyedEdit();
    const answer = recordIntent(state, edit);
    const next = run(state, { type: "record", intent: edit });
    expect(answer.ok && answer.op).toEqual(next.log.ops[0]);
    expect(recordIntent(state, edit)).toEqual(answer);
  });
});

describe("checkTrigger", () => {
  test("a new base resets the check, a moved draft changes it, and describing the base does neither", () => {
    const loaded = loadedEdit();
    expect(checkTrigger(INITIAL_BUILDER, loaded)).toBe("reset");
    const keyed = run(
      loaded,
      checked(loaded, { status: "clean", warnings: [], derived: fixtureDerived(fixtureCompany()) }),
    );
    expect(checkTrigger(loaded, keyed)).toBeNull();
    const edited = run(keyed, { type: "record", intent: { type: "remove", target: "seat:dev" } });
    expect(checkTrigger(keyed, edited)).toBe("changed");
    expect(checkTrigger(edited, run(edited, { type: "undo" }))).toBe("changed");
    expect(
      checkTrigger(edited, run(edited, { type: "saved", revisionId: "rev-2", derived: null })),
    ).toBe("reset");
    expect(checkTrigger(edited, run(edited, { type: "readerChanged" }))).toBeNull();
  });
});

describe("keeping the log", () => {
  test("a refusal or a change of reader stops keeping it, and the next operation resumes", () => {
    const state = keyedEdit();
    const edited = run(state, { type: "record", intent: { type: "remove", target: "unit:sales" } });
    const refused = run(edited, checked(edited, { status: "guarded", code: "unauthorized" }));
    expect(refused.keep).toBe(false);
    expect(refused.log).toBe(edited.log);
    expect(run(edited, { type: "readerChanged" }).keep).toBe(false);
    expect(
      run(refused, { type: "record", intent: { type: "remove", target: "seat:dev" } }).keep,
    ).toBe(true);
  });

  // A DECLINED STEP-UP IS THE SAME PERSON at the keyboard choosing not to
  // confirm yet, not the tab changing hands: dropped, the draft they were
  // about to confirm and save was gone on the next reload.
  test("a step-up the person declined keeps it", () => {
    const state = keyedEdit();
    const edited = run(state, { type: "record", intent: { type: "remove", target: "unit:sales" } });
    const declined = run(edited, checked(edited, { status: "guarded", code: "step_up_required" }));
    expect(declined.keep).toBe(true);
    expect(declined.check.outcome).toEqual({ status: "guarded", code: "step_up_required" });
  });

  test("a save makes the draft the base, keyed by the derivation the write answered with", () => {
    const create = run(INITIAL_BUILDER, {
      type: "load",
      mode: "create",
      document: null,
      revision: null,
    });
    const built = templateIntent(
      { template: "new_company", charter: { name: "Acme" } },
      countingKeys(),
    );
    if (!built.ok) throw new Error(built.message);
    const edited = run(create, { type: "record", intent: built.intent });
    const sent = toDocument(edited.draft).document;
    const saved = run(edited, {
      type: "saved",
      revisionId: "rev-9",
      derived: fixtureDerived(sent),
    });
    expect(saved).toMatchObject({
      mode: "edit",
      base: { revision: "rev-9" },
      log: { ops: [], undone: [] },
    });
    expect(saved.generation).toBe(edited.generation + 1);
    expect(saved.draft).toBe(saved.baseDraft);
    expect(toDocument(saved.draft).document).toEqual(sent);
    // The keys minted for the nodes the save created are gone: the seats it
    // created are keyed by the handles they were given.
    const keys = [...allSeats(saved.draft)].map(({ seat }) => seat.key);
    expect(keys).toContain("seat:chief-executive");
    expect(keys.some((k) => k.startsWith("new:"))).toBe(false);

    // A write that answered with no derivation still leaves a keyed base,
    // and an edit is recorded against it at once.
    const undescribed = run(edited, { type: "saved", revisionId: "rev-9", derived: null });
    expect(
      run(undescribed, {
        type: "record",
        intent: { type: "updateCompany", set: [{ path: ["vision"], value: "v" }] },
      }).refusal,
    ).toBeNull();
  });
});

describe("updating onto a newer revision", () => {
  function conflicted() {
    const doc = fixtureCompany();
    const state = run(keyedEdit(doc), {
      type: "record",
      intent: { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Mine" }] },
    });
    const theirs = fixtureCompany();
    theirs.units![0]!.roles![1]!.goal = "Theirs";
    theirs.units!.unshift({ name: "Legal" });
    return run(state, {
      type: "updateBegin",
      document: theirs,
      revision: "rev-2",
      derived: fixtureDerived(theirs),
    });
  }

  test("holds the draft until every conflict is resolved, then adopts the rebased log", () => {
    const pending = conflicted();
    expect(pending.update?.result.pending).toBe(1);
    expect(pending.base.revision).toBe("rev-1");
    expect(run(pending, { type: "updateConfirm" })).toBe(pending);

    const chosen = run(pending, { type: "updateChoose", index: 0, choice: "mine" });
    expect(chosen.update?.result.pending).toBe(0);
    const adopted = run(chosen, { type: "updateConfirm" });
    expect(adopted.base).toMatchObject({ revision: "rev-2" });
    expect(adopted.update).toBeNull();
    expect(adopted.generation).toBe(pending.generation + 1);
    const out = toDocument(adopted.draft).document;
    expect(out.units!.map((u) => u.name)).toEqual(["Legal", "Engineering", "Sales"]);
    expect(out.units![1]!.roles![1]!.goal).toBe("Mine");
    expect(JSON.stringify(replay(adopted.baseDraft, adopted.log.ops).draft)).toBe(
      JSON.stringify(adopted.draft),
    );
  });

  test("cancelling leaves the draft as it was", () => {
    const pending = conflicted();
    const cancelled = run(pending, { type: "updateCancel" });
    expect(cancelled.update).toBeNull();
    expect(cancelled.draft).toBe(pending.draft);
  });
});

describe("restoring a kept draft", () => {
  const kept = (state: BuilderState, revision = "rev-1") => ({
    v: OPERATIONS_VERSION,
    mode: "edit" as const,
    baseRevision: revision,
    ops: state.log.ops,
    undone: state.log.undone,
    savedAt: 0,
  });

  test("waits for the company to load", () => {
    expect(
      run(INITIAL_BUILDER, { type: "restore", kept: kept(keyedEdit()) }).refusal,
    ).toMatchObject({ reason: "not_loaded" });
  });

  test("on the same revision, with every operation applying, is the draft the operator left, redo stack included", () => {
    const left = run(
      keyedEdit(),
      { type: "record", intent: { type: "remove", target: "seat:dev" } },
      { type: "record", intent: { type: "renameUnit", target: "unit:sales", name: "Revenue" } },
      { type: "undo" },
    );
    const restored = run(keyedEdit(), { type: "restore", kept: kept(left) });
    expect(restored.draft).toEqual(left.draft);
    expect(restored.log).toEqual(left.log);
    expect(run(restored, { type: "redo" }).log.ops).toHaveLength(2);
  });

  test("on a moved revision is an update to review, leaving the draft on screen untouched", () => {
    const left = run(keyedEdit(), {
      type: "record",
      intent: { type: "remove", target: "seat:dev" },
    });
    const theirs = fixtureCompany();
    theirs.units![0]!.roles![1]!.goal = "Changed upstream";
    const loaded = run(INITIAL_BUILDER, {
      type: "load",
      mode: "edit",
      document: theirs,
      revision: "rev-2",
    });
    const keyed = run(
      loaded,
      checked(loaded, { status: "clean", warnings: [], derived: fixtureDerived(theirs) }),
    );
    const restoring = run(keyed, { type: "restore", kept: kept(left, "rev-1") });
    expect(restoring.update).toMatchObject({ restoring: true, result: { pending: 1 } });
    expect(restoring.draft).toBe(keyed.draft);
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
      run(keyedEdit(), { type: "record", intent: { type: "remove", target: "seat:dev" } }),
    );
    const editing = run(keyedEdit(), {
      type: "record",
      intent: { type: "renameUnit", target: "unit:sales", name: "Revenue" },
    });
    const refused = run(editing, { type: "restore", kept: keptDraft });
    expect(refused.refusal).toMatchObject({ reason: "has_changes" });
    expect(refused.log).toBe(editing.log);
    expect(refused.draft).toBe(editing.draft);

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
      keyedEdit(),
      { type: "record", intent: { type: "remove", target: "seat:dev" } },
      { type: "record", intent: { type: "renameUnit", target: "unit:sales", name: "Revenue" } },
      { type: "undo" },
    );
    // Storage handed back an undone operation that names the removed seat,
    // which no draft this log produces still holds.
    const stale = run(keyedEdit(), {
      type: "record",
      intent: { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "x" }] },
    }).log.ops[0]!;
    const restored = run(keyedEdit(), {
      type: "restore",
      kept: { ...kept(left), undone: [stale] },
    });
    expect(restored.log.ops).toEqual(left.log.ops);
    expect(restored.log.undone).toEqual([]);
    expect(() => run(restored, { type: "redo" })).not.toThrow();
  });

  test("made for another mode is refused", () => {
    const create = run(INITIAL_BUILDER, {
      type: "load",
      mode: "create",
      document: null,
      revision: null,
    });
    expect(run(create, { type: "restore", kept: kept(keyedEdit()) }).refusal).toMatchObject({
      reason: "mode",
    });
  });
});
