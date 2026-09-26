/**
 * The page bar's own search field is the BAR's field.
 *
 * The kit's `SearchTrigger` defaults to its `rail` variant — the sidebar's
 * field, the whole width of whatever holds it, label and hint at every width.
 * Left at that default here it swallowed the slack the trail is meant to take
 * on a desktop bar and dropped onto a line of its own on a phone, under the
 * trail it is written to sit beside. Nothing else on the screen looks wrong
 * when it happens, so it is held here.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { PageBar } from "./PageBar.tsx";
import { PAGE_ACTIONS_SLOT, PageActions } from "./PageActions.tsx";

afterEach(cleanup);

test("the bar's search trigger is the toolbar field, not the sidebar's", () => {
  render(<PageBar crumbs={[{ label: "Work" }]} onSearch={() => {}} />);
  const trigger = screen.getByRole("button", { name: "Search" });
  expect(trigger.className).toContain("crewlet-search-trigger--toolbar");
  expect(trigger.className).not.toContain("crewlet-search-trigger--rail");
});

test("the field sits in the wrapper that lets it give way on a full bar", () => {
  // `.page-search` (frame.css) is what makes the field's width flexible and
  // collapses it by its own width; a trigger outside it is back on the kit's
  // 200px floor, which pushed the keycap past the edge of a busy bar.
  render(<PageBar crumbs={[{ label: "Work" }]} onSearch={() => {}} />);
  const trigger = screen.getByRole("button", { name: "Search" });
  expect(trigger.parentElement?.className).toBe("page-search");
});

// A SCREEN'S CONTROLS SIT AS FAR APART AS THE FRAME'S. The slot was `gap-1`
// (4px) while the star and Copy link beside it sat at the kit's control
// spacing, so My work's "Whose day" select touched its "Inbox →" link.
test("the controls slot spaces a screen's controls at the kit's control gap", () => {
  const { container } = render(<PageBar crumbs={[{ label: "Work" }]} onSearch={() => {}} />);
  const slot = container.querySelector(`#${PAGE_ACTIONS_SLOT}`);
  expect(slot?.classList.contains("gap-2")).toBe(true);
  expect(slot?.classList.contains("gap-1")).toBe(false);
});

test("controls rendered outside a frame keep the same gap", () => {
  const { container } = render(
    <PageActions>
      <button type="button">One</button>
    </PageActions>,
  );
  const wrap = container.querySelector(".page-actions");
  expect(wrap?.classList.contains("gap-2")).toBe(true);
});
