/**
 * What the Work rail says when it has no project rows to draw.
 *
 * `useWorkSidebar` reads `work_projects`, and that read is the ACTIVE set —
 * the engine's own default — so an empty answer is two states a reader acts on
 * oppositely: a company that has never had a project, and one that has
 * archived every one of them. The rows cannot tell them apart, which is why
 * the answer carries a CENSUS of both sets whatever the limit.
 *
 * Read from the rows alone, this rail said "No project has been created yet"
 * next to a `#/work/projects` saying "All 4 of the company's projects have
 * been archived" — about the same four. Three surfaces read this one number
 * now (the rail, the directory and `#/work`), so the suite holds the rail's
 * half of it.
 */

import { cleanup, render, renderHook, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { useAdminSidebar, useWorkSidebar } from "./sidebars.tsx";
import { WorkspaceSidebar } from "../frame/WorkspaceSidebar.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import type { QueryName } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

function serving(answers: Partial<Record<QueryName, unknown>>) {
  const query = vi.fn(async (what: string) => answers[what as QueryName] ?? {});
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
}

/** The rail, drawn through the same component the shell mounts it with. */
function Rail() {
  return <WorkspaceSidebar title="Work" sections={useWorkSidebar()} />;
}

const mount = () =>
  render(
    <Router>
      <Rail />
    </Router>,
  );

test("a company with no projects at all is told a project has to be created", async () => {
  serving({
    work_projects: { projects: [], total: 0, census: { active: 0, archived: 0 }, complete: true },
    work_views: { views: [] },
  });
  mount();
  expect(await screen.findByText("No project has been created yet.")).toBeTruthy();
});

test("a company whose every project is archived is told so, with the way to them", async () => {
  serving({
    work_projects: { projects: [], total: 0, census: { active: 0, archived: 4 }, complete: true },
    work_views: { views: [] },
  });
  mount();
  expect(await screen.findByText(/All 4 of the company’s projects are archived/)).toBeTruthy();
  // THE SENTENCE THE DIRECTORY CONTRADICTED is gone, rather than drawn beside
  // the new one.
  expect(screen.queryByText("No project has been created yet.")).toBeNull();
  const link = screen.getByRole("link", { name: /See them under Archived/ });
  expect(link.getAttribute("href")).toBe("#/work/projects?shown=archived");
});

// AN ANSWER WITH NO CENSUS IN IT IS NOT A CENSUS OF NOTHING. A read still in
// flight, or one a newer build's envelope left the field off, must not be read
// as either state — and the honest fallback is the sentence that claims the
// least about what the company holds.
test("a listing with no census leaves the rail on the plain sentence", async () => {
  serving({ work_projects: { projects: [], total: 0, complete: true }, work_views: { views: [] } });
  mount();
  expect(await screen.findByText("No project has been created yet.")).toBeTruthy();
});

// AN INTEGRATION'S DOT IS THE ENGINE'S ROLL-UP, one per configured TOOL.
//
// It was one row per SURFACE, each dotted green for being configured: a GitLab
// refusing every credential sat under a green dot, and Atlassian was four rows.
test("an integration row is one tool, dotted by what the engine concluded", async () => {
  serving({
    fleet: { nodes: [] },
    retention: { domains: [] },
    integrations: {
      integrations: [
        { key: "gitlab", configured: true },
        { key: "jira", configured: true },
        { key: "confluence", configured: true },
      ],
      tools: [
        { key: "slack", surfaces: ["slack"], state: "not_in_use", label: "Not in use", reason: "" },
        {
          key: "atlassian",
          surfaces: ["atlassian", "confluence", "jira", "forge"],
          state: "connected",
          label: "Connected",
          reason: "",
        },
        {
          key: "gitlab",
          surfaces: ["gitlab"],
          state: "attention",
          label: "Credential expiring",
          reason: "the token expires",
        },
      ],
    },
  });
  const { result } = renderHook(() => useAdminSidebar(true), { wrapper: Router });
  await waitFor(() => {
    const row = result.current[0]!.rows.find((r) => r.key === "integrations");
    expect(row?.children?.length).toBe(2);
  });
  const children = result.current[0]!.rows.find((r) => r.key === "integrations")!.children!;
  expect(children.map((c) => [c.key, c.tone])).toEqual([
    ["atlassian", "positive"],
    ["gitlab", "caution"],
  ]);
});
