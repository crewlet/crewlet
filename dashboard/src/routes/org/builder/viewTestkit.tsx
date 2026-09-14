/**
 * A Builder stand-in for the view, editor and dialog suites. Imported by
 * tests only.
 *
 * THE REAL REDUCER, A RECORDED CONTEXT. Every surface reads and dispatches
 * through `BuilderContext`, so the harness provides one over `builderReducer`
 * itself: an operation a surface records really changes the draft it draws
 * next. What belongs to `Builder.tsx` (the other dialogs, the check, the live
 * region) is replaced by spies, so a suite asserts what a surface ASKED for
 * (`openDelete` with this key) rather than a dialog another surface draws.
 * ONE HARNESS builds the context every suite renders in, so a change to the
 * Builder's contract is made to the test double once, as it is to the
 * Builder.
 *
 * jsdom has no layout. The canvas measures its viewport and every card with
 * ResizeObserver, so [LayoutObserver] stands in for it: it records every
 * observed element and, when told, reports a size for each, the viewport at
 * a fixed size and each card by what it holds.
 */

import { act, render } from "@testing-library/react";
import { useCallback, useMemo, useReducer, useRef, useState, type ReactNode } from "react";
import { vi } from "vitest";
import { Router } from "~/app/router.tsx";
import type { AgentRow, ConfigProblem, ConfigWarning, SandboxEntry } from "~/protocol/index.ts";
import {
  BuilderContext,
  type BuilderApi,
  type BuilderDerived,
  type BuilderViewHandle,
} from "./BuilderContext.tsx";
import type { NodeKey } from "./model/keys.ts";
import { countingKeys } from "./model/testkit.ts";
import { builderReducer, type BuilderAction, type BuilderState } from "./model/reducer.ts";

/** The spies standing in for what the Builder owns. */
export interface BuilderSpies {
  openEditor: ReturnType<typeof vi.fn<BuilderApi["openEditor"]>>;
  openAdd: ReturnType<typeof vi.fn<BuilderApi["openAdd"]>>;
  openMove: ReturnType<typeof vi.fn<(key: NodeKey) => void>>;
  openDelete: ReturnType<typeof vi.fn<(key: NodeKey) => void>>;
  openChangeKind: ReturnType<typeof vi.fn<(key: NodeKey) => void>>;
  announce: ReturnType<typeof vi.fn<(message: string) => void>>;
  dispatched: BuilderAction[];
}

export function builderSpies(): BuilderSpies {
  return {
    openEditor: vi.fn(),
    openAdd: vi.fn(),
    openMove: vi.fn(),
    openDelete: vi.fn(),
    openChangeKind: vi.fn(),
    announce: vi.fn(),
    dispatched: [],
  };
}

/** What a test holds on to while the harness is mounted. */
export interface HarnessProbe {
  state: BuilderState;
  dispatch: (action: BuilderAction) => void;
  view: BuilderViewHandle | null;
  selection: NodeKey | null;
}

export function BuilderHarness({
  initial,
  spies,
  probe,
  readOnly = false,
  agents = [],
  sandboxes = [],
  children,
}: {
  initial: BuilderState;
  spies: BuilderSpies;
  probe: HarnessProbe;
  readOnly?: boolean;
  agents?: AgentRow[];
  sandboxes?: SandboxEntry[];
  children: ReactNode;
}) {
  const [state, rawDispatch] = useReducer(builderReducer, initial);
  const [selected, setSelected] = useState<NodeKey | null>(null);
  // One source for the harness's life, as the Builder has one.
  const [keys] = useState(() => countingKeys("test"));
  const view = useRef<BuilderViewHandle | null>(null);
  const dispatch = useCallback(
    (action: BuilderAction) => {
      spies.dispatched.push(action);
      rawDispatch(action);
    },
    [spies],
  );
  const registerView = useCallback(
    (handle: BuilderViewHandle) => {
      view.current = handle;
      probe.view = handle;
      return () => {
        if (view.current === handle) {
          view.current = null;
          probe.view = null;
        }
      };
    },
    [probe],
  );
  probe.state = state;
  probe.dispatch = dispatch;
  probe.selection = selected;

  const current = state.check.generation === state.generation;
  const placed = (key: NodeKey, severity: "problem" | "warning") =>
    current
      ? (state.check.problems.byNode.get(key) ?? [])
          .filter((p) => p.severity === severity)
          .map((p) => p.source)
      : [];
  const derived: BuilderDerived | null =
    current && state.check.derived
      ? { seats: state.check.derived.seats ?? [], units: state.check.derived.units ?? [] }
      : null;

  const api = useMemo<BuilderApi>(
    () => ({
      state,
      dispatch,
      derived,
      problemsFor: (key) => placed(key, "problem") as ConfigProblem[],
      warningsFor: (key) => placed(key, "warning") as ConfigWarning[],
      documentProblems: [],
      selection: { key: selected, select: setSelected },
      openEditor: spies.openEditor,
      openAdd: spies.openAdd,
      openMove: spies.openMove,
      openDelete: spies.openDelete,
      openChangeKind: spies.openChangeKind,
      announce: spies.announce,
      focusNode: (key) => view.current?.focusNode(key),
      readOnly,
      agents,
      sandboxes,
      keys,
      registerView,
    }),
    [state, dispatch, selected, readOnly, agents, sandboxes, keys, registerView, spies],
  );

  return (
    <Router>
      <BuilderContext.Provider value={api}>{children}</BuilderContext.Provider>
    </Router>
  );
}

/** A probe to hand to [BuilderHarness]. */
export function harnessProbe(): HarnessProbe {
  return {
    state: undefined as unknown as BuilderState,
    dispatch: () => {},
    view: null,
    selection: null,
  };
}

/** What a suite may set on the harness. */
export interface HarnessOptions {
  readonly readOnly?: boolean;
  readonly agents?: AgentRow[];
  readonly sandboxes?: SandboxEntry[];
}

/**
 * Renders `ui` inside the harness, starting from `initial`: the editor and
 * dialog suites' entry, which read the state the last render saw and the
 * spies rather than hold a probe.
 */
export function renderInBuilder(
  initial: BuilderState,
  ui: ReactNode,
  options: HarnessOptions = {},
): ReturnType<typeof render> & { state(): BuilderState; spies: BuilderSpies } {
  const spies = builderSpies();
  const probe = harnessProbe();
  const rendered = render(
    <BuilderHarness
      initial={initial}
      spies={spies}
      probe={probe}
      readOnly={options.readOnly}
      agents={options.agents}
      sandboxes={options.sandboxes}
    >
      {ui}
    </BuilderHarness>,
  );
  return { ...rendered, state: () => probe.state, spies };
}

type Sizer = (el: Element) => { width: number; height: number } | null;

/**
 * A ResizeObserver a suite drives. Every instance records what it observes;
 * [LayoutObserver.settle] reports a size for every observed element, as a
 * browser does after layout, until nothing changes.
 */
export class LayoutObserver {
  static instances: LayoutObserver[] = [];
  static sizer: Sizer = () => null;
  readonly observed = new Set<Element>();
  constructor(private readonly callback: ResizeObserverCallback) {
    LayoutObserver.instances.push(this);
  }
  observe(el: Element): void {
    this.observed.add(el);
  }
  unobserve(el: Element): void {
    this.observed.delete(el);
  }
  disconnect(): void {
    this.observed.clear();
  }

  private deliver(): void {
    const entries = [...this.observed]
      .map((target) => {
        const size = LayoutObserver.sizer(target);
        if (!size) return null;
        return {
          target,
          contentRect: { width: size.width, height: size.height },
          borderBoxSize: [{ inlineSize: size.width, blockSize: size.height }],
        };
      })
      .filter((e) => e !== null) as unknown as ResizeObserverEntry[];
    if (entries.length > 0) this.callback(entries, this as unknown as ResizeObserver);
  }

  /** Reports every observed element's size, a few rounds, so cards rendered by a layout are measured too. */
  static settle(): void {
    for (let round = 0; round < 4; round++) {
      act(() => {
        for (const observer of [...LayoutObserver.instances]) observer.deliver();
      });
    }
  }

  static install(): () => void {
    const real = globalThis.ResizeObserver;
    LayoutObserver.instances = [];
    LayoutObserver.sizer = defaultSizer;
    globalThis.ResizeObserver = LayoutObserver as unknown as typeof ResizeObserver;
    return () => {
      globalThis.ResizeObserver = real;
    };
  }
}

/** The fixed sizes the suites lay out by: a viewport, the gap probe, and cards by their rows. */
export const CARD_WIDTH = 240;
export const ROW_HEIGHT = 40;
export const VIEWPORT = { width: 1200, height: 800 };

function defaultSizer(el: Element): { width: number; height: number } | null {
  if (el.classList.contains("canvas-viewport")) return VIEWPORT;
  if (el.classList.contains("bchart-gap-probe")) return { width: 24, height: 32 };
  if (el.classList.contains("bchart-box")) {
    const items = el.querySelectorAll("[role='treeitem']").length;
    return { width: CARD_WIDTH, height: Math.max(1, items) * ROW_HEIGHT };
  }
  return null;
}
