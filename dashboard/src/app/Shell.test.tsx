/**
 * The shell's own surfaces (the token dialog, search and the engine panel)
 * are modals on the same layer stack as every screen's dialogs and drawers.
 *
 * They were hand-rolled beside it: each had its own veil, the palette closed
 * on its own Escape, and a window-level listener in the shell closed the
 * palette and the engine panel on a second one. The token dialog is the case
 * that matters most, because a refused request raises it over whatever the
 * operator had open, including an editor drawer with unsaved edits.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState, type ReactNode } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Router } from "./router.tsx";
import { Shell } from "./Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, requestToken } from "~/protocol/index.ts";
import { Drawer } from "~/ui/Drawer.tsx";

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

function mount(screenContent: ReactNode, store = new Store()) {
  const socket = new LiveSocket(store);
  // Saving a token reconnects the socket; there is no engine to dial here.
  vi.spyOn(socket, "reconnect").mockImplementation(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Shell>{screenContent}</Shell>
      </Router>
    </ClientContext.Provider>,
  );
  return { store, socket };
}

function press(key: string, init: Partial<KeyboardEventInit> = {}) {
  fireEvent.keyDown(document.activeElement ?? document.body, { key, ...init });
}

/** A screen with an editor drawer open, the way the organization builder's is. */
function EditorScreen() {
  const [open, setOpen] = useState(true);
  return open ? (
    <Drawer title="Edit Software Engineer" onClose={() => setOpen(false)}>
      <input aria-label="Name" />
    </Drawer>
  ) : (
    <p>editor closed</p>
  );
}

test("Escape with the token dialog over a drawer closes only the token dialog", () => {
  mount(<EditorScreen />);
  const name = screen.getByLabelText("Name");
  expect(document.activeElement).toBe(name);

  // A guarded answer on this screen asks for a credential mid-edit.
  act(() => requestToken());
  const token = screen.getByRole("dialog", { name: "API token" });
  expect(document.activeElement).toBe(screen.getByLabelText("Token"));

  // Tab stays in the token dialog rather than being pulled back to the drawer.
  screen.getByRole("button", { name: "Save and reconnect" }).focus();
  press("Tab");
  expect(token.contains(document.activeElement)).toBe(true);

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "API token" })).toBeNull();
  expect(screen.getByRole("dialog", { name: "Edit Software Engineer" })).toBeDefined();
  // And the editor gets its field back, where the operator was typing.
  expect(document.activeElement).toBe(name);

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Edit Software Engineer" })).toBeNull();
  expect(screen.getByText("editor closed")).toBeDefined();
});

test("search opened from its button returns focus there on Escape", () => {
  mount(<p>screen</p>);
  const search = screen.getByRole("button", { name: /^Search/ });
  search.focus();
  fireEvent.click(search);

  const palette = screen.getByRole("dialog", { name: "Search" });
  expect(palette.contains(document.activeElement)).toBe(true);
  expect(document.activeElement?.tagName).toBe("INPUT");

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
  expect(document.activeElement).toBe(search);
});

test("search opened with a shortcut returns focus to what held it, and one Escape closes it", () => {
  mount(<button>Retry</button>);
  const retry = screen.getByRole("button", { name: "Retry" });
  retry.focus();

  press("/");
  expect(screen.getByRole("dialog", { name: "Search" })).toBeDefined();
  // The same chord that opened it closes it again.
  press("k", { ctrlKey: true });
  expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
  expect(document.activeElement).toBe(retry);

  press("k", { metaKey: true });
  expect(screen.getByRole("dialog", { name: "Search" })).toBeDefined();
  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
  expect(document.activeElement).toBe(retry);
});

test("the search shortcut does not close search from beneath a token dialog raised over it", () => {
  mount(<p>screen</p>);
  press("k", { ctrlKey: true });
  const input = screen.getByRole("textbox", { name: "Search" });
  expect(document.activeElement).toBe(input);

  // A guarded answer asks for a credential while search is open.
  act(() => requestToken());
  const token = screen.getByLabelText("Token");
  expect(document.activeElement).toBe(token);

  // The chord belongs to the surface that holds the keyboard. Closing search
  // from under the dialog would change a page the reader cannot see, and
  // hand focus to what opened search, behind the dialog's veil.
  press("k", { ctrlKey: true });
  expect(screen.getByRole("dialog", { name: "Search" })).toBeDefined();
  expect(document.activeElement).toBe(token);

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "API token" })).toBeNull();
  expect(document.activeElement).toBe(input);
  press("k", { ctrlKey: true });
  expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
});

test("the shortcut hints name the keys the reader's own keyboard prints, in words", () => {
  // Not an Apple platform under the suite, so the command key is Control: a
  // hand-written "⌘K" told everybody else to press a key they do not have.
  mount(<p>screen</p>);
  const search = screen.getByRole("button", { name: /^Search/ });
  expect([...search.querySelectorAll("kbd")].map((k) => k.textContent)).toEqual(["Ctrl", "K"]);
  expect(within(search).getByText("Control plus K")).toBeDefined();

  fireEvent.click(search);
  const palette = screen.getByRole("dialog", { name: "Search" });
  // The sentence each hint reads: a glyph such as "↵" or "esc" is hidden from
  // assistive technology, which hears the key's name instead.
  const spoken = [...palette.querySelectorAll(".kbd-combo .sr-only")].map((s) => s.textContent);
  expect(spoken).toEqual(["Up arrow", "Down arrow", "Enter", "Escape"]);
  expect(
    [...palette.querySelectorAll(".kbd-keys")].every((k) => k.getAttribute("aria-hidden")),
  ).toBe(true);
});

test("one Escape closes search raised over the engine panel, and leaves the panel", () => {
  mount(<p>screen</p>);
  const pill = screen.getByRole("button", { name: /engine unreachable/ });
  pill.focus();
  fireEvent.click(pill);
  const panel = screen.getByRole("dialog", { name: "Engine" });

  press("k", { metaKey: true });
  expect(screen.getByRole("dialog", { name: "Search" })).toBeDefined();

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
  expect(screen.getByRole("dialog", { name: "Engine" })).toBe(panel);
  expect(document.activeElement).toBe(panel);

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Engine" })).toBeNull();
  expect(document.activeElement).toBe(pill);
});

test("the engine panel hands over to the token dialog, and focus comes back to the engine pill", () => {
  const store = new Store();
  store.setAuthRejected(true);
  mount(<p>screen</p>, store);
  const pill = screen.getByRole("button", { name: /token refused/ });
  pill.focus();
  fireEvent.click(pill);

  const panel = screen.getByRole("dialog", { name: "Engine" });
  // The action the operator came for, not the Close button before it. (The
  // banner behind the veil offers the same action; this is the panel's.)
  const setToken = within(panel).getByRole("button", { name: "Set token" });
  expect(document.activeElement).toBe(setToken);

  fireEvent.click(setToken);
  expect(screen.queryByRole("dialog", { name: "Engine" })).toBeNull();
  expect(document.activeElement).toBe(screen.getByLabelText("Token"));

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "API token" })).toBeNull();
  expect(document.activeElement).toBe(pill);
});

test("the engine panel closes on its veil and opens on itself when there is nothing to act on", () => {
  mount(<p>screen</p>);
  const pill = screen.getByRole("button", { name: /engine unreachable/ });
  pill.focus();
  fireEvent.click(pill);

  const panel = screen.getByRole("dialog", { name: "Engine" });
  expect(document.activeElement).toBe(panel);

  const veil = panel.parentElement!;
  // A press inside the panel is not a press on the veil.
  fireEvent.pointerDown(panel);
  fireEvent.click(panel);
  expect(screen.getByRole("dialog", { name: "Engine" })).toBeDefined();

  fireEvent.pointerDown(veil);
  fireEvent.click(veil);
  expect(screen.queryByRole("dialog", { name: "Engine" })).toBeNull();
  expect(document.activeElement).toBe(pill);
});
