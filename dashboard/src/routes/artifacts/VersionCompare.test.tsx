/**
 * Two versions compared, by their files: what the History tab shows a reader
 * told "the agent republished it". The files are compared by digest, the
 * changed text file line by line, and the older version is always the "from"
 * half whichever version the reader picked first.
 */

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { VersionCompare } from "./VersionCompare.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import type { ArtifactDetail, ArtifactEntry } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn() };
});

const ID = "0192f2c4-1111-7000-8000-000000000001";

const file = (path: string, hash: string, contentType = "text/html"): ArtifactEntry => ({
  path,
  content_type: contentType,
  object: { hash, size: 64 },
});

const version = (n: number, files: ArtifactEntry[]) => ({
  artifact: ID,
  number: n,
  version_id: `v${n}`,
  base: n - 1,
  files: files.length,
  bytes: 64 * files.length,
  author: { Name: "ada", Kind: "agent" },
  created_at: "2026-10-18T10:00:00Z",
  position: n,
  manifest: { entry: "index.html", files },
});

const detail = {
  id: ID,
  kind: "html",
  title: "Launch review",
  head_number: 12,
  role: "editor",
} as unknown as ArtifactDetail;

/** The bytes of each version's files, as the raw file route answers them. */
const texts: Record<string, string> = {
  [`/artifacts/${ID}/versions/11/files/index.html`]: "<h1>Review</h1>\n<p>Draft</p>\n<p>End</p>\n",
  [`/artifacts/${ID}/versions/12/files/index.html`]: "<h1>Review</h1>\n<p>Final</p>\n<p>End</p>\n",
};

beforeEach(() => {
  location.hash = `#/artifacts/${ID}`;
  const query = vi.fn(async (what: string, args: { n?: number }) => {
    if (what !== "artifact_version") return {};
    return args.n === 11
      ? version(11, [
          file("index.html", "a"),
          file("app.js", "b", "text/javascript"),
          file("old.css", "c", "text/css"),
        ])
      : version(12, [
          file("index.html", "A"),
          file("app.js", "b", "text/javascript"),
          file("new.css", "d", "text/css"),
        ]);
  });
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      const body = texts[path];
      return body === undefined
        ? new Response("{}", { status: 404, headers: { "Content-Type": "application/json" } })
        : new Response(body, { status: 200, headers: { "Content-Type": "text/plain" } });
    }),
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "";
});

const mount = (picked: number, against: number) =>
  render(
    <Router>
      <VersionCompare
        detail={detail}
        picked={picked}
        against={against}
        lens="source"
        onLens={() => {}}
        versions={[]}
        room={{} as never}
        presence={{} as never}
        bridgeStores={new Set()}
      />
    </Router>,
  );

test("the files are compared by digest, and the changed page line by line", async () => {
  mount(12, 11);
  expect(await screen.findByText("Version 11 and version 12")).toBeTruthy();
  const files = await screen.findAllByRole("list");
  const rows = within(files[0] as HTMLElement).getAllByRole("listitem");
  const text = rows.map((r) => r.textContent ?? "");
  expect(text.some((t) => t.includes("index.html") && t.includes("changed"))).toBe(true);
  expect(text.some((t) => t.includes("new.css") && t.includes("added"))).toBe(true);
  expect(text.some((t) => t.includes("old.css") && t.includes("removed"))).toBe(true);
  // THE CARRIED FILE IS NOT A CHANGE: one digest, one file.
  expect(text.some((t) => t.includes("app.js"))).toBe(false);
  expect(screen.getByText(/1 other file is the same in both/)).toBeTruthy();
  // AND THE PAGE'S ONE CHANGED LINE, older to newer.
  await waitFor(() => expect(screen.getByText("index.html: 1 change")).toBeTruthy());
  expect(screen.getByText("Line 2")).toBeTruthy();
  expect(screen.getByText("<p>Draft</p>")).toBeTruthy();
  expect(screen.getByText("<p>Final</p>")).toBeTruthy();
});

test("the older version is the from half whichever was picked first", async () => {
  mount(11, 12);
  expect(await screen.findByText("Version 11 and version 12")).toBeTruthy();
  await waitFor(() => expect(screen.getByText("index.html: 1 change")).toBeTruthy());
  const row = screen.getByText("Line 2").closest("li") as HTMLElement;
  const order = row.textContent ?? "";
  expect(order.indexOf("Draft")).toBeLessThan(order.indexOf("Final"));
});
