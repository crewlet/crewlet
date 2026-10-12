/**
 * The token every tab of the dashboard shares, and the one signal that says it
 * changed.
 *
 * A TAB IS TOLD WHEN ANOTHER TAB CHANGES IT. The token lives in `localStorage`,
 * which every tab of the origin shares, and a request reads it when it is sent
 * — so a token saved in one window is the credential the next write from every
 * other window goes out under. Announced only for a tab's own writes, a window
 * still drawing the last person's chat flushed their reading as the next
 * person, and kept their socket.
 */

import { afterEach, expect, test, vi } from "vitest";

import { STORAGE_KEYS } from "../lib/storage.ts";
import { clearToken, onTokenChanged, storeToken } from "./authToken.ts";

afterEach(() => {
  clearToken();
});

/** Another tab of the origin writing `key`, as this document hears it. */
function anotherTab(key: string | null, value: string | null) {
  if (key !== null) {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  }
  window.dispatchEvent(new StorageEvent("storage", { key, newValue: value }));
}

test("this tab's own save and sign-out are announced", () => {
  const heard = vi.fn();
  const stop = onTokenChanged(heard);
  storeToken("token-ada");
  clearToken();
  stop();
  expect(heard).toHaveBeenCalledTimes(2);
});

test("another tab's save, sign-out and clear are announced here", () => {
  const heard = vi.fn();
  const stop = onTokenChanged(heard);
  anotherTab(STORAGE_KEYS.apiToken, "token-bo");
  anotherTab(STORAGE_KEYS.apiToken, null);
  // A `localStorage.clear()` names no key, and takes the token with it.
  anotherTab(null, null);
  stop();
  expect(heard).toHaveBeenCalledTimes(3);
});

test("another tab writing any other key is not a token change", () => {
  const heard = vi.fn();
  const stop = onTokenChanged(heard);
  anotherTab(STORAGE_KEYS.theme, "dark");
  stop();
  expect(heard).not.toHaveBeenCalled();
});

test("the window is listened to while somebody listens, and only then", () => {
  // ONE LISTENER for every subscriber, installed with the first and removed
  // with the last — as `lib/recents.ts` follows its own key.
  const add = vi.spyOn(window, "addEventListener");
  const remove = vi.spyOn(window, "removeEventListener");
  const storage = (spy: typeof add) => spy.mock.calls.filter(([type]) => type === "storage");
  const first = onTokenChanged(() => undefined);
  const second = onTokenChanged(() => undefined);
  expect(storage(add)).toHaveLength(1);
  first();
  expect(storage(remove)).toHaveLength(0);
  second();
  expect(storage(remove)).toHaveLength(1);
  add.mockRestore();
  remove.mockRestore();
});
