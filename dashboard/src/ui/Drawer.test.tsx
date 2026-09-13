/**
 * The drawer is a modal on the shared stack, not a second set of focus rules.
 *
 * The node editor is a drawer and its unsaved-changes prompt is a dialog over
 * it, so the case that matters most is the pair: one Escape must close the
 * prompt and leave the editor, with its edits, exactly where it was.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, test } from "vitest";
import { Dialog } from "./Dialog.tsx";
import { Drawer } from "./Drawer.tsx";

afterEach(cleanup);

function press(key: string, init: Partial<KeyboardEventInit> = {}) {
  fireEvent.keyDown(document.activeElement ?? document.body, { key, ...init });
}

function Editor() {
  const [drawer, setDrawer] = useState(true);
  const [prompt, setPrompt] = useState(false);
  return (
    <>
      {drawer && (
        <Drawer title="Edit Software Engineer" onClose={() => setDrawer(false)}>
          <input aria-label="Name" />
          <button onClick={() => setPrompt(true)}>Discard</button>
        </Drawer>
      )}
      {prompt && (
        <Dialog title="Discard your edits?" onClose={() => setPrompt(false)}>
          <button>Keep editing</button>
        </Dialog>
      )}
    </>
  );
}

test("Escape with a dialog over a drawer closes only the dialog", () => {
  render(<Editor />);
  fireEvent.click(screen.getByRole("button", { name: "Discard" }));
  expect(screen.getByRole("dialog", { name: "Discard your edits?" })).toBeDefined();

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Discard your edits?" })).toBeNull();
  expect(screen.getByRole("dialog", { name: "Edit Software Engineer" })).toBeDefined();

  press("Escape");
  expect(screen.queryByRole("dialog", { name: "Edit Software Engineer" })).toBeNull();
});

test("focus starts in the body rather than on Close, and Tab wraps through Close", () => {
  render(<Editor />);
  const name = screen.getByLabelText("Name");
  const close = screen.getByRole("button", { name: "Close" });
  const discard = screen.getByRole("button", { name: "Discard" });
  expect(document.activeElement).toBe(name);

  discard.focus();
  press("Tab");
  expect(document.activeElement).toBe(close);
  press("Tab", { shiftKey: true });
  expect(document.activeElement).toBe(discard);
});

test("a drawer mid-write refuses every way out", () => {
  let closed = 0;
  const { container } = render(
    <Drawer title="Saving" onClose={() => closed++} dismissable={false}>
      <input aria-label="Name" />
    </Drawer>,
  );
  press("Escape");
  fireEvent.pointerDown(container.querySelector(".veil")!);
  const close = screen.getByRole("button", { name: "Close" }) as HTMLButtonElement;
  expect(close.disabled).toBe(true);
  expect(closed).toBe(0);
});

test("the sheet is a labelled modal dialog, and a form when it submits", () => {
  let submitted = 0;
  render(
    <Drawer title="Edit unit" onClose={() => {}} onSubmit={() => submitted++}>
      <input aria-label="Name" />
    </Drawer>,
  );
  const sheet = screen.getByRole("dialog", { name: "Edit unit" });
  expect(sheet.getAttribute("aria-modal")).toBe("true");
  expect(sheet.tagName).toBe("FORM");
  fireEvent.submit(sheet);
  expect(submitted).toBe(1);
});
