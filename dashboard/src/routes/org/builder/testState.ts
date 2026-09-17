/**
 * Builder states for the view, editor and dialog suites, made by the real
 * reducer. Imported by tests only.
 *
 * A surface is tested against the state the model actually produces (a
 * loaded base, keyed by a check, with operations recorded through the
 * reducer), never a hand-built object that could hold a shape the model never
 * makes. The engine's derivation is stated by the suite through
 * `fixtureDerived` overrides; nothing here derives a handle or a manager
 * (see `model/testkit.ts`).
 */

import type { CompanyDocument, ConfigProblem, ConfigWarning } from "~/protocol/index.ts";
import { pathOfSegments, toDocument } from "./model/document.ts";
import type { Intent } from "./model/operations.ts";
import {
  builderReducer,
  INITIAL_BUILDER,
  type BuilderAction,
  type BuilderState,
} from "./model/reducer.ts";
import type { CheckOutcome } from "./model/scheduler.ts";
import { fixtureDerived, type DerivedOverrides } from "./model/testkit.ts";

export const run = (state: BuilderState, ...actions: BuilderAction[]): BuilderState =>
  actions.reduce(builderReducer, state);

/**
 * The state after the check of its own generation answered with `outcome`.
 * A derivation in it must describe the document the draft stands for.
 */
export function answered(state: BuilderState, outcome: CheckOutcome): BuilderState {
  return run(state, {
    type: "checked",
    settled: {
      generation: state.generation,
      sent: toDocument(state.draft),
      baseRevision: state.base.revision,
      outcome,
    },
  });
}

/** The engine's derivation of the draft a state stands for, with the suite's overrides. */
const derivationOf = (state: BuilderState, overrides: DerivedOverrides) =>
  fixtureDerived(toDocument(state.draft).document, overrides);

/** The state after the answer to a clean check of the current draft. */
export function recheck(state: BuilderState, overrides: DerivedOverrides = {}): BuilderState {
  return answered(state, {
    status: "clean",
    warnings: [],
    derived: derivationOf(state, overrides),
  });
}

/** An edit-mode builder on `doc`, keyed by the first check's fixture derivation. */
export function keyedState(doc: CompanyDocument, overrides: DerivedOverrides = {}): BuilderState {
  return recheck(
    run(INITIAL_BUILDER, { type: "load", mode: "edit", document: doc, revision: "rev-1" }),
    overrides,
  );
}

/** The fixture company's root seat Designer, placed in Platform by its unit reference. */
export const PLACED: DerivedOverrides = {
  seats: { "roles[1]": { placed_by_ref: true, unit_path: "units[0].children[0]" } },
};

/**
 * The charts' starting state: [keyedState] with Designer placed by its unit
 * reference, which is the placement the views exist to draw.
 */
export function checkedEdit(doc: CompanyDocument, overrides: DerivedOverrides = PLACED) {
  return keyedState(doc, overrides);
}

/** The state after a check of the current draft that answered with these problems. */
export function checkWithProblems(
  state: BuilderState,
  problems: ConfigProblem[],
  overrides: DerivedOverrides = {},
): BuilderState {
  return answered(state, {
    status: "problems",
    problems,
    derived: derivationOf(state, overrides),
    code: "validation_error",
    hint: "",
  });
}

/** The state after a clean check of the current draft that answered with these warnings. */
export function checkWithWarnings(
  state: BuilderState,
  warnings: ConfigWarning[],
  overrides: DerivedOverrides = {},
): BuilderState {
  return answered(state, { status: "clean", warnings, derived: derivationOf(state, overrides) });
}

/** Records an intent, failing the test with the reducer's own sentence when it is refused. */
export function record(state: BuilderState, intent: Intent): BuilderState {
  const next = run(state, { type: "record", intent });
  if (next.refusal) throw new Error(next.refusal.message);
  return next;
}

/** A dangling reference warning at a path, as the engine locates one. */
export function warningAt(segments: (string | number)[], message: string): ConfigWarning {
  return {
    kind: "dangling_reference",
    ref: "lead",
    path: pathOfSegments(segments),
    segments,
    seat: "",
    unit: "",
    from: "",
    to: "",
    message,
  };
}

/** A problem at a path, as the engine locates one. */
export function problemAt(segments: (string | number)[], message: string): ConfigProblem {
  return {
    path: pathOfSegments(segments),
    segments,
    kind: "invalid",
    message,
  };
}
