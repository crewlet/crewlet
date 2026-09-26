/**
 * The one REST loader's contract.
 *
 * Each case is a rule one of the four hand-rolled loaders it replaced either
 * broke or kept by remembering to: an answer nobody was waiting for, a screen
 * that had gone, a token that changed, a refusal left beside the last answer,
 * and a wait the engine named.
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { afterEach, describe, expect, test, vi } from "vitest";

import { restErrorCode, useRest } from "./useRest.ts";
import { ClientContext } from "./store-hooks.ts";
import { clearToken, LiveSocket, RestError, Store, storeToken } from "~/protocol/index.ts";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

/** A read whose answers the test settles by hand, one per call. */
function scripted<T>() {
  const calls: {
    signal: AbortSignal;
    resolve: (value: T) => void;
    reject: (err: unknown) => void;
  }[] = [];
  const read = vi.fn(
    (signal: AbortSignal) =>
      new Promise<T>((resolve, reject) => {
        calls.push({ signal, resolve, reject });
      }),
  );
  return { read, calls };
}

const refused = (status: number, retryAfter: string | null = null) =>
  new RestError(status, { error: "refused" }, retryAfter);

describe("an answer belongs to the read that asked for it", () => {
  test("a superseded read's answer is dropped and its request aborted", async () => {
    const { read, calls } = scripted<string>();
    const { result, rerender } = renderHook(({ key }) => useRest(key, read), {
      initialProps: { key: "/a" },
    });
    rerender({ key: "/b" });
    expect(calls).toHaveLength(2);
    expect(calls[0]!.signal.aborted).toBe(true);

    // The newer read answers first; the older one landing after it must not
    // put `/a`'s answer on a screen showing `/b`.
    await act(async () => calls[1]!.resolve("b"));
    await act(async () => calls[0]!.resolve("a"));
    expect(result.current.data).toBe("b");
  });

  test("an unmounted screen's read is aborted and writes nothing", async () => {
    const { read, calls } = scripted<string>();
    const { result, unmount } = renderHook(() => useRest("/secrets", read));
    const before = result.current;
    unmount();
    expect(calls[0]!.signal.aborted).toBe(true);
    await act(async () => calls[0]!.resolve("late"));
    expect(result.current).toBe(before);
  });

  test("a changed key shows nothing of the old one until it answers", async () => {
    const { read, calls } = scripted<string>();
    const { result, rerender } = renderHook(({ key }) => useRest(key, read), {
      initialProps: { key: "/a" },
    });
    await act(async () => calls[0]!.resolve("a"));
    expect(result.current.data).toBe("a");

    rerender({ key: "/b" });
    expect(result.current.data).toBeNull();
    expect(result.current.loading).toBe(true);
  });

  test("a null key reads nothing and is not loading", () => {
    const { read } = scripted<string>();
    const { result } = renderHook(() => useRest(null, read));
    expect(read).not.toHaveBeenCalled();
    expect(result.current.loading).toBe(false);
    expect(result.current.data).toBeNull();
  });
});

describe("what a failure leaves on screen", () => {
  // A GUARDED HALF DRAWN BESIDE THE ENGINE'S REFUSAL is the one thing a
  // refused re-read must never leave: the engine said something about the
  // read, and the last answer contradicts it.
  test("a refusal replaces the last answer", async () => {
    const { read, calls } = scripted<string>();
    const { result } = renderHook(() => useRest("/secrets", read));
    await act(async () => calls[0]!.resolve("rows"));

    void act(() => void result.current.reload());
    await act(async () => calls[1]!.reject(refused(401)));
    expect(result.current.data).toBeNull();
    expect(result.current.code).toBe("unauthorized");
  });

  // A request that never reached the engine says nothing about the answer.
  test("a request that never arrived keeps the last answer beside its error", async () => {
    const { read, calls } = scripted<string>();
    const { result } = renderHook(() => useRest("/secrets", read));
    await act(async () => calls[0]!.resolve("rows"));

    void act(() => void result.current.reload());
    await act(async () => calls[1]!.reject(refused(0)));
    expect(result.current.data).toBe("rows");
    expect(result.current.code).toBe("closed");
  });

  test("a read that throws something else reports an unreadable answer", async () => {
    const { read, calls } = scripted<string>();
    const { result } = renderHook(() => useRest("/secrets", read));
    await act(async () => calls[0]!.reject(new TypeError("rows is not iterable")));
    expect(result.current.error?.code).toBe("unreadable_body");
    expect(result.current.code).toBe("query_failed");
  });
});

describe("a retry is the engine's to schedule", () => {
  test("a refusal with Retry-After is read again, quietly, after exactly that wait", async () => {
    vi.useFakeTimers();
    const { read, calls } = scripted<string>();
    const { result } = renderHook(() => useRest("/setup/integrations", read));
    await act(async () => calls[0]!.reject(refused(503, "4")));
    expect(result.current.code).toBe("unavailable");

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3_999);
    });
    expect(read).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(read).toHaveBeenCalledTimes(2);
    // QUIET: the banner stays until the answer replaces it.
    expect(result.current.loading).toBe(false);

    await act(async () => calls[1]!.resolve("listing"));
    expect(result.current.data).toBe("listing");
    expect(result.current.error).toBeNull();
  });

  // A 503 WITH NO HINT is a node no wait repairs — no keyring, no surface —
  // and re-asking it only repeats the refusal.
  test("a refusal without one is not read again on its own", async () => {
    vi.useFakeTimers();
    const { read, calls } = scripted<string>();
    const { result } = renderHook(() => useRest("/secrets", read));
    await act(async () => calls[0]!.reject(refused(503)));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(600_000);
    });
    expect(read).toHaveBeenCalledTimes(1);
    expect(result.current.code).toBe("query_failed");
  });

  test("a newer read cancels the older read's retry", async () => {
    vi.useFakeTimers();
    const { read, calls } = scripted<string>();
    const { result } = renderHook(() => useRest("/secrets", read));
    await act(async () => calls[0]!.reject(refused(503, "5")));
    void act(() => void result.current.reload());
    await act(async () => calls[1]!.resolve("rows"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    expect(read).toHaveBeenCalledTimes(2);
  });
});

describe("what makes it read again", () => {
  // Every route this loader reads is guarded, and the refusal on screen is
  // usually the one a new token answers. The Audit screen's own loader never
  // listened, so a token set there showed nothing until a navigation.
  test("a changed token re-reads, loudly", async () => {
    const { read, calls } = scripted<string>();
    const { result } = renderHook(() => useRest("/secrets", read));
    await act(async () => calls[0]!.reject(refused(401)));

    act(() => {
      storeToken("sk-test");
    });
    expect(read).toHaveBeenCalledTimes(2);
    expect(result.current.loading).toBe(true);
    await act(async () => calls[1]!.resolve("rows"));
    expect(result.current.data).toBe("rows");
    clearToken();
  });

  test("a poll re-reads quietly, keeping what is on screen", async () => {
    vi.useFakeTimers();
    const { read, calls } = scripted<string>();
    const { result } = renderHook(() => useRest("/runs", read, { pollMs: 4_000 }));
    await act(async () => calls[0]!.resolve("one"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4_000);
    });
    expect(read).toHaveBeenCalledTimes(2);
    expect(result.current.loading).toBe(false);
    expect(result.current.data).toBe("one");
  });

  test("the tab coming back re-reads when asked to", async () => {
    const { read, calls } = scripted<string>();
    renderHook(() => useRest("/setup/integrations", read, { refetchOnFocus: true }));
    await act(async () => calls[0]!.resolve("one"));
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => "visible",
    });
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(read).toHaveBeenCalledTimes(2);
  });

  // AN ANSWER TAKEN BEFORE A RECONNECT is about a company that has since
  // moved, and it is what makes `closed`'s promise — "reads again once the
  // socket is back" — true of a REST read.
  test("a socket that came back re-reads, and the first connect does not", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const wrapper = ({ children }: { children: ReactNode }) =>
      createElement(ClientContext.Provider, { value: { store, socket } }, children);
    const { read, calls } = scripted<string>();
    renderHook(() => useRest("/secrets", read), { wrapper });
    await act(async () => calls[0]!.resolve("rows"));

    act(() => store.setConnected(true));
    expect(read).toHaveBeenCalledTimes(1);
    act(() => store.setConnected(false));
    act(() => store.setConnected(true));
    expect(read).toHaveBeenCalledTimes(2);
  });

  test("the first connect re-reads a read that never reached the engine", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const wrapper = ({ children }: { children: ReactNode }) =>
      createElement(ClientContext.Provider, { value: { store, socket } }, children);
    const { read, calls } = scripted<string>();
    renderHook(() => useRest("/secrets", read), { wrapper });
    await act(async () => calls[0]!.reject(refused(0)));

    act(() => store.setConnected(true));
    expect(read).toHaveBeenCalledTimes(2);
  });
});

describe("the code a refusal renders as", () => {
  test.each([
    [401, null, "unauthorized"],
    [403, null, "unauthorized"],
    [0, null, "closed"],
    [400, null, "bad_params"],
    [404, null, "not_found"],
    [503, "2", "unavailable"],
    [503, null, "query_failed"],
    [500, null, "query_failed"],
  ] as const)("status %i with Retry-After %j is %s", (status, hint, code) => {
    expect(restErrorCode(refused(status, hint))).toBe(code);
  });

  test("no error is no code", () => {
    expect(restErrorCode(null)).toBeNull();
  });
});
