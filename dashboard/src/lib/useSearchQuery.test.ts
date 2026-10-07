/**
 * A ranked search sends nothing past the engine's bound, and says why.
 *
 * The engine refuses a phrase past `SEARCH_QUERY_MAX` as `bad_params`; a
 * picker that sent one listed "No task matches" for a refusal, and one typed
 * past the bound went on listing the hits of the shorter term before it. So the
 * hook every ranked search is asked through ([useSearchQuery]) asks nothing,
 * holds nothing and names the bound — and asks exactly as `useQuery` does
 * inside it.
 */

import { cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { SEARCH_QUERY_MAX } from "~/contract/wire.ts";
import { useSearchQuery } from "./useSearchQuery.ts";
import { useClient, useConnection } from "./store-hooks.ts";

vi.mock("./store-hooks.ts", () => ({
  useClient: vi.fn(),
  useConnection: vi.fn(),
}));

/** answering counts the queries asked and answers each with `answer`. */
function answering(answer: unknown) {
  const query = vi.fn().mockResolvedValue(answer);
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  return query;
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

test("a phrase inside the bound is asked as useQuery asks it", async () => {
  const query = answering({ hits: [{ key: "ENG-1" }] });
  const { result } = renderHook(() =>
    useSearchQuery("work_search", { q: "x".repeat(SEARCH_QUERY_MAX), mode: "hybrid" }),
  );
  await vi.waitFor(() => expect(result.current.data).not.toBeNull());
  expect(query).toHaveBeenCalledTimes(1);
  expect(query.mock.calls[0]![0]).toBe("work_search");
  expect(result.current.tooLong).toBeNull();
});

test("a phrase past the bound is never sent, and says why", async () => {
  const query = answering({ hits: [] });
  const { result } = renderHook(() =>
    useSearchQuery("knowledge", { q: "x".repeat(SEARCH_QUERY_MAX + 1) }),
  );
  await new Promise((r) => setTimeout(r, 10));
  expect(query).not.toHaveBeenCalled();
  expect(result.current.tooLong).toMatch(`at most ${SEARCH_QUERY_MAX}`);
  expect(result.current.data).toBeNull();
  expect(result.current.loading).toBe(false);
});

// TYPED PAST THE BOUND, the hits of the shorter phrase are not left on hand
// under a phrase that was never asked.
test("growing past the bound drops the answer the shorter phrase got", async () => {
  answering({ hits: [{ key: "ENG-1" }] });
  const { result, rerender } = renderHook(
    ({ q }: { q: string }) => useSearchQuery("work_search", { q }),
    { initialProps: { q: "auth" } },
  );
  await vi.waitFor(() => expect(result.current.data).not.toBeNull());
  rerender({ q: "auth ".repeat(SEARCH_QUERY_MAX) });
  await vi.waitFor(() => expect(result.current.data).toBeNull());
  expect(result.current.tooLong).not.toBeNull();
});

test("enabled keeps its meaning on top of the bound", async () => {
  const query = answering({ hits: [] });
  renderHook(() => useSearchQuery("work_search", { q: "auth" }, { enabled: false }));
  await new Promise((r) => setTimeout(r, 10));
  expect(query).not.toHaveBeenCalled();
});
