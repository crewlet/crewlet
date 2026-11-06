/**
 * A screen's controls, outside a frame, are still spaced as the frame spaces
 * them: the fallback wrapper and the page header's slot are one rule.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { OwnHeaderSlot } from "./OwnHeader.tsx";
import { PAGE_ACTIONS_SLOT, PAGE_LENSES_SLOT, PageActions, PageLenses } from "./PageActions.tsx";

afterEach(cleanup);

test("controls rendered with no frame to land in keep the kit's control gap", () => {
  const { container } = render(
    <PageActions>
      <button type="button">One</button>
      <button type="button">Two</button>
    </PageActions>,
  );
  const wrap = container.querySelector(".page-actions");
  expect(wrap?.classList.contains("gap-2")).toBe(true);
  expect(wrap?.classList.contains("gap-1")).toBe(false);
});

// A LENS ROW FINDS THE BAR'S LENS SLOT, and with no frame it is still drawn —
// in place, under the class the slot carries.
test("lenses land in the bar's lens slot, or in place without one", () => {
  const bar = document.createElement("div");
  bar.id = PAGE_LENSES_SLOT;
  document.body.append(bar);
  render(
    <PageLenses>
      <button type="button">Items</button>
    </PageLenses>,
  );
  expect(bar.textContent).toBe("Items");
  cleanup();
  bar.remove();

  const { container } = render(
    <PageLenses>
      <button type="button">About</button>
    </PageLenses>,
  );
  expect(container.querySelector(".page-lenses")?.textContent).toBe("About");
});

// A SCREEN CAN MOUNT BEFORE THE BAR COMES BACK. Leaving a screen that draws its
// own bar returns the frame's a commit after the next screen mounts, and a slot
// looked up once left that screen's controls on the page for as long as it stayed.
test("controls move into the frame's bar when it comes back after they mounted", () => {
  const ownBar = document.createElement("div");
  const frame = (own: boolean) => (
    <OwnHeaderSlot.Provider value={own ? ownBar : null}>
      {own ? null : <div data-testid="bar" id={PAGE_ACTIONS_SLOT} />}
      <PageActions>
        <button type="button">New task</button>
      </PageActions>
    </OwnHeaderSlot.Provider>
  );
  const { container, rerender, getByTestId } = render(frame(true));
  expect(container.querySelector(".page-actions")?.textContent).toBe("New task");
  rerender(frame(false));
  expect(getByTestId("bar").textContent).toBe("New task");
  expect(container.querySelector(".page-actions")).toBeNull();
});
