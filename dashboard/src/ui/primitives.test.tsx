/**
 * The one mark this file still draws.
 *
 * `PhaseTag`'s whole content is that it draws NO colour. A phase is a
 * category, and a pill that took a hue for one again would put a second
 * coloured thing beside the state on every row.
 *
 * The copy and download controls that used to be asserted here are
 * `@crewlethq/ui`'s now, and their suites live beside them in uilet.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { PhaseTag } from "./primitives.tsx";

afterEach(cleanup);

describe("PhaseTag", () => {
  // A PHASE IS A CATEGORY, AND A CATEGORY HAS NO COLOUR. The design system
  // removed its phase family: beside a state badge the state is the one
  // coloured thing on the row, and the phase is the word that says where the
  // work is. So every phase — the engine's six and one a later build sends —
  // is the same neutral pill, and the word is the whole of what it says.
  test("every phase is the neutral pill with its word", () => {
    const drawn = [
      "onboarding",
      "execute",
      "review",
      "subagent",
      "auxiliary",
      "judge",
      "a_phase_from_a_later_build",
    ].map((phase) => {
      const { container, unmount } = render(<PhaseTag phase={phase} />);
      const pill = container.querySelector(".crewlet-tag")!;
      expect(pill.classList.contains("crewlet-tag--neutral"), phase).toBe(true);
      expect(container.textContent).toBe(phase);
      const classes = [...pill.classList].sort().join(" ");
      unmount();
      return classes;
    });
    // ONE DRAWING FOR ALL OF THEM, which stays red however a per-phase fill
    // is respelled.
    expect(new Set(drawn).size).toBe(1);
  });

  // A RECORD WITH NO PHASE IS NOT A PHASE CALLED "". An empty pill is a
  // coloured gap a reader has to hover to interrogate.
  test("names an absent phase rather than drawing an empty pill", () => {
    const { container } = render(<PhaseTag phase="" />);
    // THE LABEL IS THE ASSERTION, not the glyph. `EmptyValue` draws the mark
    // for the eye and the sentence for everyone else, and the sentence is the
    // half a hover-only `title` never had.
    expect(container.querySelector(".crewlet-empty-value")).not.toBeNull();
    expect(container.textContent).toContain("No phase on this record");
    expect(container.querySelector(".crewlet-tag--neutral")).not.toBeNull();
  });

  // The value is a column in the event store, and nothing normalises its case
  // on the way out — so the pill does, or one phase reads as two words on two
  // screens.
  test("a phase is said in one case whatever the record's", () => {
    const { container } = render(<PhaseTag phase="Execute" />);
    expect(container.textContent).toBe("execute");
  });
});
