/**
 * The REST transport's own contract.
 */

import { afterEach, expect, test, vi } from "vitest";

import { REQUEST_TIMEOUT_MS, rest, RestError } from "./index.ts";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

// A REQUEST THAT NEVER SETTLES IS ABANDONED.
//
// There was no deadline, and an unresolved fetch is not a slow spinner here:
// every write runs behind a `busy` flag whose only reset is the `finally` of
// its own await, and every dialog disables Escape, the veil click and its own
// Cancel button while busy. The modal had every exit switched off and a
// reload as the only way out.
test("a request that never answers is abandoned rather than awaited", async () => {
  vi.useFakeTimers();
  vi.stubGlobal(
    "fetch",
    vi.fn(
      (_url: string, init?: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          init?.signal?.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          );
        }),
    ),
  );

  const pending = rest.get("/config");
  const settled = pending.catch((err: unknown) => err);
  await vi.advanceTimersByTimeAsync(REQUEST_TIMEOUT_MS + 1);

  const err = await settled;
  expect(err).toBeInstanceOf(RestError);
  // STATUS 0, which is what every caller already tests for as "unreachable":
  // a request this process gave up on and one the engine never answered are
  // the same fact to somebody looking at the screen.
  expect((err as RestError).status).toBe(0);
  vi.useRealTimers();
});
