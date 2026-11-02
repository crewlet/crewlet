/**
 * An artifact listing read past its first page, through the pager every paged
 * list shares (`lib/usePaged.ts`): the next page starts after the last row the
 * first was answered with, an artifact made or moved into the first page's
 * range pushes that one off its foot when it is read again on its poll, and a
 * press in the tab moves a row on any page loaded.
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { settled } from "~/test/settle.ts";
import { GALLERY_POLL_MS, useListing } from "./listing.tsx";

vi.mock("~/lib/store-hooks.ts", async (importOriginal) => ({
  ...(await importOriginal<typeof import("~/lib/store-hooks.ts")>()),
  useClient: vi.fn(),
  useConnection: vi.fn(),
}));

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const item = (id: string, title = id) => ({ id, title });

/** The listing's question, one object across renders as a screen's memo keeps it. */
const PARAMS = { sort: "-updated", "max-keys": 2 };

/**
 * A listing of `ids`, newest first, two to a page, each page's token the last
 * id it carried; `titles` renames one where it is set.
 */
function listingOf(ids: () => readonly string[], titles: Record<string, string> = {}) {
  return vi.fn((_what: string, params?: Record<string, unknown>) => {
    const after = String(params?.["continuation-token"] ?? "");
    const all = ids();
    const from = after ? all.indexOf(after) + 1 : 0;
    const page = all.slice(from, from + 2);
    return Promise.resolve({
      prefix: "",
      contents: page.map((id) => item(id, titles[id])),
      key_count: page.length,
      ...(from + page.length < all.length ? { next_continuation_token: page.at(-1) } : {}),
    });
  });
}

function mount(query: ReturnType<typeof listingOf>) {
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  return renderHook(() => useListing(PARAMS));
}

// AN ARTIFACT MADE AFTER "LOAD MORE" PUSHES NONE OUT: the first page stays
// live, and the row its poll pushes off its foot is read in from its own
// token before the listing is drawn again. Mutation: page the listing with a
// pager of its own, keeping the pages after the first as they were read, and
// a2 is on no page drawn.
test("an artifact made after Load more pushes none out of the listing", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  let ids = ["a1", "a2", "a3"];
  const query = listingOf(() => ids);
  const asked = () => query.mock.calls.length;
  const { result } = mount(query);
  await settled(asked);
  expect(result.current.more).toBe(true);
  act(() => result.current.loadMore());
  await settled(asked);
  expect(result.current.items.map((r) => r.id)).toEqual(["a1", "a2", "a3"]);

  ids = ["a0", "a1", "a2", "a3"];
  await act(async () => void vi.advanceTimersByTime(GALLERY_POLL_MS));
  await settled(asked);
  expect(result.current.items.map((r) => r.id)).toEqual(["a0", "a1", "a2", "a3"]);
});

// A RENAME, THE TRASH OR A RESTORE moves a row on any page loaded, so the
// listing's refetch reads every page again in place, not the first alone.
// Mutation: hand back the first page's own refetch, and a3 keeps its old title.
test("a refetch reads every page loaded again, in place", async () => {
  const titles: Record<string, string> = {};
  const query = listingOf(() => ["a1", "a2", "a3"], titles);
  const asked = () => query.mock.calls.length;
  const { result } = mount(query);
  await settled(asked);
  act(() => result.current.loadMore());
  await settled(asked);
  expect(result.current.items.map((r) => r.title)).toEqual(["a1", "a2", "a3"]);

  titles.a3 = "the renamed one";
  act(() => result.current.refetch());
  await settled(asked);
  expect(result.current.items.map((r) => r.title)).toEqual(["a1", "a2", "the renamed one"]);
});
