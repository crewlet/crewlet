/**
 * The design system's announcements are said, by one region the application
 * mounts once — and they are said where the reader is.
 *
 * uilet's list and tag controls speak through a module-level `announce()`
 * that is silent unless an `<Announcer>` is mounted. Nothing mounted one, so
 * every add, remove and reorder in the org builder's editor said nothing to a
 * screen reader and printed a warning into the console instead. These cases
 * hold the application, not a harness, to mounting it.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { announce } from "@crewlethq/ui";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { CHUNKS, loadChunk } from "./lazyScreen.ts";
import { App } from "./App.tsx";
import { ANNOUNCER_LABEL } from "./announcer.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

// EVERY SCREEN'S CODE IS IN BEFORE A CASE STARTS — the sign-in screens' chunk
// and every workspace's (`app/lazyScreen.ts`) — because this suite asserts
// where a reader is sent and what is drawn there, not how long a cold
// `import()` takes under a test transformer.
beforeAll(async () => {
  await Promise.all([...CHUNKS, "signin" as const].map((chunk) => loadChunk(chunk)));
});

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

function mount() {
  const store = new Store();
  const socket = new LiveSocket(store);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
}

/** The application's live region: there must be exactly one. */
function region(): HTMLElement {
  const all = screen.getAllByRole("status", { name: ANNOUNCER_LABEL });
  expect(all).toHaveLength(1);
  return all[0]!;
}

/**
 * Lets every render in flight finish. AWAITED, as every act here is: a screen
 * reads its chunk through `use`, and a synchronous act stops flushing at the
 * first render that used a promise, settled or not, so what a case asserts
 * would be left unrendered.
 */
async function settle(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 20; i++) await Promise.resolve();
  });
}

/** Moves the router the way the navigator does: `pushState` fires no `hashchange`. */
async function go(hash: string): Promise<void> {
  history.pushState(null, "", hash);
  await act(async () => {
    window.dispatchEvent(new Event("crewlet:route"));
  });
  await settle();
}

/** Says `fullscreen` is the page's fullscreen element, as a browser does. */
async function fullscreen(element: Element | null): Promise<void> {
  Object.defineProperty(document, "fullscreenElement", { configurable: true, value: element });
  await act(async () => {
    document.dispatchEvent(new Event("fullscreenchange"));
  });
}

/** Says `text` through the package's one module-level function. */
async function say(text: string): Promise<void> {
  await act(async () => {
    announce(text);
  });
}

// THE PACKAGE'S OWN WITNESS is the console: it warns on every announcement
// nothing hears, so each case below also holds that nothing went unheard.
let unheard: { mock: { calls: unknown[][] }; mockRestore: () => void };

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/";
  unheard = vi.spyOn(console, "warn");
});

afterEach(() => {
  const said = unheard.mock.calls.filter((call) => String(call[0]).includes("announce()"));
  unheard.mockRestore();
  expect(said, "an announcement nothing heard").toEqual([]);
  cleanup();
  Object.defineProperty(document, "fullscreenElement", { configurable: true, value: null });
  sessionStorage.clear();
  location.hash = "#/";
});

test("a control's announcement is said, in the one region the application mounts", async () => {
  mount();
  await settle();
  await say("Added SRE");
  expect(region().textContent).toBe("Added SRE");
});

// BESIDE THE FRAME, NOT IN IT. The frame swaps the shell for the sign-in
// screens; a region inside it would be torn down by a sign-in, and the package
// forgets its sentence when its last region goes.
test("the region is the same one on the frame's screens and on the sign-in screens", async () => {
  mount();
  await settle();
  const framed = region();
  await go("#/login");
  // The sign-in screen is drawn, outside the frame.
  expect(await screen.findByLabelText("Password")).toBeDefined();
  expect(region()).toBe(framed);
  await say("Removed SRE");
  expect(framed.textContent).toBe("Removed SRE");
  await go("#/");
  expect(screen.queryByLabelText("Password")).toBeNull();
  expect(region()).toBe(framed);
});

// A FULLSCREEN ELEMENT RENDERS ONLY ITS OWN SUBTREE, which is why the org
// builder carries its own layer host and live region inside its container.
// Its editor is where these announcements come from, so the one region moves
// in while the builder is fullscreen and back out when it leaves.
test("the region follows the fullscreen element, and comes back when it leaves", async () => {
  mount();
  await settle();
  const builder = document.createElement("div");
  document.body.append(builder);
  try {
    await fullscreen(builder);
    expect(builder.contains(region())).toBe(true);
    await say("Moved fast to position 1 of 2");
    expect(builder.textContent).toContain("Moved fast to position 1 of 2");

    await fullscreen(null);
    expect(builder.contains(region())).toBe(false);
  } finally {
    builder.remove();
  }
});
