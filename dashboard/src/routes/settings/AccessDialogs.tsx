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
  useSeatEntry,
  useUnheldSeats,
} from "~/components/people.tsx";
import { TOKEN_WITHHELD_GRANTS } from "~/contract/identity.ts";
import { useIamGesture } from "~/lib/iamWrite.ts";
import { loginProblem } from "~/lib/login.ts";
import { useWaiting } from "~/lib/waiting.ts";

/**
 * Why a machine is offered neither of the grants a token never carries: it has
 * no password and no session, so it acts only through tokens, and the engine
 * refuses either on a machine — offered as ordinary boxes, a service account
 * was created holding `people:manage` it could never exercise.
 */
const MACHINE_WITHHELD =
  "A service account acts only through tokens, and a token never carries this: it needs a person present.";

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
  const waiting = useWaiting();
  const [login, setLogin] = useState("");
  const [name, setName] = useState("");
  const [grants, setGrants] = useState<string[]>([]);
  const [minting, setMinting] = useState(false);
  const [tried, setTried] = useState(false);
  const loginWrong = loginProblem("machine", login.trim());
  const created = write.answer?.kind === "done" ? write.answer.body : null;
  const id = typeof created?.id === "string" ? created.id : "";

  if (created && minting) {
    return (
      <MintTokenDialog owner={{ id, login: login.trim(), grants }} held={held} onClose={onClose} />
    );
  }

  async function submit() {
    setTried(true);
    if (login.trim() === "" || loginWrong || created) return;
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
      closeDisabledReason={waiting.reason}
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
              {pressLabel(write, "Create", "Creating", waiting.asked)}
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
            error={tried ? (loginWrong ?? undefined) : undefined}
          >
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                aria-invalid={field.invalid || undefined}
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
          <GrantPicker
            value={grants}
            onChange={setGrants}
            held={held}
            withheld={{ grants: TOKEN_WITHHELD_GRANTS, reason: MACHINE_WITHHELD }}
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
 *
 * WHAT CHANGED IS MEASURED AGAINST THE ROW THE DIALOG OPENED ON, never the
 * live one: the directory is read again every minute and when the tab comes
 * back, and the fields hold what the dialog opened with, so measured against
 * a newer row a field nobody touched reads as an edit — and sending it puts
 * back a seat or a login another administrator has just changed. After a
 * refusal part way, what landed is sent again equal to what the engine holds,
 * and the engine skips it.
 *
 * THE GRANTS GO AS WHAT WAS TICKED AND UNTICKED (`add_grants`,
 * `remove_grants`), which the engine applies to what the person holds when it
 * decides — never as the whole list, whose untouched grants are the ones the
 * dialog opened with: sent whole, a grant another administrator took away
 * meanwhile comes back with any other tick, and one they gave goes.
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
  const waiting = useWaiting();
  const seats = useUnheldSeats();
  const [row] = useState(live);
  const [login, setLogin] = useState(row.login ?? "");
  const [seat, setSeat] = useState(row.seat ?? NO_SEAT);
  const [grants, setGrants] = useState<string[]>(row.grants ?? []);
  const [tried, setTried] = useState(false);
  // THE KIND'S OWN GRAMMAR, asked only of a login this edit changes: the
  // one the row holds is the engine's already.
  const changesLogin = login.trim() !== (row.login ?? "");
  const loginWrong = changesLogin
    ? loginProblem(row.kind === "machine" ? "machine" : "person", login.trim())
    : null;
  // WHAT THIS EDIT MAY CONFER is the engine's rule: only a grant it ADDS needs
  // the editor to hold it, so one the person already holds may be kept or
  // taken away by an administrator who does not hold it — stripping a
  // compromised colleague's config:write needs nobody to hold config:write.
  const conferrable = useMemo(() => [...held, ...(row.grants ?? [])], [held, row.grants]);
  // THEIR OWN SEAT STAYS OFFERED beside the vacant ones: nobody else holds it,
  // but it is held, so the vacancies list leaves it out. The seat they hold
  // NOW, which the vacancies are read beside — an edit that landed its seat
  // part way holds the new one, and the one it left is vacant again.
  const entry = useSeatEntry();
  const options = useMemo(() => {
    const vacant = seats.data ?? [];
    const own = live.seat && !vacant.some((s) => s.handle === live.seat);
    return seatOptions(own ? [entry(live.seat!), ...vacant] : vacant);
  }, [seats.data, live.seat, entry]);

  const before = row.grants ?? [];
  const added = grants.filter((g) => !before.includes(g));
  const removed = before.filter((g) => !grants.includes(g));
  const change: Record<string, unknown> = {
    ...(changesLogin ? { login: login.trim() } : {}),
    ...(seat !== (row.seat ?? NO_SEAT) ? { seat } : {}),
    ...(added.length > 0 ? { add_grants: added } : {}),
    ...(removed.length > 0 ? { remove_grants: removed } : {}),
  };
  const nothing = Object.keys(change).length === 0;

  async function submit() {
    setTried(true);
    if (nothing || login.trim() === "" || loginWrong) return;
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
      closeDisabledReason={waiting.reason}
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
            {pressLabel(write, "Save", "Saving", waiting.asked)}
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
        error={tried ? (loginWrong ?? undefined) : undefined}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid || undefined}
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
      <GrantPicker
        value={grants}
        onChange={setGrants}
        held={conferrable}
        withheld={
          // A MACHINE IS GIVEN NEITHER, and the engine judges what an edit
          // ADDS: one it already holds stays a box, so it can be taken away.
          row.kind === "machine"
            ? {
                grants: TOKEN_WITHHELD_GRANTS.filter((g) => !before.includes(g)),
                reason: MACHINE_WITHHELD,
              }
            : undefined
        }
      />
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}
