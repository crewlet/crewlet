/**
 * Gitlet › Search: asked over REST at no floor with the address's phrase, mode and filters, only once
 * a phrase is there; each file a card of its hits with the lines around them, each hit linking to
 * its file at the commit its repository was searched at.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { CODE_POSITION, jsonAnswer, mountCode, stubCodeEngine } from "~/test/codeScreens.tsx";
import { Search } from "./Search.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, "clipboard");
  location.hash = "";
});

const TIP = "c".repeat(40);
const WEB = "c71d0e9".padEnd(40, "4");
const API = "5c3e8b1".padEnd(40, "7");
const none = { count: 0, names: [] };
const coverage = {
  searched: 1,
  not_indexed: none,
  building: none,
  not_answering: null,
  not_permitted: none,
  grep_not_named: { count: 1, names: ["scratch"] },
  stale: none,
  no_ref: none,
  past_limit: none,
  skipped_files: 0,
  truncated: false,
};

const utf8 = new TextEncoder();
/** Each "Flush(" in a line as the engine answers it: UTF-8 byte offsets. */
function flushes(text: string): [number, number][] {
  const out: [number, number][] = [];
  for (let at = text.indexOf("Flush("); at >= 0; at = text.indexOf("Flush(", at + 1)) {
    const start = utf8.encode(text.slice(0, at)).length;
    out.push([start, start + 6]);
  }
  return out;
}

interface Around {
  before?: string[];
  after?: string[];
  total?: number;
  bare?: boolean;
}
function hit(repo: string, path: string, line: number, text: string, around: Around = {}) {
  return {
    repo,
    path,
    line,
    text,
    blob: `${repo}:${path}`.padEnd(40, "0"),
    ...(around.bare ? {} : { matches: flushes(text) }),
    ...(around.before ? { before: around.before } : {}),
    ...(around.after ? { after: around.after } : {}),
    ...(around.total !== undefined ? { file_matches: around.total } : {}),
  };
}

const MW = "internal/server/middleware.go";
/** "Flush(" over api and web, in the engine's order: 14 matching lines in 5 files. */
const STORY_HITS = [
  hit("api", "internal/http/batch.go", 47, "\t\t\tif err := bw.Flush(); err != nil {", {
    total: 2,
  }),
  hit("api", "internal/http/batch.go", 63, "\treturn bw.Flush()", { total: 2 }),
  hit("api", "internal/http/stream.go", 23, "func (s *streamWriter) Flush() {", { total: 3 }),
  hit("api", "internal/http/stream.go", 24, "\tif err := s.rc.Flush(); err != nil {", { total: 3 }),
  hit("api", "internal/http/stream.go", 61, "\t\ts.Flush()", { total: 3 }),
  hit("web", "internal/render/page.go", 96, "func (p *Page) Flush(w io.Writer) error {", {
    total: 2,
  }),
  hit("web", "internal/render/page.go", 130, "\treturn page.Flush(w)", { total: 2 }),
  hit("web", MW, 65, "func (w *logWriter) Flush() {", {
    total: 4,
    before: ["/* Flush passes a handler's flush on", "   to the client. */", ""],
  }),
  hit("web", MW, 66, "\tw.rc.Flush(); w.log.Flush()", { total: 4, after: ["\t}", "}", ""] }),
  hit("web", MW, 95, "\t// déjà vu: Flush() once more", {
    total: 4,
    before: ["func gzipped() {", "\tw := gz()", "\tdefer w.Close()"],
    after: ["}", "", "// gzip compresses."],
  }),
  hit("web", "internal/server/routes.go", 41, "func (w *statusWriter) Flush() {", {
    total: 3,
    after: ["\tif f, ok := w.ResponseWriter.(http.Flusher); ok {"],
  }),
  hit("web", "internal/server/routes.go", 43, "\t\tf.Flush()", { total: 3 }),
];
const STORY = {
  hits: STORY_HITS,
  tips: { web: WEB, api: API },
  coverage: { ...coverage, searched: 2, grep_not_named: { count: 1, names: ["docs"] } },
};

function stub(answer: unknown, more: Record<string, (url: URL) => Response> = {}) {
  return stubCodeEngine({
    "/query/code_search": () => jsonAnswer(answer),
    "/query/code_repos": () => jsonAnswer({ repos: [], position: CODE_POSITION, level: "stale" }),
    ...more,
  });
}

/** The card of a file, by its path. */
const card = (path: string) => screen.getByRole("article", { name: path });
/** A card's line numbers, block by block. */
const blocksOf = (path: string) =>
  [...card(path).querySelectorAll(".code-search-block")].map((ol) =>
    [...ol.querySelectorAll(".code-search-line__number")].map((n) => Number(n.textContent)),
  );
/** A drawn line's code, by the number in its gutter. */
const codeOf = (path: string, line: number) =>
  [...card(path).querySelectorAll<HTMLElement>(".code-search-line")]
    .find((li) => li.querySelector(".code-search-line__number")?.textContent === String(line))!
    .querySelector<HTMLElement>(".code-search-line__text")!;

test("asks the address's search over REST, at no floor, and links each line at its commit", async () => {
  const asked = stub({
    hits: [
      { repo: "web", path: "src/app.ts", line: 7, text: "const needle = 1;", blob: "1".repeat(40) },
      { repo: "web", path: "src/app.ts", line: 9, text: "needle++;", blob: "1".repeat(40) },
    ],
    tips: { web: TIP },
    coverage,
  });
  mountCode("#/gitlet/search?q=needle&mode=word&repo=web&path=src%2F", <Search />);

  const line = await screen.findByRole("link", { name: "7" });
  const search = asked.find((u) => u.pathname === "/query/code_search");
  expect(search?.searchParams.get("q")).toBe("needle");
  expect(search?.searchParams.get("mode")).toBe("word");
  expect(search?.searchParams.get("repo")).toBe("web");
  expect(search?.searchParams.get("path")).toBe("src/");
  // Three lines around each hit and three hits a file; the engine's own limit.
  expect(search?.searchParams.get("context")).toBe("3");
  expect(search?.searchParams.get("per_file")).toBe("3");
  expect(search?.searchParams.has("limit")).toBe(false);
  // No floor: the search answers at the tip it names.
  expect(search?.searchParams.has("min_position")).toBe(false);
  expect(search?.searchParams.has("read_level")).toBe(false);

  const target = new URLSearchParams(line.getAttribute("href")!.split("?")[1]);
  expect(line.getAttribute("href")).toMatch(/^#\/gitlet\/repos\/web\?/);
  expect(Object.fromEntries(target)).toEqual({ ref: TIP, path: "src/app.ts", lines: "7" });
  const repo = screen.getByRole("region", { name: "web" });
  expect(within(repo).getAllByRole("article")).toHaveLength(1);
  expect(within(repo).getByRole("link", { name: "9" })).toBeTruthy();
});

test("says the coverage where nothing matched, rather than only that nothing did", async () => {
  stub({ hits: [], coverage: { ...coverage, building: { count: 2, names: ["web", "api"] } } });
  mountCode("#/gitlet/search?q=needle", <Search />);
  expect(await screen.findByText("No line matches “needle”")).toBeTruthy();
  expect(screen.getByText(/Not searched: 2 repositories \(web, api\)/)).toBeTruthy();
});

// A reconnect is not a new search: each open tab asking again is one more scatter to every node.
test("a socket that comes back after an answer asks no second search", async () => {
  const asked = stub({ hits: [], coverage });
  const store = mountCode("#/gitlet/search?q=needle", <Search />);
  expect(await screen.findByText("No line matches “needle”")).toBeTruthy();
  const count = (path: string) => asked.filter((u) => u.pathname === path).length;
  expect(count("/query/code_search")).toBe(1);
  const listings = count("/query/code_repos");

  act(() => store.setConnected(false));
  act(() => store.setConnected(true));
  // The picker's listing was asked again, so the reconnect was heard.
  await waitFor(() => expect(count("/query/code_repos")).toBe(listings + 1));
  expect(count("/query/code_search")).toBe(1);
});

// ⌘K's "See every match" pushes a phrase onto the open screen; a box seeded once would keep the old one.
test("an address moved under the open screen puts its phrase in the box", async () => {
  stub({ hits: [], coverage });
  mountCode("#/gitlet/search?q=needle&path=src%2F", <Search />);
  const box = await screen.findByRole("searchbox", { name: "Search the company’s code" });
  expect((box as HTMLInputElement).value).toBe("needle");

  act(() => {
    history.pushState(null, "", "#/gitlet/search?q=other");
    window.dispatchEvent(new Event("crewlet:route"));
  });
  expect(await screen.findByText("No line matches “other”")).toBeTruthy();
  expect((box as HTMLInputElement).value).toBe("other");
  expect((screen.getByRole("textbox", { name: "Path" }) as HTMLInputElement).value).toBe("");
});

test("a search pressed is one history entry, and the same one pressed again is none", async () => {
  stub({ hits: [], coverage });
  mountCode("#/gitlet/search?q=needle", <Search />);
  const box = await screen.findByRole("searchbox", { name: "Search the company’s code" });
  const press = () => fireEvent.click(screen.getByRole("button", { name: "Search" }));

  const before = history.length;
  fireEvent.change(box, { target: { value: "haystack" } });
  press();
  expect(await screen.findByText("No line matches “haystack”")).toBeTruthy();
  expect(history.length).toBe(before + 1);
  expect(new URLSearchParams(location.hash.split("?")[1]).get("q")).toBe("haystack");

  press();
  expect(history.length).toBe(before + 1);
});

test("asks nothing until a phrase is there", async () => {
  const asked = stub({ hits: [], coverage });
  mountCode("#/gitlet/search", <Search />);
  expect(await screen.findByText("Search the company’s code")).toBeTruthy();
  await waitFor(() => expect(asked.some((u) => u.pathname === "/query/code_repos")).toBe(true));
  expect(asked.some((u) => u.pathname === "/query/code_search")).toBe(false);
});

// Mutation: draw the hit's text as markup.
test("a hit's text is text, never elements", async () => {
  const text = "<img src=x onerror=alert(1)>";
  stub({
    hits: [{ repo: "web", path: "page.html", line: 4, text, blob: "1".repeat(40) }],
    tips: { web: TIP },
    coverage,
  });
  mountCode("#/gitlet/search?q=img", <Search />);
  await screen.findByRole("link", { name: "4" });
  const code = codeOf("page.html", 4);
  await within(code).findByText("img", { selector: "[class]" });
  expect(code.querySelector("img")).toBeNull();
  expect(code.textContent).toBe(text);
});

// Mutation: draw each hit alone, without its before and after lines.
test("a hit's lines stand with the lines around them, near hits in one block", async () => {
  stub(STORY);
  mountCode("#/gitlet/search?q=Flush(", <Search />);
  await screen.findByRole("article", { name: MW });
  expect(blocksOf(MW)).toEqual([
    [62, 63, 64, 65, 66, 67, 68, 69],
    [92, 93, 94, 95, 96, 97, 98],
  ]);
  // A hit's number is the strong link to its line; a line around it is plain.
  expect(within(card(MW)).getByRole("link", { name: "65" }).closest("li")!.className).toContain(
    "code-search-line--hit",
  );
  expect(within(card(MW)).queryByRole("link", { name: "64" })).toBeNull();
  expect(codeOf(MW, 64).closest("li")!.className).not.toContain("code-search-line--hit");
});

// Mutation: tokenise each line alone, from an empty state.
test("a block is coloured from its first line, so a comment opened in it carries on", async () => {
  stub(STORY);
  mountCode("#/gitlet/search?q=Flush(", <Search />);
  await screen.findByRole("article", { name: MW });
  const opened = codeOf(MW, 63);
  await within(opened).findByText("to the client. */", {
    selector: ".code-syn-comment",
    exact: false,
  });
  expect(opened.querySelectorAll("[class]")).toHaveLength(1);
  expect(within(codeOf(MW, 65)).getByText("func").className).toBe("code-syn-keyword");
});

test("a file's head says how many of its lines matched, and copies and opens its path", async () => {
  const writeText = vi.fn(() => Promise.resolve());
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  stub(STORY);
  mountCode("#/gitlet/search?q=Flush(", <Search />);
  const file = await screen.findByRole("article", { name: MW });
  expect(within(file).getByText("4 matching lines")).toBeTruthy();
  const web = screen.getByRole("region", { name: "web" });
  expect(within(web).getByText("9 matching lines in 3 files")).toBeTruthy();

  fireEvent.click(within(file).getByRole("button", { name: `Copy the path ${MW}` }));
  expect(writeText).toHaveBeenCalledWith(MW);
  // Open is the file at the commit searched, with no line picked.
  const open = within(file).getByRole("link", { name: `Open ${MW} at c71d0e9` });
  const at = new URLSearchParams(open.getAttribute("href")!.split("?")[1]);
  expect(Object.fromEntries(at)).toEqual({ ref: WEB, path: MW });
});

test("a path that is not UTF-8 has no link, no copy and no open", async () => {
  stub({
    hits: [{ ...hit("web", "x�.go", 3, "Flush()", { total: 9 }), path_b64: "eP8uZ28=" }],
    tips: { web: TIP },
    coverage,
  });
  mountCode("#/gitlet/search?q=Flush(", <Search />);
  const file = await screen.findByRole("article", { name: "x�.go" });
  expect(within(file).queryAllByRole("link")).toHaveLength(0);
  expect(within(file).queryAllByRole("button")).toHaveLength(0);
  expect(within(file).getByTitle("Its path is not UTF-8, so no address names it")).toBeTruthy();
});

/** The marked runs of a drawn line. */
const marksOf = (path: string, line: number) =>
  [...codeOf(path, line).querySelectorAll("mark.code-search-match")].map((m) => m.textContent);

// Mutation: use the engine's byte offsets as string indices.
test("a match is marked where the engine says, by character after a wide one, twice on a line", async () => {
  stub(STORY);
  mountCode("#/gitlet/search?q=Flush(", <Search />);
  await screen.findByRole("article", { name: MW });
  expect(marksOf(MW, 95)).toEqual(["Flush("]);
  expect(marksOf(MW, 66)).toEqual(["Flush(", "Flush("]);
  // A match across two tokens is one run over both, once the grammar has coloured them.
  await within(codeOf(MW, 65)).findByText("func", { selector: ".code-syn-keyword" });
  expect(marksOf(MW, 65)).toEqual(["Flush("]);
  expect(codeOf(MW, 65).textContent).toBe("func (w *logWriter) Flush() {");
});

// Mutation: mark the phrase wherever a line holds it, rather than where the engine said.
test("a hit with no matches answered draws unmarked, and a line around a hit is never marked", async () => {
  stub({
    hits: [
      hit("web", "a.go", 2, "\tw.Flush()", { bare: true, before: ["\tr.Flush()"], total: 2 }),
      hit("web", "a.go", 3, "\tw.Flush() // Flush( again", { after: ["\tq.Flush()"], total: 2 }),
    ],
    tips: { web: TIP },
    coverage,
  });
  mountCode("#/gitlet/search?q=Flush(", <Search />);
  await screen.findByRole("article", { name: "a.go" });
  expect(marksOf("a.go", 1)).toEqual([]);
  expect(marksOf("a.go", 2)).toEqual([]);
  expect(marksOf("a.go", 3)).toEqual(["Flush(", "Flush("]);
  expect(marksOf("a.go", 4)).toEqual([]);
});
