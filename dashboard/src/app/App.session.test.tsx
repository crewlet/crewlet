/**
 * A browser the engine does not accept is sent to sign in — from wherever it
 * learns that, back to wherever it was.
 *
 * This replaced a token dialog raised by the socket's refusal. What it has to
 * get right is invisible in any one screen: a `401` from ANY transport moves
 * the reader, the sign-in screens themselves are never moved (they answer
 * their own refusals), and the move REPLACES the entry, so Back after signing
 * in does not land on the screen that could not be drawn.
 */

import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { App } from "./App.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, needSession, sessionRestored } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

function mount(hash: string) {
  location.hash = hash;
  const store = new Store();
  render(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
}

const loginFor = (from: string) => `#/login?next=${encodeURIComponent(from)}`;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify({ error: "invalid_token" }), { status: 401 })),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionRestored();
  location.hash = "#/";
});

describe("a session the engine does not accept", () => {
  test("sends the reader to sign in, carrying where they were", async () => {
    mount("#/work?view=board");
    const depth = history.length;
    act(() => needSession("sign_in"));
    await waitFor(() => expect(location.hash).toBe(loginFor("#/work?view=board")));
    expect(await screen.findByRole("heading", { name: "Sign in to Crewlet" })).toBeDefined();
    // A REPLACE: the screen the reader was on cannot be drawn for them.
    expect(history.length).toBe(depth);
  });

  // THE CONTROL: a sign-in form routed to itself would lose what was typed,
  // and an invitation routed to sign-in would lose the link.
  test("leaves the sign-in screens where they are", async () => {
    const at = loginFor("#/cost");
    mount(at);
    act(() => needSession("sign_in"));
    await screen.findByRole("heading", { name: "Sign in to Crewlet" });
    expect(location.hash).toBe(at);

    cleanup();
    mount("#/invite/abc");
    act(() => needSession("sign_in"));
    expect(location.hash).toBe("#/invite/abc");
  });

  test("a need recorded before anything mounted is followed when it does", async () => {
    // THE SOCKET STARTS BEFORE REACT, so its refusal probe can answer first.
    needSession("sign_in");
    mount("#/inbox");
    await waitFor(() => expect(location.hash).toBe(loginFor("#/inbox")));
  });

  test("a session that may only enrol goes to the enrolment instead", async () => {
    mount("#/admin/fleet");
    act(() => needSession("second_factor"));
    await waitFor(() =>
      expect(location.hash).toBe(`#/enrol?next=${encodeURIComponent("#/admin/fleet")}`),
    );
  });

  test("an enrolment that loses its session signs in toward where it was going", async () => {
    mount(`#/enrol?next=${encodeURIComponent("#/cost")}`);
    act(() => needSession("sign_in"));
    await waitFor(() => expect(location.hash).toBe(loginFor("#/cost")));
  });

  // END TO END THROUGH A SCREEN'S OWN REQUEST: no screen recognises a lost
  // session itself, so the one path that matters is a REST 401 reaching the
  // router without the screen saying a word.
  test("a REST 401 on any screen is enough", async () => {
    mount("#/admin/credentials");
    await waitFor(() => expect(location.hash).toBe(loginFor("#/admin/credentials")));
  });
});
