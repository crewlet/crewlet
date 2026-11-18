/**
 * A commit reverted, or picked onto another branch (`code_revert`,
 * `code_cherry_pick`): each ONE new commit on the branch the person chooses,
 * built on the repository's home Gitlet node and landed as one push. Nothing
 * is rewritten: a revert undoes the commit's change with a commit of its own.
 *
 * The dialog lists the repository's branches (`code_refs`, read as it opens)
 * and sends the head its listing showed for the one chosen (`expected_head`),
 * so a branch pushed to since is refused rather than committed onto unseen.
 * On a conflict nothing is committed, and the engine's refusal is drawn in
 * the dialog. Both are drawn for every reader and held with the reason
 * (`lib/repoWrites.ts`).
 */

import { useState } from "react";
import { Button, FormField, Input, Select, Stack } from "@crewlethq/ui";
import { GitCommitVerticalGlyph, RotateCcwGlyph } from "@crewlethq/icons/glyphs";
import { Modal } from "~/components/Modal.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { refName, shortOID } from "~/lib/code.ts";
import { branchWriteHeld } from "~/lib/repoWrites.ts";
import { useAct } from "~/lib/useAct.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeRepoDetail } from "~/protocol/index.ts";

type Kind = "revert" | "pick";

/** The commit page's two writes, in its page bar. */
export function CommitWrites({
  repo,
  sha,
  subject,
}: {
  repo: string;
  sha: string;
  subject: string;
}) {
  const revert = useAct("code_revert");
  const pick = useAct("code_cherry_pick");
  const viewer = useViewer();
  const read = useCodeQuery("code_repo", { name: repo });
  const detail = read.data?.repo;
  const [open, setOpen] = useState<Kind | null>(null);
  const held = detail ? branchWriteHeld(detail, viewer) : "Still reading the repository.";
  return (
    <div className="row gap-2 wrap" role="group" aria-label="Commit actions">
      <WriteButton
        write={revert}
        size="small"
        variant="secondary"
        blocked={held}
        leadingIcon={<RotateCcwGlyph size="sm" />}
        onPress={() => setOpen("revert")}
      >
        Revert
      </WriteButton>
      <WriteButton
        write={pick}
        size="small"
        variant="secondary"
        blocked={held}
        leadingIcon={<GitCommitVerticalGlyph size="sm" />}
        onPress={() => setOpen("pick")}
      >
        Cherry-pick
      </WriteButton>
      {open === "revert" && detail && (
        <RevertDialog repo={detail} sha={sha} subject={subject} onClose={() => setOpen(null)} />
      )}
      {open === "pick" && detail && (
        <PickDialog repo={detail} sha={sha} subject={subject} onClose={() => setOpen(null)} />
      )}
    </div>
  );
}

interface DialogProps {
  repo: CodeRepoDetail;
  sha: string;
  subject: string;
  onClose: () => void;
}

/** The branches a commit can land on, and the one chosen with the head its listing showed. */
function useBranchChoice(repo: CodeRepoDetail) {
  const refs = useCodeQuery("code_refs", { name: repo.name });
  const rows = refs.data?.refs ?? [];
  const [branch, setBranch] = useState(repo.default_branch);
  const head = rows.find((r) => refName(r.ref).name === branch)?.oid ?? "";
  const options = rows.map((r) => {
    const name = refName(r.ref).name;
    return { value: name, label: name === repo.default_branch ? `${name} (default)` : name };
  });
  if (!options.some((o) => o.value === branch)) options.unshift({ value: branch, label: branch });
  const blocked = refs.loading
    ? "Still reading the branches."
    : head === ""
      ? `${branch} is not a branch listed here: choose another.`
      : undefined;
  return { branch, setBranch, head, options, blocked };
}

function BranchFields({
  choice,
  message,
  onMessage,
  placeholder,
}: {
  choice: ReturnType<typeof useBranchChoice>;
  message: string;
  onMessage: (value: string) => void;
  placeholder: string;
}) {
  return (
    <>
      <FormField label="Onto the branch">
        {(field) => (
          <Select
            id={field.id}
            ariaLabel="Onto the branch"
            searchable
            value={choice.branch}
            onChange={(value) => choice.setBranch(String(value))}
            options={choice.options}
            emptyMessage="No branch matches"
          />
        )}
      </FormField>
      <FormField label="Commit message" optional helper="Git's own message when left empty.">
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            width="full"
            placeholder={placeholder}
            value={message}
            onChange={(event) => onMessage(event.target.value)}
          />
        )}
      </FormField>
    </>
  );
}

function RevertDialog({ repo, sha, subject, onClose }: DialogProps) {
  const write = useAct("code_revert");
  const choice = useBranchChoice(repo);
  const [message, setMessage] = useState("");
  const submit = async () => {
    if (!pressable(write, choice.blocked)) return;
    const result = await write.run(
      {
        repo: repo.name,
        branch: choice.branch,
        expected_head: choice.head,
        commit: sha,
        ...(message.trim() ? { message: message.trim() } : {}),
      },
      { done: `Reverted ${shortOID(sha)} on ${choice.branch}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Revert ${shortOID(sha)}?`}
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
            blocked={choice.blocked}
            onPress={() => void submit()}
          >
            Revert on {choice.branch}
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        <p>
          A new commit on the branch undoes what “{subject}” changed. The commit itself stays in the
          history, and nothing is committed if the two conflict.
        </p>
        <BranchFields
          choice={choice}
          message={message}
          onMessage={setMessage}
          placeholder={`Revert "${subject}"`}
        />
      </Stack>
    </Modal>
  );
}

function PickDialog({ repo, sha, subject, onClose }: DialogProps) {
  const write = useAct("code_cherry_pick");
  const choice = useBranchChoice(repo);
  const [message, setMessage] = useState("");
  const submit = async () => {
    if (!pressable(write, choice.blocked)) return;
    const result = await write.run(
      {
        repo: repo.name,
        branch: choice.branch,
        expected_head: choice.head,
        commit: sha,
        ...(message.trim() ? { message: message.trim() } : {}),
      },
      { done: `Picked ${shortOID(sha)} onto ${choice.branch}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Cherry-pick ${shortOID(sha)}`}
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
            blocked={choice.blocked}
            onPress={() => void submit()}
          >
            Pick onto {choice.branch}
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        <p>
          A new commit on the branch makes the change “{subject}” made. Nothing is committed if the
          two conflict.
        </p>
        <BranchFields
          choice={choice}
          message={message}
          onMessage={setMessage}
          placeholder={subject}
        />
      </Stack>
    </Modal>
  );
}
