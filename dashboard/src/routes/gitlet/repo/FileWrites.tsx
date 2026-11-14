/**
 * A file committed from the browser (`code_commit_files`): one edited, one
 * added, one deleted, each ONE commit on the branch the reader is looking at,
 * built on the repository's home Gitlet node and landed as one push.
 *
 * # At the head the file was read at
 *
 * Every commit names the commit the branch resolved to when the page read it
 * (`expected_head`), so a push that landed since is refused rather than
 * written over, and the reader looks again. Only a BRANCH takes a commit: at
 * a tag or a commit the controls are drawn and held, saying to choose a
 * branch.
 *
 * # The whole file, read at that commit
 *
 * The editor opens on the file's every page read at the commit, joined (the
 * pages concatenate to its bytes), never on the lines the viewer happens to
 * have loaded. It takes a text file of at most [EDIT_MAX_BYTES]: a larger or
 * a binary one is committed from a clone, and the control says so.
 *
 * # The first file of an empty repository
 *
 * A repository nobody has pushed to holds no branch, and its first file is
 * its first commit: a root commit that makes its default branch, sent with
 * no `expected_head` (nothing was read) and no `base` (nothing precedes it),
 * so a repository made in the dashboard is filled there too. A branch made
 * meanwhile refuses it rather than being written over.
 *
 * A DELETE IS CONFIRMED in the page with its commit's message. Every control
 * is drawn for every reader and held with its reason (`lib/repoWrites.ts`).
 */

import { useEffect, useState } from "react";
import { Button, Callout, FormField, Input, Modal, Skeleton, Stack, Textarea } from "@crewlethq/ui";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { isWholeOID, refName, shortOID } from "~/lib/code.ts";
import { fmtBytes } from "~/lib/format.ts";
import { branchWriteHeld, commitFormHeld } from "~/lib/repoWrites.ts";
import { useAct } from "~/lib/useAct.ts";
import { codeRest } from "~/lib/useCodeQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeBlobAnswer, CodeGitAnswer, CodeRepoDetail } from "~/protocol/index.ts";

/**
 * The largest file the browser's editor opens, in bytes: 256 KiB, a handful
 * of the engine's 56 KiB pages and well under what one act's body carries.
 */
export const EDIT_MAX_BYTES = 256 << 10;

/** Where a commit from the Files tab lands: the branch on screen and the commit it stood at. */
export interface CommitTarget {
  /**
   * The branch's name; null where the revision on screen is a tag or a
   * commit, which takes no commit; "" while the read has not said yet.
   */
  branch: string | null;
  /** The commit the branch resolved to on the page's read, whole; "" before it answered. */
  head: string;
}

/**
 * Where a commit from a Files read lands, BY WHAT THE READ RESOLVED rather
 * than by how the address spells the revision: the address may name a branch
 * bare, a tag or a branch as a whole ref, or nothing for the default branch,
 * and only the engine's resolution says which branch, if any, that was and
 * which commit it stood at.
 */
export function targetOf(rev: string, answer: CodeGitAnswer | null | undefined): CommitTarget {
  if (isWholeOID(rev)) return { branch: null, head: rev };
  const resolved = answer?.resolved?.["ref"];
  if (!resolved) return { branch: "", head: "" };
  const named = refName(resolved.ref);
  return { branch: named.kind === "branch" ? named.name : null, head: resolved.oid };
}

/** Why no commit can be made at this place, or undefined. */
export function targetHeld(target: CommitTarget): string | undefined {
  if (target.branch === null) {
    return "Choose a branch to commit to: a tag or a commit takes none.";
  }
  if (target.head === "" || target.branch === "") return "Still reading where the branch stands.";
  return undefined;
}

/** "New file", in a directory's bar: a file added under it. */
export function NewFileButton({
  repo,
  target,
  dir,
}: {
  repo: CodeRepoDetail;
  target: CommitTarget;
  /** The directory on screen, "" for the root. */
  dir: string;
}) {
  const write = useAct("code_commit_files");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={branchWriteHeld(repo, viewer) ?? targetHeld(target)}
        onPress={() => setOpen(true)}
      >
        New file
      </WriteButton>
      {open && target.branch && (
        <CommitDialog
          repo={repo}
          branch={target.branch}
          head={target.head}
          path={dir ? `${dir}/` : ""}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  );
}

/**
 * "Add a file", on an empty repository's Files tab: its first commit, which
 * makes its default branch.
 */
export function FirstFileButton({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_commit_files");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="primary"
        blocked={branchWriteHeld(repo, viewer)}
        onPress={() => setOpen(true)}
      >
        Add a file
      </WriteButton>
      {open && (
        <CommitDialog
          repo={repo}
          branch={repo.default_branch}
          head=""
          path=""
          onClose={() => setOpen(false)}
        />
      )}
    </>
  );
}

/** "Edit" and "Delete file", in a file's bar. */
export function FileWriteButtons({
  repo,
  target,
  path,
  file,
}: {
  repo: CodeRepoDetail;
  target: CommitTarget;
  path: string;
  /** The file as its first page answered it: its size and whether git calls it binary. */
  file: CodeBlobAnswer;
}) {
  const write = useAct("code_commit_files");
  const viewer = useViewer();
  const [editing, setEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const held = branchWriteHeld(repo, viewer) ?? targetHeld(target);
  const editHeld =
    held ??
    (file.binary
      ? "A binary file is not edited here: commit it from a clone."
      : file.size > EDIT_MAX_BYTES
        ? `This file is ${fmtBytes(file.size)}: the editor takes up to ${fmtBytes(EDIT_MAX_BYTES)}, so commit it from a clone.`
        : undefined);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={editHeld}
        onPress={() => setEditing(true)}
      >
        Edit
      </WriteButton>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        blocked={held}
        onPress={() => setDeleting(true)}
      >
        Delete file
      </WriteButton>
      {editing && target.branch && (
        <CommitDialog
          repo={repo}
          branch={target.branch}
          head={target.head}
          path={path}
          existing
          onClose={() => setEditing(false)}
        />
      )}
      {deleting && target.branch && (
        <DeleteFileDialog
          repo={repo}
          branch={target.branch}
          head={target.head}
          path={path}
          onClose={() => setDeleting(false)}
        />
      )}
    </>
  );
}

/** A file's whole text at a commit: every page of it, joined. */
async function readWhole(repo: string, path: string, commit: string, signal: AbortSignal) {
  let text = "";
  let cursor = "";
  for (;;) {
    const answer = await codeRest.query(
      "code_blob",
      { name: repo, path, ref: commit, ...(cursor ? { cursor } : {}) },
      signal,
    );
    text += answer.blob?.text ?? "";
    cursor = answer.next ?? "";
    if (!cursor) return text;
  }
}

/** The editor: a file's path, its text and the commit's message. */
function CommitDialog({
  repo,
  branch,
  head,
  path,
  existing = false,
  onClose,
}: {
  repo: CodeRepoDetail;
  branch: string;
  /** The commit the branch stood at when read; "" for an empty repository's first commit. */
  head: string;
  path: string;
  /** The path names a file the branch holds, opened on its text; else a new one. */
  existing?: boolean;
  onClose: () => void;
}) {
  const write = useAct("code_commit_files");
  const [name, setName] = useState(path);
  const [original, setOriginal] = useState<string | null>(existing ? null : "");
  const [content, setContent] = useState("");
  const [failed, setFailed] = useState(false);
  const [message, setMessage] = useState("");
  useEffect(() => {
    if (!existing) return;
    const abort = new AbortController();
    readWhole(repo.name, path, head, abort.signal).then(
      (text) => {
        setOriginal(text);
        setContent(text);
      },
      () => {
        if (!abort.signal.aborted) setFailed(true);
      },
    );
    return () => abort.abort();
  }, [existing, repo.name, path, head]);
  const file = name.trim();
  const blocked =
    original === null
      ? "Still reading the file."
      : commitFormHeld(message, file, existing ? content !== original : true);
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      {
        repo: repo.name,
        branch,
        // NOTHING READ, NOTHING NAMED: a root commit makes the branch.
        ...(head ? { expected_head: head } : {}),
        message: message.trim(),
        files: [{ op: "put", path: file, content }],
      },
      { done: existing ? `Committed ${file} to ${branch}` : `Added ${file} to ${branch}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="lg"
      title={existing ? `Edit ${path}` : `New file in ${repo.name}`}
      onClose={onClose}
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
            Commit to {branch}
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        {head ? (
          <p className="muted">
            One commit on <span className="mono">{branch}</span>, at{" "}
            <span className="mono" title={head}>
              {shortOID(head)}
            </span>
            : a push made to it since is refused, never written over.
          </p>
        ) : (
          <p className="muted">
            The first commit of {repo.name}, which makes <span className="mono">{branch}</span>, its
            default branch.
          </p>
        )}
        {!existing && (
          <FormField label="Path" helper="From the repository's root, with its folders.">
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                width="full"
                spellCheck={false}
                placeholder="docs/notes.md"
                value={name}
                onChange={(event) => setName(event.target.value)}
              />
            )}
          </FormField>
        )}
        {failed ? (
          <Callout variant="danger">
            The file could not be read at this commit. Close this and open it again.
          </Callout>
        ) : original === null ? (
          <Skeleton variant="text" rows={8} label={`Loading ${path}`} />
        ) : (
          <Textarea
            aria-label="File contents"
            className="mono"
            rows={18}
            spellCheck={false}
            value={content}
            onChange={(event) => setContent(event.target.value)}
          />
        )}
        <FormField label="Commit message">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              placeholder={existing ? `Update ${path}` : "Add a file"}
              value={message}
              onChange={(event) => setMessage(event.target.value)}
            />
          )}
        </FormField>
      </Stack>
    </Modal>
  );
}

/** The delete's confirmation: what goes, and the commit's message. */
function DeleteFileDialog({
  repo,
  branch,
  head,
  path,
  onClose,
}: {
  repo: CodeRepoDetail;
  branch: string;
  head: string;
  path: string;
  onClose: () => void;
}) {
  const write = useAct("code_commit_files");
  const [message, setMessage] = useState(`Delete ${path}`);
  const blocked = commitFormHeld(message, path, true);
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      {
        repo: repo.name,
        branch,
        expected_head: head,
        message: message.trim(),
        files: [{ op: "delete", path }],
      },
      { done: `Deleted ${path} from ${branch}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Delete ${path}?`}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
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
            Delete file
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        <p>
          One commit on <span className="mono">{branch}</span> removes the file. Its earlier
          revisions stay in the history.
        </p>
        <FormField label="Commit message">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              value={message}
              onChange={(event) => setMessage(event.target.value)}
            />
          )}
        </FormField>
      </Stack>
    </Modal>
  );
}
