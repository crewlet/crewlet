/**
 * A repository's branches, written: one made at a revision
 * (`code_create_branch`) and one deleted (`code_delete_branch`), each an
 * engine write Gitlet builds on the repository's home node and lands as one
 * push.
 *
 * A DELETE NAMES THE HEAD THE LISTING SHOWED (`expected_head`), so a branch
 * somebody pushed to since is refused rather than deleted unseen, and is
 * confirmed in the page: its commits stay in the history, and a change that
 * proposes it loses its source. Both are drawn for every reader and held with
 * the reason (`lib/repoWrites.ts`): a repository that is archived, deleted,
 * importing or a pull mirror takes no push, a new branch takes write, and a
 * delete takes maintain.
 */

import { useState } from "react";
import { Button, ConfirmModal, FormField, Input, Stack } from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { refName, shortOID } from "~/lib/code.ts";
import { branchDeleteHeld, branchFormHeld, branchWriteHeld } from "~/lib/repoWrites.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeRefRow, CodeRepoDetail } from "~/protocol/index.ts";

/** "New branch": the Branches tab's button and its dialog. */
export function NewBranchButton({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_create_branch");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  const held =
    branchWriteHeld(repo, viewer) ??
    (repo.head
      ? undefined
      : `${repo.name} holds no commit yet: its first branch arrives with its first push, or its first file added on the Files tab.`);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="primary"
        blocked={held}
        onPress={() => setOpen(true)}
      >
        New branch
      </WriteButton>
      {open && <NewBranchDialog repo={repo} onClose={() => setOpen(false)} />}
    </>
  );
}

function NewBranchDialog({ repo, onClose }: { repo: CodeRepoDetail; onClose: () => void }) {
  const write = useAct("code_create_branch");
  const [name, setName] = useState("");
  const [at, setAt] = useState("");
  const blocked = branchFormHeld(name);
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const branch = name.trim();
    const result = await write.run(
      { repo: repo.name, branch, at: at.trim() || repo.default_branch },
      { done: `Created ${branch}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`New branch of ${repo.name}`}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            blocked={blocked}
            onPress={() => void submit()}
          >
            Create branch
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        <FormField label="Name">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              spellCheck={false}
              placeholder="fix/login"
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          )}
        </FormField>
        <FormField
          label="Starts at"
          optional
          helper={`A branch, a tag or a commit. ${repo.default_branch} when left empty.`}
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              spellCheck={false}
              placeholder={repo.default_branch}
              value={at}
              onChange={(event) => setAt(event.target.value)}
            />
          )}
        </FormField>
      </Stack>
    </Modal>
  );
}

/** One branch's Delete, confirmed in the page. */
export function DeleteBranchButton({ repo, row }: { repo: CodeRepoDetail; row: CodeRefRow }) {
  const write = useAct("code_delete_branch");
  const viewer = useViewer();
  const [confirming, setConfirming] = useState(false);
  const name = refName(row.ref).name;
  const isDefault = name === repo.default_branch;
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        blocked={branchDeleteHeld(repo, viewer)}
        aria-label={`Delete ${name}`}
        onPress={() => setConfirming(true)}
      >
        Delete
      </WriteButton>
      <ConfirmModal
        open={confirming}
        destructive
        title={`Delete the branch ${name}?`}
        message={
          `${name} is deleted at ${row.oid ? shortOID(row.oid) : "its head"}, the head listed here: a push made to it since is refused rather than lost. Its commits stay in the repository's history, and a change that proposes it loses its source.` +
          (isDefault
            ? ` It is ${repo.name}'s default branch, which a clone checks out and a change is proposed into.`
            : "")
        }
        cancelLabel="Keep it"
        confirmLabel="Delete branch"
        onClose={() => setConfirming(false)}
        submitting={write.busy}
        busyReason="Waiting for the engine to answer"
        onConfirm={async () => {
          await write.run(
            { repo: repo.name, branch: name, expected_head: row.oid ?? "" },
            { done: `Deleted ${name}` },
          );
          setConfirming(false);
        }}
      />
    </>
  );
}
