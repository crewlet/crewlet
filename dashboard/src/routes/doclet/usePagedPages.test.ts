/**
 * A knowledge listing read past its first window, through the pager every
 * paged list shares (`lib/usePaged.ts`): the next window starts after the last
 * page the first was answered with, and a page filed into the first window's
 * range pushes that one off its foot when the first is read again on its
 * poll — onto neither window, unless the pager joins the two.
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { settled } from "~/test/settle.ts";
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

// THE LISTING STAYS WHOLE when its first window moves under the windows
// loaded after it: the page pushed off the first window's foot is read in,
// from the first window's own cursor. Mutation: hand the pager a cursor
// reader that reads another key than `after`, or page the listing with a
// pager of its own again, and p2 is on no window drawn.
test("a page pushed off the first window by a new one stays listed", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  let filed = false;
  const query = vi.fn((_what: string, params?: Record<string, unknown>) => {
    const after = params?.after;
    if (after === "p2") return Promise.resolve({ pages: [page("p3")], total: filed ? 4 : 3 });
    if (after === "p1") return Promise.resolve({ pages: [page("p2")], total: 4, after: "p2" });
    return Promise.resolve(
      filed
        ? { pages: [page("p0"), page("p1")], total: 4, after: "p1" }
        : { pages: [page("p1"), page("p2")], total: 3, after: "p2" },
    );
  });
  const asked = () => query.mock.calls.length;
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  const { result } = renderHook(() => usePagedPages(PARAMS, { pollMs: 60_000 }));
  await settled(asked);
  act(() => result.current.loadMore());
  await settled(asked);
  expect(result.current.rows.map((r) => r.id)).toEqual(["p1", "p2", "p3"]);

  // A PAGE FILED AT THE TOP, read on the first window's next poll.
  filed = true;
  await act(async () => void vi.advanceTimersByTime(60_000));
  await settled(asked);
  expect(result.current.rows.map((r) => r.id)).toEqual(["p0", "p1", "p2", "p3"]);
  expect(
    (query.mock.calls as [string, Record<string, unknown>?][]).some(
      ([, params]) => params?.after === "p1",
    ),
  ).toBe(true);
});
