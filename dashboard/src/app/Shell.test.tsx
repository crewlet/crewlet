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
import { Modal } from "@crewlethq/ui";

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

function press(key: string, init: Partial<KeyboardEventInit> = {}): boolean {
  return fireEvent.keyDown(document.activeElement ?? document.body, { key, ...init });
}

/** A screen with an editor drawer open, the way the organization builder's is. */
function EditorScreen() {
  const [open, setOpen] = useState(true);
  return open ? (
    <Modal
      open
      variant="sheet"
      stackBody
      title="Edit Software Engineer"
      onClose={() => setOpen(false)}
    >
      <input aria-label="Name" />
    </Modal>
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

test("the token field keeps its label over a screen that has a field with the same id", () => {
  // Not contrived: the dialog is raised over any screen, and "token" is the
  // obvious id for a screen's own credential field.
  mount(<input id="token" aria-label="Webhook token" />);
  act(() => requestToken());
  const dialog = screen.getByRole("dialog", { name: "API token" });
  const field = within(dialog).getByLabelText("Token");
  expect(field.getAttribute("type")).toBe("password");
  expect(document.activeElement).toBe(field);
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
  const input = screen.getByRole("combobox", { name: "Search" });
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

test("the search button keeps its name where the narrow layout hides its label", () => {
  mount(<p>screen</p>);
  // What the stylesheet's one breakpoint does to the button: the label and
  // the hint go, and the icon is all that is drawn.
  const narrow = document.createElement("style");
  narrow.textContent = ".omni .omni-label, .omni .kbd-combo { display: none; }";
  document.head.append(narrow);
  try {
    const search = screen.getByRole("button", { name: "Search" });
    expect(search.getAttribute("aria-keyshortcuts")).toBe("Control+K Meta+K /");
  } finally {
    narrow.remove();
  }
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
  // The sentence each hint reads: a glyph such as the return arrow or "esc" is
  // hidden from assistive technology, which hears the key's name instead.
  const hints = [...palette.querySelectorAll("kbd")].map(
    (cap) => cap.closest("[aria-hidden]")?.nextElementSibling?.textContent,
  );
  expect(hints).toEqual(["Up arrow", "Down arrow", "Enter", "Escape"]);
  // Every drawn cap is hidden from it: read as glyphs, the row says nothing.
  expect(
    [...palette.querySelectorAll("kbd")].every((cap) => cap.closest("[aria-hidden]") !== null),
  ).toBe(true);
});

test("the search shortcuts do nothing while a modal holds the keyboard", () => {
  mount(<p>screen</p>);
  const pill = screen.getByRole("button", { name: /engine unreachable/ });
  pill.focus();
  fireEvent.click(pill);
  const panel = screen.getByRole("dialog", { name: "Engine" });
  expect(document.activeElement).toBe(panel);

  // The page behind the panel is inert: search opened over it would sit on
  // a page the reader cannot reach, and could navigate it away.
  for (const init of [{ key: "k", metaKey: true }, { key: "k", ctrlKey: true }, { key: "/" }]) {
    expect(press(init.key, init)).toBe(true);
    expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
    expect(document.activeElement).toBe(panel);
  }

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Engine" })).toBeNull();
  expect(document.activeElement).toBe(pill);
  // Back on the page, the same keys open search again.
  press("k", { metaKey: true });
  expect(screen.getByRole("dialog", { name: "Search" })).toBeDefined();
});

test("search never opens over a dialog whose write is still in flight", () => {
  // A screen's dialog that may not close yet. Search opened over it could
  // navigate, and the navigation would unmount the dialog before the
  // operator saw whether the write took.
  function Saving() {
    return (
      <Modal open stackBody title="Saving" onClose={() => {}} dismissable={false}>
        <button>Wait</button>
      </Modal>
    );
  }
  mount(<Saving />);
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Wait" }));
  press("k", { ctrlKey: true });
  press("/");
  expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
  expect(screen.getByRole("dialog", { name: "Saving" })).toBeDefined();
});

test("a slash opens search only on its own, never as part of a chord or a composition", () => {
  mount(<button>Retry</button>);
  screen.getByRole("button", { name: "Retry" }).focus();
  for (const init of [
    { ctrlKey: true },
    { metaKey: true },
    { altKey: true },
    { isComposing: true },
  ]) {
    expect(press("/", init)).toBe(true);
    expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
  }
  press("/");
  expect(screen.getByRole("dialog", { name: "Search" })).toBeDefined();
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

test("the sections drawer is a modal on the stack: focus goes in, Tab stays, Escape and a route change close it", async () => {
  mount(<p>screen</p>);
  const toggle = screen.getByRole("button", { name: "Sections" });
  toggle.focus();
  fireEvent.click(toggle);

  // Only while it is open is the rail a dialog; beside a wide layout it is
  // the page's own navigation and must not announce itself as modal.
  const drawer = screen.getByRole("dialog", { name: "Sections" });
  expect(drawer.tagName).toBe("ASIDE");
  // It opens on the row for the screen the reader is on.
  const overview = within(drawer).getByRole("link", { name: /Overview/ });
  expect(overview.getAttribute("aria-current")).toBe("page");
  expect(document.activeElement).toBe(overview);

  // Shift+Tab from the first control wraps inside rather than leaving for
  // the page behind the veil.
  within(drawer)
    .getByRole("link", { name: /Crewlet/ })
    .focus();
  press("Tab", { shiftKey: true });
  expect(drawer.contains(document.activeElement)).toBe(true);

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Sections" })).toBeNull();
  expect(document.activeElement).toBe(toggle);

  // Navigating from the drawer closes it, and focus comes back to the toggle
  // rather than staying on a link that has just slid out of view.
  fireEvent.click(toggle);
  const open = screen.getByRole("dialog", { name: "Sections" });
  const link = within(open)
    .getAllByRole("link")
    .find((a) => a.getAttribute("href") !== "#/")!;
  link.focus();
  await act(async () => {
    location.hash = link.getAttribute("href")!;
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  expect(screen.queryByRole("dialog", { name: "Sections" })).toBeNull();
  expect(document.activeElement).toBe(toggle);
});

test("the sections drawer closes on its veil, and a dialog raised over it closes first", () => {
  mount(<p>screen</p>);
  const toggle = screen.getByRole("button", { name: "Sections" });
  toggle.focus();
  fireEvent.click(toggle);
  const drawer = screen.getByRole("dialog", { name: "Sections" });

  // The engine panel opened from the rail sits above it.
  const pill = within(drawer).getByRole("button", { name: /engine unreachable/ });
  pill.focus();
  fireEvent.click(pill);
  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Engine" })).toBeNull();
  expect(screen.getByRole("dialog", { name: "Sections" })).toBe(drawer);
  expect(document.activeElement).toBe(pill);

  const veil = document.querySelector(".drawer-veil")!;
  fireEvent.pointerDown(veil);
  fireEvent.click(veil);
  expect(screen.queryByRole("dialog", { name: "Sections" })).toBeNull();
  expect(document.activeElement).toBe(toggle);
});

test("the sections drawer closes when the layout it belongs to ends", () => {
  mount(<p>screen</p>);
  fireEvent.click(screen.getByRole("button", { name: "Sections" }));
  expect(screen.getByRole("dialog", { name: "Sections" })).toBeDefined();

  // Still narrow: a resize that keeps the toggle on screen changes nothing.
  act(() => void window.dispatchEvent(new Event("resize")));
  expect(screen.getByRole("dialog", { name: "Sections" })).toBeDefined();

  // Past the breakpoint the stylesheet hides the toggle, and the rail is the
  // page's column again rather than a modal over it.
  const wide = document.createElement("style");
  wide.textContent = ".drawer-toggle { display: none; }";
  document.head.append(wide);
  try {
    act(() => void window.dispatchEvent(new Event("resize")));
    expect(screen.queryByRole("dialog", { name: "Sections" })).toBeNull();
    expect(screen.getByRole("navigation", { name: "Sections" })).toBeDefined();
  } finally {
    wide.remove();
  }
});

// EB16. The density row draws one letter per choice because that is what fits
// in the rail, and a screen reader was told "S", "M" and "L": three names that
// say nothing about what pressing one does. The theme row beside it was
// already right, because its options draw no label at all and fall back to
// their tooltip.
test("the rail's settings are announced by what they are, not by the letter drawn", () => {
  mount(<p>a screen</p>);
  const density = screen.getByRole("radiogroup", { name: "Density" });
  expect(
    within(density)
      .getAllByRole("radio")
      .map((radio) => radio.getAttribute("aria-label")),
  ).toEqual(["Compact", "Normal", "Comfortable"]);
  const theme = screen.getByRole("radiogroup", { name: "Theme" });
  expect(
    within(theme)
      .getAllByRole("radio")
      .map((radio) => radio.getAttribute("aria-label")),
  ).toEqual(["Light", "Follow the system", "Dark"]);
});
