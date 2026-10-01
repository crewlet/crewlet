/**
 * A banner that names a missing grant has to offer the door to it.
 *
 * `/setup` is guarded in full, reads included, so a reader the engine refuses
 * gets a 401 or 403 on the listing and the screen falls back to what the
 * socket can see. That much is deliberate. What was not is that the banner
 * explaining it once offered nothing that could act on it: the reader was
 * told what is missing and left with no way to supply it. The door is a
 * sign-in, and it comes back to this screen.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Integrations } from "./Integrations.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

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
  location.hash = "#/admin/integrations";
  // The engine's own refusal, byte for byte: internal/api/auth answers 401
  // with this body, and RestError.unauthorized is what the screen branches on.
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify({ error: "unauthorized" }), { status: 401 })),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount() {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve([]);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Integrations />
      </Router>
    </ClientContext.Provider>,
  );
}

test("the guarded banner offers a sign-in that comes back to this screen", async () => {
  mount();

  fireEvent.click(await screen.findByRole("button", { name: "Sign in" }));
  await waitFor(() =>
    expect(location.hash).toBe(`#/login?next=${encodeURIComponent("#/admin/integrations")}`),
  );
});
