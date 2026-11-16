/**
 * The repository page's writes: its details (description, default branch,
 * owner and who reads it), its name, its archive, its search mode, its
 * mirrors, and a purge of its unreachable history, each through `useAct` and a
 * WriteButton, never optimistic (the page is redrawn from the read the write's
 * floor brings), and each drawn for every reader, held with its reason
 * (`lib/repoWrites.ts`). A rename and an archive are each a record of their
 * own, so each is its own `code_update_repo` call, never folded into a
 * details change.
 *
 * A repository the company document declares is changed in Settings ›
 * Configuration, so its details, search mode and mirrors are held here with
 * that sentence; a purge changes none of its settings and is offered for it
 * too.
 */

import { useMemo, useState } from "react";
import { Button, FormField, Input, Select, Stack } from "@crewlethq/ui";
import { useNavigator } from "~/app/router.tsx";
import { ConfirmModal, Modal } from "~/components/Modal.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { refName, repoPath } from "~/lib/code.ts";
import { fieldError, formHeld, holdReason } from "~/lib/formHold.ts";
import { indexOrg, useSeatBadgeOf } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import {
  archiveHeld,
  credentialName,
  credentialRef,
  detailsFormHeld,
  detailsHeld,
  ownerKey,
  mirrorFormHeld,
  mirrorsHeld,
  pullMirrorHeld,
  purgeHeld,
  purgeMessage,
  renameFormHeld,
  renameHeld,
  searchModeHeld,
  type MirrorKind,
} from "~/lib/repoWrites.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeRepoDetail } from "~/protocol/index.ts";
import { REPO_SEARCH_MODES } from "~/contract/code.ts";

/**
 * "Edit details": the repository's description, default branch, owner and who
 * reads it, one `code_update_repo` carrying only what the person changed. The
 * default branch is chosen from the branches the repository holds, so a
 * repository whose default branch holds nothing is repaired here by naming one
 * that does.
 */
export function DetailsControl({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_update_repo");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={detailsHeld(repo, viewer)}
        onPress={() => setOpen(true)}
      >
        Edit details
      </WriteButton>
      {open && <DetailsDialog repo={repo} onClose={() => setOpen(false)} />}
    </>
  );
}

function DetailsDialog({ repo, onClose }: { repo: CodeRepoDetail; onClose: () => void }) {
  const write = useAct("code_update_repo");
  const org = useOrg();
  const seatBadge = useSeatBadgeOf();
  const units = useMemo(() => indexOrg(org).units, [org]);
  // ITS BRANCHES, the first page of the engine's listing by name: what a
  // default branch is chosen from.
  const refs = useCodeQuery("code_refs", { name: repo.name });
  const branches = useMemo(
    () => (refs.data?.refs ?? []).map((r) => refName(r.ref).name),
    [refs.data],
  );
  const [description, setDescription] = useState(repo.description ?? "");
  const [defaultBranch, setDefaultBranch] = useState(repo.default_branch);
  const [owner, setOwner] = useState(ownerKey(repo.owner));
  const [visibility, setVisibility] = useState(repo.visibility);
  const hold =
    refs.loading && !refs.data
      ? formHeld("Reading its branches…")
      : detailsFormHeld(repo, { description, defaultBranch, owner, visibility }, branches);
  const blocked = holdReason(hold);
  const ownerOptions = units.map((u) => ({
    value: ownerKey({ kind: "unit", ref: u.id }),
    label: u.name,
  }));
  if (!ownerOptions.some((o) => o.value === ownerKey(repo.owner))) {
    // AN OWNER THE PICKER DOES NOT LIST (a seat's repository) stays its own
    // choice, so the form never moves it unasked.
    ownerOptions.unshift({
      value: ownerKey(repo.owner),
      label: repo.owner.kind === "seat" ? seatBadge(repo.owner.ref).name : repo.owner.ref,
    });
  }
  const branchOptions = branches.map((b) => ({ value: b, label: b }));
  if (!branches.includes(repo.default_branch)) {
    branchOptions.unshift({
      value: repo.default_branch,
      label: `${repo.default_branch} (holds no commit)`,
    });
  }
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const [kind, ref] = owner.split(/:(.*)/s) as [string, string];
    const result = await write.run(
      {
        repo: repo.name,
        ...(description.trim() !== (repo.description ?? "")
          ? { description: description.trim() }
          : {}),
        ...(defaultBranch !== repo.default_branch ? { default_branch: defaultBranch } : {}),
        ...(owner !== ownerKey(repo.owner) ? { owner: { kind, ref } } : {}),
        ...(visibility !== repo.visibility ? { visibility } : {}),
      },
      { done: `Changed ${repo.name}'s details` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`${repo.name}'s details`}
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
            Save details
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        <FormField label="Description" optional>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              value={description}
              onChange={(event) => setDescription(event.target.value)}
            />
          )}
        </FormField>
        <FormField
          label="Default branch"
          helper="What a clone checks out and a change is proposed into. The branch it replaces stays, and can then be deleted."
          error={fieldError(hold, "defaultBranch")}
        >
          {(field) => (
            <Select
              id={field.id}
              ariaLabel="Default branch"
              searchable
              value={defaultBranch}
              onChange={(value) => setDefaultBranch(String(value))}
              options={branchOptions}
            />
          )}
        </FormField>
        <FormField label="Owning unit" helper="Its lead and its seats maintain and write it.">
          {(field) => (
            <Select
              id={field.id}
              ariaLabel="Owning unit"
              value={owner}
              onChange={(value) => setOwner(String(value))}
              options={ownerOptions}
            />
          )}
        </FormField>
        <FormField
          label="Read by"
          helper="Which seats read it. Every person reads every repository."
        >
          {(field) => (
            <Select
              id={field.id}
              ariaLabel="Read by"
              value={visibility}
              onChange={(value) => setVisibility(String(value))}
              options={VISIBILITY_OPTIONS}
            />
          )}
        </FormField>
      </Stack>
    </Modal>
  );
}

/** Who reads a repository, as its picker names each choice (`gitlet.Visibility`). */
const VISIBILITY_OPTIONS = [
  { value: "company", label: "Every seat" },
  { value: "unit", label: "Its owner's seats" },
];

/**
 * "Rename": the repository's new name, one `code_update_repo` naming
 * `rename_to` alone, since a rename is a record of its own. The old name
 * keeps leading here for a while, and the page moves to the new one once the
 * engine applied it.
 */
export function RenameControl({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_update_repo");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={renameHeld(repo, viewer)}
        onPress={() => setOpen(true)}
      >
        Rename
      </WriteButton>
      {open && <RenameDialog repo={repo} onClose={() => setOpen(false)} />}
    </>
  );
}

function RenameDialog({ repo, onClose }: { repo: CodeRepoDetail; onClose: () => void }) {
  const write = useAct("code_update_repo");
  const nav = useNavigator();
  const [name, setName] = useState(repo.name);
  // A name in the wrong shape is said under the field and holds Rename.
  const hold = renameFormHeld(repo, name);
  const blocked = holdReason(hold);
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const to = name.trim();
    const result = await write.run(
      { repo: repo.name, rename_to: to },
      { done: `Renamed ${repo.name} to ${to}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      onClose();
      if (result.kind === "applied") nav.to(repoPath(to), { tab: "settings" });
    }
  };
  return (
    <Modal
      open
      size="sm"
      title={`Rename ${repo.name}`}
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
            Rename
          </WriteButton>
        </>
      }
    >
      <FormField
        label="New name"
        helper="Clones, pushes and links to the old name keep reaching it for a while, then the old name is released."
        error={fieldError(hold, "name")}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            width="full"
            spellCheck={false}
            autoFocus
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        )}
      </FormField>
    </Modal>
  );
}

/**
 * "Archive repository" and "Unarchive repository": one `code_update_repo`
 * naming `archived` alone. An archive is confirmed first, naming what stops;
 * an unarchive gives back what the archive took, so it is pressed directly.
 */
export function ArchiveControl({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_update_repo");
  const viewer = useViewer();
  const [confirming, setConfirming] = useState(false);
  const archived = repo.status === "archived";
  if (archived) {
    return (
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={detailsHeld(repo, viewer)}
        onPress={() =>
          void write.run({ repo: repo.name, archived: false }, { done: `Unarchived ${repo.name}` })
        }
      >
        Unarchive repository
      </WriteButton>
    );
  }
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={archiveHeld(repo, viewer)}
        onPress={() => setConfirming(true)}
      >
        Archive repository
      </WriteButton>
      <ConfirmModal
        open={confirming}
        title={`Archive ${repo.name}?`}
        message={`${repo.name} becomes read-only: it is still cloned, fetched and read, and takes no push, so nothing merges into it. It is unarchived here.`}
        cancelLabel="Keep it active"
        confirmLabel="Archive"
        onClose={() => setConfirming(false)}
        submitting={write.busy}
        onConfirm={async () => {
          await write.run({ repo: repo.name, archived: true }, { done: `Archived ${repo.name}` });
          setConfirming(false);
        }}
      />
    </>
  );
}

/** What each search mode is called on its button. */
const MODE_LABELS: Readonly<Record<(typeof REPO_SEARCH_MODES)[number], string>> = {
  indexed: "Index it",
  grep: "Search it with grep",
};

/** The search mode's two buttons: the one it has is held as no change. */
export function SearchModeControl({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_update_repo");
  const viewer = useViewer();
  return (
    <div className="row wrap gap-2" role="group" aria-label="Search mode">
      {REPO_SEARCH_MODES.map((mode) => (
        <WriteButton
          key={mode}
          write={write}
          size="small"
          variant="secondary"
          blocked={searchModeHeld(repo, viewer, mode)}
          onPress={() =>
            void write.run(
              { repo: repo.name, search: mode },
              {
                done: `${repo.name} is now ${mode === "indexed" ? "indexed" : "searched with grep"}`,
              },
            )
          }
        >
          {MODE_LABELS[mode]}
        </WriteButton>
      ))}
    </div>
  );
}

/** The mirrors' controls: set or remove the pull mirror and the push mirror, a person's alone. */
export function MirrorControls({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_update_repo");
  const viewer = useViewer();
  const [editing, setEditing] = useState<MirrorKind | null>(null);
  const held = mirrorsHeld(repo, viewer);
  const from = repo.remotes?.mirror_from;
  const to = repo.remotes?.mirror_to;
  const remove = (kind: MirrorKind) =>
    void write.run(
      { repo: repo.name, ...(kind === "pull" ? { mirror_from: {} } : { mirror_to: {} }) },
      { done: `Removed ${repo.name}'s ${kind} mirror` },
    );
  return (
    <div className="row wrap gap-2" role="group" aria-label="Mirrors">
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={pullMirrorHeld(repo, viewer)}
        onPress={() => setEditing("pull")}
      >
        {from ? "Change the pull mirror" : "Make it a pull mirror"}
      </WriteButton>
      {from && (
        <WriteButton
          write={write}
          size="small"
          variant="ghost"
          blocked={held}
          onPress={() => remove("pull")}
        >
          Remove the pull mirror
        </WriteButton>
      )}
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={held}
        onPress={() => setEditing("push")}
      >
        {to ? "Change the push mirror" : "Add a push mirror"}
      </WriteButton>
      {to && (
        <WriteButton
          write={write}
          size="small"
          variant="ghost"
          blocked={held}
          onPress={() => remove("push")}
        >
          Remove the push mirror
        </WriteButton>
      )}
      {editing && <MirrorDialog repo={repo} kind={editing} onClose={() => setEditing(null)} />}
    </div>
  );
}

/** Set one mirror whole: its URL, the variable holding its token, and (a push mirror's) its seat and refs. */
function MirrorDialog({
  repo,
  kind,
  onClose,
}: {
  repo: CodeRepoDetail;
  kind: MirrorKind;
  onClose: () => void;
}) {
  const write = useAct("code_update_repo");
  const from = repo.remotes?.mirror_from;
  const to = repo.remotes?.mirror_to;
  const [url, setURL] = useState((kind === "pull" ? from?.url : to?.url) ?? "");
  const [credential, setCredential] = useState(
    credentialName(kind === "pull" ? from?.credential : to?.credential),
  );
  const [as, setAs] = useState(to?.as ?? "");
  const [refs, setRefs] = useState((to?.refs ?? []).join(", "));
  const hold = mirrorFormHeld(kind, url, credential, as);
  const blocked = holdReason(hold);
  const submit = async () => {
    if (blocked !== undefined) return;
    const ref = credentialRef(credential) ?? "";
    const patterns = refs
      .split(/[\s,]+/)
      .map((r) => r.trim())
      .filter(Boolean);
    const mirror =
      kind === "pull"
        ? { mirror_from: { url: url.trim(), ...(ref ? { credential: ref } : {}) } }
        : {
            mirror_to: {
              url: url.trim(),
              credential: ref,
              as: as.trim().replace(/^@/, ""),
              ...(patterns.length ? { refs: patterns } : {}),
            },
          };
    const result = await write.run(
      { repo: repo.name, ...mirror },
      { done: `Set ${repo.name}'s ${kind} mirror` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={
        kind === "pull" ? `Make ${repo.name} a pull mirror` : `Push ${repo.name} to another host`
      }
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
            Save
          </WriteButton>
        </>
      }
    >
      <Stack gap={2}>
        <p className="muted">
          {kind === "pull"
            ? "A source it does not follow yet is imported first (its branches and tags, but a seat's own, become the source's, and one the source lacks is deleted), and then it moves only by Gitlet's syncs from it, every 15 minutes, and takes no push of its own."
            : "Gitlet pushes these refs there as the seat you name: that seat's own identity at the other host, never a bot."}
        </p>
        <FormField label="URL" hideLabel error={fieldError(hold, "url")}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              placeholder="https://forge.example.com/acme/web.git"
              value={url}
              onChange={(event) => setURL(event.target.value)}
            />
          )}
        </FormField>
        <FormField label="Credential" hideLabel error={fieldError(hold, "credential")}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              placeholder={
                kind === "pull"
                  ? "Variable holding the token (optional)"
                  : "Variable holding the token"
              }
              value={credential}
              onChange={(event) => setCredential(event.target.value)}
            />
          )}
        </FormField>
        {kind === "push" && (
          <>
            <Input
              aria-label="Push as"
              placeholder="The seat's handle"
              value={as}
              onChange={(event) => setAs(event.target.value)}
            />
            <Input
              aria-label="Refs"
              placeholder="Its default branch and refs/tags/* when left empty"
              value={refs}
              onChange={(event) => setRefs(event.target.value)}
            />
          </>
        )}
      </Stack>
    </Modal>
  );
}

/**
 * "Purge unreachable history": a ConfirmModal that names, BEFORE the press,
 * who keeps copies of what the purge drops — the Gitlet nodes' copies, the
 * backups taken before it, the object store for a day, and every clone and
 * fetch that already took it, the push mirror's host by its URL where the
 * repository has one ([purgeKeepers]) — then the request, which the
 * repository's maintainer serves.
 */
export function PurgeControl({ repo }: { repo: CodeRepoDetail }) {
  const write = useAct("code_purge_unreachable");
  const viewer = useViewer();
  const [confirming, setConfirming] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={purgeHeld(repo, viewer)}
        onPress={() => setConfirming(true)}
      >
        Purge unreachable history
      </WriteButton>
      <ConfirmModal
        open={confirming}
        destructive
        title={`Purge ${repo.name}'s unreachable history?`}
        message={purgeMessage(repo)}
        cancelLabel="Keep it"
        confirmLabel="Purge"
        onClose={() => setConfirming(false)}
        // Held open, busy, until the engine answers: closed on a refusal too,
        // since the WriteButton draws the refusal beside itself, where the
        // person pressed, and a purge refused is never one asked.
        submitting={write.busy}
        onConfirm={async () => {
          await write.run({ repo: repo.name }, { done: `Asked for a purge of ${repo.name}` });
          setConfirming(false);
        }}
      />
    </>
  );
}
