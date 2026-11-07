/**
 * A tab whose engine never answers who it is says so, and asks again on a
 * press.
 *
 * The frame's first question is `GET /auth/session`, and it dials the socket
 * only on an answer. A response the browser held (Chrome holds one carrying
 * `Clear-Site-Data` until it has scanned its whole cache) or an engine that
 * stopped answering left every screen drawing placeholders, with nothing
 * saying why and nothing to press. The read now ends at
 * `SESSION_READ_TIMEOUT_MS`, and the frame draws one panel in place of the
 * screen with a way to ask again.
 *
 * Mutations: drop the session read's deadline (the request default of thirty
 * seconds), or draw the screen whatever the session read did, and the first
 * case is red; draw the panel whatever the viewer said, and the last is red.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { App } from "./App.tsx";
import { CHUNKS, loadChunk } from "./lazyScreen.ts";
import { Router } from "./router.tsx";
import { SESSION_READ_TIMEOUT_MS } from "~/contract/identity.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, sessionRestored } from "~/protocol/index.ts";

const UNANSWERED = "The engine hasn’t answered";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** How many times the frame asked who this browser is. */
let asked: number;
/** Whether `GET /auth/session` answers, or is held until its deadline. */
let answering: boolean;

beforeAll(async () => {
  await Promise.all(CHUNKS.map((chunk) => loadChunk(chunk)));
});

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
  location.hash = "#/worklet";
  asked = 0;
  answering = false;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      if (url.pathname === "/auth/session") {
        asked += 1;
        if (answering) {
          return new Response(
            JSON.stringify({
              person: "p-1",
              login: "jane.doe",
              kind: "person",
              grants: [],
              expires_at: "2026-10-05T00:00:00Z",
              status: "signed_in",
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          );
        }
      }
      // HELD, as a browser holds a response: settled only by the request's
      // own abort, which is what its deadline fires.
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () =>
          reject(new DOMException("aborted", "AbortError")),
        );
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  sessionRestored();
  sessionStorage.clear();
  location.hash = "#/";
});

/**
 * The frame over a socket that never dials: the case is about the REST read.
 * `viewer` is what the socket's viewer question answers, or held.
 */
function mount(viewer: unknown = null) {
  const store = new Store();
  const socket = new LiveSocket(store);
  vi.spyOn(socket, "start").mockImplementation(() => {});
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" && viewer !== null ? Promise.resolve(viewer) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
}

test("a session read the engine never answers says so at its deadline, with a retry", async () => {
  mount();
  await act(() => vi.advanceTimersByTimeAsync(SESSION_READ_TIMEOUT_MS - 1));
  expect(screen.queryByText(UNANSWERED)).toBeNull();

  await act(() => vi.advanceTimersByTimeAsync(1));
  expect(screen.getByText(UNANSWERED)).toBeDefined();
  expect(asked).toBe(1);

  // ASKED AGAIN ON A PRESS, and an answer gives the tab back its screen.
  answering = true;
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(asked).toBe(2);
  expect(screen.queryByText(UNANSWERED)).toBeNull();
});

test("a session read that answers in time draws no panel", async () => {
  answering = true;
  mount();
  await act(() => vi.advanceTimersByTimeAsync(SESSION_READ_TIMEOUT_MS + 1));
  expect(asked).toBeGreaterThan(0);
  expect(screen.queryByText(UNANSWERED)).toBeNull();
});

// THE SOCKET ANSWERED WHO THIS IS, so the engine has answered: a session read
// that failed beside it is no reason to take the screen away.
test("a tab whose viewer answered keeps its screen when the session read does not", async () => {
  mount({ login: "jane.doe", owner: "", handle: "", name: "Jane", kind: "human", grants: [] });
  await act(() => vi.advanceTimersByTimeAsync(SESSION_READ_TIMEOUT_MS + 1));
  expect(asked).toBeGreaterThan(0);
  expect(screen.queryByText(UNANSWERED)).toBeNull();
});
