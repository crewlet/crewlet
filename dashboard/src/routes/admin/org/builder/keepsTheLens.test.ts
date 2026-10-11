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
 * The frame around it is this tree's: the console's sidebar, the Company
 * workspace's section tabs, the command palette and an object peek. Each one of those
 * moves is written out below as the ROUTE it produces, because the router puts
 * a route to the guard and nothing else. It came here from a `lens=builder` on
 * the company screen, and before that from `#/org`.
 */

import { expect, test } from "vitest";
import { parseHash } from "~/app/router.tsx";
import { keepsTheLens } from "./BuilderContext.tsx";

/** What the router hands a guard for a hash. */
const to = (hash: string) => parseHash(hash);

/*
 * THE MOVES INSIDE THE BUILDER. Each is a `section` — the router PUSHES for one,
 * so each really is put to the guard — and each keeps the Builder, its draft
 * and whatever dialog is open exactly as they were. Asking about any of them
 * would put "discard your changes?" in front of a reader who chose a view.
 */
test("a move within the builder keeps it: the view, the chart and a selection", () => {
  for (const hash of [
    "#/admin/company/org",
    "#/admin/company/org?view=table",
    "#/admin/company/org?view=visualization",
    "#/admin/company/org?view=visualization&chart=reporting",
    "#/admin/company/org?view=table&unit=Engineering",
    "#/admin/company/org?view=table&seat=ceo",
    // The peek rail is a FILTER, so the router replaces and never asks at all
    // — but the route it produces still answers honestly here, because a
    // reader who opened a peek and then pressed Back traverses to it.
    "#/admin/company/org?peek=unit%3AEngineering",
  ]) {
    expect(keepsTheLens(to(hash)), hash).toBe(true);
  }
});

/*
 * AND THE MOVES OFF IT. The Company workspace's other sections — the charter,
 * people and access, the logs — and the seats the "Edit org" button sits on
 * are as much a departure as another workspace is: the Builder unmounts and takes the draft
 * with it.
 */
test("a move off the builder does not keep it, including to its own workspace's sections", () => {
  for (const hash of [
    // The workspace's own sections, from the tabs in the page header.
    "#/admin/company",
    "#/admin/company/people",
    "#/admin/company/audit",
    "#/admin/company/changes",
    // The console's sidebar, which is ordinary links, and its door back.
    "#/admin/agents",
    "#/admin/agents/schedules",
    "#/admin/engine/config",
    "#/today",
    // The person shell's chart, which an admin crosses to with `g .`.
    "#/team",
    "#/team/people",
    // The command palette, which pushes a path with no query at all.
    "#/admin/agents/seats/ceo",
    "#/team/units/Engineering",
    // Another screen wearing the same first segment: a parameter the builder
    // once lived under, riding along, changes nothing.
    "#/team/units/Engineering?lens=builder",
    "#/team?lens=builder",
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
test("the old address is not this one, and would hold every move in the builder", () => {
  const atCompany = (route: ReturnType<typeof to>) =>
    route.path[0] === "company" && route.query.get("lens") === "builder";
  const inside = to("#/admin/company/org?view=table");
  expect(keepsTheLens(inside)).toBe(true);
  expect(atCompany(inside)).toBe(false);
});
