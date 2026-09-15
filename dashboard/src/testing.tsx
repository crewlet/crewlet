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

import { createElement } from "react";
import { createRoot } from "react-dom/client";
import { act } from "react";
import type { ComponentProps, ElementType } from "react";

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
