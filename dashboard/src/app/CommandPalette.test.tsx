/**
 * Search is a launcher reached from anywhere, so what it must get right is
 * the keyboard: the highlighted row is the one Enter opens, and it has to be
 * on screen while the reader moves it.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { CommandPalette } from "./CommandPalette.tsx";
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

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/";
});

afterEach(() => {
  // Explicit: the suite runs with `globals: false`, so testing-library
  // registers no cleanup of its own.
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

function mount(onClose = () => {}) {
  const store = new Store();
  store.applyOrg({
    name: "Acme",
    roles: Array.from({ length: 30 }, (_, i) => ({
      name: `Engineer ${i + 1}`,
      handle: `engineer-${i + 1}`,
      goal: "Ship the product",
    })),
  });
  const socket = new LiveSocket(store);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <CommandPalette onClose={onClose} />
      </Router>
    </ClientContext.Provider>,
  );
}

/**
 * A layout for a DOM that has none: the result list is 100px tall and every
 * row 20px, stacked from the list's own scroll position.
 */
function layOut() {
  vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
    const list = this.closest(".palette-results") as HTMLElement | null;
    const rect = (top: number, height: number) =>
      ({ top, bottom: top + height, left: 0, right: 0, width: 0, height }) as DOMRect;
    if (!list) return rect(0, 0);
    if (this === list) return rect(0, 100);
    const rows = [...list.querySelectorAll("[role='option']")];
    const index = rows.indexOf(this);
    return index < 0 ? rect(0, 0) : rect(index * 20 - list.scrollTop, 20);
  });
}

test("the highlighted result is scrolled into the list's view as the cursor moves", () => {
  layOut();
  mount();
  const input = screen.getByRole("textbox", { name: "Search" });
  const list = document.querySelector<HTMLElement>(".palette-results")!;
  expect(list.scrollTop).toBe(0);

  // Row 5 (0-based) ends at 120px, below a 100px list: it scrolls by 20.
  for (let i = 0; i < 5; i++) fireEvent.keyDown(input, { key: "ArrowDown" });
  expect(list.scrollTop).toBe(20);

  // Back to the top row: the list follows it up.
  for (let i = 0; i < 5; i++) fireEvent.keyDown(input, { key: "ArrowUp" });
  expect(list.scrollTop).toBe(0);
});
