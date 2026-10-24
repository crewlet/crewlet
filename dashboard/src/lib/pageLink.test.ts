/**
 * Copying a page's address says what the browser did, including when it said
 * no.
 *
 * The engine serves the dashboard over plain HTTP on `api.port` unless
 * something terminates TLS in front of it, and an insecure origin has NO
 * `navigator.clipboard` at all, so the ordinary deployment is the one where
 * the write cannot even be attempted. The palette's command used to call it
 * through an optional chain, which skipped the whole thing: no copy, no word.
 */

import { afterEach, expect, test, vi } from "vitest";

import { copyPageLink } from "./pageLink.ts";

const had = Object.getOwnPropertyDescriptor(navigator, "clipboard");

function clipboard(value: unknown): void {
  Object.defineProperty(navigator, "clipboard", { configurable: true, value });
}

afterEach(() => {
  if (had) Object.defineProperty(navigator, "clipboard", had);
  else delete (navigator as unknown as Record<string, unknown>).clipboard;
});

test("an insecure origin, with no clipboard at all, is Blocked", async () => {
  clipboard(undefined);
  expect(await copyPageLink()).toBe("Blocked");
});

test("a write the browser refuses is Blocked", async () => {
  clipboard({ writeText: vi.fn().mockRejectedValue(new Error("NotAllowedError")) });
  expect(await copyPageLink()).toBe("Blocked");
});

test("a write that lands copies the whole URL and is Copied", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  clipboard({ writeText });
  expect(await copyPageLink()).toBe("Copied");
  expect(writeText).toHaveBeenCalledWith(location.href);
});
