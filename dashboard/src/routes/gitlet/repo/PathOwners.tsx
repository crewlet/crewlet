/**
 * A repository's path owners, written (`code_update_repo`'s `path_owners`): an
 * owner given globs, an owner's globs changed, and an owner taken off.
 *
 * GLOBS MOVE BY NAME, never as the list whole (`gitlet.PathOwnersChange`): the
 * form sends the globs it gives an owner and the ones it takes, so a glob
 * another request gave meanwhile is kept. An owner left with no glob is no
 * path owner any more, so Remove takes every glob the page read.
 *
 * Every control is drawn for every reader and held with its reason
 * (`lib/repoWrites.ts`): a declared repository's path owners are the company
 * document's, and taking a glob while a rule makes path owners approve its
 * merges is a person's.
 */

import { useMemo, useState } from "react";
import { Button, FormField, ListInput, Select, Stack, withDraft } from "@crewlethq/ui";
import { fieldError, holdReason } from "~/lib/formHold.ts";
import { ConfirmModal, Modal } from "~/components/Modal.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import {
  ownerKey,
  pathOwnerFormHeld,
  pathOwnerTakeHeld,
  pathOwnersArgs,
  pathOwnersHeld,
} from "~/lib/repoWrites.ts";
import { useOrgIndex } from "~/lib/seats.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodePathOwner, CodeRepoDetail } from "~/protocol/index.ts";
import { ChoiceFace } from "~/ui/SeatAvatar.tsx";

/** "Add a path owner": the button that opens the form for an owner and its globs. */
export function AddPathOwnerButton({
  repo,
  owners,
}: {
  repo: CodeRepoDetail;
  owners: readonly CodePathOwner[];
}) {
  const write = useAct("code_update_repo");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={pathOwnersHeld(repo, viewer)}
        onPress={() => setOpen(true)}
      >
        Add a path owner
      </WriteButton>
      {open && <PathOwnerDialog repo={repo} owners={owners} onClose={() => setOpen(false)} />}
    </>
  );
}

/** One owner's Edit and Remove; `gating` is whether a rule makes path owners approve merges. */
export function PathOwnerControls({
  repo,
  owner,
  owners,
  name,
  gating,
}: {
  repo: CodeRepoDetail;
  owner: CodePathOwner;
  owners: readonly CodePathOwner[];
  name: string;
  gating: boolean;
}) {
  const write = useAct("code_update_repo");
  const viewer = useViewer();
  const [editing, setEditing] = useState(false);
  const [removing, setRemoving] = useState(false);
  return (
    <span className="row gap-1">
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        blocked={pathOwnersHeld(repo, viewer)}
        aria-label={`Edit the paths ${name} owns`}
        onPress={() => setEditing(true)}
      >
        Edit
      </WriteButton>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        blocked={pathOwnerTakeHeld(repo, viewer, gating)}
        aria-label={`Remove ${name} as a path owner`}
        onPress={() => setRemoving(true)}
      >
        Remove
      </WriteButton>
      {editing && (
        <PathOwnerDialog
          repo={repo}
          owners={owners}
          editing={owner}
          name={name}
          gating={gating}
          onClose={() => setEditing(false)}
        />
      )}
      <ConfirmModal
        open={removing}
        destructive
        title={`Remove ${name} as a path owner?`}
        message={`A change touching ${owner.paths.join(", ")} no longer asks ${name} for a review${
          gating
            ? ", and a merge into a branch whose rule makes path owners approve no longer waits for one"
            : ""
        }.`}
        cancelLabel="Keep it"
        confirmLabel="Remove owner"
        onClose={() => setRemoving(false)}
        submitting={write.busy}
        onConfirm={async () => {
          await write.run(
            { repo: repo.name, path_owners: pathOwnersArgs(owner.owner, [], owner.paths) },
            { done: `Removed ${name} as a path owner of ${repo.name}` },
          );
          setRemoving(false);
        }}
      />
    </span>
  );
}

/**
 * The form: a new owner chosen from the chart's units and seats, or the one
 * being edited, and its globs.
 */
function PathOwnerDialog({
  repo,
  owners,
  editing,
  name,
  gating = false,
  onClose,
}: {
  repo: CodeRepoDetail;
  owners: readonly CodePathOwner[];
  editing?: CodePathOwner;
  name?: string;
  gating?: boolean;
  onClose: () => void;
}) {
  const write = useAct("code_update_repo");
  const viewer = useViewer();
  const index = useOrgIndex();
  const [owner, setOwner] = useState(editing ? ownerKey(editing.owner) : "");
  const [paths, setPaths] = useState<string[]>(editing?.paths ?? []);
  const [draft, setDraft] = useState("");
  // AN OWNER THAT HOLDS GLOBS ALREADY is edited from its own row, so a new
  // owner is chosen from those that hold none.
  const options = useMemo(() => {
    const taken = new Set(owners.map((o) => ownerKey(o.owner)));
    return [
      { value: "", label: "Choose a unit or a seat" },
      ...index.units
        .map((u) => ({ value: ownerKey({ kind: "unit", ref: u.id }), label: u.name }))
        .filter((o) => !taken.has(o.value)),
      ...index.seats
        .map((s) => ({
          value: ownerKey({ kind: "seat", ref: s.handle }),
          label: s.name,
          leading: <ChoiceFace seat={s} />,
        }))
        .filter((o) => !taken.has(o.value)),
    ];
  }, [index, owners]);
  const had = editing?.paths ?? [];
  const wanted = withDraft(paths, draft);
  const [kind, ref] = owner.split(/:(.*)/s) as [string, string];
  const change = owner === "" ? null : pathOwnersArgs({ kind, ref }, wanted, had);
  const hold = pathOwnerFormHeld(owner, wanted, had);
  const blocked =
    holdReason(hold) ?? (change?.remove ? pathOwnerTakeHeld(repo, viewer, gating) : undefined);
  const submit = async () => {
    if (!pressable(write, blocked) || change === null) return;
    const result = await write.run(
      { repo: repo.name, path_owners: change },
      { done: editing ? `Changed the paths ${name} owns` : `Added a path owner to ${repo.name}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={editing ? `Paths ${name} owns` : `Add a path owner to ${repo.name}`}
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
            {editing ? "Save" : "Add owner"}
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        {!editing && (
          <FormField
            label="Owner"
            helper="A unit or a seat of the chart, whose review a change touching its paths asks for."
          >
            {(field) => (
              <Select
                id={field.id}
                ariaLabel="Owner"
                value={owner}
                onChange={(value) => setOwner(String(value))}
                options={options}
              />
            )}
          </FormField>
        )}
        <ListInput
          label="Paths"
          itemName="path"
          helper="Globs over its tree from the root, such as src/** or docs/*.md."
          error={fieldError(hold, "paths")}
          placeholder="src/**"
          value={paths}
          draft={draft}
          onDraftChange={setDraft}
          onChange={setPaths}
          focusOnMount={Boolean(editing)}
        />
      </Stack>
    </Modal>
  );
}
