/**
 * A person the socket refuses can still end their own session.
 *
 * The engine refuses a person whose seat was taken out of the chart `403
 * seat_unavailable` everywhere but `/auth/`, and a person without `state:read`
 * the socket — and the socket then stops dialling, so the `viewer` question,
 * which is asked over it, is never answered. The identity menu (now the
 * sidebar's user block) drew nothing until it was and the refusal's banner
 * offered only a retry, so the people
 * the engine had shut out were exactly the people left with no way to sign
 * out: on a shared machine their cookie stayed live until its own deadline.
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

test("the refusal's banner offers a sign-out, and it signs out", async () => {
  await mount();
  const banner = (await screen.findByText(/will not serve this dashboard to you/)).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  fireEvent.click(within(banner).getByRole("button", { name: "Sign out" }));

  await waitFor(() => expect(reloads).toHaveBeenCalledWith("#/login"));
  expect(sent.some((s) => s.method === "POST" && s.path === "/auth/logout")).toBe(true);
});

test("the user block offers both sign-outs though the viewer never answers", async () => {
  await mount();
  fireEvent.click(await screen.findByRole("button", { name: "Account and preferences" }));
  const popover = await screen.findByRole("dialog", { name: "Account and preferences" });
  // UNDER THE SESSION'S OWN LOGIN, and nothing that needs a seat or a factor
  // settled — the viewer that would settle them never answers.
  await waitFor(() => expect(within(popover).getByText("jane.doe")).toBeDefined());
  expect(within(popover).queryByRole("button", { name: "Two-step verification…" })).toBeNull();
  expect(within(popover).getByRole("button", { name: "Sign out everywhere" })).toBeDefined();
  expect(within(popover).getByRole("button", { name: "Sign out" })).toBeDefined();
});
