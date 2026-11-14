/**
 * "New repository": a project repository made empty (`code_create_repo`), named
 * in the engine's grammar and owned by a unit picked from the chart. What it
 * is, its default branch and which seats read it are said only where the
 * person says them: left out, the engine's own defaults stand (`main`, read by
 * every seat). A scratch repository is a seat's own and is never made here.
 * An applied create leads to the repository's page.
 */

import { useMemo, useState } from "react";
import { Button, FormField, Input, Modal, Select, Stack } from "@crewlethq/ui";
import { useNavigator } from "~/app/router.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { repoPath } from "~/lib/code.ts";
import { createFormHeld, createHeld } from "~/lib/repoWrites.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";

/** The toolbar's button and its dialog. */
export function NewRepoButton() {
  const write = useAct("code_create_repo");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="primary"
        blocked={createHeld(viewer)}
        onPress={() => setOpen(true)}
      >
        New repository
      </WriteButton>
      {open && <NewRepoDialog onClose={() => setOpen(false)} />}
    </>
  );
}

function NewRepoDialog({ onClose }: { onClose: () => void }) {
  const write = useAct("code_create_repo");
  const nav = useNavigator();
  const org = useOrg();
  const units = useMemo(() => indexOrg(org).units, [org]);
  const [name, setName] = useState("");
  const [owner, setOwner] = useState("");
  const [description, setDescription] = useState("");
  const [branch, setBranch] = useState("");
  const [visibility, setVisibility] = useState("");
  const blocked = createFormHeld(name, owner);
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const made = name.trim();
    const result = await write.run(
      {
        name: made,
        owner: { kind: "unit", ref: owner },
        ...(description.trim() ? { description: description.trim() } : {}),
        ...(branch.trim() ? { default_branch: branch.trim() } : {}),
        ...(visibility ? { visibility } : {}),
      },
      { done: `Created ${made}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      onClose();
      if (result.kind === "applied") nav.to(repoPath(made));
    }
  };
  return (
    <Modal
      open
      size="sm"
      title="New repository"
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
            Create repository
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        <FormField label="Name" helper="Lowercase letters, digits and hyphens.">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              spellCheck={false}
              placeholder="web"
              value={name}
              onChange={(event) => setName(event.target.value)}
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
              options={[
                { value: "", label: "Choose a unit" },
                ...units.map((u) => ({ value: u.id, label: u.name })),
              ]}
            />
          )}
        </FormField>
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
        <FormField label="Default branch" optional helper="main when left empty.">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              spellCheck={false}
              placeholder="main"
              value={branch}
              onChange={(event) => setBranch(event.target.value)}
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
              options={[
                { value: "", label: "Every seat" },
                { value: "unit", label: "Its owner's seats" },
              ]}
            />
          )}
        </FormField>
      </Stack>
    </Modal>
  );
}
