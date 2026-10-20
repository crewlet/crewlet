/**
 * Leaving a create page with something made: a draft kept or discarded, or
 * words not yet saved anywhere given up, once the person says so.
 *
 * THE SAME PROMPT AS EVERY CREATE PAGE'S: unsaved words are the shared
 * `ConfirmModal` ("Discard this page?", Keep editing or Discard), and a draft
 * adds its third choice in the same shape, so leaving reads alike whichever
 * kind was being made.
 */

import { useId } from "react";
import { Button, ConfirmModal, Modal } from "@crewlethq/ui";
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
  const messageId = useId();
  if (!hasDraft) {
    return (
      <ConfirmModal
        open
        destructive
        title="Discard this page?"
        message="What you wrote is not saved anywhere."
        cancelLabel="Keep editing"
        confirmLabel="Discard"
        onClose={onStay}
        onConfirm={onLeave}
      />
    );
  }
  return (
    <Modal
      open
      onClose={onStay}
      size="sm"
      shape="prompt"
      role="alertdialog"
      describedBy={messageId}
      dismissable={!discarding}
      stackBody
      title="Keep this draft?"
      footer={
        <>
          <Button variant="ghost" onClick={onStay}>
            Keep editing
          </Button>
          <Button variant="danger" loading={discarding} onClick={onDiscard}>
            Discard draft
          </Button>
          <Button variant="primary" onClick={onLeave}>
            Keep draft
          </Button>
        </>
      }
    >
      <p className="html-create-leave" id={messageId}>
        {`“${title || "Untitled page"}” is saved as a private draft only you can see. Keep it to find it under Mine in Artifacts, or discard it to the trash, where it is kept for ${ARTIFACT_TRASH_RETENTION_DAYS} days.`}
      </p>
    </Modal>
  );
}
