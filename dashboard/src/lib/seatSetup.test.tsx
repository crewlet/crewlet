/**
 * The guarded half of a seat, read once and never asked for where the answer
 * is already known.
 *
 * The company document is an admin's (ADR-0031), so a browser presenting no
 * key is refused on every ask, and so is a member's key: the answer is known
 * before the question. Asking anyway put a refusal on the wire on every seat
 * page such a reader opened. And the refusal it WOULD get is reported, rather
 * than nothing: reported as nothing, the Settings tab told a reader it never
 * asked that no company configuration was active.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { useSeatSetup } from "./seats.ts";
import { ClientContext } from "./store-hooks.ts";
import { knownRefusal, ViewerProvider } from "./viewer.ts";
import { LiveSocket, Store, storeToken } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";

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

/** The viewer answers each reader's key gets. */
const ANONYMOUS = { token_id: "", role: "", reach: "public" };
const MEMBER = { token_id: "ada", role: "member", reach: "member", linked: true, handle: "ada" };
const ADMIN = { token_id: "jane", role: "admin", reach: "admin" };

function probe(viewer: Record<string, unknown> | Promise<never> = ANONYMOUS) {
  const store = new Store();
  store.applyOrg(withDerived({ name: "Acme", roles: [{ name: "PM", handle: "pm" }], units: [] }));
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    if (what === "viewer") return viewer instanceof Promise ? viewer : Promise.resolve(viewer);
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
      <ViewerProvider>
        <Reader />
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  return { asked, reading: () => seen!.reading, error: () => seen!.config.error };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 4; i++) await Promise.resolve();
  });
}

test("an anonymous reader is refused as the engine would refuse them, and nothing is asked", async () => {
  const { asked, reading, error } = probe();
  await settle();
  expect(asked).not.toContain("config");
  expect(reading()).toEqual({ state: "refused" });
  expect(error()).toBe("unauthorized");
});

// A MEMBER'S KEY WORKS AND DOES NOT REACH THE DOCUMENT: once the viewer says
// so, the refusal is `forbidden` — "this is for admins" — and not a request.
// The ONE ask a stored key makes before the viewer answers is the price of
// not making an admin wait a round trip (see the case below), and it is the
// only one: nothing is asked again once the engine has said who this is.
test("a member is told it is for admins, and asked no more once the viewer says so", async () => {
  storeToken("t");
  const { asked, reading, error } = probe(MEMBER);
  await settle();
  expect(asked.filter((what) => what === "config").length).toBeLessThanOrEqual(1);
  expect(reading()).toEqual({ state: "refused" });
  expect(error()).toBe("forbidden");
});

// AND A MEMBER WHOSE VIEWER ANSWERED FIRST asks nothing at all — the second
// seat page of a session, where the frame's answer is already in.
test("a member the viewer has already named is asked nothing", () => {
  expect(knownRefusal({ loading: false, admin: false, anonymous: false }, true)).toBe("forbidden");
  expect(knownRefusal({ loading: false, admin: false, anonymous: true }, false)).toBe(
    "unauthorized",
  );
});

test("an admin is read", async () => {
  storeToken("t");
  const { asked, reading } = probe(ADMIN);
  await settle();
  expect(asked).toContain("config");
  expect(reading().state).toBe("read");
});

// A KEY IS NOT MADE TO WAIT FOR THE VIEWER: an admin's every seat page would
// pay a round trip for a refusal only a member gets.
test("a stored key is asked before the viewer has answered", async () => {
  storeToken("t");
  const { asked } = probe(new Promise<never>(() => {}));
  await settle();
  expect(asked).toContain("config");
});

// THE DISABLED GUARD ANSWERS EVERY CALLER AS AN ADMIN, with no key stored:
// the viewer's word outranks the empty token.
test("a reader the viewer calls an admin is asked with no key stored", () => {
  expect(knownRefusal({ loading: false, admin: true, anonymous: false }, false)).toBeNull();
  expect(knownRefusal({ loading: true, admin: false, anonymous: false }, false)).toBe(
    "unauthorized",
  );
});
