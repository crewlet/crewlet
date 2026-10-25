/**
 * Renaming something in place, from its menu: an artifact's title or a
 * folder's name, one field, Save and Cancel.
 *
 * ONE DIALOG FOR BOTH, told what the name is called, what the engine will not
 * take, and which write sends it, so an artifact and a folder are renamed the
 * same way and say a wrong name the same way: under the field, before Save,
 * and in the engine's own words when it refuses one this let through.
 */

import { useState } from "react";
import { Button, FormField, Input, Modal, useToast } from "@crewlethq/ui";
import { useKeyedGesture, type KeyedRequest } from "~/lib/keyedWrite.ts";

export function RenameDialog({
  label,
  initial,
  problem,
  request,
  onClose,
  onRenamed,
}: {
  /** What the name is called in the field: "Title", "Name". */
  label: string;
  initial: string;
  /** What is wrong with a value, in the sentence the field says under it, or undefined. */
  problem: (value: string) => string | undefined;
  /** The write that renames it to `value` (trimmed). */
  request: (value: string) => Omit<KeyedRequest, "key">;
  onClose: () => void;
  /** It was renamed: the caller's listing moved. */
  onRenamed: (value: string) => void;
}) {
  const [value, setValue] = useState(initial);
  const gesture = useKeyedGesture();
  const toast = useToast();
  const trimmed = value.trim();
  const wrong = problem(trimmed);
  const save = async () => {
    if (wrong || trimmed === initial) return onClose();
    const answer = await gesture.run(request(trimmed));
    if (answer?.kind === "done") {
      toast.ok("Renamed");
      onClose();
      onRenamed(trimmed);
    }
  };
  return (
    <Modal
      open
      onClose={onClose}
      title="Rename"
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="primary"
            loading={gesture.busy}
            disabledReason={wrong}
            onClick={() => void save()}
          >
            Save
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <FormField label={label} error={value === initial ? undefined : wrong}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              width="full"
              autoFocus
              value={value}
              onChange={(e) => setValue(e.target.value)}
            />
          )}
        </FormField>
      </form>
      {gesture.answer && gesture.answer.kind !== "done" && (
        <p className="artifact-refusal">{gesture.answer.text}</p>
      )}
    </Modal>
  );
}
