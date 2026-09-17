/**
 * THE ADDRESS OF ONE SPRINT ASKS FOR THAT SPRINT.
 *
 * `#/work/ENG/sprints/3` names a particular sprint, and the screen behind it
 * used to drop the segment: the router parsed it, the breadcrumb said
 * "Sprint 3", and the query asked only for the project — so every deep link
 * rendered the whole recent report with the running sprint's series under a
 * crumb naming a different one. The reader was told where they were by the
 * crumb and shown somewhere else by the screen, which is worse than a 404:
 * nothing about the page says it is not the sprint they asked for.
 *
 * Held here rather than in `Sprints.test.tsx`, which renders the panels
 * directly: this is about what the SCREEN asks the engine, so it needs the
 * query layer, and mocking the store hooks for the panel cases would buy them
 * a dependency none of them has.
 */

import { cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { Sprints } from "./Sprints.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

function serving(answers: Partial<Record<QueryName, unknown>>) {
  // BOTH PARAMETERS, matching `socket.query(what, params)`: the cases below
  // read the params off the call, and a one-argument mock makes that a type
  // error rather than a missing assertion.
  const query = vi.fn(
    async (what: string, _params?: Record<string, unknown>) => answers[what as QueryName] ?? {},
  );
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({ name: "Acme", roles: [] } as never);
  return query;
}

// A REAL ANSWER, because `points` is never absent on the wire: the Go field
// carries no `omitempty` and its builder uses `make([]T, 0, n)`, so an empty
// series marshals as `[]` rather than `null`. Stubbing `{}` here would have
// the screen crash on a shape the engine cannot send, and the case would be
// measuring the fixture.
const burndown = {
  project: "ENG",
  sprint: 3,
  measure: "points",
  start_at: "2031-03-01T00:00:00Z",
  end_at: "2031-03-15T00:00:00Z",
  points: [],
  ideal: 0,
  tasks: 0,
  unestimated: 0,
  complete: true,
};

const report = {
  measure: "points",
  sprints: [
    {
      project: "ENG",
      number: 3,
      name: "Hardening",
      state: "closed",
      start_at: "2031-03-01T00:00:00Z",
      end_at: "2031-03-15T00:00:00Z",
      version: 1,
      figures: {
        measure: "points",
        committed: 20,
        added: 0,
        removed: 0,
        done: 20,
        remaining: 0,
        open_after_close: 0,
        tasks: 5,
        unestimated: 0,
      },
    },
  ],
};

/** The parameters one question was asked with, or undefined if it never was. */
async function asked(
  query: ReturnType<typeof serving>,
  what: QueryName,
): Promise<Record<string, unknown> | undefined> {
  await waitFor(() => expect(query).toHaveBeenCalled());
  const call = query.mock.calls.find(([name]) => name === what);
  return call?.[1] as Record<string, unknown> | undefined;
}

test("the sprint segment reaches the query, archived included", async () => {
  const query = serving({ work_sprints: report, work_burndown: burndown });
  render(
    <Router>
      <Sprints project="ENG" sprint="3" />
    </Router>,
  );
  // ARCHIVED INCLUDED, which the report itself deliberately excludes: an
  // address names a particular sprint, and a link that stopped resolving the
  // day the cadence duty archived it would rot every bookmark behind it.
  expect(await asked(query, "work_sprints")).toMatchObject({
    project: "ENG",
    sprint: 3,
    archived: true,
  });
});

test("the report asks for no sprint at all", async () => {
  // THE OTHER DIRECTION, so the case above cannot pass by the screen always
  // sending a sprint: without the segment this is the recent report, and a
  // number sent here would narrow it to one.
  const query = serving({ work_sprints: report, work_burndown: burndown });
  render(
    <Router>
      <Sprints project="ENG" />
    </Router>,
  );
  const params = await asked(query, "work_sprints");
  expect(params).toMatchObject({ project: "ENG" });
  expect(params).not.toHaveProperty("sprint");
});

test("a segment that is not a sprint number falls back to the report", async () => {
  // `#/work/ENG/sprints/none` is not an address. Asking for sprint NaN would
  // be a bad_params banner where the honest rendering is the report.
  const query = serving({ work_sprints: report, work_burndown: burndown });
  render(
    <Router>
      <Sprints project="ENG" sprint="none" />
    </Router>,
  );
  expect(await asked(query, "work_sprints")).not.toHaveProperty("sprint");
});
