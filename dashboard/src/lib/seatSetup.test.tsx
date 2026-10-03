/**
 * The guarded half of a seat, read once and never asked for without a
 * credential.
 *
 * The company document is behind an operator token, so a browser presenting
 * none is refused on every ask: the answer is known before the question.
 * Asking anyway put a refusal on the wire on every seat page an anonymous
 * reader opened, and a `refused` banner over a page that never had a chance.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { useSeatSetup } from "./seats.ts";
import { ClientContext } from "./store-hooks.ts";
import { LiveSocket, Store, storeToken } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  localStorage.clear();
});

afterEach(() => {
  cleanup();
  localStorage.clear();
});

function probe() {
  const store = new Store();
  store.applyOrg({ name: "Acme", roles: [{ name: "PM", handle: "pm" }], units: [] });
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    return what === "config"
      ? Promise.resolve({ name: "Acme", roles: [{ name: "PM", handle: "pm" }] })
      : Promise.resolve({});
  };
  let seen: ReturnType<typeof useSeatSetup> | undefined;
  function Reader() {
    seen = useSeatSetup("pm");
    return null;
  }
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Reader />
    </ClientContext.Provider>,
  );
  return { asked, reading: () => seen!.reading };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 4; i++) await Promise.resolve();
  });
}

test("an anonymous reader stays unread, and nothing is asked", async () => {
  const { asked, reading } = probe();
  await settle();
  expect(asked).not.toContain("config");
  expect(reading()).toEqual({ state: "unread" });
});

test("a reader with a credential is read", async () => {
  storeToken("t");
  const { asked, reading } = probe();
  await settle();
  expect(asked).toContain("config");
  expect(reading().state).toBe("read");
});
