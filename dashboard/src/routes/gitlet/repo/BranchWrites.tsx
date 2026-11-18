/**
 * A repository's branches, written: one made at a revision
 * (`code_create_branch`) and one deleted (`code_delete_branch`), each an
 * engine write Gitlet builds on the repository's home node and lands as one
 * push.
 *
 * A DELETE NAMES THE HEAD THE LISTING SHOWED (`expected_head`), so a branch
 * somebody pushed to since is refused rather than deleted unseen, and is
 * confirmed in the page: its commits stay in the history, and a change that
 * proposes it loses its source. Its toast's Undo makes it again at that
 * head. Both are drawn for every reader and held with the reason
 * (`lib/repoWrites.ts`): a repository that is archived, deleted, importing or
 * a pull mirror takes no push, a new branch takes write, and a delete takes
 * maintain.
 *
 * THE DEFAULT BRANCH HAS NO DELETE. The engine refuses its deletion whoever
 * asks (`default_branch`): every clone checks it out, and a repository whose
 * default branch is gone reads as empty while it holds every other branch. It
 * is let go by making another branch the default first, on the Settings tab,
 * after which its row offers Delete like any other.
 * A listing read in detail also holds Delete with the engine's own sentence.
 *
 * A NEW BRANCH STARTS AT ANY BRANCH, so it is held only for a repository that
 * holds none. One whose default branch holds nothing while others are live
 * names where it starts, since the default branch it would start at is empty.
 */

import { useState } from "react";
import { Button, FormField, Input, Stack, VisuallyHidden } from "@crewlethq/ui";
import { TrashGlyph } from "@crewlethq/icons/glyphs";
import { ConfirmModal, Modal } from "~/components/Modal.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { refName, shortOID } from "~/lib/code.ts";
import { branchDeleteHeld, branchFormHeld, branchWriteHeld } from "~/lib/repoWrites.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeRefListing, CodeRepoDetail } from "~/protocol/index.ts";

/** "New branch": the Branches tab's button and its dialog. */
export function NewBranchButton({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_create_branch");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  const held =
    branchWriteHeld(repo, viewer) ??
    (repo.branches > 0
      ? undefined
      : `${repo.name} holds no branch yet: its first arrives with its first push, or with its first file added on the Code tab.`);
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
  // WHERE IT STARTS, which the empty default branch cannot be.
  const startsAtDefault = Boolean(repo.head);
  const blocked =
    branchFormHeld(name) ??
    (startsAtDefault || at.trim() !== ""
      ? undefined
      : `Name where it starts: ${repo.default_branch}, the default branch, holds no commit.`);
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
          optional={startsAtDefault}
          helper={
            startsAtDefault
              ? `A branch, a tag or a commit. ${repo.default_branch} when left empty.`
              : `A branch, a tag or a commit: ${repo.default_branch}, the default branch, holds no commit.`
          }
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              spellCheck={false}
              placeholder={startsAtDefault ? repo.default_branch : "release"}
              value={at}
              onChange={(event) => setAt(event.target.value)}
            />
          )}
        </FormField>
      </Stack>
    </Modal>
  );
}

/** A branch's row as Delete reads it: its head, and the engine's sentence where it refuses the delete. */
type DeletedRow = Pick<CodeRefListing, "ref" | "oid"> & { held?: string | undefined };

/**
 * One branch's Delete, confirmed in the page; none on the repository's default
 * branch, which nothing deletes (see the file's doc).
 */
export function DeleteBranchButton({ repo, row }: { repo: CodeRepoDetail; row: DeletedRow }) {
  const name = refName(row.ref).name;
  if (name === repo.default_branch) return null;
  return <DeleteBranch repo={repo} row={row} name={name} />;
}

function DeleteBranch({
  repo,
  row,
  name,
}: {
  repo: CodeRepoDetail;
  row: DeletedRow;
  name: string;
}) {
  const write = useAct("code_delete_branch");
  const restore = useAct("code_create_branch");
  const viewer = useViewer();
  const [confirming, setConfirming] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        blocked={branchDeleteHeld(repo, viewer) ?? row.held}
        aria-label={`Delete ${name}`}
        leadingIcon={<TrashGlyph size="sm" />}
        onPress={() => setConfirming(true)}
      >
        <VisuallyHidden>Delete</VisuallyHidden>
      </WriteButton>
      <ConfirmModal
        open={confirming}
        destructive
        title={`Delete the branch ${name}?`}
        message={`${name} is deleted at ${row.oid ? shortOID(row.oid) : "its head"}, the head listed here: a push made to it since is refused rather than lost. Its commits stay in the repository's history, and a change that proposes it loses its source.`}
        cancelLabel="Keep it"
        confirmLabel="Delete branch"
        onClose={() => setConfirming(false)}
        submitting={write.busy}
        onConfirm={async () => {
          await write.run(
            { repo: repo.name, branch: name, expected_head: row.oid },
            {
              done: `Deleted ${name}`,
              // ITS UNDO MAKES IT AGAIN AT THE HEAD IT WAS DELETED AT.
              undo: () =>
                void restore.run(
                  { repo: repo.name, branch: name, at: row.oid },
                  { done: `Restored ${name} at ${shortOID(row.oid)}` },
                ),
            },
          );
          setConfirming(false);
        }}
      />
    </>
  );
}
