/**
 * A banner that names a missing credential has to offer the door to it.
 *
 * `/setup` is guarded in full, reads included, so a reader with no operator
 * token gets a 401 on the listing and the screen falls back to what the
 * socket can see. That much is deliberate. What was not is that the banner
 * explaining it offered nothing that could set a token: with anonymous reads
 * allowed the socket is never refused, so the dialog's other two doors (a
 * socket refusal, and the engine panel) both stay shut on exactly the screen
 * that needs it. The reader is told what is missing and left with no way to
 * supply it.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Integrations } from "./Integrations.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, onTokenRequested } from "~/protocol/index.ts";

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

test("the guarded banner offers the token dialog", async () => {
  const raised = vi.fn();
  const stop = onTokenRequested(raised);
  mount();

  const button = await screen.findByRole("button", { name: /set token/i });
  fireEvent.click(button);
  expect(raised).toHaveBeenCalled();
  stop();
});
