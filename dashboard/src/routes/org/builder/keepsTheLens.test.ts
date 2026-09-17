// @vitest-environment node
/**
 * Which moves keep the Builder lens mounted, over the addresses THIS frame
 * makes.
 *
 * WHY THIS IS ITS OWN FILE. [keepsTheLens] is the whole of what every guard in
 * the lens asks — the draft's and the node editor's both — and it is a pure
 * function of a route, so the cases that matter are cheap here and expensive
 * anywhere else. It is also the one thing in the lens that a move between
 * screens silently invalidates: the predicate names an address, and an address
 * that has moved does not fail, it simply answers `false` for everything, and
 * a guard that answers `false` for everything HOLDS EVERY MOVE — including the
 * ones inside the lens it was written to let through. The builder came here
 * from a screen at `#/org`, so that is not a hypothetical.
 *
 * The frame around it is this tree's: an app rail, a workspace sidebar, a
 * command palette, an object peek and the digit chords on a lens strip. Each
 * one of those moves is written out below as the ROUTE it produces, because
 * the router puts a route to the guard and nothing else.
 */

import { expect, test } from "vitest";
import { parseHash } from "~/app/router.tsx";
import { keepsTheLens } from "./BuilderContext.tsx";

/** What the router hands a guard for a hash. */
const to = (hash: string) => parseHash(hash);

/*
 * THE MOVES INSIDE THE LENS. Each is a `section` — the router PUSHES for one,
 * so each really is put to the guard — and each keeps the Builder, its draft
 * and whatever dialog is open exactly as they were. Asking about any of them
 * would put "discard your changes?" in front of a reader who chose a view.
 */
test("a move within the lens keeps it: the view, the chart and a selection", () => {
  for (const hash of [
    "#/company?lens=builder",
    "#/company?lens=builder&view=table",
    "#/company?lens=builder&view=visualization",
    "#/company?lens=builder&view=visualization&chart=reporting",
    "#/company?lens=builder&view=table&unit=Engineering",
    "#/company?lens=builder&view=table&seat=ceo",
    // The peek rail is a FILTER, so the router replaces and never asks at all
    // — but the route it produces still answers honestly here, because a
    // reader who opened a peek and then pressed Back traverses to it.
    "#/company?lens=builder&peek=unit%3AEngineering",
  ]) {
    expect(keepsTheLens(to(hash)), hash).toBe(true);
  }
});

/*
 * AND THE MOVES OFF IT. The two read lenses of this same screen are as much a
 * departure as another screen is: the Builder unmounts and takes the draft
 * with it. `1`, `2` and `3` on the lens strip are those same three moves —
 * `useTab` binds a digit per tab in this frame, which the screen this lens
 * came from had no equivalent of.
 */
test("a move off the lens does not keep it, including to this screen's read lenses", () => {
  for (const hash of [
    // The lens strip, and its digit chords.
    "#/company",
    "#/company?lens=chart",
    "#/company?lens=charter",
    // The app rail and the workspace sidebar, which are ordinary links.
    "#/company/people",
    "#/inbox",
    "#/work",
    "#/admin/config",
    // The command palette, which pushes a path with no query at all.
    "#/company/people/ceo",
    "#/company/units/Engineering",
    // A unit page under the company, which is another screen wearing the same
    // first segment: the lens parameter riding along changes nothing.
    "#/company/units/Engineering?lens=builder",
    "#/company/people?lens=builder",
  ]) {
    expect(keepsTheLens(to(hash)), hash).toBe(false);
  }
});

/*
 * THE MUTATION THIS FILE EXISTS FOR. A predicate keyed on the screen the
 * builder USED to hang off answers false for every address above, so both
 * cases above would still pass their `false` half while the `true` half — the
 * one that says a reader choosing a view is not leaving — went red. This is
 * that failure written down, so the next move of this screen reads it.
 */
test("the old address is not this one, and would hold every move in the lens", () => {
  const atOrg = (route: ReturnType<typeof to>) =>
    route.path[0] === "org" && route.query.get("lens") === "builder";
  const inside = to("#/company?lens=builder&view=table");
  expect(keepsTheLens(inside)).toBe(true);
  expect(atOrg(inside)).toBe(false);
});
