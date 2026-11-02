/**
 * Arriving at a section an address names: revealed and focused where it is
 * drawn, and nothing at all where it is not.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Router } from "~/app/router.tsx";
import { useSectionArrival } from "./arrival.ts";

afterEach(() => {
  cleanup();
  location.hash = "";
});

/** A focusable section whose inner target `find` looks for. */
function Section({ find }: { find?: (el: HTMLElement) => HTMLElement | null }) {
  const { ref } = useSectionArrival("seat:ghost", find);
  return <div ref={ref} tabIndex={-1} data-testid="section" />;
}

test("a section the address names is focused when drawn", async () => {
  location.hash = "#/company/budgets?section=seat:ghost";
  render(
    <Router>
      <Section />
    </Router>,
  );
  await act(async () => {});
  expect(document.activeElement).toBe(screen.getByTestId("section"));
});

// A SECTION INSIDE ONE THAT IS NOT DRAWN IS NOT THAT ONE: a seat the table no
// longer lists leaves the table where it is, and focus where it was, rather
// than focusing the table as if it were the seat.
//
// Mutation: fall back to the section when `find` finds nothing, and the
// section takes focus.
test("an inner target that is not drawn reveals and focuses nothing", async () => {
  location.hash = "#/company/budgets?section=seat:ghost";
  render(
    <Router>
      <Section find={() => null} />
    </Router>,
  );
  await act(async () => {});
  expect(document.activeElement).toBe(document.body);
});
