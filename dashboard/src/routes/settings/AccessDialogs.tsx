/**
 * People & access's own write dialogs: a service account, and an edit of
 * somebody's login, seat and grants.
 *
 * Each is one write through `lib/iamWrite.ts` — a create is keyed so the retry
 * an unknown answer asks for names what its first attempt made. See
 * `components/people.tsx` for the dialogs another screen offers too: an
 * invitation (a seat's page) and a token's mint (the Account page).
 */

import { useMemo, useState } from "react";
import { Button, FormField, Input, Modal, Select, Text } from "@crewlethq/ui";
import { PencilGlyph, UserPlusGlyph } from "@crewlethq/icons/glyphs";
import {
  GrantPicker,
  IamOutcome,
  MintTokenDialog,
  NO_SEAT,
  pressLabel,
  seatOptions,
  useUnheldSeats,
} from "~/components/people.tsx";
import { useIamGesture } from "~/lib/iamWrite.ts";

/** A directory row, as much of it as an edit and a mint need. */
export interface EditableRow {
  id: string;
  kind: string;
  login?: string;
  name?: string;
  seat?: string;
  grants?: string[] | null;
}

/**
 * A new service account — a MACHINE: a login in the colon grammar, a name and
 * grants — and then, once it exists, the offer to mint its token.
 */
export function ServiceAccountDialog({
  held,
  onClose,
  onDone,
}: {
  held: readonly string[];
  onClose: () => void;
  onDone: () => void;
}) {
  const write = useIamGesture();
  const [login, setLogin] = useState("");
  const [name, setName] = useState("");
  const [grants, setGrants] = useState<string[]>([]);
  const [minting, setMinting] = useState(false);
  const created = write.answer?.kind === "done" ? write.answer.body : null;
  const id = typeof created?.id === "string" ? created.id : "";

  if (created && minting) {
    return (
      <MintTokenDialog owner={{ id, login: login.trim(), grants }} held={held} onClose={onClose} />
    );
  }

  async function submit() {
    if (login.trim() === "" || created) return;
    const answer = await write.run({
      method: "POST",
      path: "/iam/people",
      body: { kind: "machine", login: login.trim(), name: name.trim(), grants },
    });
    if (answer) onDone();
  }

  return (
    <Modal
      open
      size="md"
      title="New service account"
      icon={<UserPlusGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      closeDisabledReason="Waiting for the engine to answer."
      stackBody
      onSubmit={() => void submit()}
      footer={
        created ? (
          <>
            <Button variant="ghost" onClick={onClose}>
              Done
            </Button>
            <Button variant="primary" onClick={() => setMinting(true)}>
              Mint its token
            </Button>
          </>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose} disabled={write.busy}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" disabled={write.busy || login.trim() === ""}>
              {pressLabel(write, "Create", "Creating")}
            </Button>
          </>
        )
      }
    >
      {created ? (
        <Text as="p" variant="body">
          <span className="mono">{login.trim()}</span> exists. A service account signs in with a
          token, never a password: mint one now, or later from its row.
        </Text>
      ) : (
        <>
          <FormField
            label="Login"
            helper="Lowercase words joined by colons, such as ci:release — at most 64 characters. The colon is what tells a machine from a person (jane.doe) and a seat (jane)."
          >
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                autoFocus
                width="full"
                spellCheck={false}
                placeholder="ci:release"
                value={login}
                onChange={(e) => setLogin(e.target.value)}
              />
            )}
          </FormField>
          <FormField label="Name" optional>
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                width="full"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            )}
          </FormField>
          <GrantPicker value={grants} onChange={setGrants} held={held} />
        </>
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}

/**
 * Change somebody's login, seat or grants — one `PATCH`, carrying only what
 * changed. The engine moves the login and the seat in one record before the
 * grants, and a refusal part way names what had already landed.
 *
 * WHAT CHANGED IS MEASURED AGAINST THE ROW THE DIALOG OPENED ON, never the
 * live one: the directory is read again every minute and when the tab comes
 * back, and the fields hold what the dialog opened with, so measured against
 * a newer row a field nobody touched reads as an edit — and sending it puts
 * back a grant, a seat or a login another administrator has just changed.
 * After a refusal part way, what landed is sent again equal to what the
 * engine holds, and the engine skips it.
 */
export function EditPersonDialog({
  row: live,
  held,
  onClose,
  onDone,
}: {
  row: EditableRow;
  held: readonly string[];
  onClose: () => void;
  onDone: () => void;
}) {
  const write = useIamGesture();
  const seats = useUnheldSeats();
  const [row] = useState(live);
  const [login, setLogin] = useState(row.login ?? "");
  const [seat, setSeat] = useState(row.seat ?? NO_SEAT);
  const [grants, setGrants] = useState<string[]>(row.grants ?? []);
  // WHAT THIS EDIT MAY CONFER is the engine's rule: only a grant it ADDS needs
  // the editor to hold it, so one the person already holds may be kept or
  // taken away by an administrator who does not hold it — stripping a
  // compromised colleague's config:write needs nobody to hold config:write.
  const conferrable = useMemo(() => [...held, ...(row.grants ?? [])], [held, row.grants]);
  // THEIR OWN SEAT STAYS OFFERED beside the vacant ones: nobody else holds it,
  // but it is held, so the vacancies list leaves it out. The seat they hold
  // NOW, which the vacancies are read beside — an edit that landed its seat
  // part way holds the new one, and the one it left is vacant again.
  const options = useMemo(() => {
    const vacant = seats.data ?? [];
    const own = live.seat && !vacant.some((s) => s.handle === live.seat);
    return seatOptions(own ? [{ handle: live.seat!, name: live.seat! }, ...vacant] : vacant);
  }, [seats.data, live.seat]);

  const before = new Set(row.grants ?? []);
  const grantsMoved = grants.length !== before.size || grants.some((g) => !before.has(g));
  const change: Record<string, unknown> = {
    ...(login.trim() !== (row.login ?? "") ? { login: login.trim() } : {}),
    ...(seat !== (row.seat ?? NO_SEAT) ? { seat } : {}),
    ...(grantsMoved ? { grants } : {}),
  };
  const nothing = Object.keys(change).length === 0;

  async function submit() {
    if (nothing || login.trim() === "") return;
    const answer = await write.run({
      method: "PATCH",
      path: `/iam/people/${encodeURIComponent(row.id)}`,
      body: change,
    });
    if (!answer) return;
    onDone();
    if (answer.kind === "done" && !answer.pending) onClose();
  }

  return (
    <Modal
      open
      size="md"
      title={`Edit ${row.name || row.login || row.id}`}
      icon={<PencilGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      closeDisabledReason="Waiting for the engine to answer."
      stackBody
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={write.busy}>
            {write.answer?.kind === "done" ? "Done" : "Cancel"}
          </Button>
          <Button
            type="submit"
            variant="primary"
            disabled={write.busy || nothing || login.trim() === ""}
          >
            {pressLabel(write, "Save", "Saving")}
          </Button>
        </>
      }
    >
      <FormField
        label="Login"
        helper={
          row.kind === "machine"
            ? "Words joined by colons, such as ci:release."
            : "Words joined by dots, such as jane.doe. Their changes are recorded under it while they hold no seat."
        }
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            width="full"
            spellCheck={false}
            value={login}
            onChange={(e) => setLogin(e.target.value)}
          />
        )}
      </FormField>
      <FormField
        label="Seat"
        helper="A human seat nobody else holds; bound to it, they act as that seat. No seat unbinds them."
      >
        {(field) => (
          <Select
            id={field.id}
            ariaLabel="Seat"
            searchable
            value={seat}
            options={options}
            onChange={(next) => setSeat(String(next))}
          />
        )}
      </FormField>
      <GrantPicker value={grants} onChange={setGrants} held={conferrable} />
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}
