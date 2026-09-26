/**
 * A project's files: what is kept, a page at a time, and a download that
 * carries the operator's token rather than a bare link.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { fileName, fileURL, ProjectFiles } from "./Files.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { rest } from "~/protocol/rest.ts";
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
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
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

test("a download fetches the bytes with the token, and a refusal is said on the row", async () => {
  serving({ "": { project: "ENG", files: [file("reports/q3 plan.md")], complete: true } });
  const blob = vi.spyOn(rest, "blob").mockRejectedValueOnce(new Error("no member holds the chunk"));
  mount();
  fireEvent.click(await screen.findByText("Download"));
  await waitFor(() =>
    expect(screen.getByText(/Not downloaded: no member holds the chunk/)).toBeTruthy(),
  );
  expect(blob).toHaveBeenCalledWith("/work/files/ENG/reports/q3%20plan.md");
});

test("a path is escaped segment by segment and saved under its own name", () => {
  expect(fileURL("ENG", "a b/c#d.md")).toBe("/work/files/ENG/a%20b/c%23d.md");
  expect(fileName("reports/2026/q3.md")).toBe("q3.md");
  expect(fileName("notes.txt")).toBe("notes.txt");
});
