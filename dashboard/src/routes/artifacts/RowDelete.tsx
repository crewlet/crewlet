/**
 * Deleting one row of a sheet: asked for from the grid's row menu and
 * confirmed here, the way every destructive gesture on an artifact is (a
 * dialog that says what goes, Cancel first and focused, the danger button
 * second). The row was deleted from a cell's history dialog, where the delete
 * was the dialog's only control and so had focus on open: Enter deleted a row
 * with no question asked.
 */

import { useState } from "react";
import { Button, Modal } from "@crewlethq/ui";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";

/** The row a reader asked to delete, as the dialog names it. */
export interface RowToDelete {
  id: string;
  /** "Row 4", or the row's own name where it has one. */
  name: string;
}

/** The row-delete gesture: what the grid's `onDeleteRow` sets, and the dialog that confirms it. */
export function useRowDelete(store: ArtifactDataStore, collection: string, canDelete: boolean) {
  const [asked, setAsked] = useState<RowToDelete | null>(null);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const close = () => {
    setAsked(null);
    setRefusal(null);
  };
  const confirm = async () => {
    if (!asked) return;
    setBusy(true);
    try {
      const result = await store.deleteRows(collection, [asked.id]);
      if (result && result.verdict !== "applied" && result.verdict !== "pending") {
        setRefusal(result.reason);
        return;
      }
      close();
    } finally {
      setBusy(false);
    }
  };
  const dialog = (
    <Modal
      open={asked !== null}
      onClose={close}
      dismissable={!busy}
      title={asked ? `Delete ${asked.name}?` : ""}
      size="sm"
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={close} disabled={busy}>
            Cancel
          </Button>
          <Button size="small" variant="danger" loading={busy} onClick={() => void confirm()}>
            Delete row
          </Button>
        </>
      }
    >
      <p>The row is deleted for everyone. Its history keeps what it held for seven days.</p>
      {refusal && <p className="artifact-refusal">{refusal}</p>}
    </Modal>
  );
  return {
    onDeleteRow: canDelete ? (row: RowToDelete) => setAsked(row) : undefined,
    dialog,
  };
}
