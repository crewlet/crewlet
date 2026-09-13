/**
 * The one REST loader, and the rules each hand-rolled copy had to learn.
 */

import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { useRest } from "./useRest.ts";
import { clearToken, storeToken } from "~/protocol/index.ts";

interface Call {
  path: string;
  signal: AbortSignal | undefined;
  resolve: (response: Response) => void;
  reject: (err: unknown) => void;
}

/** A fetch whose every call waits for the test to answer it. */
function manualFetch(): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(
      (input: string, init?: RequestInit) =>
        new Promise<Response>((resolve, reject) => {
          init?.signal?.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          );
          calls.push({
            path: new URL(input).pathname,
            signal: init?.signal ?? undefined,
            resolve,
            reject,
          });
        }),
    ),
  );
  return calls;
}

const json = (payload: unknown, status = 200, etag?: string) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json", ...(etag ? { ETag: etag } : {}) },
  });

beforeEach(() => {
  Object.defineProperty(document, "visibilityState", { value: "visible", configurable: true });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  clearToken();
});

describe("a read", () => {
  test("answers the body, the status and the entity-tag", async () => {
    const calls = manualFetch();
    const { result } = renderHook(() => useRest<{ name: string }>("/config"));
    expect(result.current.loading).toBe(true);
    expect(calls.map((c) => c.path)).toEqual(["/config"]);

    await act(async () => calls[0]!.resolve(json({ name: "Acme" }, 200, '"01JREV"')));
    expect(result.current).toMatchObject({
      data: { name: "Acme" },
      etag: '"01JREV"',
      status: 200,
      error: null,
      loading: false,
    });
  });

  test("a disabled read asks nothing", () => {
    const calls = manualFetch();
    const { result } = renderHook(() => useRest("/config", { enabled: false }));
    expect(calls).toEqual([]);
    expect(result.current.loading).toBe(false);
  });
});

describe("the read that answers last is not the read that was asked last", () => {
  test("a superseded read is aborted, and its late answer is never written", async () => {
    const calls = manualFetch();
    const { result } = renderHook(() => useRest<{ rev: string }>("/config"));
    act(() => result.current.reload());
    expect(calls.length).toBe(2);
    // ABORTED, not merely ignored: it held a connection for an answer nobody
    // would read.
    expect(calls[0]!.signal?.aborted).toBe(true);

    await act(async () => calls[1]!.resolve(json({ rev: "new" })));
    // The first call's promise already rejected on abort, so even a server
    // that answered it anyway cannot reach state.
    await act(async () => calls[0]!.resolve(json({ rev: "old" })));
    expect(result.current.data).toEqual({ rev: "new" });
    expect(result.current.error).toBeNull();
  });

  test("an unmounted screen aborts what it was waiting for", () => {
    const calls = manualFetch();
    const { unmount } = renderHook(() => useRest("/secrets"));
    unmount();
    expect(calls[0]!.signal?.aborted).toBe(true);
  });
});

describe("what a failure does to the answer on screen", () => {
  // A REFUSAL IS A NEW FACT ABOUT THE RESOURCE. Keeping the previous body
  // beside a 401 would draw content the current credential cannot read.
  test("a refusal replaces the answer", async () => {
    const calls = manualFetch();
    const { result } = renderHook(() => useRest<{ name: string }>("/config"));
    await act(async () => calls[0]!.resolve(json({ name: "Acme" }, 200, '"01JA"')));

    act(() => result.current.reload());
    await act(async () => calls[1]!.resolve(json({ error: "unauthorized" }, 401)));
    expect(result.current.data).toBeNull();
    expect(result.current.etag).toBeNull();
    expect(result.current.status).toBe(401);
    expect(result.current.error?.unauthorized).toBe(true);
  });

  // AN UNREACHABLE ENGINE SAYS NOTHING ABOUT THE RESOURCE, so the last answer
  // stays, as useQuery keeps its last good answer through a failed poll.
  test("a request that never reached the engine keeps the last answer", async () => {
    const calls = manualFetch();
    const { result } = renderHook(() => useRest<{ name: string }>("/config"));
    await act(async () => calls[0]!.resolve(json({ name: "Acme" }, 200, '"01JA"')));

    act(() => result.current.reload(true));
    await act(async () => calls[1]!.reject(new TypeError("Failed to fetch")));
    expect(result.current.data).toEqual({ name: "Acme" });
    expect(result.current.etag).toBe('"01JA"');
    expect(result.current.status).toBe(0);
    expect(result.current.error?.code).toBe("unreachable");
  });
});

describe("an answer belongs to the path it was read from", () => {
  // A NEW PATH IS A NEW RESOURCE. The previous path's body, drawn while the
  // new read is in flight, is one resource's content under another's name.
  test("a changed path reports nothing until it answers, and never the old body", async () => {
    const calls = manualFetch();
    const { result, rerender } = renderHook(({ path }) => useRest<{ rev: string }>(path), {
      initialProps: { path: "/config/revisions/01JA" },
    });
    await act(async () => calls[0]!.resolve(json({ rev: "a" }, 200, '"01JA"')));
    expect(result.current.data).toEqual({ rev: "a" });

    rerender({ path: "/config/revisions/01JB" });
    expect(result.current).toMatchObject({ data: null, etag: null, status: null, loading: true });
    expect(calls.map((c) => c.path)).toEqual(["/config/revisions/01JA", "/config/revisions/01JB"]);

    // An engine unreachable for the NEW path keeps nothing from the old one.
    await act(async () => calls[1]!.reject(new TypeError("Failed to fetch")));
    expect(result.current.data).toBeNull();
    expect(result.current.etag).toBeNull();
    expect(result.current.status).toBe(0);
  });
});

describe("when it is worth reading again", () => {
  // The banner on a guarded screen asks for a token; setting one has to land
  // on that same screen without a reload.
  test("a token change reads again", async () => {
    const calls = manualFetch();
    renderHook(() => useRest("/setup/integrations"));
    await act(async () => calls[0]!.resolve(json({ error: "unauthorized" }, 401)));
    act(() => void storeToken("operator-token"));
    expect(calls.length).toBe(2);
  });

  test("a tab coming back reads again quietly, and only when asked to", async () => {
    const calls = manualFetch();
    const { result } = renderHook(() => useRest("/setup/integrations", { refetchOnFocus: true }));
    await act(async () => calls[0]!.resolve(json({ tools: [] })));

    Object.defineProperty(document, "visibilityState", { value: "hidden", configurable: true });
    act(() => void document.dispatchEvent(new Event("visibilitychange")));
    // A tab going away is not a reason to read anything: nobody is looking.
    expect(calls.length).toBe(1);

    Object.defineProperty(document, "visibilityState", { value: "visible", configurable: true });
    act(() => void document.dispatchEvent(new Event("visibilitychange")));
    expect(calls.length).toBe(2);
    // QUIETLY: blanking what is on screen because somebody switched tabs
    // would report an absence that is not there.
    expect(result.current.loading).toBe(false);
    await waitFor(() => expect(result.current.status).toBe(200));
  });

  test("without the option, a tab coming back reads nothing", async () => {
    const calls = manualFetch();
    renderHook(() => useRest("/secrets"));
    await act(async () => calls[0]!.resolve(json({ secrets: [] })));
    act(() => void document.dispatchEvent(new Event("visibilitychange")));
    expect(calls.length).toBe(1);
  });
});
