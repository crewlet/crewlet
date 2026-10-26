/**
 * Two versions compared, by their files: what the History panel shows a reader
 * told "the agent republished it". The files are compared by digest, the
 * changed text file line by line, and the older version is always the "from"
 * half whichever version the reader picked first.
 */

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { ArtifactCompare, lineHunks } from "./ArtifactCompare.tsx";
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
  capabilities: [],
  files: files.length,
  bytes: 64 * files.length,
  author: { name: "ada", kind: "agent" },
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
      <ArtifactCompare
        detail={detail}
        picked={picked}
        against={against}
        lens="source"
        onLens={() => {}}
        room={{} as never}
        presence={{} as never}
        bridgeStores={new Set()}
      />
    </Router>,
  );

/** A diff's lines as `[kind, path or the older and newer numbers, text]`, in order. */
function linesOf(diff: HTMLElement): string[][] {
  return [...diff.querySelectorAll<HTMLElement>(".diff-view__line")].map((line) => [
    line.dataset.kind ?? "",
    line.querySelector(".diff-view__path")?.textContent ??
      [...line.querySelectorAll(".diff-view__number")].map((n) => n.textContent).join("|"),
    line.querySelector(".diff-view__text")?.textContent ?? "",
  ]);
}

test("the files are compared by digest, and the changed page line by line", async () => {
  mount(12, 11);
  expect(await screen.findByText("Version 11 and version 12")).toBeTruthy();
  const files = await screen.findByRole("group", {
    name: "Files changed from version 11 to version 12",
  });
  // A CHANGED FILE IS ITS OLD FACTS REMOVED AND ITS NEW ONES ADDED, the digest
  // among them: the page kept its type and its size, and only the digest says
  // why it is a change at all.
  expect(linesOf(files)).toEqual([
    ["remove", "index.html", "text/html · 64 B · a"],
    ["add", "index.html", "text/html · 64 B · A"],
    ["add", "new.css", "text/css · 64 B · d"],
    ["remove", "old.css", "text/css · 64 B · c"],
  ]);
  // THE CARRIED FILE IS NOT A CHANGE: one digest, one file.
  expect(files.textContent).not.toContain("app.js");
  expect(screen.getByText(/1 other file is the same in both/)).toBeTruthy();
  // AND THE PAGE'S ONE CHANGED LINE, older to newer, at its number in each
  // version, after the one unchanged line above it.
  await waitFor(() => expect(screen.getByText("index.html: 1 change")).toBeTruthy());
  const lines = screen.getByRole("group", { name: "index.html: the lines that changed" });
  expect(linesOf(lines)).toEqual([
    ["remove", "2|", "<p>Draft</p>"],
    ["add", "|2", "<p>Final</p>"],
  ]);
  expect(within(lines).getByText("1 unchanged line")).toBeTruthy();
});

// THE GAP BEFORE EACH RUN IS COUNTED IN THE OLDER VERSION'S LINES: after two
// lines replaced from line 3, a line inserted after line 9 skips the five
// unchanged lines between them, and each line keeps its own number.
test("a text file's runs skip the unchanged lines between them", () => {
  const hunks = lineHunks([
    { fromStart: 3, from: ["c", "d"], toStart: 3, to: ["C"] },
    { fromStart: 10, from: [], toStart: 9, to: ["new"] },
  ]);
  expect(hunks.map((h) => h.skipped)).toEqual([2, 5]);
  expect(hunks.map((h) => h.lines.map((l) => [l.kind, l.before, l.after, l.text]))).toEqual([
    [
      ["remove", 3, null, "c"],
      ["remove", 4, null, "d"],
      ["add", null, 3, "C"],
    ],
    [["add", null, 9, "new"]],
  ]);
});

test("the older version is the from half whichever was picked first", async () => {
  mount(11, 12);
  expect(await screen.findByText("Version 11 and version 12")).toBeTruthy();
  await waitFor(() => expect(screen.getByText("index.html: 1 change")).toBeTruthy());
  const order =
    screen.getByRole("group", { name: "index.html: the lines that changed" }).textContent ?? "";
  expect(order.indexOf("Draft")).toBeLessThan(order.indexOf("Final"));
});

// A DECK IS COMPARED BY SLIDE: the side by side lens is named By slide and
// draws each slide's two versions paired by id, saying what changed in words.
test("a deck's versions are compared slide by slide", async () => {
  const slide = (id: string, text: string) => ({
    id,
    path: `slides/${id}.html`,
    title: text,
    root: {
      tag: "section",
      style: [{ p: "background", v: "#ffffff" }],
      children: [{ tag: "h2", children: [{ text }] }],
    },
  });
  const deck = (n: number, slides: unknown[]) => ({
    artifact: ID,
    version: n,
    deck: { title: "Plan", cover: "a", sections: [], faces: [], slides },
  });
  const answers: Record<string, unknown> = {
    [`/artifacts/${ID}/versions/11/deck`]: deck(11, [
      slide("a", "Plan"),
      slide("b", "Typo here"),
      slide("c", "Team"),
      slide("d", "Gone"),
    ]),
    [`/artifacts/${ID}/versions/12/deck`]: deck(12, [
      slide("a", "Plan"),
      slide("c", "Team"),
      slide("b", "Typo fixed"),
      slide("e", "New"),
    ]),
  };
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL) => {
      const body = answers[new URL(String(input), "http://engine.test").pathname];
      return new Response(JSON.stringify(body ?? {}), {
        status: body ? 200 : 404,
        headers: { "Content-Type": "application/json" },
      });
    }),
  });
  render(
    <Router>
      <ArtifactCompare
        detail={{ ...detail, kind: "slides" } as ArtifactDetail}
        picked={12}
        against={11}
        lens="visual"
        onLens={() => {}}
        room={{} as never}
        presence={{} as never}
        bridgeStores={new Set()}
      />
    </Router>,
  );
  expect(await screen.findByText("1 slide changed, 1 added, 1 removed, 1 moved.")).toBeTruthy();
  expect(screen.getByRole("radio", { name: "By slide" })).toBeTruthy();
  // EACH SIDE IS NAMED by its version, over the columns and to a screen reader.
  expect(screen.getAllByText(/Not in this version/).map((p) => p.textContent)).toEqual([
    "Version 12: Not in this version",
    "Version 11: Not in this version",
  ]);
  expect(screen.getByText(/^Moved from/)).toBeTruthy();
});
