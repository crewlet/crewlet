/**
 * A branch the reader folded away still opens for the two things that are not
 * a preference: where they are, and what they searched for.
 *
 * The tree held one nullable flag and read it as `openedByHand ?? (onPath ||
 * filter !== "")`. `??` falls through on null only, and the twist writes a
 * boolean — so the first hand collapse was permanent and neither force-open
 * could ever apply again. Both failures are silent and both look like the data
 * is missing rather than hidden: a filter that matches a child keeps its
 * parent row (a parent matches THROUGH its children) and renders the child
 * nowhere, which is indistinguishable from a search that found nothing; and a
 * navigation into the branch leaves the row marked `current` undrawn, which is
 * exactly what the component's own comment says a tree must never do.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { WorkspaceSidebar, type SidebarRow, type SidebarSection } from "./WorkspaceSidebar.tsx";
import { Router } from "~/app/router.tsx";

const SECTIONS: SidebarSection[] = [
  {
    key: "projects",
    label: "Projects",
    rows: [
      {
        key: "apollo",
        label: "Apollo",
        path: ["work", "projects", "apollo"],
        children: [
          {
            key: "auth",
            label: "Auth rewrite",
            path: ["work", "projects", "apollo", "views", "auth"],
          },
        ],
      },
    ],
  },
];

beforeEach(() => {
  location.hash = "#/work";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

function sidebar() {
  return render(
    <Router>
      <WorkspaceSidebar title="Work" sections={SECTIONS} />
    </Router>,
  );
}

/** Open the branch and then fold it away again, as a reader would. */
function collapseApollo(): void {
  fireEvent.click(screen.getByLabelText("Expand Apollo"));
  expect(screen.queryByText("Auth rewrite")).not.toBeNull();
  fireEvent.click(screen.getByLabelText("Collapse Apollo"));
  expect(screen.queryByText("Auth rewrite")).toBeNull();
}

test("a filter that matches a child opens the branch the reader collapsed", () => {
  sidebar();
  collapseApollo();
  fireEvent.change(screen.getByLabelText("Filter Work"), { target: { value: "auth" } });
  // The row that was searched for is DRAWN. Without this the project row
  // survives the filter on its child's behalf and the child appears nowhere,
  // so the reader is told the tree matched and shown nothing that did.
  expect(screen.queryByText("Auth rewrite")).not.toBeNull();
});

test("navigating into the branch opens the one the reader collapsed", () => {
  sidebar();
  collapseApollo();
  location.hash = "#/work/projects/apollo/views/auth";
  fireEvent(window, new Event("crewlet:route"));
  const row = screen.queryByText("Auth rewrite");
  expect(row).not.toBeNull();
  expect(row!.closest("a")?.getAttribute("aria-current")).toBe("page");
});

test("the twist still folds a branch the reader is standing in", () => {
  // The forced state is FORGOTTEN, not overridden: a reader who collapses the
  // branch they are inside keeps it collapsed, or the twist would be a control
  // that does nothing on exactly the branch they are looking at.
  location.hash = "#/work/projects/apollo/views/auth";
  sidebar();
  expect(screen.queryByText("Auth rewrite")).not.toBeNull();
  fireEvent.click(screen.getByLabelText("Collapse Apollo"));
  expect(screen.queryByText("Auth rewrite")).toBeNull();
});

// ONE ROW IS CURRENT, EVEN WHERE A GROUP IS ONE PATH AND MANY QUERIES.
//
// Activity's seats are eight rows on `activity/turns` differing only by
// `?seat=`. Selection compared paths alone, so all eight matched: eight tinted
// rows and eight `aria-current="page"`, which is what a screen reader reads out
// as eight current pages. It was invisible under the old palette's 10%-alpha
// tint and unmissable under a stronger one.
test("a group that differs only by its query marks exactly one row", async () => {
  location.hash = "#/activity/turns?seat=agent-cto";
  const rows: SidebarRow[] = ["agent-ceo", "agent-cto", "agent-pm"].map((handle) => ({
    key: handle,
    label: handle,
    path: ["activity", "turns"],
    query: { seat: handle },
  }));
  render(
    <Router>
      <WorkspaceSidebar title="Activity" sections={[{ key: "seats", rows }]} />
    </Router>,
  );
  const marked = await screen.findAllByRole("link", { current: "page" });
  expect(marked.map((a) => a.textContent)).toEqual(["agent-cto"]);
});

// AND A ROW WITH NO QUERY STILL WINS WHERE NOTHING NARROWER IS DRAWN. A `?seat=`
// for a handle the roster no longer has has no seat row, and demanding an exact
// query would leave a reader inside a filtered log with nothing in the tree
// marked at all.
test("the unfiltered row stays current while a query narrows it", async () => {
  location.hash = "#/activity/turns?seat=agent-cto";
  render(
    <Router>
      <WorkspaceSidebar
        title="Activity"
        sections={[
          { key: "fixed", rows: [{ key: "turns", label: "Turns", path: ["activity", "turns"] }] },
        ]}
      />
    </Router>,
  );
  const marked = await screen.findAllByRole("link", { current: "page" });
  expect(marked.map((a) => a.textContent)).toEqual(["Turns"]);
});

// ONE DESTINATION IN TWO SECTIONS IS STILL ONE PAGE.
//
// Starred and Recent repeat the tree's own rows on purpose. Selection was a
// predicate each row answered about itself, so both answered yes: two rows in
// the accent and two links reading out as the current page, on every screen the
// reader had opened before.
test("a destination listed in two sections marks exactly one row", async () => {
  location.hash = "#/work/ENG";
  render(
    <Router>
      <WorkspaceSidebar
        title="Work"
        sections={[
          {
            key: "projects",
            label: "Projects",
            rows: [{ key: "ENG", label: "Engineering", path: ["work", "ENG"] }],
          },
          {
            key: "recents",
            label: "Recent",
            rows: [{ key: "recent-work/ENG", label: "ENG", path: ["work", "ENG"] }],
          },
        ]}
      />
    </Router>,
  );
  // THE TREE'S ROW, not the shortcut: the tree is the address and it is drawn
  // first, which is the tie-break.
  const marked = await screen.findAllByRole("link", { current: "page" });
  expect(marked.map((a) => a.textContent)).toEqual(["Engineering"]);
  // AND BOTH ROWS ARE STILL DRAWN — the two-sided half. Deduping the sections
  // would satisfy the line above and delete the reason to star anything.
  expect(screen.getAllByRole("link").map((a) => a.textContent)).toEqual(["Engineering", "ENG"]);
});

// `Turns` carries no query, so it is compatible with every `?seat=`. Rendered
// together — which is exactly how the Activity sidebar ships — the fixed row and
// the seat row were both marked.
test("the seat row wins over the list it narrows", async () => {
  location.hash = "#/activity/turns?seat=agent-cto";
  render(
    <Router>
      <WorkspaceSidebar
        title="Activity"
        sections={[
          { key: "fixed", rows: [{ key: "turns", label: "Turns", path: ["activity", "turns"] }] },
          {
            key: "seats",
            label: "Seats",
            rows: ["agent-ceo", "agent-cto"].map((handle) => ({
              key: handle,
              label: handle,
              path: ["activity", "turns"],
              query: { seat: handle },
            })),
          },
        ]}
      />
    </Router>,
  );
  const marked = await screen.findAllByRole("link", { current: "page" });
  expect(marked.map((a) => a.textContent)).toEqual(["agent-cto"]);
});

// ---------------------------------------------------------------------------
// The count beside a row
// ---------------------------------------------------------------------------

/**
 * A NUMBER BESIDE A NAME SAYS WHAT IT COUNTS, or it says something false.
 *
 * This rail drew "Leadership 5" — the unit's whole subtree — while the org
 * chart's block for the same unit read "2 seats" and the roster's group head a
 * third figure. The count and its explanation were two fields, `count` and an
 * optional `countTitle`, and the optional one is the half that went missing.
 * One field cannot be half-given.
 */
test("a row's count carries what it counted, or there is no count", () => {
  render(
    <Router>
      <WorkspaceSidebar
        title="Company"
        sections={[
          {
            key: "units",
            rows: [
              {
                key: "leadership",
                label: "Leadership",
                path: ["company", "units", "Leadership"],
                count: { value: 5, of: "seats in this unit and everything under it" },
              },
              { key: "bare", label: "Nothing counted", path: ["company", "units", "Bare"] },
            ],
          },
        ]}
      />
    </Router>,
  );
  const badge = screen.getByText("5");
  expect(badge.getAttribute("title")).toBe("seats in this unit and everything under it");
  // THE CONTROL: a row with no count draws no badge at all, or this would pass
  // on a rail that put a title on every row in the tree.
  const bare = screen.getByText("Nothing counted").closest("a")!;
  expect(bare.querySelector(".side-count")).toBeNull();
});
