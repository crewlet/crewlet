/**
 * Builder states for the view suites, made by the real reducer. Imported by
 * tests only.
 *
 * A view is tested against the state the model actually produces (a loaded
 * base, keyed by a check, with operations recorded through the reducer),
 * never a hand-built object that could hold a shape the model never makes.
 */

import type { CompanyDocument } from "~/protocol/index.ts";
import { toDocument } from "./model/document.ts";
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

/** The fixture company's root seat Designer, placed in Platform by its unit reference. */
export const PLACED: DerivedOverrides = {
  seats: { "roles[1]": { placed_by_ref: true, unit_path: "units[0].children[0]" } },
};

/** An edit-mode builder on `doc`, loaded and keyed by a check answering with the overrides. */
export function checkedEdit(doc: CompanyDocument, overrides: DerivedOverrides = PLACED) {
  const loaded = run(INITIAL_BUILDER, {
    type: "load",
    mode: "edit",
    document: doc,
    revision: "rev-1",
  });
  return answered(loaded, {
    status: "clean",
    warnings: [],
    derived: fixtureDerived(doc, overrides),
  });
}

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

/** Records an intent, failing the test with the reducer's own sentence when it is refused. */
export function record(state: BuilderState, intent: Intent): BuilderState {
  const next = run(state, { type: "record", intent });
  if (next.refusal) throw new Error(next.refusal.message);
  return next;
}
