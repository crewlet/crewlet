/**
 * A repository's delete and its restore (`code_delete_repo`,
 * `code_restore_repo`), each one lifecycle request a data node decides.
 *
 * A DELETE IS ASKED ONLY ONCE ITS NAME IS TYPED: the dialog says what goes
 * (its clones and pushes are refused from then on, and its open changes with
 * it) and that it is restorable until its purge, and its press is held until
 * the name matches. The reason the person gives is kept with the delete. A
 * RESTORE brings it back as it was. Both are drawn for every reader and held,
 * with the sentence, where the engine would refuse: a declared repository is
 * the company document's, and either takes maintain (`lib/repoWrites.ts`).
 */

import { useState } from "react";
import {
  Button,
  Callout,
  ConfirmField,
  FormField,
  Input,
  Stack,
  confirmMatches,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { fmtDateTime } from "~/lib/format.ts";
import { deleteHeld, restoreHeld } from "~/lib/repoWrites.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeRepoDetail } from "~/protocol/index.ts";

/** "Delete repository", in the Settings tab's last section. */
export function DeleteRepoControl({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_delete_repo");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="danger"
        blocked={deleteHeld(repo, viewer)}
        onPress={() => setOpen(true)}
      >
        Delete repository
      </WriteButton>
      {open && <DeleteDialog repo={repo} onClose={() => setOpen(false)} />}
    </>
  );
}

function DeleteDialog({ repo, onClose }: { repo: CodeRepoDetail; onClose: () => void }) {
  const write = useAct("code_delete_repo");
  const [typed, setTyped] = useState("");
  const [reason, setReason] = useState("");
  const blocked = confirmMatches(typed, repo.name) ? undefined : "Type its name to confirm.";
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      { repo: repo.name, ...(reason.trim() ? { reason: reason.trim() } : {}) },
      { done: `Deleted ${repo.name}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Delete ${repo.name}?`}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Keep it
          </Button>
          <WriteButton
            write={write}
            variant="danger"
            blocked={blocked}
            onPress={() => void submit()}
          >
            Delete repository
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        <p>
          Nobody clones it, pushes to it or reads it once it is deleted, and its changes stop with
          it. It can be restored as it was until it is purged.
        </p>
        <FormField label="Reason" optional helper="Kept with the delete.">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              value={reason}
              onChange={(event) => setReason(event.target.value)}
            />
          )}
        </FormField>
        <ConfirmField phrase={repo.name} value={typed} onValueChange={setTyped} autoFocus />
      </Stack>
    </Modal>
  );
}

/**
 * A deleted repository's notice, above its tabs: that it is deleted and when,
 * and Restore.
 */
export function DeletedNotice({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_restore_repo");
  const viewer = useViewer();
  return (
    <Callout variant="warning">
      <Stack gap={2}>
        <span>
          {repo.name} is deleted
          {repo.deleted_at ? `, since ${fmtDateTime(repo.deleted_at)}` : ""}: nobody clones it or
          pushes to it. It can be restored as it was until it is purged.
        </span>
        <span>
          <WriteButton
            write={write}
            size="small"
            variant="secondary"
            blocked={restoreHeld(repo, viewer)}
            onPress={() => void write.run({ repo: repo.name }, { done: `Restored ${repo.name}` })}
          >
            Restore repository
          </WriteButton>
        </span>
      </Stack>
    </Callout>
  );
}
