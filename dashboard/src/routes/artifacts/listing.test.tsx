/**
 * An artifact listing read past its first page, through the pager every paged
 * list shares: from "Load more" its first page is held, because the next page
 * starts after the last row the first was answered with, and an artifact made
 * or moved into its range would push that one out of both when the first was
 * read again on its poll. A pin or a filing in the tab still asks it again,
 * and the pages loaded are then read again from it.
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { afterMicrotasks } from "~/test/settle.ts";
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

const item = (id: string) => ({ id, title: id });

/** The listing's question, one object across renders as a screen's memo keeps it. */
const PARAMS = { sort: "-updated", "max-keys": 2 };

// Mutation: ask the first page without the pager's hold, and the tick asks it
// again.
test("from Load more, the listing's poll asks nothing of its first page", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  const query = vi.fn((_what: string, params?: Record<string, unknown>) =>
    Promise.resolve(
      params?.["continuation-token"] === "t2"
        ? { prefix: "", contents: [item("a3")], key_count: 1 }
        : {
            prefix: "",
            contents: [item("a1"), item("a2")],
            key_count: 2,
            next_continuation_token: "t2",
          },
    ),
  );
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  const { result } = renderHook(() => useListing(PARAMS));
  await afterMicrotasks();
  expect(result.current.more).toBe(true);
  act(() => result.current.loadMore());
  await afterMicrotasks();
  expect(result.current.items.map((r) => r.id)).toEqual(["a1", "a2", "a3"]);

  await act(async () => void vi.advanceTimersByTime(5 * GALLERY_POLL_MS));
  await afterMicrotasks();
  const firsts = (query.mock.calls as [string, Record<string, unknown>?][]).filter(
    ([, params]) => !params?.["continuation-token"],
  );
  expect(firsts).toHaveLength(1);
});
