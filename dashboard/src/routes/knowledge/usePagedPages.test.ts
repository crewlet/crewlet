/**
 * A knowledge listing read past its first window, through the pager every
 * paged list shares: from "Load more" its first window is held, because the
 * next one starts after the last page the first was answered with, and a page
 * filed into its range would push that one out of both when the first was
 * read again on its poll.
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { usePagedPages } from "./usePagedPages.ts";

vi.mock("~/lib/store-hooks.ts", () => ({
  useClient: vi.fn(),
  useConnection: vi.fn(),
}));

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const page = (id: string) => ({ id, title: id });

/** The listing's question, one object across renders as a screen's memo keeps it. */
const PARAMS = { container: "c-1" };

// Mutation: ask the first window without the pager's hold, and the tick asks
// it again.
test("from Load more, the first window's poll asks nothing of it", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  const query = vi.fn((_what: string, params?: Record<string, unknown>) =>
    Promise.resolve(
      params?.after === "p2"
        ? { pages: [page("p3")], total: 3 }
        : { pages: [page("p1"), page("p2")], total: 3, after: "p2" },
    ),
  );
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  const { result } = renderHook(() => usePagedPages(PARAMS, { pollMs: 60_000 }));
  await afterMicrotasks();
  act(() => result.current.loadMore());
  await afterMicrotasks();
  expect(result.current.rows.map((r) => r.id)).toEqual(["p1", "p2", "p3"]);
  expect(result.current.held).toBe(true);

  await act(async () => void vi.advanceTimersByTime(5 * 60_000));
  await afterMicrotasks();
  const firsts = (query.mock.calls as [string, Record<string, unknown>?][]).filter(
    ([, params]) => !params?.after,
  );
  expect(firsts).toHaveLength(1);
});
