/**
 * A related row is a link for a reader who may open its page, and nothing for
 * one who may not.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { GaugeGlyph } from "@crewlethq/icons/glyphs";
import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { RelatedRow } from "./RelatedRow.tsx";
import { settled } from "~/test/settle.ts";

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
  // THE SESSION READ IS REFUSED, so what the reader holds is the viewer's.
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("{}", { status: 404 })),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function mount(grants: string[]) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer"
      ? Promise.resolve({ login: "ana", owner: "ana", handle: "ana", grants, acts: [] })
      : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <RelatedRow
              path={["settings", "limits"]}
              icon={<GaugeGlyph size="sm" />}
              title="Engine limits"
              description="The caps the engine holds every turn to."
            />
            <p>loaded</p>
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

test("the row is one link, named by its place and its sentence, in a Related landmark", async () => {
  mount(["admin"]);
  const related = await screen.findByRole("navigation", { name: "Related" });
  const link = screen.getByRole("link");
  expect(related.contains(link)).toBe(true);
  expect(link.getAttribute("href")).toBe("#/settings/limits");
  expect(link.textContent).toContain("Engine limits");
  expect(link.textContent).toContain("The caps the engine holds every turn to.");
});

// A LINK TO A LOCK IS A DEAD END: a reader the page would refuse is offered
// nothing. Mutation: drop the access check and the reader holding Worklet
// alone finds the link.
test("a reader the page would refuse is offered no row", async () => {
  mount(["worklet"]);
  await screen.findByText("loaded");
  // THE READER IS KNOWN: the viewer answered and the session read was
  // refused, and what either caused has drawn — so a row that was coming
  // would be here.
  await settled(() => vi.mocked(fetch).mock.calls.length);
  expect(screen.queryByRole("navigation", { name: "Related" })).toBeNull();
  expect(screen.queryByRole("link")).toBeNull();
});
