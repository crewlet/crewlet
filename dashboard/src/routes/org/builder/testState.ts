/**
 * Builder states for the editor and dialog suites. Imported by tests only.
 *
 * A state here is what the real reducer makes of a fixture document and the
 * answer to a clean check of it, with the engine's derivation stated by the
 * suite through `fixtureDerived` overrides. Nothing here derives a handle or
 * a manager: see `model/testkit.ts`.
 */

import type { CompanyDocument, ConfigProblem, ConfigWarning } from "~/protocol/index.ts";
import { pathOfSegments, toDocument } from "./model/document.ts";
import { builderReducer, INITIAL_BUILDER, type BuilderState } from "./model/reducer.ts";
import { fixtureDerived, type DerivedOverrides } from "./model/testkit.ts";

/** The state after the answer to a clean check of the current draft, with a fixture derivation. */
export function recheck(state: BuilderState, overrides: DerivedOverrides = {}): BuilderState {
  const sent = toDocument(state.draft);
  return builderReducer(state, {
    type: "checked",
    settled: {
      generation: state.generation,
      sent,
      baseRevision: state.base.revision,
      outcome: {
        status: "clean",
        warnings: [],
        derived: fixtureDerived(sent.document, overrides),
      },
    },
  });
}

/** An edit-mode builder on `doc`, keyed by the first check's fixture derivation. */
export function keyedState(doc: CompanyDocument, overrides: DerivedOverrides = {}): BuilderState {
  const loaded = builderReducer(INITIAL_BUILDER, {
    type: "load",
    mode: "edit",
    document: doc,
    revision: "rev-1",
  });
  return recheck(loaded, overrides);
}

/** The state after a check of the current draft that answered with these problems. */
export function checkWithProblems(
  state: BuilderState,
  problems: ConfigProblem[],
  overrides: DerivedOverrides = {},
): BuilderState {
  const sent = toDocument(state.draft);
  return builderReducer(state, {
    type: "checked",
    settled: {
      generation: state.generation,
      sent,
      baseRevision: state.base.revision,
      outcome: {
        status: "problems",
        problems,
        derived: fixtureDerived(sent.document, overrides),
        code: "validation_error",
        hint: "",
      },
    },
  });
}

/** The state after a clean check of the current draft that answered with these warnings. */
export function checkWithWarnings(
  state: BuilderState,
  warnings: ConfigWarning[],
  overrides: DerivedOverrides = {},
): BuilderState {
  const sent = toDocument(state.draft);
  return builderReducer(state, {
    type: "checked",
    settled: {
      generation: state.generation,
      sent,
      baseRevision: state.base.revision,
      outcome: { status: "clean", warnings, derived: fixtureDerived(sent.document, overrides) },
    },
  });
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
