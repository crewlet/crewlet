/**
 * Leaving a create page with something made: a draft kept or discarded, or
 * words not yet saved anywhere given up, once the person says so.
 */

import { Button, Modal } from "@crewlethq/ui";
import { ARTIFACT_TRASH_RETENTION_DAYS } from "~/contract/artifacts.ts";

/** Leaving with something made: a draft kept or discarded, or unsaved text lost. */
export function LeaveDialog({
  title,
  hasDraft,
  discarding,
  onStay,
  onLeave,
  onDiscard,
}: {
  title: string;
  hasDraft: boolean;
  discarding: boolean;
  onStay: () => void;
  onLeave: () => void;
  onDiscard: () => void;
}) {
  return (
    <Modal
      open
      onClose={onStay}
      size="sm"
      title={hasDraft ? "Keep this draft?" : "Leave without saving?"}
      footer={
        hasDraft ? (
          <>
            <Button size="small" variant="ghost" onClick={onStay}>
              Stay
            </Button>
            <Button size="small" variant="danger" loading={discarding} onClick={onDiscard}>
              Discard draft
            </Button>
            <Button size="small" variant="primary" onClick={onLeave}>
              Keep draft
            </Button>
          </>
        ) : (
          <>
            <Button size="small" variant="ghost" onClick={onStay}>
              Stay
            </Button>
            <Button size="small" variant="danger" onClick={onLeave}>
              Leave
            </Button>
          </>
        )
      }
    >
      <p className="html-create-leave">
        {hasDraft
          ? `“${title || "Untitled page"}” is saved as a private draft only you can see. Keep it to find it under Mine in Artifacts, or discard it to the trash, where it is kept for ${ARTIFACT_TRASH_RETENTION_DAYS} days.`
          : "Nothing has been saved yet, so what was written here will be lost."}
      </p>
    </Modal>
  );
}
