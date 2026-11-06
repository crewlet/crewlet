/**
 * A dialog a control opens is drawn where the screen presents it: outside the
 * layer host the control sits in (a TreeGrid's, around a table's rows), so a
 * kit Modal opens over the window rather than inside the table's box.
 */

import { useState, type ReactNode } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { LayerHost, Modal } from "@crewlethq/ui";
import { DialogPresenter, usePresentedDialog } from "./dialogPresenter.tsx";

afterEach(cleanup);

/** A control that opens a kit Modal through its dialog. */
function Opener() {
  const [dialog, show] = usePresentedDialog();
  return (
    <>
      <button
        type="button"
        onClick={() =>
          show(
            <Modal open onClose={() => show(null)} title="Raise the ceiling">
              Body
            </Modal>,
          )
        }
      >
        Open
      </button>
      {dialog}
    </>
  );
}

/** A screen that presents its controls' dialogs from outside the host they sit in. */
function Screen() {
  const [presented, setPresented] = useState<ReactNode | null>(null);
  return (
    <>
      <DialogPresenter value={setPresented}>
        <LayerHost className="grid-host">
          <Opener />
        </LayerHost>
      </DialogPresenter>
      {presented}
    </>
  );
}

// PRESENTED, the dialog leaves the host and closes from there. Mutation: draw
// it in place whatever the context says, and it opens inside the host.
test("a presented dialog opens outside the layer host its control sits in, and closes", () => {
  render(<Screen />);
  fireEvent.click(screen.getByRole("button", { name: "Open" }));
  const dialog = screen.getByRole("dialog", { name: "Raise the ceiling" });
  expect(dialog.closest(".grid-host")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: /Close/ }));
  expect(screen.queryByRole("dialog")).toBeNull();
});

// WITHOUT A PRESENTER a control draws its dialog in place, as everywhere else.
test("with no presenter a dialog opens in place", () => {
  render(
    <LayerHost className="grid-host">
      <Opener />
    </LayerHost>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Open" }));
  expect(screen.getByRole("dialog").closest(".grid-host")).not.toBeNull();
});
