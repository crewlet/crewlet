/**
 * The knowledge search's ranked hits.
 *
 * A snippet is a cut of a MARKDOWN page — the backend's own excerpt — so it is
 * drawn as the prose that page renders to, never with its `#` and `**` in it.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Knowledge } from "./Knowledge.tsx";
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
  location.hash = "#/knowledge?q=runbook";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

test("a page's snippet is drawn as prose, without its markdown marks", async () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  socket.query = ((what: string) =>
    Promise.resolve(
      what === "knowledge"
        ? {
            backend: "native",
            hits: [
              {
                id: "p-1",
                title: "Provisioner runbook",
                container: "ENG",
                snippet: "## Recovery **Drain** the node, then `crewlet run` again",
              },
            ],
          }
        : {},
    )) as typeof socket.query;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Knowledge />
      </Router>
    </ClientContext.Provider>,
  );
  await waitFor(() => expect(screen.getByText("Provisioner runbook")).toBeTruthy());
  const snippet = document.querySelector(".hit-snippet");
  expect(snippet?.textContent).toBe("Recovery Drain the node, then crewlet run again");
});
