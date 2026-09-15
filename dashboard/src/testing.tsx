/**
 * What the design system draws, asked of the design system.
 *
 * A screen's claim is the PROP it passes: `variant="danger"` on a tag, a
 * dashed avatar for a human seat. The name of the class that paints it belongs
 * to uilet, changes on a bump, and is invisible to a reader; a test that
 * spells one is a test of the package rather than of the screen, and it goes
 * green again the moment the package renames it.
 *
 * So a case that has to reach for an element uilet drew renders a REFERENCE of
 * the same component here and compares against that. The assertion then says
 * "this is the element uilet draws for danger" without saying what uilet calls
 * it, which is the only form of that claim a screen suite is entitled to make.
 *
 * Only a suite imports this. It ships in no bundle: nothing under `app/`,
 * `routes/` or `components/` reaches for it.
 */

import { fireEvent, render, screen } from "@testing-library/react";
import { act, createElement } from "react";
import { createRoot } from "react-dom/client";
import type { ComponentProps, ElementType } from "react";
import { TreeCanvas, type TreeCardContext } from "@crewlethq/ui";

/**
 * Pick an option, which is how every choice in this product is made.
 *
 * Every dropdown is the design system's listbox rather than the platform's
 * own, so a choice is a press on the trigger and a press on a row, not a
 * `change` event carrying a value. The row is named by its LABEL, because
 * that is the only thing a reader ever sees; a suite that reached for the
 * stored value was reaching past the control.
 */
export function pick(control: HTMLElement, option: string | RegExp): void {
  fireEvent.click(control);
  fireEvent.mouseDown(screen.getByRole("option", { name: option }));
}

/** The class list uilet draws one component with, for these props. */
export function drawnClasses<T extends ElementType>(
  component: T,
  props: ComponentProps<T>,
): string[] {
  const host = document.createElement("div");
  const root = createRoot(host);
  act(() => {
    root.render(createElement(component, props));
  });
  const classes = [...(host.firstElementChild?.classList ?? [])];
  act(() => {
    root.unmount();
  });
  return classes;
}

/**
 * The classes that tell one set of props from another: what `variant="danger"`
 * adds over the default. The shared geometry drops out, so a match is about
 * the state and nothing else.
 */
export function distinguishing<T extends ElementType>(
  component: T,
  props: ComponentProps<T>,
  base: ComponentProps<T>,
): string[] {
  const plain = drawnClasses(component, base);
  const marks = drawnClasses(component, props).filter((name) => !plain.includes(name));
  if (marks.length === 0) {
    throw new Error("these props draw the same element as the default, so nothing can be asserted");
  }
  return marks;
}

/** Whether an element carries every class the design system draws for those props. */
export function isDrawnAs<T extends ElementType>(
  element: Element,
  component: T,
  props: ComponentProps<T>,
  base: ComponentProps<T>,
): boolean {
  return distinguishing(component, props, base).every((name) => element.classList.contains(name));
}

/**
 * The parts of the design system's tree canvas, asked of the design system.
 *
 * A CHART SUITE HAS TO REACH FOR THREE ELEMENTS THE PACKAGE DRAWS, and no prop
 * and no role names any of them: the CARD a node is drawn in (which a jsdom
 * harness has to report a size for, because jsdom has no layout), the probe the
 * gaps are measured from, and the drawing of the connectors. Spelling their
 * classes in a screen suite is what this file exists to prevent, so the names
 * are taken from a reference chart rendered here instead: one place that knows
 * them, and it learns them from the component rather than from a comment.
 *
 * Rendered with no ResizeObserver, which is what jsdom has: the component then
 * draws every card with nothing measured yet, which is all that is needed to
 * read a class off each.
 */
export function treeCanvasParts(): {
  card: string;
  gap: string;
  links: string;
  /** What a card gains when it stands for somebody outside the system. */
  outlined: string;
} {
  const plain = referenceChart(false);
  const marked = referenceChart(true);
  const names = {
    ...plain,
    outlined: marked.cardClasses.filter((name) => !plain.cardClasses.includes(name))[0] ?? "",
  };
  for (const [part, name] of Object.entries(names)) {
    if (typeof name === "string" && !name) {
      throw new Error(`the tree canvas draws no ${part} this harness can find`);
    }
  }
  return names;
}

/** One chart rendered to be read: what it calls each part it draws. */
function referenceChart(outline: boolean): {
  card: string;
  gap: string;
  links: string;
  cardClasses: string[];
} {
  // RENDERED THE WAY EVERY SUITE RENDERS, into the document, because this one
  // is a whole chart rather than a single element: it holds a layer host that
  // portals into a node it has to be able to find, and a viewport whose layout
  // effects read the element they are on.
  const { container: host, unmount } = render(
    createElement(TreeCanvas, {
      label: "Reference chart",
      nodes: [{ id: "a", label: "A" }],
      cards: () => [{ id: "a", children: [] }],
      cardOf: (id: string) => id,
      cardOutline: () => outline,
      renderCard: (id: string, card: TreeCardContext) => createElement("div", card.item(id), "A"),
    }),
  );
  const item = host.querySelector("[role='treeitem']");
  const tree = host.querySelector("[role='tree']");
  const svg = host.querySelector("svg");
  // The treeitem is the caller's own element here, drawn with no class, so the
  // card is simply what holds it.
  const card = item?.parentElement ?? null;
  // The probe is the one element the tree's own parent holds that is neither
  // the tree nor the connectors: it has no content and no role, by design.
  const probe = [...(tree?.parentElement?.children ?? [])].find((el) => el !== tree && el !== svg);
  const read = {
    card: className(card),
    gap: className(probe ?? null),
    links: className(svg),
    cardClasses: [...(card?.classList ?? [])],
  };
  unmount();
  return read;
}

/** An element's first class, or "" when it has none. */
function className(el: Element | null | undefined): string {
  const first = el?.classList[0];
  return first ?? "";
}

/**
 * Narrow a list screen the way a reader does: open the Filter menu, pick the
 * axis, then pick the answer from the editor the chip opens with.
 *
 * The editor opens by itself the moment an axis is added, which is the whole
 * reason the chip appears before it has a value; a helper that pressed the
 * chip again would close it.
 */
export function narrow(axis: string, answer: string | RegExp): void {
  fireEvent.click(screen.getByRole("button", { name: "Filter" }));
  fireEvent.click(screen.getByRole("menuitem", { name: axis }));
  fireEvent.mouseDown(screen.getByRole("option", { name: answer }));
}

/**
 * The layer the notifications are drawn into, found through the live region
 * every toast host keeps at the foot of its stack.
 *
 * THE REGION RATHER THAN A TOAST, because the host is always mounted and a
 * toast is not: a suite asking "which layer did these get portalled into"
 * has to be able to ask it of an empty host. A fullscreen surface renders
 * only its own subtree, so a toast portalled past it is a report nobody sees.
 *
 * Where more than one host is mounted (the application's, and the one the
 * builder nests inside its fullscreen container), the one holding a toast
 * wins, because that is the one the caller is asking about.
 */
export function toastHost(): HTMLElement | null {
  const hosts = screen
    .queryAllByRole("status", { name: /notification/i })
    .map((region) => region.parentElement?.parentElement ?? null)
    .filter((host): host is HTMLElement => host !== null);
  return hosts.find((host) => toastsIn(host).length > 0) ?? hosts[0] ?? null;
}

/**
 * The notifications a host is showing: its children that carry a control,
 * which is what tells a toast from the hidden live regions beside it.
 */
function toastsIn(host: HTMLElement): HTMLElement[] {
  return [...host.children].filter(
    (child): child is HTMLElement =>
      child instanceof HTMLElement && !!child.querySelector("button"),
  );
}

export function toasts(): HTMLElement[] {
  const host = toastHost();
  return host ? toastsIn(host) : [];
}

/**
 * What the notifications SAY, the hidden live regions excluded.
 *
 * The same sentence is in the document twice on purpose, drawn and spoken, so
 * a query over the whole page cannot tell a screen that reported something
 * once from one that reported it twice.
 */
export function toastText(): string {
  return toasts()
    .map((toast) => toast.textContent ?? "")
    .join(" ");
}

/**
 * A menu entry's own words, without the keyboard hint drawn beside it.
 *
 * The hint is part of the entry's accessible name, which is right: a reader
 * who cannot see the caps is told the shortcut. A suite comparing two menus
 * entry for entry is asking a different question, so it takes the words and
 * leaves the hint, and it does that by finding the caps rather than by naming
 * the class the design system wraps them in.
 */
export function menuEntryLabel(item: HTMLElement): string {
  const clone = item.cloneNode(true) as HTMLElement;
  for (const cap of [...clone.querySelectorAll("kbd")]) {
    let part: HTMLElement | null = cap as HTMLElement;
    while (part?.parentElement && part.parentElement !== clone) part = part.parentElement;
    part?.remove();
  }
  return (clone.textContent ?? "").trim();
}

/**
 * A media query list the suite drives.
 *
 * jsdom has no `matchMedia` and the setup file's stub answers "never
 * matches", which is the wide layout. Crossing the shell's breakpoint is a
 * state only a controllable one can reach, and it is the state the narrow
 * layout's drawer lives and dies in.
 */
export function installMedia(initial: boolean): {
  set: (matches: boolean) => void;
  restore: () => void;
} {
  const listeners = new Set<() => void>();
  let matches = initial;
  const had = Object.getOwnPropertyDescriptor(globalThis, "matchMedia");
  Object.defineProperty(globalThis, "matchMedia", {
    configurable: true,
    writable: true,
    value: (query: string) => ({
      get matches() {
        return matches;
      },
      media: query,
      onchange: null,
      addEventListener: (_: string, listener: () => void) => void listeners.add(listener),
      removeEventListener: (_: string, listener: () => void) => void listeners.delete(listener),
      addListener: (listener: () => void) => void listeners.add(listener),
      removeListener: (listener: () => void) => void listeners.delete(listener),
      dispatchEvent: () => false,
    }),
  });
  return {
    set: (next: boolean) => {
      matches = next;
      act(() => {
        for (const listener of [...listeners]) listener();
      });
    },
    restore: () => {
      if (had) Object.defineProperty(globalThis, "matchMedia", had);
      else delete (globalThis as Record<string, unknown>).matchMedia;
    },
  };
}
