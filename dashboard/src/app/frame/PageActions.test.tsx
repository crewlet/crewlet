/**
 * A screen's controls, outside a frame, are still spaced as the frame spaces
 * them: the fallback wrapper and the page header's slot are one rule.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { PageActions } from "./PageActions.tsx";

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
