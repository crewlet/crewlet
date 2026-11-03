/**
 * The repository picker every Code screen narrows by asks the engine for what
 * was typed, so it finds a repository however many sort before it.
 *
 * The invariants worth breaking a build over: typed text is asked as the
 * listing's `prefix`, lowercased, once it rests, and never per keystroke; an
 * opening no repository name could have is said in the list and never asked;
 * Load more asks the page after the last one under the same prefix, and the
 * list stays open for it; a row taken is the choice; and the chosen name stays
 * a row where the pages loaded do not hold it.
 */
import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { QUIET_MS } from "~/lib/useQuiet.ts";
import { CODE_POSITION, jsonAnswer, mountCode, stubCodeEngine } from "~/test/codeScreens.tsx";
import { settled } from "~/test/settle.ts";
import { RepoPicker } from "./RepoPicker.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

/** Every repository the reader may read, by name. */
const NAMES = ["ap", "api", "api-gw", "apiary", "web"];

/**
 * The engine's listing, two names a page: those the `prefix` begins, past the
 * `cursor`, each page's last name its `next` while more remain.
 */
function stubListing(): URL[] {
  return stubCodeEngine({
    "/query/code_repos": (url) => {
      const prefix = url.searchParams.get("prefix") ?? "";
      const cursor = url.searchParams.get("cursor") ?? "";
      const left = NAMES.filter((name) => name.startsWith(prefix) && name > cursor);
      const names = left.slice(0, 2);
      return jsonAnswer({
        repos: names.map((name) => ({ name })),
        ...(left.length > names.length ? { next: names[names.length - 1] } : {}),
        position: CODE_POSITION,
        level: "stale",
      });
    },
  });
}

const listings = (asked: URL[]) => asked.filter((u) => u.pathname === "/query/code_repos");
const prefixes = (asked: URL[]) => listings(asked).map((u) => u.searchParams.get("prefix"));
const field = () => screen.getByRole("combobox", { name: "Repository" });
const rows = () => screen.queryAllByRole("option").map((o) => o.textContent);

// WHAT IS TYPED IS ASKED, ONCE IT RESTS: three keystrokes are one question,
// under the lowercase opening every name has. Mutations: ask the bare
// listing and filter it here — no request carries the prefix, red; ask per
// keystroke — `a` and `ap` are asked, red; send what was typed as it is —
// `API` is asked and the engine refuses it, red.
test("typed text is asked as the listing's prefix, lowercased, once it rests", async () => {
  const asked = stubListing();
  mountCode("#/code", <RepoPicker repo="" onChoose={() => {}} every="Every repository" />);
  await waitFor(() => expect(listings(asked)).toHaveLength(1));
  fireEvent.change(field(), { target: { value: "A" } });
  fireEvent.change(field(), { target: { value: "AP" } });
  fireEvent.change(field(), { target: { value: "API" } });
  await waitFor(() => expect(prefixes(asked)).toContain("api"));
  expect(prefixes(asked)).toEqual([null, "api"]);
  await waitFor(() => expect(rows()).toEqual(["api", "api-gw", "Load more repositories"]));
});

// AN OPENING NO NAME COULD HAVE is said in place and never sent: the engine
// refuses a prefix its grammar cannot begin. ON A FAKED CLOCK, moved past the
// quiet the field waits out before it asks, so the absence is asserted where
// the question would have gone out, never after a guess at the machine's
// load. Mutation: drop the check, and `a_b` is asked.
test("an opening no repository name could have is said and never asked", async () => {
  const asked = stubListing();
  mountCode("#/code", <RepoPicker repo="" onChoose={() => {}} />);
  await waitFor(() => expect(listings(asked)).toHaveLength(1));
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  try {
    fireEvent.change(field(), { target: { value: "a_b" } });
    expect(screen.getByText("No repository name begins with “a_b”")).toBeTruthy();
    act(() => {
      vi.advanceTimersByTime(QUIET_MS);
    });
    await settled(() => listings(asked).length);
    expect(prefixes(asked)).toEqual([null]);
  } finally {
    vi.useRealTimers();
  }
});

// LOAD MORE PAGES ON UNDER THE SAME PREFIX, and the list stays open for the
// page it asked. Mutations: ask the next page without the prefix — the
// cursor reads the bare listing, red; let the row close the list like any
// other — apiary is never offered, red.
test("Load more asks the page after the last under the same prefix and keeps the list open", async () => {
  const asked = stubListing();
  mountCode("#/code", <RepoPicker repo="" onChoose={() => {}} />);
  fireEvent.change(field(), { target: { value: "api" } });
  const more = await screen.findByRole("option", { name: "Load more repositories" });
  fireEvent.mouseDown(more);
  await waitFor(() => expect(rows()).toEqual(["api", "api-gw", "apiary"]));
  const after = listings(asked).find((u) => u.searchParams.get("cursor") === "api-gw");
  expect(after?.searchParams.get("prefix")).toBe("api");
});

// A ROW TAKEN IS THE CHOICE, and the screen's own row for every repository
// chooses none. Mutation: commit the typed text instead — `ap` is chosen, red.
test("a row taken is the choice, and the every row chooses none", async () => {
  stubListing();
  const onChoose = vi.fn();
  mountCode("#/code", <RepoPicker repo="web" onChoose={onChoose} every="Every repository" />);
  fireEvent.click(field());
  await waitFor(() =>
    expect(rows()).toEqual(["Every repository", "web", "ap", "api", "Load more repositories"]),
  );
  fireEvent.change(field(), { target: { value: "ap" } });
  fireEvent.mouseDown(await screen.findByRole("option", { name: "api" }));
  expect(onChoose).toHaveBeenLastCalledWith("api");
  fireEvent.click(field());
  fireEvent.mouseDown(await screen.findByRole("option", { name: "Every repository" }));
  expect(onChoose).toHaveBeenLastCalledWith("");
});

// THE CHOSEN NAME STAYS A ROW where the pages loaded lack it, and the field
// says it until somebody types, so an address naming any repository reads as
// itself. Mutation: list the pages alone — zeta is no row, red.
test("the chosen name stays a row where the pages loaded do not hold it", async () => {
  stubListing();
  mountCode("#/code", <RepoPicker repo="zeta" onChoose={() => {}} />);
  expect((field() as HTMLInputElement).value).toBe("zeta");
  fireEvent.click(field());
  await waitFor(() => expect(rows()).toEqual(["zeta", "ap", "api", "Load more repositories"]));
});
