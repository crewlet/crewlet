/**
 * Search is a launcher reached from anywhere, and what this file asserts is
 * the ENGINE's half of it: what is offered, in what order, and that opening a
 * result goes where it says.
 *
 * The surface is the design system's, so its own rules are its own suite's:
 * the highlight wrapping, the highlighted row staying in the list's view, the
 * veil, the Tab trap. The cases here that touch those are asserting the SEAM,
 * which is what a package bump can move under this application silently.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { CommandPalette } from "./CommandPalette.tsx";
import { ALL_NAV } from "./nav.ts";
import { Router, href } from "./router.tsx";
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
  return store;
}

test("search is a combobox: the arrows move a highlight the input names, and Tab never walks the results", () => {
  const onClose = vi.fn();
  mount(onClose);
  const input = screen.getByRole("combobox", { name: "Search" });
  const list = screen.getByRole("listbox", { name: "Results" });
  expect(input.getAttribute("aria-controls")).toBe(list.id);
  expect(document.activeElement).toBe(input);

  const highlighted = () => document.getElementById(input.getAttribute("aria-activedescendant")!);
  const first = within(list).getAllByRole("option")[0]!;
  expect(highlighted()).toBe(first);
  expect(first.getAttribute("aria-selected")).toBe("true");

  fireEvent.keyDown(input, { key: "ArrowDown" });
  const second = within(list).getAllByRole("option")[1]!;
  expect(highlighted()).toBe(second);
  expect(second.getAttribute("aria-selected")).toBe("true");
  expect(first.getAttribute("aria-selected")).toBe("false");
  // Focus stayed where the reader types.
  expect(document.activeElement).toBe(input);

  // Each group is named by its heading.
  const seats = within(list).getByRole("group", { name: "Seats" });
  expect(within(seats).getAllByRole("option")[0]!.textContent).toContain("Engineer 1");

  // No result is a tab stop, so Tab wraps straight back to the input.
  expect(
    within(list)
      .queryAllByRole("option")
      .some((o) => o.tabIndex >= 0),
  ).toBe(false);
  // The trap takes the press (it is the last stop) and puts focus back on it.
  expect(fireEvent.keyDown(input, { key: "Tab" })).toBe(false);
  expect(document.activeElement).toBe(input);

  // Enter opens the highlighted result (with no query, the screens in the
  // rail's order) and closes search.
  expect(second.textContent).toContain(ALL_NAV[1]!.label);
  fireEvent.keyDown(input, { key: "Enter" });
  expect(location.hash).toBe(href(ALL_NAV[1]!.path));
  expect(onClose).toHaveBeenCalledTimes(1);
});

test("a highlight past the end of results that shrank under it stays on a real result", () => {
  const store = mount();
  const input = screen.getByRole("combobox", { name: "Search" });
  const count = screen.getAllByRole("option").length;
  // Up from the first result wraps to the last, as every list's highlight does.
  fireEvent.keyDown(input, { key: "ArrowUp" });
  const options = screen.getAllByRole("option");
  expect(input.getAttribute("aria-activedescendant")).toBe(options[count - 1]!.id);

  // A push removes every seat while the highlight sits on the last of them.
  act(() => store.applyOrg({ name: "Acme", roles: [] }));
  const remaining = screen.getAllByRole("option");
  expect(remaining.length).toBeLessThan(count);
  const last = remaining[remaining.length - 1]!;
  expect(input.getAttribute("aria-activedescendant")).toBe(last.id);
  expect(last.getAttribute("aria-selected")).toBe("true");
});

test("a press on a result keeps focus in the search box, and its click opens the result", () => {
  const onClose = vi.fn();
  mount(onClose);
  const input = screen.getByRole("combobox", { name: "Search" });
  expect(document.activeElement).toBe(input);
  const option = screen.getAllByRole("option")[2]!;
  // Prevented: a press on something that is not a control moves focus to
  // the nearest focusable ancestor, the dialog, which jsdom does not model.
  expect(fireEvent.mouseDown(option)).toBe(false);
  expect(onClose).not.toHaveBeenCalled();
  fireEvent.click(option);
  expect(location.hash).toBe(href(ALL_NAV[2]!.path));
  expect(onClose).toHaveBeenCalledTimes(1);
});

test("a press on the veil closes search on its click, as every modal's veil does", () => {
  const onClose = vi.fn();
  mount(onClose);
  // The veil is the presentational layer the dialog sits inside, which is how
  // a suite reaches it without naming the class the design system draws it in.
  const veil = screen.getByRole("dialog").closest('[role="presentation"]')!;
  // The results are the modal's body, not a popup above it: a popup would
  // close on the press, before the click a tap ends with, and let that click
  // land on the screen the veil was covering.
  fireEvent.pointerDown(veil);
  expect(onClose).not.toHaveBeenCalled();
  fireEvent.click(veil);
  expect(onClose).toHaveBeenCalledTimes(1);
});
