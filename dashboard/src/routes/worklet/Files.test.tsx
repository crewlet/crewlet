/**
 * A project's files: what is kept, a page at a time, and a download fetched
 * over the session rather than a bare link, so its refusal is the engine's.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { fileName, ProjectFiles } from "./Files.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { rest, RestError } from "~/protocol/rest.ts";
import type { WorkFilesAnswer } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function serving(pages: Record<string, WorkFilesAnswer>) {
  const query = vi.fn(async (_what: string, params: Record<string, unknown>) => {
    return pages[String(params.after ?? "")] ?? { project: "ENG", files: [], complete: true };
  });
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  return query;
}

const file = (path: string) => ({
  project: "ENG",
  path,
  content_type: "text/markdown",
  size: 2048,
  version: 7,
  created_at: "2026-09-01T10:00:00Z",
  updated_at: "2026-09-01T10:00:00Z",
  updated_by: "ada",
});

const mount = () =>
  render(
    <Router>
      <ProjectFiles project="ENG" />
    </Router>,
  );

test("an empty project says so rather than drawing an empty table", async () => {
  serving({});
  mount();
  expect(await screen.findByText("No files are kept in this project yet")).toBeTruthy();
});

test("a page of files is listed, and the next page is the engine's own cursor", async () => {
  const query = serving({
    "": {
      project: "ENG",
      files: [file("a.md"), file("reports/b.md")],
      next: "reports/b.md",
      complete: true,
    },
    "reports/b.md": { project: "ENG", files: [file("zeta.txt")], complete: true },
  });
  mount();
  expect(await screen.findByText("reports/b.md")).toBeTruthy();
  fireEvent.click(screen.getByText("Next page"));
  expect(await screen.findByText("zeta.txt")).toBeTruthy();
  const calls = query.mock.calls as unknown as [string, Record<string, unknown>][];
  expect(calls.at(-1)?.[1]).toEqual({ project: "ENG", after: "reports/b.md" });
  fireEvent.click(screen.getByText("First page"));
  expect(await screen.findByText("a.md")).toBeTruthy();
});

test("a download fetches the bytes over the session, and a refusal is said on the row", async () => {
  serving({ "": { project: "ENG", files: [file("reports/q3 plan.md")], complete: true } });
  const blob = vi.spyOn(rest, "blob").mockRejectedValueOnce(
    new RestError(503, {
      error: "content_unavailable",
      detail: "the object store did not answer",
    }),
  );
  mount();
  fireEvent.click(await screen.findByText("Download"));
  await waitFor(() =>
    expect(screen.getByText(/Not downloaded: The object store did not answer\./)).toBeTruthy(),
  );
  expect(blob).toHaveBeenCalledWith("/worklet/files/ENG/reports/q3%20plan.md");
});

// A REFUSAL ON AUTHORITY NAMES THE GRANT the engine said would admit the
// reader, rather than its code: "unauthorized" on the row tells a person
// nothing they can act on.
test("a download refused on authority says which grant would admit the reader", async () => {
  serving({ "": { project: "ENG", files: [file("a.md")], complete: true } });
  vi.spyOn(rest, "blob").mockRejectedValueOnce(
    new RestError(403, { error: "unauthorized", reason: "no_grant", grants: ["worklet"] }),
  );
  mount();
  fireEvent.click(await screen.findByText("Download"));
  await waitFor(() =>
    expect(
      screen.getByText(
        /Not downloaded: Downloading a project's file needs Worklet access\. An admin can give it to you\./,
      ),
    ).toBeTruthy(),
  );
});

// A REFUSAL NO ACCESS CLEARS IS THE ENGINE'S OWN WORDS, whatever it names: a
// proof gone stale names the access that admitted the reader. Mutation: decide
// by the grants alone and this names Admin.
test("a download refused for a stale proof says the engine's words, never a grant", async () => {
  serving({ "": { project: "ENG", files: [file("a.md")], complete: true } });
  vi.spyOn(rest, "blob").mockRejectedValueOnce(
    new RestError(403, {
      error: "step_up_required",
      message: "Confirm it's you to do this, then try again.",
      reason: "step_up",
      grants: ["admin"],
    }),
  );
  mount();
  fireEvent.click(await screen.findByText("Download"));
  await waitFor(() =>
    expect(
      screen.getByText(/Not downloaded: Confirm it's you to do this, then try again\./),
    ).toBeTruthy(),
  );
  expect(screen.queryByText(/needs Admin access/)).toBeNull();
});

test("a file is saved under its own name", () => {
  expect(fileName("reports/2026/q3.md")).toBe("q3.md");
  expect(fileName("notes.txt")).toBe("notes.txt");
});
