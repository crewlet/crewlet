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

import { cleanup, render, screen } from "@testing-library/react";
import { act } from "~/test/inCase.ts";
import { announce } from "@crewlethq/ui";
import { afterEach, beforeEach, expect, test } from "vitest";
import { App } from "./App.tsx";
import { ANNOUNCER_LABEL } from "./announcer.tsx";
import { Router } from "./router.tsx";
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

/** Moves the router the way the navigator does: `pushState` fires no `hashchange`. */
function go(hash: string): void {
  history.pushState(null, "", hash);
  act(() => {
    window.dispatchEvent(new Event("crewlet:route"));
  });
}

/** Says `fullscreen` is the page's fullscreen element, as a browser does. */
function fullscreen(element: Element | null): void {
  Object.defineProperty(document, "fullscreenElement", { configurable: true, value: element });
  act(() => {
    document.dispatchEvent(new Event("fullscreenchange"));
  });
}

// THE PACKAGE'S OWN WITNESS is the console: it warns on every announcement
// nothing hears, and a case that writes to the console fails
// (`src/test/setup.ts`), so each case below also holds that nothing went
// unheard.
beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/";
});

afterEach(() => {
  cleanup();
  Object.defineProperty(document, "fullscreenElement", { configurable: true, value: null });
  sessionStorage.clear();
  location.hash = "#/";
});

test("a control's announcement is said, in the one region the application mounts", () => {
  mount();
  act(() => announce("Added SRE"));
  expect(region().textContent).toBe("Added SRE");
});

// BESIDE THE FRAME, NOT IN IT. The frame swaps the shell for the sign-in
// screens; a region inside it would be torn down by a sign-in, and the package
// forgets its sentence when its last region goes.
test("the region is the same one on the frame's screens and on the sign-in screens", () => {
  mount();
  const framed = region();
  go("#/login");
  // The sign-in screen is drawn, outside the frame.
  expect(screen.getByLabelText("Password")).toBeDefined();
  expect(region()).toBe(framed);
  act(() => announce("Removed SRE"));
  expect(framed.textContent).toBe("Removed SRE");
  go("#/");
  expect(screen.queryByLabelText("Password")).toBeNull();
  expect(region()).toBe(framed);
});

// A FULLSCREEN ELEMENT RENDERS ONLY ITS OWN SUBTREE, which is why the org
// builder carries its own layer host and live region inside its container.
// Its editor is where these announcements come from, so the one region moves
// in while the builder is fullscreen and back out when it leaves.
test("the region follows the fullscreen element, and comes back when it leaves", () => {
  mount();
  const builder = document.createElement("div");
  document.body.append(builder);
  try {
    fullscreen(builder);
    expect(builder.contains(region())).toBe(true);
    act(() => announce("Moved fast to position 1 of 2"));
    expect(builder.textContent).toContain("Moved fast to position 1 of 2");

    fullscreen(null);
    expect(builder.contains(region())).toBe(false);
  } finally {
    builder.remove();
  }
});
