/**
 * People's own write dialogs: a service account, and an edit of
 * somebody's login, seat and access.
 *
 * Each is one write through `lib/iamWrite.ts` — a create is keyed so the retry
 * an unknown answer asks for names what its first attempt made. See
 * `components/people.tsx` for the dialogs a seat offers — an invitation and a
 * person's create, which every person comes from, since every person holds a
 * human seat — and a token's mint (the Account page too).
 *
 * # "No seat" is a service account's answer only
 *
 * A person holds exactly one human seat for as long as they exist: their seat
 * is changed among the vacant ones and never cleared, and the engine refuses
 * an edit that clears it (`seat_required`). A service account may hold one or
 * none, so its dialogs offer both.
 */

import { ORG_EDIT_PATH } from "~/app/routes.ts";
import { useMemo, useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  FormField,
  InlineCode,
  Input,
  Modal,
  Select,
  Skeleton,
  Text,
} from "@crewlethq/ui";
import { PencilGlyph, SendGlyph, UserPlusGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import {
  AccessPicker,
  AppReachNote,
  IamOutcome,
  InviteDialog,
  MintTokenDialog,
  NO_SEAT,
  pressLabel,
  seatOptions,
  useSeatEntry,
  useUnheldSeats,
} from "~/components/people.tsx";
import { ADMIN, expand } from "~/lib/access.ts";
import { useIamGesture } from "~/lib/iamWrite.ts";
import { loginProblem } from "~/lib/login.ts";
import { useWaiting } from "~/lib/waiting.ts";
import { pathSegment } from "~/protocol/rest.ts";

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
 * A new service account — a MACHINE: a login in the colon grammar, a name,
 * its access and, if it should act as one, a human seat nothing holds — and
 * then, once it exists, the offer to mint its token.
 *
 * ITS ACCESS STARTS EMPTY: a service account is a credential somebody
 * scripts, and what it may do is decided on purpose rather than defaulted —
 * the engine takes no default for one either. It may hold Admin, which its
 * tokens then carry for configuration; the engine still refuses a token every
 * administrative record about a person and every gesture that needs a person
 * present.
 *
 * THE SEAT GOES IN THE SAME RECORD as the account, which the engine takes:
 * created bare and bound after, a pipeline meant to act as a seat acted as
 * itself until a second write landed.
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
  const seats = useUnheldSeats();
  const [login, setLogin] = useState("");
  const [name, setName] = useState("");
  const [seat, setSeat] = useState(NO_SEAT);
  const [grants, setGrants] = useState<string[]>([]);
  const [minting, setMinting] = useState(false);
  const [tried, setTried] = useState(false);
  const loginWrong = loginProblem("machine", login.trim());
  const options = useMemo(() => seatOptions(seats.data ?? [], { none: true }), [seats.data]);
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
      body: {
        kind: "machine",
        login: login.trim(),
        name: name.trim(),
        ...(seat !== NO_SEAT ? { seat } : {}),
        grants,
      },
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
              Create token
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
          <Text variant="inherit" mono>
            {login.trim()}
          </Text>{" "}
          created
          {seat !== NO_SEAT
            ? `, acting as the seat ${options.find((o) => o.value === seat)?.label ?? seat}`
            : ""}
          . It signs in with a token.
        </Text>
      ) : (
        <>
          <FormField
            label="Login"
            helper="Lowercase words with colons, like ci:release."
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
          <FormField label="Seat" optional helper="Optional. Pick an open seat for it to act as.">
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
          {seats.error && (
            <Text as="p" variant="caption" tone="secondary">
              The seats nothing holds could not be read: {seats.error.message}
            </Text>
          )}
          <AccessPicker value={grants} onChange={setGrants} held={held} />
        </>
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}

/**
 * Invite a person from People: pick an open seat, then the seat's own
 * invitation ([InviteDialog]).
 *
 * A PERSON STILL COMES FROM A SEAT (ADR-0032): the picker offers only the
 * seats nothing holds (`?unheld=true`), which are exactly the ones an
 * invitation can name, so the second step is the dialog the chart, the seat's
 * peek and its page open. Inviting from the chart was the only way in, a text
 * link to another page for the most common task on this one.
 */
export function InvitePersonDialog({
  held,
  onClose,
  onDone,
}: {
  /** The viewer's own grants. */
  held: readonly string[];
  onClose: () => void;
  /** After every answer, so the lists are read again. */
  onDone: () => void;
}) {
  const seats = useUnheldSeats();
  const [seat, setSeat] = useState(NO_SEAT);
  const [chosen, setChosen] = useState<{ handle: string; name: string } | null>(null);
  const open = useMemo(() => seats.data ?? [], [seats.data]);
  const options = useMemo(() => seatOptions(open), [open]);

  if (chosen) {
    return <InviteDialog seat={chosen} held={held} onClose={onClose} onDone={onDone} />;
  }

  const pick = () => {
    const row = open.find((s) => s.handle === seat);
    if (row) setChosen({ handle: row.handle, name: row.name });
  };

  return (
    <Modal
      open
      size="md"
      title="Invite person"
      icon={<SendGlyph />}
      onClose={onClose}
      stackBody
      onSubmit={pick}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={seat === NO_SEAT}>
            Continue
          </Button>
        </>
      }
    >
      {seats.loading && !seats.data ? (
        <Skeleton variant="text" rows={2} label="Loading the open seats" />
      ) : !seats.data ? (
        <QueryState
          error={seats.code}
          refusal={seats.refusal}
          detail={seats.error?.detail || undefined}
          loading={false}
        />
      ) : open.length === 0 ? (
        <Callout
          variant="neutral"
          action={
            <ButtonLink
              size="small"
              variant="secondary"
              href={href(ORG_EDIT_PATH, { add: "human" })}
            >
              Add a human seat
            </ButtonLink>
          }
        >
          No open seats. Add a human seat in Edit org.
        </Callout>
      ) : (
        <FormField label="Seat" helper="Pick an open seat. They join as this seat.">
          {(field) => (
            <Select
              id={field.id}
              ariaLabel="Seat"
              searchable
              value={seat}
              placeholder="Choose a seat"
              options={options}
              onChange={(next) => setSeat(String(next))}
            />
          )}
        </FormField>
      )}
    </Modal>
  );
}

/**
 * What a gesture on the reader's OWN row says first: that it is them, and what
 * it does to them — `what`, by default that it signs this browser out at once —
 * and, where it stops them administering and nobody else could, that only a
 * Tier A token could afterwards.
 *
 * The panel names a row by its person, in the third person, and a person
 * suspending "Jane Doe" from Jane Doe's own session was told none of it. AND
 * THE WHOLE DIALOG SPEAKS TO THEM there, not only this callout and the title:
 * a body saying "Jane Doe may not act while suspended" under "This is you"
 * read as two people.
 */
export function OnYourself({
  alone = false,
  what = "It signs you out of this browser at once.",
}: {
  alone?: boolean;
  what?: string;
}) {
  return (
    <Callout variant="warning" title="This is you">
      {what}
      {alone && (
        <>
          {what && " "}
          You’re the only admin; after this, only an API token (
          <InlineCode>api.auth.tokens</InlineCode>) can manage people.
        </>
      )}
    </Callout>
  );
}

/**
 * Change somebody's login, seat or access — one `PATCH`, carrying only what
 * changed. The engine moves the login and the seat in one record before the
 * access, and a refusal part way names what had already landed.
 *
 * WHAT CHANGED IS MEASURED AGAINST THE ROW THE DIALOG OPENED ON, never the
 * live one: the directory is read again every minute and when the tab comes
 * back, and the fields hold what the dialog opened with, so measured against
 * a newer row a field nobody touched reads as an edit — and sending it puts
 * back a seat or a login another administrator has just changed. After a
 * refusal part way, what landed is sent again equal to what the engine holds,
 * and the engine skips it.
 *
 * THE ACCESS GOES AS WHAT WAS TICKED AND UNTICKED (`add_grants`,
 * `remove_grants`), which the engine applies to what the person holds when it
 * decides — never as the whole list, whose untouched names are the ones the
 * dialog opened with: sent whole, a permission another administrator took
 * away meanwhile comes back with any other tick, and one they gave goes.
 * Ticking Admin adds every app beside it, and unticking it keeps them
 * (`components/people.tsx`' `AccessPicker`), so taking Admin away leaves the
 * person their apps.
 */
export function EditPersonDialog({
  row: live,
  held,
  you = false,
  alone = false,
  onClose,
  onDone,
}: {
  row: EditableRow;
  held: readonly string[];
  /** Whether this row is the person reading it. */
  you?: boolean;
  /** Whether they are the one active administrator left (`OnYourself`). */
  alone?: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const write = useIamGesture();
  const waiting = useWaiting();
  const seats = useUnheldSeats();
  const [row] = useState(live);
  const machine = row.kind === "machine";
  const [login, setLogin] = useState(row.login ?? "");
  const [seat, setSeat] = useState(row.seat ?? NO_SEAT);
  const [grants, setGrants] = useState<string[]>(row.grants ?? []);
  const [tried, setTried] = useState(false);
  // THE KIND'S OWN GRAMMAR, asked only of a login this edit changes: the
  // one the row holds is the engine's already.
  const changesLogin = login.trim() !== (row.login ?? "");
  const loginWrong = changesLogin
    ? loginProblem(machine ? "machine" : "person", login.trim())
    : null;
  // WHAT THIS EDIT MAY CONFER is the engine's rule: only what it ADDS needs
  // the editor to hold it, so what the person already holds may be kept or
  // taken away by an editor who does not hold it — and an edit is an admin's,
  // who holds everything, so in practice this widens only to a name this
  // build does not know.
  const conferrable = useMemo(
    () => [...expand(held), ...expand(row.grants ?? [])],
    [held, row.grants],
  );
  // THEIR OWN SEAT STAYS OFFERED beside the vacant ones: nobody else holds it,
  // but it is held, so the vacancies list leaves it out. The seat they hold
  // NOW, which the vacancies are read beside — an edit that landed its seat
  // part way holds the new one, and the one it left is vacant again. "No
  // seat" is a MACHINE's only: see the file's doc.
  const entry = useSeatEntry();
  const options = useMemo(() => {
    const vacant = seats.data ?? [];
    const own = live.seat && !vacant.some((s) => s.handle === live.seat);
    return seatOptions(own ? [entry(live.seat!), ...vacant] : vacant, { none: machine });
  }, [seats.data, live.seat, entry, machine]);

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

  // DONE IS DONE, a 202 included — see `ConfirmDialog`: Save sent the same
  // change again under a new key.
  const done = write.answer?.kind === "done";

  async function submit() {
    setTried(true);
    if (done || nothing || login.trim() === "" || loginWrong) return;
    const answer = await write.run({
      method: "PATCH",
      path: `/iam/people/${pathSegment(row.id)}`,
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
        done ? (
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose} disabled={write.busy}>
              Cancel
            </Button>
            <Button
              type="submit"
              variant="primary"
              disabled={write.busy || nothing || login.trim() === ""}
            >
              {pressLabel(write, "Save", "Saving", waiting.asked)}
            </Button>
          </>
        )
      }
    >
      <FormField
        label="Login"
        helper={
          machine
            ? "Lowercase words with colons, like ci:release."
            : "Lowercase words with dots, like jane.doe."
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
        helper={
          machine ? "Pick an open seat for it to act as. No seat unbinds it." : "Pick an open seat."
        }
      >
        {(field) => (
          <Select
            id={field.id}
            ariaLabel="Seat"
            searchable
            value={seat}
            placeholder="Choose a seat"
            options={options}
            onChange={(next) => setSeat(String(next))}
          />
        )}
      </FormField>
      <AccessPicker value={grants} onChange={setGrants} held={conferrable} kept={before} />
      {!machine && <AppReachNote grants={grants} you={you} />}
      {/* TAKING YOUR OWN ADMIN stops you administering once it saves — and
          where nobody else active holds it, anybody but a Tier A token. It was
          the one gesture on your own row that said neither. */}
      {you && removed.includes(ADMIN) && (
        <OnYourself alone={alone} what="You’ll lose admin access when you save." />
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}
