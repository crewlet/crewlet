/**
 * The layer stack: one Escape closes one surface, and focus never walks out.
 *
 * Every case here was a real way for a modal to misbehave before the stack
 * existed: each dialog listened for Escape on its own, so a prompt over
 * another modal took both with it; nothing trapped Tab; and a dialog whose
 * first field took `autoFocus` returned focus to nowhere when it closed.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState, type ReactNode } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { Dialog } from "./Dialog.tsx";
import { usePopup } from "./useModal.ts";

afterEach(cleanup);

function press(key: string, init: Partial<KeyboardEventInit> = {}): boolean {
  const target = document.activeElement ?? document.body;
  return fireEvent.keyDown(target, { key, ...init });
}

/** A dialog with a button that opens a second one over it. */
function Stacked({ inner }: { inner?: ReactNode }) {
  const [outer, setOuter] = useState(true);
  const [prompt, setPrompt] = useState(false);
  return (
    <>
      <button>page</button>
      {outer && (
        <Dialog title="Editor" onClose={() => setOuter(false)}>
          <button onClick={() => setPrompt(true)}>Ask</button>
          {inner}
        </Dialog>
      )}
      {prompt && (
        <Dialog title="Discard changes?" onClose={() => setPrompt(false)}>
          <button>Keep editing</button>
        </Dialog>
      )}
    </>
  );
}

test("Escape closes only the topmost modal", () => {
  render(<Stacked />);
  fireEvent.click(screen.getByRole("button", { name: "Ask" }));
  expect(screen.getByRole("dialog", { name: "Discard changes?" })).toBeDefined();

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Discard changes?" })).toBeNull();
  expect(screen.getByRole("dialog", { name: "Editor" })).toBeDefined();

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Editor" })).toBeNull();
});

test("a modal that cannot close right now still owns Escape", () => {
  function Busy() {
    const [outer, setOuter] = useState(true);
    return (
      <>
        {outer && (
          <Dialog title="Editor" onClose={() => setOuter(false)}>
            <button>field</button>
          </Dialog>
        )}
        <Dialog title="Saving" onClose={() => {}} dismissable={false}>
          <button>Saving</button>
        </Dialog>
      </>
    );
  }
  render(<Busy />);
  press("Escape");
  // Neither closes: the busy one refuses, and the one beneath is not reached.
  expect(screen.getByRole("dialog", { name: "Saving" })).toBeDefined();
  expect(screen.getByRole("dialog", { name: "Editor" })).toBeDefined();
});

test("a dialog mounted inside another in the same render still sits above it", () => {
  function Nested() {
    const [outer, setOuter] = useState(true);
    const [inner, setInner] = useState(true);
    return outer ? (
      <Dialog title="Outer" onClose={() => setOuter(false)}>
        <button>outer control</button>
        {inner && (
          <Dialog title="Inner" onClose={() => setInner(false)}>
            <button>inner control</button>
          </Dialog>
        )}
      </Dialog>
    ) : null;
  }
  render(<Nested />);
  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Inner" })).toBeNull();
  expect(screen.getByRole("dialog", { name: "Outer" })).toBeDefined();
});

test("Tab wraps inside the modal in both directions, and comes back in from outside", () => {
  render(
    <>
      <button>behind the veil</button>
      <Dialog title="Form" onClose={() => {}}>
        <input aria-label="first" />
        <button disabled>disabled</button>
        <button>last</button>
      </Dialog>
    </>,
  );
  const first = screen.getByLabelText("first");
  const last = screen.getByRole("button", { name: "last" });
  // Focus went in on open, to the first control that can take it.
  expect(document.activeElement).toBe(first);

  last.focus();
  press("Tab");
  expect(document.activeElement).toBe(first);

  press("Tab", { shiftKey: true });
  expect(document.activeElement).toBe(last);

  screen.getByRole("button", { name: "behind the veil" }).focus();
  press("Tab");
  expect(document.activeElement).toBe(first);
});

test("a press on the veil closes the modal, and a press inside it does not", () => {
  function One() {
    const [open, setOpen] = useState(true);
    return open ? (
      <Dialog title="Veiled" onClose={() => setOpen(false)}>
        <button>inside</button>
      </Dialog>
    ) : null;
  }
  const { container } = render(<One />);
  const inside = screen.getByRole("button", { name: "inside" });
  fireEvent.pointerDown(inside);
  fireEvent.click(inside);
  expect(screen.getByRole("dialog", { name: "Veiled" })).toBeDefined();

  // A press that starts inside and is released on the veil (a text selection
  // dragged past the edge) is not a press on the veil.
  const veil = container.querySelector(".veil")!;
  fireEvent.pointerDown(inside);
  fireEvent.click(veil);
  expect(screen.getByRole("dialog", { name: "Veiled" })).toBeDefined();

  fireEvent.pointerDown(veil);
  fireEvent.click(veil);
  expect(screen.queryByRole("dialog", { name: "Veiled" })).toBeNull();
});

test("the veil stays until its press's click, so a tap never clicks what it was covering", () => {
  const behind = vi.fn();
  function Covering() {
    const [open, setOpen] = useState(true);
    return (
      <>
        <button onClick={behind}>Delete</button>
        {open && (
          <Dialog title="Veiled" onClose={() => setOpen(false)}>
            <button>inside</button>
          </Dialog>
        )}
      </>
    );
  }
  const { container } = render(<Covering />);
  const veil = container.querySelector(".veil")!;
  // A browser hit-tests a tap's click after the finger lifts. Were the veil
  // gone on pointerdown, that click would reach the button beneath it.
  fireEvent.pointerDown(veil);
  expect(veil.isConnected).toBe(true);
  expect(screen.getByRole("dialog", { name: "Veiled" })).toBeDefined();
  fireEvent.click(veil);
  expect(screen.queryByRole("dialog", { name: "Veiled" })).toBeNull();
  expect(behind).not.toHaveBeenCalled();
});

test("a busy modal ignores its veil's click as well as its Escape", () => {
  const onClose = vi.fn();
  render(
    <Dialog title="Saving" onClose={onClose} dismissable={false}>
      <button>Saving</button>
    </Dialog>,
  );
  const veil = document.querySelector(".veil")!;
  fireEvent.pointerDown(veil);
  fireEvent.click(veil);
  press("Escape");
  expect(onClose).not.toHaveBeenCalled();
});

test("Tab from a focused element that is not a tab stop, past the last one, wraps inside", () => {
  render(
    <Dialog title="Move to" onClose={() => {}}>
      <button>first</button>
      <button>last</button>
      <div role="treeitem" aria-selected={false} tabIndex={-1}>
        Engineering
      </div>
    </Dialog>,
  );
  const item = screen.getByRole("treeitem");
  item.focus();
  press("Tab");
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "first" }));

  item.focus();
  // Backwards there IS a stop before it, so the browser's own Shift+Tab is left alone.
  expect(press("Tab", { shiftKey: true })).toBe(true);
});

test("focus returns to the opener even when a field in the dialog took autoFocus", () => {
  function Opener() {
    const [open, setOpen] = useState(false);
    return (
      <>
        <button onClick={() => setOpen(true)}>Store a secret</button>
        {open && (
          <Dialog title="Store" onClose={() => setOpen(false)}>
            <button>before</button>
            <input aria-label="Name" autoFocus />
          </Dialog>
        )}
      </>
    );
  }
  render(<Opener />);
  const opener = screen.getByRole("button", { name: "Store a secret" });
  opener.focus();
  fireEvent.click(opener);
  // autoFocus is honoured rather than overridden by "the first control".
  expect(document.activeElement).toBe(screen.getByLabelText("Name"));

  press("Escape");
  expect(document.activeElement).toBe(opener);
});

test("a modal opened as the modal around its opener closes returns focus where that one would have", () => {
  // A status panel's "Set token" closes the panel and opens the credential
  // dialog in one click. The button that opened the dialog is gone by the
  // time the dialog closes, and focus restored to it would fall to the body.
  function Handover() {
    const [panel, setPanel] = useState(false);
    const [token, setToken] = useState(false);
    return (
      <>
        <button onClick={() => setPanel(true)}>engine</button>
        {panel && (
          <Dialog title="Engine" onClose={() => setPanel(false)}>
            <button
              onClick={() => {
                setPanel(false);
                setToken(true);
              }}
            >
              Set token
            </button>
          </Dialog>
        )}
        {token && (
          <Dialog title="API token" onClose={() => setToken(false)}>
            <input aria-label="Token" />
          </Dialog>
        )}
      </>
    );
  }
  render(<Handover />);
  const opener = screen.getByRole("button", { name: "engine" });
  opener.focus();
  fireEvent.click(opener);
  const setToken = screen.getByRole("button", { name: "Set token" });
  expect(document.activeElement).toBe(setToken);

  fireEvent.click(setToken);
  expect(screen.queryByRole("dialog", { name: "Engine" })).toBeNull();
  expect(document.activeElement).toBe(screen.getByLabelText("Token"));

  press("Escape");
  expect(document.activeElement).toBe(opener);
});

test("when the opener has gone from a modal that is still open, focus goes to that modal, not behind it", () => {
  function Removed() {
    const [row, setRow] = useState(true);
    const [confirm, setConfirm] = useState(false);
    return (
      <>
        <button>behind the veil</button>
        <Dialog title="Editor" onClose={() => {}}>
          <button>first</button>
          {row && <button onClick={() => setConfirm(true)}>Delete row</button>}
        </Dialog>
        {confirm && (
          <Dialog
            title="Delete this row?"
            onClose={() => {
              setRow(false);
              setConfirm(false);
            }}
          >
            <button>Delete</button>
          </Dialog>
        )}
      </>
    );
  }
  render(<Removed />);
  screen.getByRole("button", { name: "Delete row" }).focus();
  fireEvent.click(screen.getByRole("button", { name: "Delete row" }));
  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Delete this row?" })).toBeNull();
  expect(document.activeElement).toBe(screen.getByRole("dialog", { name: "Editor" }));
});

/** A minimal popup on the stack, rendered inside a dialog the way a menu is. */
function Popup() {
  const [open, setOpen] = useState(true);
  const popup = usePopup({ open, onDismiss: () => setOpen(false) });
  return (
    <>
      <button ref={popup.insideRef} onClick={() => setOpen((v) => !v)}>
        toggle
      </button>
      {open && (
        <ul ref={popup.panelRef} role="menu" aria-label="Actions">
          <li role="menuitem">Edit</li>
        </ul>
      )}
    </>
  );
}

function PopupInDialog({ onDialogClose }: { onDialogClose: () => void }) {
  return (
    <Dialog title="Host" onClose={onDialogClose}>
      <Popup />
    </Dialog>
  );
}

test("an open popup inside a modal closes before the modal on Escape", () => {
  let closed = 0;
  render(<PopupInDialog onDialogClose={() => closed++} />);
  expect(screen.getByRole("menu")).toBeDefined();

  press("Escape");
  expect(screen.queryByRole("menu")).toBeNull();
  expect(closed).toBe(0);

  press("Escape");
  expect(closed).toBe(1);
});

test("a press on the veil dismisses the popup above the modal, not the modal", () => {
  let closed = 0;
  const { container } = render(<PopupInDialog onDialogClose={() => closed++} />);
  const veil = container.querySelector(".veil")!;
  fireEvent.pointerDown(veil);
  fireEvent.click(veil);
  expect(screen.queryByRole("menu")).toBeNull();
  expect(closed).toBe(0);
});

test("the element that toggles a popup is not outside it", () => {
  render(<PopupInDialog onDialogClose={() => {}} />);
  const toggle = screen.getByRole("button", { name: "toggle" });
  // A press on the toggle must not close the popup on pointerdown only for
  // the click to open it again.
  fireEvent.pointerDown(toggle);
  expect(screen.getByRole("menu")).toBeDefined();
  act(() => toggle.click());
  expect(screen.queryByRole("menu")).toBeNull();
});

test("a control that consumes Escape keeps it", () => {
  let closed = 0;
  render(
    <Dialog title="Completion" onClose={() => closed++}>
      <input
        aria-label="value"
        onKeyDown={(e) => {
          if (e.key === "Escape") e.stopPropagation();
        }}
      />
    </Dialog>,
  );
  fireEvent.keyDown(screen.getByLabelText("value"), { key: "Escape" });
  expect(closed).toBe(0);
});
