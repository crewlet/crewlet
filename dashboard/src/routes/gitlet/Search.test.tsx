/**
 * Gitlet › Search.
 *
 * The invariants: the search is asked over REST with the address's phrase,
 * mode and filters and at no session floor, and only once a phrase is there;
 * the coverage is drawn above every answer, the lines grouped by repository
 * and file, each line linking to its file at the commit its repository was
 * searched at with the line picked; a reconnect asks no second search; a
 * press is one history entry; and the box follows the address when it moves
 * under the open screen.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { CODE_POSITION, jsonAnswer, mountCode, stubCodeEngine } from "~/test/codeScreens.tsx";
import { Search } from "./Search.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const TIP = "c".repeat(40);
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

function stub(answer: unknown) {
  return stubCodeEngine({
    "/query/code_search": () => jsonAnswer(answer),
    "/query/code_repos": () => jsonAnswer({ repos: [], position: CODE_POSITION, level: "stale" }),
  });
}

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
  // NO FLOOR: the search answers at the tip it names.
  expect(search?.searchParams.has("min_position")).toBe(false);
  expect(search?.searchParams.has("read_level")).toBe(false);

  // THE LINE AT THE COMMIT ITS REPOSITORY WAS SEARCHED AT, picked.
  const target = new URLSearchParams(line.getAttribute("href")!.split("?")[1]);
  expect(line.getAttribute("href")).toMatch(/^#\/gitlet\/repos\/web\?/);
  expect(Object.fromEntries(target)).toEqual({
    tab: "files",
    ref: TIP,
    path: "src/app.ts",
    lines: "7",
  });
  // ONE FILE, BOTH ITS LINES under it.
  const repo = screen.getByRole("region", { name: "web" });
  expect(within(repo).getAllByRole("link", { name: "src/app.ts" })).toHaveLength(1);
  expect(within(repo).getByRole("link", { name: "9" })).toBeTruthy();
  // THE COVERAGE, ABOVE THE LINES, never hidden.
  expect(screen.getByText("Searched 1 repository.")).toBeTruthy();
  expect(screen.getByText(/Left out: 1 repository \(scratch\)/)).toBeTruthy();
});

test("says the coverage where nothing matched, rather than only that nothing did", async () => {
  stub({ hits: [], coverage: { ...coverage, building: { count: 2, names: ["web", "api"] } } });
  mountCode("#/gitlet/search?q=needle", <Search />);
  expect(await screen.findByText("No line matches “needle”")).toBeTruthy();
  expect(screen.getByText(/Not searched: 2 repositories \(web, api\)/)).toBeTruthy();
});

// A RECONNECT IS NOT A NEW SEARCH: an answer names the commit each repository
// was searched at, and a rolling restart reconnects every socket at once, so
// each open tab asking again would be one more scatter to every Gitlet node.
test("a socket that comes back after an answer asks no second search", async () => {
  const asked = stub({ hits: [], coverage });
  const store = mountCode("#/gitlet/search?q=needle", <Search />);
  expect(await screen.findByText("No line matches “needle”")).toBeTruthy();
  const count = (path: string) => asked.filter((u) => u.pathname === path).length;
  expect(count("/query/code_search")).toBe(1);
  const listings = count("/query/code_repos");

  act(() => store.setConnected(false));
  act(() => store.setConnected(true));
  // THE RECONNECT WAS HEARD: the picker's listing, which a reconnect does
  // correct, was asked again — and the search beside it was not.
  await waitFor(() => expect(count("/query/code_repos")).toBe(listings + 1));
  expect(count("/query/code_search")).toBe(1);
});

// THE BOX FOLLOWS THE ADDRESS: ⌘K's "See every match" pushes a phrase onto
// the screen already open, and a box seeded once would show the old phrase
// over the new lines — and send it on the next press.
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

// A PRESS OF SEARCH IS A STEP BACK TAKES, and a press that asks what the
// address already says is none.
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
