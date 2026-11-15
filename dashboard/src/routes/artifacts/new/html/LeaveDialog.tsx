/**
 * Leaving a create page with something made: a draft discarded or kept, or
 * words not yet saved anywhere given up, once the person says so.
 *
 * THE SAME PROMPT AS EVERY CREATE PAGE'S: unsaved words are the shared
 * `ConfirmModal` ("Discard this page?", Keep editing or Discard), and a draft
 * adds its third choice in the same shape, so leaving reads alike whichever
 * kind was being made.
 *
 * LEAVING DISCARDS, KEEPING IS CHOSEN. The person is leaving a page they
 * never pressed Create on, so the question is "Discard this draft?" and its
 * primary answer moves the draft to the trash; keeping it is the second
 * answer, named for where it will be found. A prompt whose primary kept the
 * draft left an artifact behind for every person who pressed Cancel.
 */

import { useId } from "react";
import { Button, ConfirmModal } from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
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
        message="What you wrote here is not saved anywhere yet."
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
      title="Discard this draft?"
      footer={
        <>
          <Button variant="ghost" disabled={discarding} onClick={onStay}>
            Keep editing
          </Button>
          <Button variant="secondary" disabled={discarding} onClick={onLeave}>
            Keep draft
          </Button>
          <Button variant="danger" loading={discarding} onClick={onDiscard}>
            Discard draft
          </Button>
        </>
      }
    >
      <p className="html-create-leave" id={messageId}>
        {`“${title || "Untitled page"}” was saved as a private draft only you can see, to show it here. Discard it to the trash, where it is kept for ${ARTIFACT_TRASH_RETENTION_DAYS} days, or keep it to find it under Yours in Artifacts.`}
      </p>
    </Modal>
  );
}
