/**
 * The one sentence this component exists for, and when it is a lie.
 *
 * `FacetRail` makes "over what is loaded or over what exists" a required prop
 * rather than a caption a caller remembers to add, because a number beside a
 * chip is read as "how many there are". It then printed that sentence
 * UNCONDITIONALLY — so it sat beside the Inbox's unread/all/snoozed control,
 * three scopes asked of the ENGINE whose rows the loaded page does not hold, and
 * qualified figures the reader could not see. Nothing asserted the caption
 * anywhere: the rule was held by memory, which is how it decayed.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { FacetRail } from "./FacetRail.tsx";

afterEach(cleanup);

// THE HALF THAT GOES RED IF SOMEBODY "FIXES" THE DEFECT BY DELETING THE NOTE.
test("a rail that draws a count says what the count is over", () => {
  render(
    <FacetRail
      name="Reason"
      value=""
      onChange={() => {}}
      over="loaded"
      facets={[{ value: "assignee", label: "assigned to you", count: 3 }]}
    />,
  );
  expect(screen.getByText("counts over the rows loaded")).toBeTruthy();
});

// AND THE HALF THAT GOES RED THE INSTANT THE GUARD REVERTS TO AN
// UNCONDITIONAL SPAN. The chips are asserted first, so the case cannot pass
// because the component returned null.
test("a rail whose counts have not answered says nothing about counts", () => {
  render(
    <FacetRail
      name="Category"
      value=""
      onChange={() => {}}
      over="window"
      facets={[
        { value: "agent", label: "agent", count: null },
        { value: "config", label: "config", count: null },
      ]}
    />,
  );
  expect(screen.getByRole("group", { name: "Category" }).querySelectorAll("button")).toHaveLength(
    3,
  );
  expect(screen.queryByText("counts over the whole window")).toBeNull();
});

// A NUMBER IS DRAWN, not every number is drawn: the sentence arrives with the
// first figure a reader can see rather than waiting for the last.
test("one answered count is enough to qualify the row", () => {
  render(
    <FacetRail
      name="Category"
      value=""
      onChange={() => {}}
      over="window"
      facets={[
        { value: "agent", label: "agent", count: null },
        { value: "config", label: "config", count: 7 },
      ]}
    />,
  );
  expect(screen.getByText("counts over the whole window")).toBeTruthy();
});
