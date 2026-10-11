/**
 * The browser bindings keep the model's contracts: tokens the model accepts.
 * The transport they used to hold is `protocol/configWrite.ts`, and its
 * contract is held in that module's own suite.
 */

import { expect, test } from "vitest";
import { isMintedKey, mintKey } from "./model/keys.ts";
import { newWriteId } from "./model/writes.ts";
import { randomKeys } from "./runtime.ts";

// The model refuses a token it cannot use, and a token that repeats would
// give two nodes one key or two saves one write id.
test("random tokens are accepted as keys and write ids, and do not repeat", () => {
  const seen = new Set<string>();
  for (let i = 0; i < 200; i++) {
    const token = randomKeys.next();
    expect(() => mintKey({ next: () => token })).not.toThrow();
    expect(() => newWriteId({ next: () => token })).not.toThrow();
    seen.add(token);
  }
  expect(seen.size).toBe(200);
});

// THE DASHBOARD IS SERVED OVER PLAIN HTTP. `crypto.randomUUID` exists only in
// a secure context, so on every engine reached by its address rather than as
// localhost it is absent, and a source that called it would throw on the
// first Add. The Builder's one source reads `getRandomValues`, which every
// browsing context has.
test("random tokens need no randomUUID", () => {
  // It lives on Crypto.prototype, so hiding it means an own property that
  // shadows it, and putting it back means deleting that property again.
  const own = Object.getOwnPropertyDescriptor(crypto, "randomUUID");
  Object.defineProperty(crypto, "randomUUID", { value: undefined, configurable: true });
  try {
    expect(crypto.randomUUID).toBeUndefined();
    const keys = new Set([mintKey(randomKeys), mintKey(randomKeys)]);
    expect(keys.size).toBe(2);
    for (const key of keys) expect(isMintedKey(key)).toBe(true);
  } finally {
    if (own) Object.defineProperty(crypto, "randomUUID", own);
    else delete (crypto as { randomUUID?: unknown }).randomUUID;
  }
  expect(typeof crypto.randomUUID).toBe("function");
});
