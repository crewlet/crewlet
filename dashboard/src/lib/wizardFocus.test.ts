/**
 * Where focus goes around a wizard that stands in its page's place.
 */

import { afterEach, expect, test, vi } from "vitest";
import { focusQuietly } from "./wizardFocus.ts";

afterEach(() => {
  document.body.innerHTML = "";
});

// A HEADING TAKES FOCUS AS A READING POSITION, WITH NO RING: a wizard opened at
// its own address focuses its step's heading for assistive technology, and a
// script's focus with no pointer before it would otherwise draw the
// document's focus ring around a heading nobody can act on. A control keeps
// the browser's own treatment. Mutation: focus the heading plainly, and this
// is red.
test("a heading is focused without the ring, and a control with the browser's own treatment", () => {
  document.body.innerHTML = `<h2 tabindex="-1">Source</h2><input aria-label="Name" />`;
  const heading = document.querySelector("h2")!;
  const field = document.querySelector("input")!;
  const headingFocus = vi.spyOn(heading, "focus");
  const fieldFocus = vi.spyOn(field, "focus");

  focusQuietly(heading);
  expect(headingFocus).toHaveBeenCalledWith({ focusVisible: false });
  expect(document.activeElement).toBe(heading);

  focusQuietly(field);
  expect(fieldFocus).toHaveBeenCalledWith();
  expect(document.activeElement).toBe(field);
});
