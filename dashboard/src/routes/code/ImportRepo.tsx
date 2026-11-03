/**
 * "Import a repository": a new project filled from a repository at another
 * host (`code_import_repo`), or made a pull mirror of it (`mirror`), a
 * person's alone — the import has the code host dial another host for the
 * company. The repository is `importing` until its home has fetched it, and
 * its page says how far the import went.
 */

import { useMemo, useState } from "react";
import { Button, Checkbox, Input, Modal, Select, Stack } from "@crewlethq/ui";
import { WriteButton } from "~/components/WriteButton.tsx";
import { importFormHeld, importHeld } from "~/lib/repoWrites.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";

/** The toolbar's button and its dialog. */
export function ImportRepoButton() {
  const write = useAct("code_import_repo");
  const viewer = useViewer();
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={importHeld(viewer)}
        onPress={() => setOpen(true)}
      >
        Import a repository
      </WriteButton>
      {open && <ImportDialog onClose={() => setOpen(false)} />}
    </>
  );
}

function ImportDialog({ onClose }: { onClose: () => void }) {
  const write = useAct("code_import_repo");
  const org = useOrg();
  const units = useMemo(() => indexOrg(org).units, [org]);
  const [name, setName] = useState("");
  const [url, setURL] = useState("");
  const [credential, setCredential] = useState("");
  const [owner, setOwner] = useState("");
  const [mirror, setMirror] = useState(false);
  const blocked = importFormHeld(name, url, credential, owner);
  const submit = async () => {
    if (blocked !== undefined) return;
    const result = await write.run(
      {
        name: name.trim(),
        url: url.trim(),
        ...(credential.trim() ? { credential: credential.trim() } : {}),
        owner: { kind: "unit", ref: owner },
        ...(mirror ? { mirror: true } : {}),
      },
      { done: `Importing ${name.trim()}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title="Import a repository"
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
            Import
          </WriteButton>
        </>
      }
    >
      <Stack gap={2}>
        <Input
          aria-label="Name"
          placeholder="The project's name here"
          value={name}
          onChange={(event) => setName(event.target.value)}
        />
        <Input
          aria-label="URL"
          placeholder="https://forge.example.com/acme/web.git"
          value={url}
          onChange={(event) => setURL(event.target.value)}
        />
        <Input
          aria-label="Credential"
          placeholder="Variable holding the token (optional)"
          value={credential}
          onChange={(event) => setCredential(event.target.value)}
        />
        <Select
          ariaLabel="Owner"
          value={owner}
          onChange={(value) => setOwner(String(value))}
          options={[
            { value: "", label: "Choose the owning unit" },
            ...units.map((u) => ({ value: u.id, label: u.name })),
          ]}
        />
        <Checkbox
          label="Keep following it"
          description="A pull mirror syncs from it every 15 minutes and takes no push of its own."
          checked={mirror}
          onCheckedChange={setMirror}
        />
      </Stack>
    </Modal>
  );
}
