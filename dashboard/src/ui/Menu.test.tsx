/**
 * The menu button pattern: where focus goes in, where it goes back, and which
 * surface a key closes.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { Dialog } from "./Dialog.tsx";
import { Menu, type MenuEntry } from "./Menu.tsx";
import { VIEW_CHANGE_EVENT } from "./viewport.ts";
import { EditGlyph, MoveItemGlyph } from "@crewlethq/icons/glyphs";

afterEach(cleanup);

function entries(overrides: { onEdit?: () => void; onDelete?: () => void } = {}): MenuEntry[] {
  return [
    { key: "edit", label: "Edit", icon: EditGlyph, onSelect: overrides.onEdit ?? (() => {}) },
    { key: "move", label: "Move to", icon: MoveItemGlyph, onSelect: () => {} },
    { key: "open", label: "Open seat", disabled: true, onSelect: () => {} },
    { kind: "separator", key: "sep" },
    { key: "delete", label: "Delete", danger: true, onSelect: overrides.onDelete ?? (() => {}) },
  ];
}

const trigger = () => screen.getByRole("button", { name: "Actions for Software Engineer" });
const item = (name: string) => screen.getByRole("menuitem", { name });
const press = (key: string, init: Partial<KeyboardEventInit> = {}) =>
  fireEvent.keyDown(document.activeElement ?? document.body, { key, ...init });

test("the trigger announces a menu, and opening it puts focus on the first item", () => {
  render(<Menu label="Actions for Software Engineer" items={entries()} />);
  expect(trigger().getAttribute("aria-haspopup")).toBe("menu");
  expect(trigger().getAttribute("aria-expanded")).toBe("false");

  fireEvent.click(trigger());
  expect(trigger().getAttribute("aria-expanded")).toBe("true");
  expect(screen.getByRole("menu").id).toBe(trigger().getAttribute("aria-controls"));
  expect(document.activeElement).toBe(item("Edit"));
});

test("ArrowUp on the trigger opens on the last item; arrows wrap, Home and End jump, a letter finds", () => {
  render(<Menu label="Actions for Software Engineer" items={entries()} />);
  trigger().focus();
  fireEvent.keyDown(trigger(), { key: "ArrowUp" });
  expect(document.activeElement).toBe(item("Delete"));

  press("ArrowDown");
  expect(document.activeElement).toBe(item("Edit"));
  press("ArrowUp");
  expect(document.activeElement).toBe(item("Delete"));
  press("Home");
  expect(document.activeElement).toBe(item("Edit"));
  press("End");
  expect(document.activeElement).toBe(item("Delete"));
  press("m");
  expect(document.activeElement).toBe(item("Move to"));
  // A disabled item is still reached, and says it is unavailable.
  press("ArrowDown");
  expect(document.activeElement).toBe(item("Open seat"));
  expect(item("Open seat").getAttribute("aria-disabled")).toBe("true");
});

test("a choice's answers are radio items that say which one is current, and the arrows walk them", () => {
  const chosen = vi.fn();
  const items: MenuEntry[] = [
    { key: "none", label: "No lead", checked: false, onSelect: () => chosen("none") },
    { key: "vpe", label: "VP Engineering", checked: true, onSelect: () => chosen("vpe") },
    { kind: "separator", key: "sep" },
    { key: "other", label: "Choose another seat", onSelect: () => chosen("other") },
  ];
  render(<Menu label="Lead of Engineering" items={items} />);
  fireEvent.click(screen.getByRole("button", { name: "Lead of Engineering" }));
  const none = screen.getByRole("menuitemradio", { name: "No lead" });
  const vpe = screen.getByRole("menuitemradio", { name: "VP Engineering" });
  expect(none.getAttribute("aria-checked")).toBe("false");
  expect(vpe.getAttribute("aria-checked")).toBe("true");
  // An item that says nothing about `checked` stays an action.
  expect(item("Choose another seat")).toBeDefined();
  // The answers stand in one group, apart from the action, so a screen reader
  // counts them among themselves.
  const group = screen.getByRole("group");
  expect(within(group).getAllByRole("menuitemradio")).toEqual([none, vpe]);
  expect(group.contains(item("Choose another seat"))).toBe(false);

  expect(document.activeElement).toBe(none);
  press("ArrowDown");
  expect(document.activeElement).toBe(vpe);
  press("ArrowDown");
  expect(document.activeElement).toBe(item("Choose another seat"));
  press("ArrowDown");
  expect(document.activeElement).toBe(none);
  fireEvent.click(vpe);
  expect(chosen).toHaveBeenCalledWith("vpe");
});

test("Escape closes the menu and returns focus to the trigger, even inside a dialog", () => {
  const closed = vi.fn();
  render(
    <Dialog title="Edit unit" onClose={closed}>
      <Menu label="Actions for Software Engineer" items={entries()} />
    </Dialog>,
  );
  fireEvent.click(trigger());
  expect(screen.getByRole("menu")).toBeDefined();

  press("Escape");
  expect(screen.queryByRole("menu")).toBeNull();
  expect(document.activeElement).toBe(trigger());
  expect(closed).not.toHaveBeenCalled();
});

test("an action runs after focus is back on the opener, so a dialog it opens can return there", () => {
  let focusedWhenSelected: Element | null = null;
  render(
    <Menu
      label="Actions for Software Engineer"
      items={entries({ onEdit: () => (focusedWhenSelected = document.activeElement) })}
    />,
  );
  trigger().focus();
  fireEvent.click(trigger());
  fireEvent.click(item("Edit"));
  expect(focusedWhenSelected).toBe(trigger());
  expect(screen.queryByRole("menu")).toBeNull();
});

test("a disabled item does nothing and leaves the menu open", () => {
  const onSelect = vi.fn();
  const items: MenuEntry[] = [{ key: "x", label: "Open seat", disabled: true, onSelect }];
  render(<Menu label="Actions for Software Engineer" items={items} />);
  fireEvent.click(trigger());
  fireEvent.click(item("Open seat"));
  expect(onSelect).not.toHaveBeenCalled();
  expect(screen.getByRole("menu")).toBeDefined();
});

test("a press outside closes it without pulling focus back", () => {
  render(
    <>
      <input aria-label="elsewhere" />
      <Menu label="Actions for Software Engineer" items={entries()} />
    </>,
  );
  fireEvent.click(trigger());
  const elsewhere = screen.getByLabelText("elsewhere");
  fireEvent.pointerDown(elsewhere);
  expect(screen.queryByRole("menu")).toBeNull();
  expect(document.activeElement).not.toBe(trigger());
});

test("Tab closes it and hands focus back to the opener for the Tab to carry on from", () => {
  render(<Menu label="Actions for Software Engineer" items={entries()} />);
  trigger().focus();
  fireEvent.click(trigger());
  // Not prevented: the browser's own Tab then moves on from the trigger.
  expect(press("Tab")).toBe(true);
  expect(screen.queryByRole("menu")).toBeNull();
  expect(document.activeElement).toBe(trigger());
});

test("Tab out of a menu that is the last control in a dialog stays inside the dialog", () => {
  render(
    <Dialog title="Edit seat" onClose={() => {}}>
      <input aria-label="Name" />
      <Menu label="Actions for Software Engineer" items={entries()} />
    </Dialog>,
  );
  trigger().focus();
  fireEvent.click(trigger());
  press("Tab");
  // Focus went back to the trigger, which is the dialog's last control, so
  // the trap wraps the Tab to the first one instead of letting it leave.
  expect(screen.queryByRole("menu")).toBeNull();
  expect(document.activeElement).toBe(screen.getByLabelText("Name"));
});

test("opened from a focused tree item, it returns there rather than to its pointer-only trigger", () => {
  function Card() {
    const [open, setOpen] = useState(false);
    return (
      <div
        role="treeitem"
        aria-selected={false}
        tabIndex={0}
        onKeyDown={(e) => {
          if (e.key === "ContextMenu" || (e.key === "F10" && e.shiftKey)) setOpen(true);
        }}
      >
        Software Engineer
        <Menu
          label="Actions for Software Engineer"
          items={entries()}
          open={open}
          onOpenChange={setOpen}
          triggerTabIndex={-1}
        />
      </div>
    );
  }
  render(<Card />);
  expect(trigger().tabIndex).toBe(-1);
  const card = screen.getByRole("treeitem");
  card.focus();
  fireEvent.keyDown(card, { key: "F10", shiftKey: true });
  expect(document.activeElement).toBe(item("Edit"));
  press("Escape");
  expect(screen.queryByRole("menu")).toBeNull();
  expect(document.activeElement).toBe(card);
});

test("given a layer, it renders there, placed from the trigger's rectangle, and follows a pan", () => {
  const layer = document.createElement("div");
  document.body.appendChild(layer);
  const rect = (left: number, top: number, width: number, height: number) =>
    ({
      left,
      top,
      width,
      height,
      right: left + width,
      bottom: top + height,
      x: left,
      y: top,
    }) as DOMRect;
  layer.getBoundingClientRect = () => rect(100, 50, 600, 400);
  try {
    render(<Menu label="Actions for Software Engineer" items={entries()} layer={layer} />);
    let anchor = rect(150, 80, 24, 24);
    trigger().getBoundingClientRect = () => anchor;
    // A browser refuses focus to an element under `visibility: hidden`, which
    // jsdom does not model, so the menu's style is read at the moment focus
    // arrives.
    let visibilityOnFocus: string | null = null;
    layer.addEventListener("focusin", (e) => {
      visibilityOnFocus = (e.target as HTMLElement).closest<HTMLElement>("[role='menu']")!.style
        .visibility;
    });
    fireEvent.click(trigger());
    const menu = screen.getByRole("menu");
    expect(layer.contains(menu)).toBe(true);
    expect(document.activeElement).toBe(item("Edit"));
    expect(visibilityOnFocus).toBe("");
    // Below the trigger by the gap, lined up with its start, in layer coordinates.
    expect(menu.style.left).toBe("50px");
    expect(menu.style.top).toBe("58px");

    anchor = rect(250, 120, 24, 24);
    act(() => {
      layer.dispatchEvent(new CustomEvent(VIEW_CHANGE_EVENT));
    });
    expect(menu.style.left).toBe("150px");
    expect(menu.style.top).toBe("98px");

    // Panned out of the layer entirely: the menu closes, and focus goes back.
    anchor = rect(2000, 120, 24, 24);
    act(() => {
      layer.dispatchEvent(new CustomEvent(VIEW_CHANGE_EVENT));
    });
    expect(screen.queryByRole("menu")).toBeNull();
    expect(document.activeElement).toBe(trigger());
  } finally {
    cleanup();
    layer.remove();
  }
});

test("keys, presses and clicks inside the menu do not reach the element it opened from", () => {
  const card = { keys: [] as string[], clicks: 0, presses: 0 };
  const onEdit = vi.fn();
  const layer = document.createElement("div");
  document.body.appendChild(layer);
  function Card({ inLayer }: { inLayer: boolean }) {
    return (
      <div
        role="treeitem"
        aria-selected={false}
        tabIndex={0}
        onKeyDown={(e) => card.keys.push(e.key)}
        onClick={() => card.clicks++}
        onPointerDown={() => card.presses++}
      >
        Software Engineer
        <Menu
          label="Actions for Software Engineer"
          items={entries({ onEdit })}
          layer={inLayer ? layer : undefined}
        />
      </div>
    );
  }
  try {
    // Both routes a menu reaches its card by: the document (inline) and the
    // component tree through a portal (in a layer).
    for (const inLayer of [false, true]) {
      card.keys = [];
      card.clicks = 0;
      card.presses = 0;
      const { unmount } = render(<Card inLayer={inLayer} />);
      fireEvent.click(trigger());
      card.clicks = 0;
      for (const key of ["ArrowDown", "Enter", " ", "Delete", "Backspace", "m"]) {
        fireEvent.keyDown(item("Move to"), { key });
      }
      fireEvent.pointerDown(item("Edit"));
      fireEvent.click(item("Edit"));
      expect(onEdit).toHaveBeenCalled();
      expect({ inLayer, ...card }).toEqual({ inLayer, keys: [], clicks: 0, presses: 0 });

      // Escape and Tab still travel: the layer stack acts on them at the document.
      fireEvent.click(trigger());
      card.clicks = 0;
      fireEvent.keyDown(item("Edit"), { key: "Tab" });
      expect(card.keys).toEqual(["Tab"]);
      unmount();
      onEdit.mockClear();
    }
  } finally {
    cleanup();
    layer.remove();
  }
});
