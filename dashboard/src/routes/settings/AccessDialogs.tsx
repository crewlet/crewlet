/**
 * People & access's own write dialogs: a service account, a token minted for
 * one, and an edit of somebody's login, seat and grants.
 *
 * Each is one write through `lib/iamWrite.ts` — a create is keyed so the retry
 * an unknown answer asks for names what its first attempt made, and a mint
 * reads no key, so its retry is a new token. See `components/people.tsx` for
 * the dialogs a seat's page offers too.
 */

import { useMemo, useState } from "react";
import { Button, FormField, Input, Modal, Select, Text } from "@crewlethq/ui";
import { KeyGlyph, PencilGlyph, UserPlusGlyph } from "@crewlethq/icons/glyphs";
import { TOKEN_WITHHELD_GRANTS } from "~/contract/identity.ts";
import {
  GrantPicker,
  IamOutcome,
  NO_SEAT,
  pressLabel,
  seatOptions,
  ShownOnce,
  useUnheldSeats,
} from "~/components/people.tsx";
import { fmtDateTime } from "~/lib/format.ts";
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
      <MintTokenDialog
        owner={{ id, kind: "machine", login: login.trim(), grants }}
        held={held}
        onClose={onClose}
      />
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
 * Mint a token for a service account: a label, a lifetime, and grants out of
 * the account's own — never one a token may not carry, nor one the minter does
 * not hold. The value is shown ONCE; the engine keeps a hash of it.
 */
export function MintTokenDialog({
  owner,
  held,
  onClose,
  onDone,
}: {
  owner: EditableRow;
  held: readonly string[];
  onClose: () => void;
  onDone?: () => void;
}) {
  const write = useIamGesture();
  const [label, setLabel] = useState("");
  const [days, setDays] = useState("");
  // WHAT STARTS TICKED is what this mint may send: the account's grants a
  // token may carry AND the minter holds — the engine refuses a token carrying
  // a grant its minter does not hold, and the picker locks such a box, so one
  // ticked here would be a refusal nobody could clear.
  const carried = useMemo(
    () =>
      (owner.grants ?? []).filter(
        (g) => !(TOKEN_WITHHELD_GRANTS as readonly string[]).includes(g) && held.includes(g),
      ),
    [owner.grants, held],
  );
  const [grants, setGrants] = useState<string[]>(carried);
  const minted = write.answer?.kind === "done" ? write.answer.body : null;
  const token = typeof minted?.token === "string" ? minted.token : "";
  const expires = typeof minted?.expires_at === "string" ? minted.expires_at : "";
  const lifetime = days.trim() === "" ? 0 : Number(days);
  const badDays = days.trim() !== "" && (!Number.isInteger(lifetime) || lifetime < 1);

  async function submit() {
    if (minted || badDays) return;
    const answer = await write.run(
      {
        method: "POST",
        path: "/iam/credentials",
        query: { person: owner.id },
        body: {
          ...(label.trim() ? { label: label.trim() } : {}),
          ...(lifetime > 0 ? { expires_in_days: lifetime } : {}),
          grants,
        },
      },
      // A MINT READS NO KEY: a replay could not hand back a value its first
      // attempt never showed, so a retry is a new token.
      false,
    );
    if (answer) onDone?.();
  }

  return (
    <Modal
      open
      size="md"
      title={`Mint a token for ${owner.login || owner.id}`}
      icon={<KeyGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      closeDisabledReason="Waiting for the engine to answer."
      stackBody
      onSubmit={() => void submit()}
      footer={
        minted ? (
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose} disabled={write.busy}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" disabled={write.busy || badDays}>
              {pressLabel(write, "Mint", "Minting")}
            </Button>
          </>
        )
      }
    >
      {minted ? (
        <ShownOnce label="Token" value={token}>
          This token acts as {owner.login || "the account"} until {fmtDateTime(expires)}. It is
          shown only now and cannot be read back; present it as{" "}
          <span className="mono">Authorization: Bearer …</span>.
        </ShownOnce>
      ) : (
        <>
          <FormField label="Label" optional helper="What it is for, as its row will say.">
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                autoFocus
                width="full"
                value={label}
                onChange={(e) => setLabel(e.target.value)}
              />
            )}
          </FormField>
          <FormField
            label="Expires in days"
            optional
            helper="Empty takes the engine's default of 90 days; at most 365."
            error={badDays ? "A whole number of days, at least one." : undefined}
          >
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                inputMode="numeric"
                width="full"
                value={days}
                onChange={(e) => setDays(e.target.value)}
              />
            )}
          </FormField>
          <GrantPicker
            legend="Grants the token carries"
            value={grants}
            onChange={setGrants}
            held={held}
            only={owner.grants ?? []}
            withheld={{
              grants: TOKEN_WITHHELD_GRANTS,
              reason: "A token never carries this: it needs a person present.",
            }}
          />
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
 */
export function EditPersonDialog({
  row,
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
  const [login, setLogin] = useState(row.login ?? "");
  const [seat, setSeat] = useState(row.seat ?? NO_SEAT);
  const [grants, setGrants] = useState<string[]>(row.grants ?? []);
  // WHAT THIS EDIT MAY CONFER is the engine's rule: only a grant it ADDS needs
  // the editor to hold it, so one the person already holds may be kept or
  // taken away by an administrator who does not hold it — stripping a
  // compromised colleague's config:write needs nobody to hold config:write.
  const conferrable = useMemo(() => [...held, ...(row.grants ?? [])], [held, row.grants]);
  // THEIR OWN SEAT STAYS OFFERED beside the vacant ones: nobody else holds it,
  // but it is held, so the vacancies list leaves it out.
  const options = useMemo(() => {
    const vacant = seats.data ?? [];
    const own = row.seat && !vacant.some((s) => s.handle === row.seat);
    return seatOptions(own ? [{ handle: row.seat!, name: row.seat! }, ...vacant] : vacant);
  }, [seats.data, row.seat]);

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
