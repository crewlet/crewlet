/**
 * A banner that names a missing credential has to offer the door to it — and
 * one that names a missing GRANT must not offer a door that does not open.
 *
 * `/setup` is guarded in full, reads included, so a reader the engine refuses
 * gets a refusal on the listing and the screen falls back to what the socket
 * can see. That much is deliberate. What was not is that the banner
 * explaining it offered nothing that could change it. Nobody signed in is
 * offered the sign-in; a signed-in reader lacking the grant is told which
 * grant, since signing in again as themselves would change nothing.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Integrations } from "./Integrations.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, sessionRestored } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** The engine's refusal of every `/setup` read, at `status`, naming `grants`. */
function refusing(status: number, grants: string[] = []) {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            error: "unauthorized",
            ...(grants.length ? { reason: "no_grant", grants } : {}),
          }),
          { status },
        ),
    ),
  );
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/settings/integrations";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionRestored();
  location.hash = "#/";
});

function mount(viewer: unknown) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "viewer" ? viewer : []);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ViewerProvider>
          <Integrations />
        </ViewerProvider>
      </Router>
    </ClientContext.Provider>,
  );
}

test("for nobody signed in, the guarded banner offers the sign-in", async () => {
  refusing(401);
  mount({ login: "", owner: "", grants: [] });

  fireEvent.click(await screen.findByRole("button", { name: "Sign in" }));
  expect(location.hash).toMatch(/^#\/login/);
});

test("for a reader without the grant, the banner names it and offers no sign-in", async () => {
  refusing(403, ["config:read"]);
  mount({ login: "jane.doe", owner: "jane.doe", grants: ["state:read"] });

  expect(
    await screen.findByText(/Reading the integrations' setup state needs config:read/),
  ).toBeDefined();
  expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
});
