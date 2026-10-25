/**
 * A brand's loaded assets: every logo URL this page makes is one it revokes,
 * including for a read that settles after the screen that asked for it has
 * gone (the adopted brand's reads are shared and ignore the signal).
 */

import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { BrandDocument } from "~/protocol/index.ts";
import { useBrandAssets, type BrandSource } from "./useBrand.ts";

const { createObjectURL, revokeObjectURL } = URL;

afterEach(() => {
  cleanup();
  Object.assign(URL, { createObjectURL, revokeObjectURL });
});

const DOC: BrandDocument = {
  v: 1,
  name: "FairSplit",
  logos: { primary: "logos/primary.svg" },
  colors: { primary: "#0f7b5f" },
  type: { heading: { kit: "geist" }, body: { kit: "geist" } },
  voice: {},
};

/** A source whose one logo read the test settles by hand, ignoring the signal as the adopted brand's does. */
function heldSource(): BrandSource & { settle: () => void } {
  let settle = () => {};
  const held = new Promise<Blob>((resolve) => {
    settle = () => resolve(new Blob(["<svg/>"], { type: "image/svg+xml" }));
  });
  return {
    key: "adopted:held",
    document: DOC,
    files: new Map([
      ["logos/primary.svg", { path: "logos/primary.svg", type: "image/svg+xml", hash: "h" }],
    ]),
    read: () => held,
    settle: () => settle(),
  };
}

function spyOnUrls() {
  let n = 0;
  const made = vi.fn(() => `blob:test/${++n}`);
  const revoked = vi.fn();
  Object.assign(URL, { createObjectURL: made, revokeObjectURL: revoked });
  return { made, revoked };
}

test("a logo read is made a URL while the screen is there, and the URL is revoked when it goes", async () => {
  const { made, revoked } = spyOnUrls();
  const source = heldSource();
  const { result, unmount } = renderHook(() => useBrandAssets(source));
  source.settle();
  await waitFor(() => expect(result.current.logo("primary")).toBe("blob:test/1"));
  unmount();
  expect(made).toHaveBeenCalledTimes(1);
  expect(revoked).toHaveBeenCalledWith("blob:test/1");
});

test("a logo read that settles after the screen has gone makes no URL", async () => {
  const { made } = spyOnUrls();
  const source = heldSource();
  const { unmount } = renderHook(() => useBrandAssets(source));
  unmount();
  source.settle();
  // Let the settled read's continuations run.
  await new Promise((r) => setTimeout(r, 0));
  expect(made).not.toHaveBeenCalled();
});
