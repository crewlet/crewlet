/**
 * Builder states for the view, editor and dialog suites, made by the real
 * reducer. Imported by tests only.
 *
 * A surface is tested against the state the model actually produces (a base
 * loaded from the settings and the chart, with operations recorded through the
 * reducer), never a hand-built object that could hold a shape the model never
 * makes. The engine's derivation is stated by the suite through
 * `fixtureDerived` overrides, and describes the SAVED chart as the org push
 * does; nothing here derives a manager (see `model/testkit.ts`).
 */

import type { ChartRead, CompanyDocument } from "~/protocol/index.ts";
import type { Segment } from "./model/document.ts";
import type { NodeKey } from "./model/keys.ts";
import type { Intent } from "./model/operations.ts";
import type { PlacedProblem } from "./model/problems.ts";
import {
  builderReducer,
  INITIAL_BUILDER,
  type BuilderAction,
  type BuilderState,
} from "./model/reducer.ts";
import type { CheckOutcome } from "./model/scheduler.ts";
import {
  chartOfDraft,
  fixtureChart,
  fixtureDerived,
  fixtureSettings,
  type DerivedOverrides,
} from "./model/testkit.ts";

export const run = (state: BuilderState, ...actions: BuilderAction[]): BuilderState =>
  actions.reduce(builderReducer, state);

/** The state after the check of its own generation answered with `outcome`. */
export function answered(state: BuilderState, outcome: CheckOutcome): BuilderState {
  return run(state, {
    type: "checked",
    settled: {
      generation: state.generation,
      baseRevision: state.base.revision,
      basePrint: state.base.print,
      outcome,
    },
  });
}

/** An edit-mode builder on these settings and this chart, as a load reads them. */
export function loadedState(
  chart: ChartRead = fixtureChart(),
  settings: CompanyDocument = fixtureSettings(),
): BuilderState {
  return run(INITIAL_BUILDER, { type: "load", mode: "edit", settings, revision: "rev-1", chart });
}

/**
 * The state with the org push's derivation of its SAVED chart — the one
 * derivation there is — shaped by the suite's overrides.
 */
export function withDerivation(
  state: BuilderState,
  overrides: DerivedOverrides = {},
): BuilderState {
  return run(state, {
    type: "derived",
    derived: fixtureDerived(chartOfDraft(state.baseDraft), overrides),
  });
}

/** The charts' starting state: a loaded base, derived by the org push, and checked clean. */
export function checkedEdit(
  chart: ChartRead = fixtureChart(),
  overrides: DerivedOverrides = {},
  settings: CompanyDocument = fixtureSettings(),
): BuilderState {
  return recheck(withDerivation(loadedState(chart, settings), overrides));
}

/** The state after the answer to a clean check of the current draft. */
export function recheck(state: BuilderState): BuilderState {
  return answered(state, { status: "clean", findings: [] });
}

/** A finding on a node, placed as the check places the draft's own. */
export function findingOn(
  node: NodeKey | null,
  field: Segment[],
  message: string,
  severity: "problem" | "warning" = "problem",
): PlacedProblem {
  const kind = severity === "problem" ? "invalid" : "dangling_reference";
  return {
    severity,
    kind,
    message,
    node,
    field,
    link: null,
    source: { path: field.join("."), segments: [...field], kind, message },
  };
}

/** The state after a check of the current draft that answered with these findings. */
export function checkWith(state: BuilderState, findings: PlacedProblem[]): BuilderState {
  return answered(
    state,
    findings.some((f) => f.severity === "problem")
      ? { status: "problems", findings, code: "", hint: "" }
      : { status: "clean", findings },
  );
}

/** Records an intent, failing the test with the reducer's own sentence when it is refused. */
export function record(state: BuilderState, intent: Intent): BuilderState {
  const next = run(state, { type: "record", intent });
  if (next.refusal) throw new Error(next.refusal.message);
  return next;
}
