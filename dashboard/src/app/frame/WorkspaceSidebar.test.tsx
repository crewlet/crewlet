/**
 * A branch the reader folded away still opens for the two things that are not
 * a preference: where they are, and what they searched for.
 *
 * The tree held one nullable flag and read it as `openedByHand ?? (onPath ||
 * filter !== "")`. `??` falls through on null only, and the twist writes a
 * boolean — so the first hand collapse was permanent and neither force-open
 * could ever apply again. Both failures are silent and both look like the data
 * is missing rather than hidden: a filter that matches a sprint keeps its
 * project row (a parent matches THROUGH its children) and renders the sprint
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
            path: ["work", "projects", "apollo", "sprints", "auth"],
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
  location.hash = "#/work/projects/apollo/sprints/auth";
  fireEvent(window, new Event("crewlet:route"));
  const row = screen.queryByText("Auth rewrite");
  expect(row).not.toBeNull();
  expect(row!.closest("a")?.getAttribute("aria-current")).toBe("page");
});

test("the twist still folds a branch the reader is standing in", () => {
  // The forced state is FORGOTTEN, not overridden: a reader who collapses the
  // branch they are inside keeps it collapsed, or the twist would be a control
  // that does nothing on exactly the branch they are looking at.
  location.hash = "#/work/projects/apollo/sprints/auth";
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

// AND A ROW WITH NO QUERY STILL MATCHES A ROUTE THAT HAS ONE. Demanding an
// exact query would leave a reader inside a filtered log with nothing in the
// tree marked at all — the seat row is the narrower answer, not the only one.
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
