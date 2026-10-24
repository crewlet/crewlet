/**
 * Deleting a row is asked, never done on the press that opened a dialog: the
 * dialog opens on Cancel, not on the danger button, and only its confirm
 * deletes.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { Button } from "@crewlethq/ui";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import { useRowDelete } from "./RowDelete.tsx";

afterEach(cleanup);

function Host({ store, canDelete = true }: { store: ArtifactDataStore; canDelete?: boolean }) {
  const remove = useRowDelete(store, "gates", canDelete);
  return (
    <>
      {remove.onDeleteRow && (
        <Button onClick={() => remove.onDeleteRow?.({ id: "r4", name: "row 4" })}>
          Row actions
        </Button>
      )}
      {remove.dialog}
    </>
  );
}

test("asks before it deletes, and opens on Cancel rather than the danger button", async () => {
  const deleteRows = vi.fn(async () => ({ verdict: "applied", seq: 1, position: 1 }));
  render(<Host store={{ deleteRows } as unknown as ArtifactDataStore} />);
  fireEvent.click(screen.getByRole("button", { name: "Row actions" }));
  const dialog = await screen.findByRole("dialog", { name: "Delete row 4?" });
  await waitFor(() => expect(document.activeElement?.textContent).toBe("Cancel"));
  expect(dialog.contains(document.activeElement)).toBe(true);
  expect(deleteRows).not.toHaveBeenCalled();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Delete row" }));
  });
  expect(deleteRows).toHaveBeenCalledWith("gates", ["r4"]);
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

test("a refusal stays in the dialog, in words", async () => {
  const deleteRows = vi.fn(async () => ({
    verdict: "forbidden",
    reason: "Not saved: you cannot change this.",
  }));
  render(<Host store={{ deleteRows } as unknown as ArtifactDataStore} />);
  fireEvent.click(screen.getByRole("button", { name: "Row actions" }));
  await screen.findByRole("dialog", { name: "Delete row 4?" });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Delete row" }));
  });
  expect(await screen.findByText("Not saved: you cannot change this.")).toBeTruthy();
});

test("offers nothing to a reader who cannot delete rows", () => {
  render(<Host store={{} as ArtifactDataStore} canDelete={false} />);
  expect(screen.queryByRole("button", { name: "Row actions" })).toBeNull();
});
