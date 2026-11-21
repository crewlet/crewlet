/** A repository's tags, written: New tag (`code_create_tag`, annotated where a message is given) and
 *  a tag's Delete at the value its row shows (`code_delete_tag`), each held as a branch's write is. */

import { useState } from "react";
import { Button, Callout, FormField, Input, Stack, Textarea, VisuallyHidden } from "@crewlethq/ui";
import { TagGlyph, TrashGlyph } from "@crewlethq/icons/glyphs";
import { Modal } from "~/components/Modal.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { refName, shortOID } from "~/lib/code.ts";
import { tagDeleteHeld, tagFormHeld, tagWriteHeld } from "~/lib/repoWrites.ts";
import { useAct } from "~/lib/useAct.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeRepoDetail, CodeTagRow } from "~/protocol/index.ts";
import { useRefDetail } from "../branchDetail.ts";

/** "New tag": the Tags view's button and its dialog. */
export function NewTagButton({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_create_tag");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  const held =
    tagWriteHeld(repo, viewer) ??
    (repo.head ? undefined : `${repo.default_branch}, the default branch, holds no commit to tag.`);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="primary"
        blocked={held}
        onPress={() => setOpen(true)}
      >
        New tag
      </WriteButton>
      {open && <NewTagDialog repo={repo} onClose={() => setOpen(false)} />}
    </>
  );
}

/** "Tag" in a commit's page bar: a new tag at that commit. */
export function TagCommitButton({ repo, sha }: { repo: string; sha: string }) {
  const write = useAct("code_create_tag");
  const viewer = useViewer();
  const detail = useCodeQuery("code_repo", { name: repo }).data?.repo;
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={detail ? tagWriteHeld(detail, viewer) : "Still reading the repository."}
        leadingIcon={<TagGlyph size="sm" />}
        onPress={() => setOpen(true)}
      >
        Tag
      </WriteButton>
      {open && detail && <NewTagDialog repo={detail} at={sha} onClose={() => setOpen(false)} />}
    </>
  );
}

function NewTagDialog({
  repo,
  at: fixed,
  onClose,
}: {
  repo: CodeRepoDetail;
  /** The commit it names, where the page chose it; else typed, the default branch when empty. */
  at?: string;
  onClose: () => void;
}) {
  const write = useAct("code_create_tag");
  const [name, setName] = useState("");
  const [at, setAt] = useState("");
  const [message, setMessage] = useState("");
  const blocked = tagFormHeld(name);
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const tag = name.trim();
    const result = await write.run(
      {
        repo: repo.name,
        tag,
        at: fixed ?? (at.trim() || repo.default_branch),
        ...(message.trim() ? { message: message.trim() } : {}),
      },
      { done: `Created the tag ${tag}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={fixed ? `Tag ${shortOID(fixed)}` : `New tag of ${repo.name}`}
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
            Create tag
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        <FormField label="Name" helper="A tag never moves once it is made.">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              spellCheck={false}
              placeholder="v1.0.0"
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          )}
        </FormField>
        {!fixed && (
          <FormField
            label="Points at"
            optional
            helper={`A branch or a commit. ${repo.default_branch} when left empty.`}
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
        )}
        <FormField
          label="Message"
          optional
          helper="Makes it an annotated tag, with you as its tagger. Left empty, the tag is lightweight."
        >
          {(field) => (
            <Textarea
              id={field.id}
              aria-describedby={field.describedBy}
              rows={3}
              value={message}
              onChange={(event) => setMessage(event.target.value)}
            />
          )}
        </FormField>
      </Stack>
    </Modal>
  );
}

/** One tag's Delete, confirmed in a dialog that reads the tag's rules first. */
export function DeleteTagButton({ repo, tag }: { repo: CodeRepoDetail; tag: CodeTagRow }) {
  const write = useAct("code_delete_tag");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  const name = refName(tag.ref).name;
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        blocked={tagDeleteHeld(repo, viewer)}
        aria-label={`Delete the tag ${name}`}
        leadingIcon={<TrashGlyph size="sm" />}
        onPress={() => setOpen(true)}
      >
        <VisuallyHidden>Delete</VisuallyHidden>
      </WriteButton>
      {open && <DeleteTagDialog repo={repo} tag={tag} name={name} onClose={() => setOpen(false)} />}
    </>
  );
}

function DeleteTagDialog({
  repo,
  tag,
  name,
  onClose,
}: {
  repo: CodeRepoDetail;
  tag: CodeTagRow;
  name: string;
  onClose: () => void;
}) {
  const write = useAct("code_delete_tag");
  // A PROTECTION RULE OR THE GRANT may keep this tag: its row in detail says so in the engine's words.
  const detail = useRefDetail(repo.name, tag.ref);
  const blocked = detail.loading
    ? "Reading whether a rule keeps this tag."
    : detail.row && !detail.row.may_delete
      ? detail.row.delete_detail || "A rule of the repository keeps this tag."
      : undefined;
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      { repo: repo.name, tag: name, expected_target: tag.oid },
      { done: `Deleted the tag ${name}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Delete the tag ${name}?`}
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
            Delete tag
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        <p>
          {`${name} is deleted at ${shortOID(tag.oid)}, the value listed here. The commit it names, ${shortOID(tag.peeled)}, stays in the repository's history.`}
        </p>
        {detail.row && !detail.row.may_delete && (
          <Callout variant="warning">
            {detail.row.delete_detail || "A rule of the repository keeps this tag."}
          </Callout>
        )}
      </Stack>
    </Modal>
  );
}
