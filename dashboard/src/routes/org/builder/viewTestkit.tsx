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
import { OrgNodeLabel } from "@crewlethq/ui";
import { treeCanvasParts } from "~/testing.tsx";
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

  /**
   * Reports every observed element's size, a few rounds, so cards rendered by a
   * layout are measured too, and then lands every card at its target.
   *
   * THE CHART TRAVELS. The design system tweens a relayout over real frames, so
   * a card's transform in the tick a suite changed the draft in is where the
   * card WAS; jsdom's own frames fire on a timer nothing here waits for. The
   * harness owns the frame queue instead and runs it past the tween's length,
   * so every suite reads the chart at rest, which is what each of them is
   * about. A suite about the travel itself drives [runFrames].
   */
  static settle(motion = true): void {
    for (let round = 0; round < 4; round++) {
      act(() => {
        for (const observer of [...LayoutObserver.instances]) observer.deliver();
      });
    }
    if (motion) settleMotion();
  }

  static install(): () => void {
    const real = globalThis.ResizeObserver;
    const realFrame = globalThis.requestAnimationFrame;
    const realCancelFrame = globalThis.cancelAnimationFrame;
    LayoutObserver.instances = [];
    LayoutObserver.sizer = defaultSizer;
    frames.length = 0;
    // Before any suite render, for the reason `known` gives.
    parts ??= treeCanvasParts();
    globalThis.ResizeObserver = LayoutObserver as unknown as typeof ResizeObserver;
    globalThis.requestAnimationFrame = ((frame: FrameRequestCallback) =>
      frames.push(frame)) as typeof globalThis.requestAnimationFrame;
    globalThis.cancelAnimationFrame = ((id: number) => {
      frames[id - 1] = null;
    }) as typeof globalThis.cancelAnimationFrame;
    return () => {
      globalThis.ResizeObserver = real;
      globalThis.requestAnimationFrame = realFrame;
      globalThis.cancelAnimationFrame = realCancelFrame;
    };
  }
}

/** The frames the chart asked for and has not been given: see [LayoutObserver.settle]. */
const frames: (FrameRequestCallback | null)[] = [];

/** Runs every pending frame at `at`, as a browser does when it paints. */
export function runFrames(at: number): void {
  const due = [...frames];
  frames.length = 0;
  act(() => {
    for (const frame of due) frame?.(at);
  });
}

/** Runs frames until nothing is travelling: a relayout takes well under a second. */
function settleMotion(): void {
  for (let round = 0; round < 4 && frames.some(Boolean); round++) {
    runFrames(performance.now() + 1000);
  }
}

/** The fixed sizes the suites lay out by: a viewport, the gap probe, and cards by their rows. */
export const CARD_WIDTH = 240;
export const ROW_HEIGHT = 40;
export const VIEWPORT = { width: 1200, height: 800 };

/**
 * The canvas viewport, found by what it IS rather than by what the design
 * system calls its class: a focusable group announced as a canvas.
 */
export function isCanvasViewport(el: Element): boolean {
  return el.getAttribute("aria-roledescription") === "canvas";
}

/**
 * The panned and zoomed layer: the viewport's own child, which is where the
 * transform lives. Reaching for it through a class would be a claim about the
 * design system's markup rather than about this screen.
 */
export function canvasWorld(container: HTMLElement): HTMLElement {
  const viewport = [...container.querySelectorAll("*")].find(isCanvasViewport);
  if (!viewport) throw new Error("no canvas is mounted");
  return viewport.firstElementChild as HTMLElement;
}

/*
 * The chart is the design system's `TreeCanvas`, and the two boxes it measures
 * are ITS elements: a gap probe drawn from spacing tokens, and a card per node.
 * Neither has a role or any content to find it by, so the harness asks the
 * package what it draws (`treeCanvasParts`) rather than spelling its class
 * names here, where a bump would leave them silently matching nothing and every
 * chart suite reporting an unmeasured canvas. The VIEWPORT is found by what it
 * IS: a focusable group announced as a canvas is a thing a reader meets.
 *
 * ASKED ONCE, BY `install`, AND NEVER LATER. Finding out means rendering a
 * reference chart, and every later caller is inside an `act` of the suite's own
 * render (the sizer is, being driven by `settle`); a render started there does
 * not commit before the callback returns, so the reference chart would be read
 * while it was still empty.
 */
let parts: ReturnType<typeof treeCanvasParts> | null = null;
const known = (): ReturnType<typeof treeCanvasParts> => {
  if (!parts)
    throw new Error("LayoutObserver.install has not run, so the chart's parts are unknown");
  return parts;
};

function defaultSizer(el: Element): { width: number; height: number } | null {
  if (isCanvasViewport(el)) return VIEWPORT;
  const { card, gap, margin } = known();
  if (el.classList.contains(gap)) return { width: 24, height: 32 };
  // NONE, so a suite reads the coordinates the layout alone decides. The space
  // the chart keeps around itself is the design system's own measurement from
  // its own tokens, and a suite that added it to every expected position would
  // be asserting that value here as well as there.
  if (el.classList.contains(margin)) return { width: 0, height: 0 };
  if (el.classList.contains(card)) {
    const items = el.querySelectorAll("[role='treeitem']").length;
    return { width: CARD_WIDTH, height: Math.max(1, items) * ROW_HEIGHT };
  }
  return null;
}

/** The card a node is drawn in, which the chart suites measure and compare. */
export function chartCard(el: Element): HTMLElement {
  const card = el.closest<HTMLElement>(`.${known().card}`);
  if (!card) throw new Error("this element is not drawn in a chart card");
  return card;
}

/** Every card on the chart, in the order the layout placed them. */
export function chartCards(container: HTMLElement): HTMLElement[] {
  return [...container.querySelectorAll<HTMLElement>(`.${known().card}`)];
}

/** Whether a card carries the mark the chart draws for somebody outside the system. */
export function isOutlinedCard(card: HTMLElement): boolean {
  return card.classList.contains(known().outlined);
}

/** The drawing of the connectors between the cards. */
export function chartLinks(container: HTMLElement): HTMLElement {
  const links = container.querySelector<HTMLElement>(`.${known().links}`);
  if (!links) throw new Error("the chart draws no connectors");
  return links;
}

/*
 * WHAT A NODE SAYS IS THE DESIGN SYSTEM'S TOO. `OrgNodeLabel` draws the name,
 * the caption under it, the slot for what arrives from a push and the zone the
 * icon sits in, and each is a class of the package. A suite that spelled those
 * class names would be this application naming what the package draws, which
 * is the thing `designSystem.test.ts` refuses: bump the package and they match
 * nothing, silently. So the harness renders one label and reads them off it.
 */
let labelParts: { name: string; caption: string; trailing: string } | null = null;

/** What `OrgNodeLabel` calls each part it draws. Asked once, then remembered. */
export function orgNodeParts(): { name: string; caption: string; trailing: string } {
  if (labelParts) return labelParts;
  const { container, unmount } = render(
    <OrgNodeLabel name="Name" caption="Caption" trailing={<i data-probe="trailing" />} />,
  );
  const className = (el: Element | null | undefined) => el?.className ?? "";
  const found = {
    name: className(
      [...container.querySelectorAll("span")].find((el) => el.textContent === "Name"),
    ),
    caption: className(
      [...container.querySelectorAll("span")].find((el) => el.textContent === "Caption")
        ?.parentElement,
    ),
    trailing: className(container.querySelector("[data-probe='trailing']")?.parentElement),
  };
  unmount();
  for (const [part, name] of Object.entries(found)) {
    if (!name) throw new Error(`the org node label draws no ${part} this harness can find`);
  }
  labelParts = found;
  return found;
}

/**
 * The first element under `item` carrying the class the label calls `part`.
 *
 * Matched on the classList rather than through a selector, because a selector
 * assembled from a variable still READS as a query for a class named after
 * that variable, and `designSystem.test.ts` scans for exactly that.
 */
function labelPart(item: HTMLElement, part: keyof ReturnType<typeof orgNodeParts>) {
  const wanted = orgNodeParts()[part];
  return [...item.querySelectorAll<HTMLElement>("*")].find((el) => el.classList.contains(wanted));
}

/** A node's name, as the label drew it. */
export function nodeName(item: HTMLElement): string | null {
  return labelPart(item, "name")?.textContent ?? null;
}

/** The slot a node keeps for what arrives from a push, whatever it holds. */
export function nodeTrailing(item: HTMLElement): HTMLElement | null {
  return labelPart(item, "trailing") ?? null;
}
