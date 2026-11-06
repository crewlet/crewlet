// @vitest-environment node
/**
 * Which moves keep the builder mounted, over the addresses THIS frame
 * makes.
 *
 * WHY THIS IS ITS OWN FILE. [keepsTheLens] is the whole of what every guard in
 * the builder asks — the draft's and the node editor's both — and it is a pure
 * function of a route, so the cases that matter are cheap here and expensive
 * anywhere else. It is also the one thing in the builder that a move between
 * screens silently invalidates: the predicate names an address, and an address
 * that has moved does not fail, it simply answers `false` for everything, and
 * a guard that answers `false` for everything HOLDS EVERY MOVE — including the
 * ones inside the builder it was written to let through. The builder came here
 * from a screen at `#/org`, so that is not a hypothetical.
 *
 * The frame around it is this tree's: the sidebar, Structure's two tabs, a
 * seat's page and an object peek. Each one of those moves is written out
 * below as the ROUTE it produces, because the router puts a route to the guard
 * and nothing else. It came here from `#/company/org/edit`, and before that
 * from a `lens=builder` on the company screen and from `#/org`.
 */

import { expect, test } from "vitest";
import { parseHash } from "~/app/router.tsx";
import { keepsTheLens } from "./BuilderContext.tsx";

/** What the router hands a guard for a hash. */
const to = (hash: string) => parseHash(hash);

/*
 * THE MOVES INSIDE THE BUILDER. Structure's two tabs are both the builder, and
 * the chart is a `section` (the router PUSHES for one, so each really is put
 * to the guard); each keeps the Builder, its draft and whatever dialog is open
 * exactly as they were. Asking about any of them would put "discard your
 * changes?" in front of a reader who chose a tab.
 */
test("a move within the builder keeps it: either tab, the chart and a selection", () => {
  for (const hash of [
    "#/company/org",
    "#/company/org/table",
    "#/company/org?chart=reporting",
    "#/company/org/table?unit=engineering",
    "#/company/org?seat=ceo",
    "#/company/org?seat=ceo&editor=open",
    // The peek rail is a FILTER, so the router replaces and never asks at all,
    // but the route it produces still answers honestly here, because a
    // reader who opened a peek and then pressed Back traverses to it.
    "#/company/org/table?peek=unit%3Aengineering",
  ]) {
    expect(keepsTheLens(to(hash)), hash).toBe(true);
  }
});

/*
 * AND THE MOVES OFF IT. A seat's page, the org pages Structure took over and
 * every other screen are a departure: the Builder unmounts and takes the draft
 * with it.
 */
test("a move off the builder does not keep it, including to a seat's page", () => {
  for (const hash of [
    // A seat's page, which a card's "Open seat" goes to.
    "#/company/org/seats/ceo",
    "#/company/org/seats/ceo?from=structure",
    // The pages Structure took over, which resolve to a screen that moves the
    // reader on: until it has, the Builder is not mounted.
    "#/company/org/edit",
    "#/company/org/roster",
    "#/company/org/teams",
    "#/company/org/teams/engineering",
    // Another screen of the Company workspace.
    "#/company/schedules",
    // The sidebar, which is ordinary links.
    "#/inbox",
    "#/worklet",
    "#/settings/config",
    // An address under a tab is no tab.
    "#/company/org/table/engineering",
  ]) {
    expect(keepsTheLens(to(hash)), hash).toBe(false);
  }
});

/*
 * THE MUTATION THIS FILE EXISTS FOR. A predicate keyed on the screen the
 * builder USED to be (`org-edit`, at `#/company/org/edit`) answers false for
 * every address in the first case, so the second would still pass while the
 * first, the one that says a reader choosing a tab is not leaving, went red.
 * This is that failure written down, so the next move of this screen reads it.
 */
test("the old address is not this one, and would hold every move in the builder", () => {
  const atOldEdit = (route: ReturnType<typeof to>) => route.path.join("/") === "company/org/edit";
  const inside = to("#/company/org/table");
  expect(keepsTheLens(inside)).toBe(true);
  expect(atOldEdit(inside)).toBe(false);
  expect(keepsTheLens(to("#/company/org/edit"))).toBe(false);
});
