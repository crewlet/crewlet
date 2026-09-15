/**
 * What a modal does, whatever it looks like.
 *
 * Two presentations sit on this: the dialog with its title bar, and the
 * command palette, whose header is its own input and which therefore
 * hand-rolled the veil rather than grow a title it did not want. That copy
 * had the LOOK of a modal and none of the behaviour — focus moved in but was
 * never returned, and Escape was bound to the input alone, so it did nothing
 * the moment the reader arrowed into the results beside it.
 *
 * Both failures are silent to anybody using a mouse, which is why they are
 * tested rather than reviewed.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Dialog, useModal } from "./Dialog.tsx";

afterEach(cleanup);

/** A modal with no title bar — the palette's shape, minimally. */
function Bare({ onClose }: { onClose: () => void }) {
  const { veil, shell } = useModal({ label: "Search", onClose });
  return (
    <div {...veil}>
      <div {...shell} className="palette">
        <input aria-label="Search" />
        <button>A result</button>
      </div>
    </div>
  );
}

test("escape closes from anywhere inside, not only from the first control", () => {
  const onClose = vi.fn();
  render(<Bare onClose={onClose} />);
  // FROM THE RESULT, which is where a reader arrowing down actually is. A
  // handler on the input alone passes a test that presses escape on the
  // input and fails every real reader.
  fireEvent.keyDown(screen.getByText("A result"), { key: "Escape" });
  expect(onClose).toHaveBeenCalled();
});

test("focus moves to the first control on open", () => {
  render(<Bare onClose={() => {}} />);
  expect(document.activeElement).toBe(screen.getByRole("textbox"));
});

test("focus returns to whatever opened it", () => {
  const opener = document.createElement("button");
  document.body.appendChild(opener);
  opener.focus();
  const view = render(<Bare onClose={() => {}} />);
  expect(document.activeElement).not.toBe(opener);
  view.unmount();
  // NOT THE DOCUMENT BODY. A modal that returns focus there leaves a keyboard
  // reader at the top of the page, which on this dashboard is the rail rather
  // than the row they opened.
  expect(document.activeElement).toBe(opener);
  opener.remove();
});

test("a click on the veil closes, and one inside does not", () => {
  const onClose = vi.fn();
  const { container } = render(<Bare onClose={onClose} />);
  fireEvent.mouseDown(screen.getByText("A result"));
  expect(onClose).not.toHaveBeenCalled();
  fireEvent.mouseDown(container.querySelector(".veil") as Element);
  expect(onClose).toHaveBeenCalledTimes(1);
});

test("a dialog mid-write cannot be dismissed by a stray click or key", () => {
  // The one case where closing is not the caller's to allow: a request whose
  // outcome the operator has not seen yet.
  const onClose = vi.fn();
  const { container } = render(
    <Dialog title="Rotating" onClose={onClose} dismissable={false}>
      <p>Working…</p>
    </Dialog>,
  );
  fireEvent.mouseDown(container.querySelector(".veil") as Element);
  fireEvent.keyDown(screen.getByText("Working…"), { key: "Escape" });
  expect(onClose).not.toHaveBeenCalled();
});

test("the dialog keeps its title bar, and the bare shell has none", () => {
  const { container } = render(
    <Dialog title="Set a token" onClose={() => {}}>
      <p>Body</p>
    </Dialog>,
  );
  expect(container.querySelector(".dialog-head")).not.toBeNull();
  expect(screen.getByRole("dialog").getAttribute("aria-label")).toBe("Set a token");
  cleanup();
  const bare = render(<Bare onClose={() => {}} />);
  expect(bare.container.querySelector(".dialog-head")).toBeNull();
  expect(screen.getByRole("dialog").getAttribute("aria-label")).toBe("Search");
});
