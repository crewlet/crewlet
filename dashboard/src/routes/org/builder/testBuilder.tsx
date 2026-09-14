/**
 * A Builder for the editor and dialog suites. Imported by tests only.
 *
 * THE REAL REDUCER, A FAKE SHELL. What a dialog dispatches is recorded by the
 * model's own reducer, so a suite asserts the operation that lands in the log
 * rather than a call a spy happened to receive. What belongs to `Builder.tsx`
 * (the check, the live region, focus and the open functions) is a spy: a
 * suite states the engine's answer itself, with `recheck`, exactly as the
 * model suites do with `fixtureDerived` (see `testState.ts`).
 */

import { render } from "@testing-library/react";
import { useReducer, type ReactNode } from "react";
import { vi } from "vitest";
import type { AgentRow, ConfigProblem, ConfigWarning, SandboxEntry } from "~/protocol/index.ts";
import { BuilderContext, type BuilderApi } from "./BuilderContext.tsx";
import type { NodeKey } from "./model/keys.ts";
import { builderReducer, type BuilderAction, type BuilderState } from "./model/reducer.ts";

export interface Harness {
  /** The state as the last render saw it. */
  state(): BuilderState;
  readonly spies: {
    readonly openEditor: ReturnType<typeof vi.fn>;
    readonly openAdd: ReturnType<typeof vi.fn>;
    readonly openMove: ReturnType<typeof vi.fn>;
    readonly openDelete: ReturnType<typeof vi.fn>;
    readonly openChangeKind: ReturnType<typeof vi.fn>;
    readonly announce: ReturnType<typeof vi.fn>;
    readonly focusNode: ReturnType<typeof vi.fn>;
  };
}

export interface HarnessOptions {
  readonly readOnly?: boolean;
  readonly agents?: AgentRow[];
  readonly sandboxes?: SandboxEntry[];
}

/** Renders `ui` inside a Builder context over the real reducer, starting from `initial`. */
export function renderInBuilder(
  initial: BuilderState,
  ui: ReactNode,
  options: HarnessOptions = {},
): Harness & ReturnType<typeof render> {
  let current = initial;
  const spies = {
    openEditor: vi.fn(),
    openAdd: vi.fn(),
    openMove: vi.fn(),
    openDelete: vi.fn(),
    openChangeKind: vi.fn(),
    announce: vi.fn(),
    focusNode: vi.fn(),
  };
  function Host({ children }: { children: ReactNode }) {
    const [state, dispatch] = useReducer(builderReducer, initial);
    current = state;
    const placed = (key: NodeKey) =>
      state.check.generation === state.generation
        ? (state.check.problems.byNode.get(key) ?? [])
        : [];
    const api: BuilderApi = {
      state,
      dispatch: (action: BuilderAction) => dispatch(action),
      derived:
        state.check.derived === null
          ? null
          : { seats: state.check.derived.seats ?? [], units: state.check.derived.units ?? [] },
      problemsFor: (key) =>
        placed(key)
          .filter((p) => p.severity === "problem")
          .map((p) => p.source as ConfigProblem),
      warningsFor: (key) =>
        placed(key)
          .filter((p) => p.severity === "warning")
          .map((p) => p.source as ConfigWarning),
      documentProblems: [],
      selection: { key: null, select: () => {} },
      openEditor: spies.openEditor,
      openAdd: spies.openAdd,
      openMove: spies.openMove,
      openDelete: spies.openDelete,
      openChangeKind: spies.openChangeKind,
      announce: spies.announce,
      focusNode: spies.focusNode,
      readOnly: options.readOnly ?? false,
      agents: options.agents ?? [],
      sandboxes: options.sandboxes ?? [],
      registerView: () => () => {},
    };
    return <BuilderContext.Provider value={api}>{children}</BuilderContext.Provider>;
  }
  const rendered = render(<Host>{ui}</Host>);
  return { ...rendered, state: () => current, spies };
}
