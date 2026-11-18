/**
 * Ask reviewers with a note, from the page bar and the rail's Request. Authors are never
 * offered: an author's review counts for nothing, and the engine refuses asking one.
 */

import { useMemo, useState } from "react";
import { Button, FormField, Stack, Textarea } from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import type { CodeChangeDetail } from "~/protocol/index.ts";
import { ReviewerPicker } from "../ReviewerPicker.tsx";

export function AskDialog({ change, onClose }: { change: CodeChangeDetail; onClose: () => void }) {
  const write = useAct("code_request_review");
  const [reviewers, setReviewers] = useState<string[]>([]);
  const [note, setNote] = useState("");
  const authors = useMemo(() => change.authors.map((a) => a.author), [change.authors]);
  const submit = async () => {
    const result = await write.run(
      { change: change.ref, reviewers, ...(note.trim() ? { note } : {}) },
      { done: `Asked for a review of ${change.ref}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Request review of ${change.ref}`}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            blocked={reviewers.length === 0 ? "Pick a reviewer to ask." : undefined}
            onPress={() => void submit()}
          >
            Ask
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        <FormField label="Reviewers">
          {(field) => (
            <ReviewerPicker
              id={field.id}
              describedBy={field.describedBy}
              value={reviewers}
              onChange={setReviewers}
              exclude={authors}
            />
          )}
        </FormField>
        <FormField label="Note" optional helper="What to look at.">
          {(field) => (
            <Textarea
              id={field.id}
              aria-describedby={field.describedBy}
              value={note}
              rows={3}
              onChange={(event) => setNote(event.target.value)}
            />
          )}
        </FormField>
      </Stack>
    </Modal>
  );
}
