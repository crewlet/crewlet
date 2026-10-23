/**
 * A workspace that draws no star in its bar (`WorkspaceRow.starless`).
 *
 * A workspace's own page is a sidebar row already, so the frame draws its star
 * there only unavailable, to hold the bar still between the workspace's
 * sections. Home has no sections, and its bar carries Edit Home instead: a
 * star that can never be pressed would be the one control in it that does
 * nothing.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
import { AppShell } from "@crewlethq/ui";

import { PageHeader } from "./PageHeader.tsx";
import { WORKSPACES, type Workspace } from "~/app/nav.ts";
import { Router } from "~/app/router.tsx";
import { noteReader } from "~/lib/reader.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

// STARS ARE KEPT PER READER, so the bar is read by somebody.
beforeEach(() => noteReader("p-1"));
afterEach(() => {
  cleanup();
  sessionStorage.clear();
  location.hash = "#/";
});

function bar(workspace: Workspace) {
  const row = WORKSPACES.find((w) => w.key === workspace)!;
  location.hash = `#/${row.path.join("/")}`;
  const store = new Store();
  const socket = new LiveSocket(store);
  render(
    <Router>
      <ClientContext.Provider value={{ store, socket }}>
        <AppShell
          topbar={
            <PageHeader
              crumbs={[]}
              row={row}
              counts={{}}
              title={row.label}
              workspace={workspace}
              runs={{ tracker: true, knowledge: true, chat: true }}
              grants={null}
            />
          }
        />
      </ClientContext.Provider>
    </Router>,
  );
}

test("Home is the one workspace that says it draws no star", () => {
  expect(WORKSPACES.filter((w) => w.starless).map((w) => w.key)).toEqual(["home"]);
});

test("Home's bar draws no star, inline or in the phone's More", () => {
  bar("home");
  expect(screen.queryByRole("button", { name: "Keep in Starred" })).toBeNull();
  expect(document.querySelector(".page-more")).toBeNull();
});

test("another workspace's own page still draws its star, unavailable", () => {
  bar("inbox");
  const stars = screen.getAllByRole("button", { name: "Keep in Starred" });
  expect(stars.length).toBeGreaterThan(0);
  expect(stars.every((s) => s.getAttribute("aria-disabled") === "true")).toBe(true);
});
