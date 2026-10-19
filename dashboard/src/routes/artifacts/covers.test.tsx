/**
 * The gallery covers: a cover with nothing to draw (its outline still
 * loading, a sheet with no columns) draws the kind's glyph, as a card with no
 * cover does, rather than an empty box the card cannot tell from a picture.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { DashboardCardCover } from "./DashboardCardCover.tsx";
import { DesignCover } from "./DesignCover.tsx";
import { DocCover } from "./DocCover.tsx";
import { SheetCover } from "./SheetCover.tsx";
import { SlidesCover } from "./SlidesCover.tsx";

vi.mock("~/lib/useRest.ts", () => ({ useRest: () => ({ data: null }) }));
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: () => ({ data: null, loading: true, error: null, refusal: null, refetch: () => {} }),
}));

afterEach(cleanup);

const item = { id: "a1", title: "Untitled", head_number: 1 };

test.each([
  ["doc", <DocCover item={item} />],
  ["slides", <SlidesCover item={item} />],
  ["design", <DesignCover item={item} />],
  ["dashboard", <DashboardCardCover item={item} />],
  ["sheet", <SheetCover item={item} />],
])("a %s cover with nothing to draw yet draws the kind's glyph", (_, cover) => {
  const { container } = render(<>{cover}</>);
  expect(container.querySelector(".crewlet-artifact-card__glyph svg")).not.toBeNull();
});
