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
import { useViewer } from "./viewer.ts";
import { eventually, settled } from "~/test/settle.ts";

vi.mock("./store-hooks.ts", () => ({
  useClient: vi.fn(),
  useConnection: vi.fn(),
}));
vi.mock("./viewer.ts", () => ({ useViewer: vi.fn() }));

/** answering counts the queries asked and answers each with `answer`, to a
 *  reader holding `grants` (both apps unless a case says otherwise). */
function answering(answer: unknown, grants: string[] = ["worklet", "doclet"]) {
  const query = vi.fn().mockResolvedValue(answer);
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  vi.mocked(useViewer).mockReturnValue({ grants, loading: false } as never);
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
  await eventually(() => expect(result.current.data).not.toBeNull());
  expect(query).toHaveBeenCalledTimes(1);
  expect(query.mock.calls[0]![0]).toBe("work_search");
  expect(result.current.tooLong).toBeNull();
});

test("a phrase past the bound is never sent, and says why", async () => {
  const query = answering({ hits: [] });
  const { result } = renderHook(() =>
    useSearchQuery("knowledge", { q: "x".repeat(SEARCH_QUERY_MAX + 1) }),
  );
  await settled(() => query.mock.calls.length);
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
  await eventually(() => expect(result.current.data).not.toBeNull());
  rerender({ q: "auth ".repeat(SEARCH_QUERY_MAX) });
  await eventually(() => expect(result.current.data).toBeNull());
  expect(result.current.tooLong).not.toBeNull();
});

// EACH SEARCH IS ITS APP'S: a reader without Worklet is never sent the task
// search, nor one without Doclet the page search, and the hook says why in
// the words every lock uses rather than handing back "nothing matched". The
// CONTROL is the other app's search, which the same reader is sent.
test("a search in an app the reader lacks is never sent, and says which app", async () => {
  const query = answering({ hits: [] }, ["doclet"]);
  const tasks = renderHook(() => useSearchQuery("work_search", { q: "auth" }));
  const pages = renderHook(() => useSearchQuery("knowledge", { q: "auth" }));
  await eventually(() => expect(query).toHaveBeenCalledTimes(1));
  expect(query.mock.calls[0]![0]).toBe("knowledge");
  expect(tasks.result.current.withheld).toBe(
    "Search needs Worklet access. An admin can give it to you.",
  );
  expect(tasks.result.current.data).toBeNull();
  expect(pages.result.current.withheld).toBeNull();
});

test("enabled keeps its meaning on top of the bound", async () => {
  const query = answering({ hits: [] });
  renderHook(() => useSearchQuery("work_search", { q: "auth" }, { enabled: false }));
  await settled(() => query.mock.calls.length);
  expect(query).not.toHaveBeenCalled();
});
