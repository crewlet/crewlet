import { renderHook } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { useOwned } from "./useOwned.ts";

/** A resource that knows whether it was stopped, and counts how many were made. */
class Resource {
  static made: Resource[] = [];
  stopped = 0;
  constructor(readonly key: string) {
    Resource.made.push(this);
  }
}

function useResource(key: string | null) {
  return useOwned(
    () => (key === null ? null : new Resource(key)),
    (r) => void r.stopped++,
    [key],
  );
}

describe("useOwned", () => {
  test("never hands back a resource its cleanup stopped, under StrictMode's replayed effects", () => {
    Resource.made = [];
    const { result } = renderHook(() => useResource("a"), { reactStrictMode: true });
    const held = result.current;
    expect(held).not.toBeNull();
    expect(held!.stopped).toBe(0);
    // Every resource made was either stopped once or is the one on show.
    for (const r of Resource.made) expect(r.stopped).toBe(r === held ? 0 : 1);
  });

  test("a change of deps stops the old resource and never shows it under the new deps", () => {
    Resource.made = [];
    const seen: { asked: string | null; got: Resource | null }[] = [];
    const { result, rerender } = renderHook(
      ({ key }) => {
        const r = useResource(key);
        seen.push({ asked: key, got: r });
        return r;
      },
      { initialProps: { key: "a" as string | null } },
    );
    const first = result.current!;
    rerender({ key: "b" });
    expect(first.stopped).toBe(1);
    expect(result.current!.key).toBe("b");
    // Every render saw the resource its own key built, or nothing while it was made.
    for (const { asked, got } of seen) if (got) expect(got.key).toBe(asked);
    rerender({ key: null });
    expect(result.current).toBeNull();
    expect(Resource.made.every((r) => r.stopped === 1)).toBe(true);
  });

  test("unmounting stops what it holds", () => {
    Resource.made = [];
    const { result, unmount } = renderHook(() => useResource("a"));
    const held = result.current!;
    unmount();
    expect(held.stopped).toBe(1);
  });
});
