/**
 * A person the socket refuses is told who they are, and can still reach their
 * Account and end their own session.
 *
 * The engine refuses a person whose seat was taken out of the chart `403
 * seat_unavailable` everywhere but `/auth/`, and a person without `state:read`
 * the socket — and the socket then stops dialling, so the `viewer` question,
 * which is asked over it, is never answered. The identity menu (now the
 * sidebar's user block) drew nothing until it was and the refusal's banner
 * offered only a retry, so the people the engine had shut out were exactly the
 * people left with no way to sign out: on a shared machine their cookie stayed
 * live until its own deadline. And every screen mounted anyway — Home said it
 * was "not connected" and showing "the last state it sent" when nothing was
 * ever sent, beside figures that never resolved, while the sidebar's foot said
 * "Checking who you are" for good. So every screen but the Account is replaced
 * by one panel that says who they are, what they hold and why.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { App } from "./App.tsx";
import { loadChunk } from "./lazyScreen.ts";
import { Router } from "./router.tsx";
import { page } from "~/lib/session.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, sessionRestored } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

interface Sent {
  method: string;
  path: string;
}

let sent: Sent[];
let reloads: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
  location.hash = "#/work";
  sent = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      const method = (init?.method ?? "GET").toUpperCase();
      sent.push({ method, path: url.pathname });
      const json = (body: unknown, status = 200) =>
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        });
      // `/auth/` IS STILL SERVED to this session, which is the whole point.
      if (method === "GET" && url.pathname === "/auth/session") {
        return json({
          person: "p-1",
          login: "jane.doe",
          kind: "person",
          grants: [],
          expires_at: "2026-10-05T00:00:00Z",
          status: "signed_in",
        });
      }
      if (method === "POST" && url.pathname === "/auth/logout") return json({});
      // Everything else is refused, as the guard refuses it.
      return json(
        { error: "seat_unavailable", detail: "your seat is no longer in the chart" },
        403,
      );
    }),
  );
  reloads = vi.spyOn(page, "reloadInto").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  sessionRestored();
  sessionStorage.clear();
  location.hash = "#/";
});

/**
 * The shell over a socket that was refused, and whose questions never answer.
 * The Work screen's chunk is fetched first, so the screen draws rather than
 * suspending inside the render.
 */
async function mount() {
  await loadChunk("work");
  await loadChunk("account");
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => new Promise(() => {});
  act(() => store.setAccessRefused("your seat is no longer in the chart"));
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
}

test("every screen is one panel naming who you are, and it signs out", async () => {
  await mount();
  const panel = (await screen.findByText("The engine will not serve this screen to you")).closest(
    ".crewlet-empty-state",
  ) as HTMLElement;
  await waitFor(() => expect(within(panel).getByText("jane.doe")).toBeDefined());
  expect(within(panel).getByText(/you hold no grants yet/)).toBeDefined();
  expect(within(panel).getByText(/your seat is no longer in the chart/)).toBeDefined();
  // NOTHING SAYS IT IS RECONNECTING, and nothing the screen would have asked
  // is drawn: the socket was refused and stopped.
  expect(screen.queryByText(/Reconnecting|Not connected/)).toBeNull();
  // AND THE FOOT NAMES THEM, from the session, though the viewer never answers.
  expect(screen.queryByText("Checking who you are")).toBeNull();

  fireEvent.click(within(panel).getByRole("button", { name: "Sign out" }));
  await waitFor(() => expect(reloads).toHaveBeenCalledWith("#/login"));
  expect(sent.some((s) => s.method === "POST" && s.path === "/auth/logout")).toBe(true);
});

test("the panel's Account is the one screen it does not replace", async () => {
  await mount();
  fireEvent.click(await screen.findByRole("button", { name: "Account" }));
  await waitFor(() => expect(location.hash).toBe("#/account"));
  await waitFor(() =>
    expect(screen.queryByText("The engine will not serve this screen to you")).toBeNull(),
  );
});

test("the user block offers both sign-outs though the viewer never answers", async () => {
  await mount();
  fireEvent.click(await screen.findByRole("button", { name: "Account and preferences" }));
  const popover = await screen.findByRole("dialog", { name: "Account and preferences" });
  // UNDER THE SESSION'S OWN LOGIN, the viewer that would name a seat never
  // answering — with the way to their Account, which reads no socket.
  await waitFor(() => expect(within(popover).getByText("jane.doe")).toBeDefined());
  expect(within(popover).getByRole("link", { name: "Account" })).toBeDefined();
  expect(within(popover).getByRole("button", { name: "Sign out everywhere" })).toBeDefined();
  expect(within(popover).getByRole("button", { name: "Sign out" })).toBeDefined();
});
