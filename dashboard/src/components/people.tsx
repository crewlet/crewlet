/**
 * The identity directory's writes that more than one screen offers: inviting
 * somebody (Settings › People & access, and a human seat nobody holds), minting
 * a token (a service account's there, your own on the Account page), the grants
 * a write confers, a value shown once, and what a write came to.
 *
 * Every write goes through `lib/iamWrite.ts`, so the step-up, the operation key
 * a retry sends and the four answers are decided once. And they are offered to
 * a reader holding `people:manage` only — the grant every one of them is
 * refused without ([canManagePeople]). People & access is read by auditors
 * (`audit:read`) as well, and a dozen controls disabled with one reason is
 * noise on the record they came to read; the engine refuses whatever reaches
 * it all the same.
 */

import { useCallback, useMemo, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  Checkbox,
  Copyable,
  EmptyValue,
  FormField,
  InlineCode,
  Input,
  Modal,
  Select,
  Tag,
  Text,
  type SelectOption,
} from "@crewlethq/ui";
import { KeyGlyph, UserPlusGlyph } from "@crewlethq/icons/glyphs";
import { DateCell } from "~/app/frame/cells.tsx";
import {
  GRANTS,
  TOKEN_DEFAULT_DAYS,
  TOKEN_MAX_DAYS,
  TOKEN_WITHHELD_GRANTS,
} from "~/contract/identity.ts";
import { fmtDateTime, tsKey } from "~/lib/format.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useIamGesture, type IamAnswer, type IamGesture } from "~/lib/iamWrite.ts";
import { useWaiting } from "~/lib/waiting.ts";
import { useRest } from "~/lib/useRest.ts";
import { rest } from "~/protocol/index.ts";

/** The grant every directory write is decided on. */
export const PEOPLE_MANAGE = "people:manage";

/**
 * The word for a credential the engine reports `revoked`: USED for a reset
 * link its person spent, which setting the password revokes as well — read as
 * "Revoked" it said somebody had withdrawn the link they had just used.
 * EXPIRED only past its own deadline with nobody's revocation stamped on it. A
 * token ended by a counter — its owner changing their password or signing out
 * everywhere, or a restore — carries neither stamp, and it was revoked rather
 * than aged out.
 */
export function endedWord(
  c: { revoked_at?: string; expires_at?: string; spent?: boolean },
  now: number,
): "Revoked" | "Expired" | "Used" {
  if (c.spent) return "Used";
  return !c.revoked_at && c.expires_at && tsKey(c.expires_at) <= now ? "Expired" : "Revoked";
}

/**
 * When a credential stops working: its deadline, none for one that has none —
 * and NONE for one already ended some other way. A used reset link and a
 * revoked token keep the deadline they were issued with, and it read "in 1d"
 * beside "Used", counting down to an end the credential will never reach. One
 * that EXPIRED did reach it, so its deadline is shown.
 */
export function ExpiresCell({
  c,
  now,
}: {
  c: { revoked?: boolean; revoked_at?: string; expires_at?: string; spent?: boolean };
  now: number;
}) {
  if (c.revoked && endedWord(c, now) !== "Expired") return <EmptyValue label="Already ended" />;
  return c.expires_at ? (
    <DateCell at={c.expires_at} now={now} />
  ) : (
    <EmptyValue label="Does not expire" />
  );
}

/** Whether a reader's grants reach the directory's writes. */
export function canManagePeople(grants: readonly string[]): boolean {
  return grants.includes(PEOPLE_MANAGE);
}

/** What each grant opens, in a line — beside the grant's own name, never instead of it. */
export const GRANT_WORDS: Record<(typeof GRANTS)[number], string> = {
  "state:read": "Read the company's state: the roster, work, pages, spend",
  "audit:read": "Read transcripts, events and the identity trail",
  "config:read": "Read the company configuration",
  "secrets:read": "Reveal a stored credential's value",
  "work:write": "File and change work items",
  "knowledge:write": "Write knowledge-base pages",
  "config:write": "Change the company configuration — host access, by design",
  "secrets:write": "Store and remove credentials",
  "fleet:operate": "Operate the deployment: backups, retention, the node gate",
  "people:manage": "Invite, change and remove people — the grant that grants",
  "sandbox:run": "Start code-sandbox runs",
};

/** Grants as written — a directory row's, or what a token carries. */
export function GrantTags({ grants }: { grants?: readonly string[] | null }) {
  if (!grants || grants.length === 0) return <EmptyValue label="No grants" />;
  return (
    <span className="row gap-1" style={{ flexWrap: "wrap" }}>
      {grants.map((g) => (
        <Tag key={g} size="sm" appearance="outline">
          <span className="mono">{g}</span>
        </Tag>
      ))}
    </span>
  );
}

/**
 * Checkboxes over the grants. One outside `held` is drawn disabled with why —
 * the engine refuses to confer it anyway (a caller may not confer a grant they
 * do not hold), and a box that only fails on submit is a guess. `only` narrows
 * the offer (a token carries a subset of its owner's), and `withheld` greys
 * out what the engine refuses outright, with its reason.
 */
export function GrantPicker({
  value,
  onChange,
  held,
  only,
  withheld,
  legend = "Grants",
}: {
  value: readonly string[];
  onChange: (next: string[]) => void;
  /**
   * What this write may confer: the viewer's own grants — and, on an edit,
   * what the person already holds, which keeping or taking away confers
   * nothing (the engine checks only what an edit ADDS).
   */
  held: readonly string[];
  only?: readonly string[];
  withheld?: { grants: readonly string[]; reason: string };
  legend?: string;
}) {
  const offered = GRANTS.filter((g) => !only || only.includes(g));
  return (
    <fieldset className="col gap-1" style={{ border: 0, margin: 0, padding: 0 }}>
      <legend className="t-label">{legend}</legend>
      {offered.length === 0 && (
        <Text as="p" variant="caption" tone="secondary">
          Nothing to offer.
        </Text>
      )}
      {offered.map((g) => {
        const refused = withheld?.grants.includes(g)
          ? withheld.reason
          : held.includes(g)
            ? null
            : "You do not hold this grant, so you cannot confer it.";
        return (
          <Checkbox
            key={g}
            label={<span className="mono">{g}</span>}
            description={refused ?? GRANT_WORDS[g]}
            checked={value.includes(g)}
            disabled={refused !== null}
            onCheckedChange={(on) => onChange(on ? [...value, g] : value.filter((x) => x !== g))}
          />
        );
      })}
    </fieldset>
  );
}

/**
 * What a person conferred these grants will be able to open, said where it is
 * only their Account: every other screen reads the live view, and the engine
 * refuses its socket to anybody without `state:read`. An invitation issued
 * with nothing ticked used to go out with no word, and its person landed on a
 * dashboard that could show them nothing.
 */
export function ReachNote({ grants }: { grants: readonly string[] }) {
  if (grants.includes("state:read")) return null;
  return (
    <Callout variant="warning">
      Without <span className="mono">state:read</span> they can open nothing but their own Account:
      every other screen reads the company&apos;s live state, which needs it.
    </Callout>
  );
}

/**
 * A value the engine shows once — an invitation's link, a reset link, a token —
 * with a copy control and the sentence saying what it is. Nothing keeps it:
 * closing the dialog is the last anybody sees of it.
 */
export function ShownOnce({
  label,
  value,
  children,
}: {
  label: string;
  value: string;
  /** What the value is, and how long it lasts. */
  children: React.ReactNode;
}) {
  return (
    <div className="col gap-2">
      <FormField label={label}>
        {() => <Copyable value={value} variant="block" monospace ariaLabel={`Copy the ${label}`} />}
      </FormField>
      <Callout variant="warning">{children}</Callout>
    </div>
  );
}

/** What a write came to, where it is not simply done. */
export function IamOutcome({ answer }: { answer: IamAnswer | null }) {
  if (!answer) return null;
  if (answer.kind === "refused") {
    return (
      <Callout variant="danger" role="alert">
        {answer.text}
      </Callout>
    );
  }
  if (answer.kind === "unknown") {
    return (
      <Callout variant="warning" role="alert" title="Not confirmed">
        {answer.text}
      </Callout>
    );
  }
  if (answer.pending) {
    return (
      <Callout variant="neutral" role="status">
        Recorded. This node has not applied it yet, so the lists here catch up in a moment.
      </Callout>
    );
  }
  return null;
}

/** The label a write's button carries: a retry after an unknown answer says so. */
export function pressLabel(
  write: IamGesture,
  idle: string,
  busy: string,
  /** A step-up is asking the person (`useWaiting`), which is what the write waits on. */
  asked = false,
): string {
  if (write.busy) return asked ? "Waiting for you" : busy;
  return write.answer?.kind === "unknown" ? "Try again" : idle;
}

/** One human seat of the running company and who holds it (`GET /iam/seats`). */
export interface HumanSeat {
  handle: string;
  name: string;
  unit?: string;
  holder?: { person: string; login?: string; stage?: string };
}

/** The seats an invitation, a create or a bind may name: those nobody holds. */
export function useUnheldSeats() {
  return useRest("/iam/seats?unheld=true", async (signal) => {
    const answer = (await rest.get("/iam/seats?unheld=true", signal)) as {
      seats?: HumanSeat[] | null;
    } | null;
    return answer?.seats ?? [];
  });
}

/**
 * A seat the vacancy list leaves out — the one a person holds now, or one a
 * dialog proposes before the list has arrived — named as the running org chart
 * names it. Named by its handle, it was offered as "jane-founder / jane-founder"
 * beside every vacancy's name.
 */
export function useSeatEntry(): (handle: string) => HumanSeat {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  return useCallback(
    (handle: string) => ({ handle, name: index.byHandle.get(handle)?.name ?? handle }),
    [index],
  );
}

/** The value of the "nobody" option in a seat select. */
export const NO_SEAT = "";

/** A seat select's options: nobody, then each seat by name with its handle. */
export function seatOptions(seats: readonly HumanSeat[]): SelectOption[] {
  return [
    { value: NO_SEAT, label: "No seat", description: "Acts under their own login" },
    ...seats.map((s) => ({
      value: s.handle,
      label: s.name || s.handle,
      description: s.handle,
      text: `${s.name} ${s.handle}`,
    })),
  ];
}

/**
 * Invite somebody: an address, the seat redeeming it binds them to, and the
 * grants it confers. On success the link is shown ONCE — it is the credential,
 * and the engine keeps only a hash of its secret.
 */
export function InviteDialog({
  held,
  seat = NO_SEAT,
  onClose,
  onDone,
}: {
  /** The viewer's own grants. */
  held: readonly string[];
  /** A seat to propose — the seat page's own. */
  seat?: string;
  onClose: () => void;
  /** After every answer, so the lists are read again. */
  onDone: () => void;
}) {
  const write = useIamGesture();
  const waiting = useWaiting();
  const seats = useUnheldSeats();
  const entry = useSeatEntry();
  const [email, setEmail] = useState("");
  const [bind, setBind] = useState(seat);
  const [grants, setGrants] = useState<string[]>([]);
  const options = useMemo(() => {
    const list = seats.data ?? [];
    // THE PROPOSED SEAT STAYS OFFERED while the read is out, so a dialog
    // opened from a seat's page does not drop it on the first render.
    const named = list.some((s) => s.handle === seat) || seat === NO_SEAT;
    return seatOptions(named ? list : [...list, entry(seat)]);
  }, [seats.data, seat, entry]);

  const done = write.answer?.kind === "done" ? write.answer.body : null;
  const url = typeof done?.url === "string" ? done.url : "";
  const expires = typeof done?.expires_at === "string" ? done.expires_at : "";

  async function submit() {
    if (email.trim() === "" || done) return;
    const answer = await write.run({
      method: "POST",
      path: "/iam/invitations",
      body: { email: email.trim(), grants, ...(bind ? { seat: bind } : {}) },
    });
    if (answer) onDone();
  }

  return (
    <Modal
      open
      size="md"
      title="Invite a person"
      icon={<UserPlusGlyph />}
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
            <Button type="submit" variant="primary" disabled={write.busy || email.trim() === ""}>
              {pressLabel(write, "Invite", "Inviting", waiting.asked)}
            </Button>
          </>
        )
      }
    >
      {done ? (
        <ShownOnce label="Invitation link" value={url}>
          This link is the credential: it works once, for {email.trim()}, and expires{" "}
          {fmtDateTime(expires)}. It is shown only now — send it yourself; this engine sends no
          mail. Opening it is where they choose a login and a password.
        </ShownOnce>
      ) : (
        <>
          <FormField label="Email address">
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                type="email"
                autoFocus
                width="full"
                spellCheck={false}
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            )}
          </FormField>
          <FormField
            label="Seat"
            optional
            helper={
              // EVERY SEAT HELD IS A FACT, said where the choice is: the
              // select offered only "No seat" under a helper describing the
              // seat it would bind. Only on an ANSWER — a list still out, or
              // one that failed (said below), is no such fact.
              seats.data?.length === 0 && bind === NO_SEAT
                ? "Every human seat is held, so the person joins bound to no seat — add a human seat to the org chart first if they should act as one."
                : "A human seat nobody holds. Redeeming the invitation binds them to it, so they act as that seat."
            }
          >
            {(field) => (
              <Select
                id={field.id}
                ariaLabel="Seat"
                searchable
                value={bind}
                options={options}
                onChange={(next) => setBind(String(next))}
              />
            )}
          </FormField>
          {seats.error && (
            <Text as="p" variant="caption" tone="secondary">
              The seats nobody holds could not be read: {seats.error.message}
            </Text>
          )}
          <GrantPicker value={grants} onChange={setGrants} held={held} />
          <ReachNote grants={grants} />
        </>
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}

/**
 * A gesture that needs only a yes: what it does, said before it is done, and
 * — for a removal — the login typed back, because it cannot be undone.
 */
export function ConfirmDialog({
  title,
  confirm,
  dismiss = "Cancel",
  danger,
  typeToConfirm,
  write,
  onConfirm,
  onClose,
  children,
}: {
  title: string;
  /** The button's word: "Suspend", "Remove". */
  confirm: string;
  /**
   * The word on the button that does nothing, for a gesture whose own word
   * is "Cancel": "Cancel" beside "Cancel invitation" kept the invitation for
   * somebody who pressed it meaning to cancel it.
   */
  dismiss?: string;
  danger?: boolean;
  /** A value the person types to confirm, where the gesture cannot be undone. */
  typeToConfirm?: string;
  write: IamGesture;
  onConfirm: () => void;
  onClose: () => void;
  children: React.ReactNode;
}) {
  const [typed, setTyped] = useState("");
  const waiting = useWaiting();
  const ready = !typeToConfirm || typed.trim() === typeToConfirm;
  // REFUSED AS STALE, what this was about has moved on — an invitation
  // redeemed in the meantime — and the same request is refused the same
  // however often it is pressed. So the dialog offers only a way out: it kept
  // its dismissal and an enabled confirm that sent the same 409 again.
  const stale = write.answer?.kind === "refused" && write.answer.stale;
  return (
    <Modal
      open
      size="sm"
      title={title}
      onClose={onClose}
      dismissable={!write.busy}
      closeDisabledReason={waiting.reason}
      stackBody
      onSubmit={() => ready && !stale && onConfirm()}
      footer={
        stale ? (
          <Button variant="primary" onClick={onClose}>
            Close
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose} disabled={write.busy}>
              {dismiss}
            </Button>
            <Button
              type="submit"
              variant={danger ? "danger" : "primary"}
              disabled={write.busy || !ready}
            >
              {pressLabel(write, confirm, "Working", waiting.asked)}
            </Button>
          </>
        )
      }
    >
      <div className="t-body">{children}</div>
      {typeToConfirm && (
        // NOT A FormField, whose label is set in capitals: the value is typed
        // exactly, so it is shown exactly. Read as ROBERT.SMITH, typing that
        // kept the button disabled with no word why.
        <label className="col" style={{ gap: 6 }}>
          <span className="t-caption">
            Type <InlineCode>{typeToConfirm}</InlineCode> to confirm
          </span>
          <Input
            autoFocus
            width="full"
            spellCheck={false}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
          />
        </label>
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}

/** Whose token a mint makes: the directory row, as much of it as a mint needs. */
export interface TokenOwner {
  id: string;
  login?: string;
  grants?: string[] | null;
}

/**
 * Mint a token: a label, a lifetime, and grants out of the owner's own — never
 * one a token may not carry, nor one the minter does not hold. The value is
 * shown ONCE; the engine keeps a hash of it.
 *
 * TWO OWNERS, ONE DIALOG. An administrator mints for a SERVICE ACCOUNT, named
 * by `?person=`; a person mints their OWN (`self`) with no `?person=` at all,
 * from their own session — the route's owner is then the caller, and the
 * engine refuses anybody minting on another person's account, because whoever
 * mints a token is shown a value that acts as its owner.
 */
export function MintTokenDialog({
  owner,
  held,
  self = false,
  onClose,
  onDone,
}: {
  owner: TokenOwner;
  /** The minter's own grants: a token carries nothing its minter does not hold. */
  held: readonly string[];
  /** The caller minting their own personal access token. */
  self?: boolean;
  onClose: () => void;
  onDone?: () => void;
}) {
  const write = useIamGesture();
  const waiting = useWaiting();
  const [label, setLabel] = useState("");
  const [days, setDays] = useState("");
  // WHAT STARTS TICKED. A SERVICE ACCOUNT'S MINT: what this mint may send —
  // the account's grants a token may carry AND the minter holds, since the
  // account exists to act with them, and the engine refuses a token carrying
  // a grant its minter does not hold (the picker locks such a box, so one
  // ticked here would be a refusal nobody could clear). A PERSON'S OWN:
  // nothing. Every grant they held started ticked, so somebody pressing Mint
  // without reading held a bearer secret carrying config:write, secrets:write
  // and fleet:operate for their script; ticked one at a time, a token carries
  // what its holder chose.
  const carried = useMemo(
    () =>
      self
        ? []
        : (owner.grants ?? []).filter(
            (g) => !(TOKEN_WITHHELD_GRANTS as readonly string[]).includes(g) && held.includes(g),
          ),
    [self, owner.grants, held],
  );
  const [grants, setGrants] = useState<string[]>(carried);
  const minted = write.answer?.kind === "done" ? write.answer.body : null;
  const token = typeof minted?.token === "string" ? minted.token : "";
  const expires = typeof minted?.expires_at === "string" ? minted.expires_at : "";
  const lifetime = days.trim() === "" ? 0 : Number(days);
  // THE ENGINE'S CEILING, said here rather than posted for a 400.
  const daysProblem =
    days.trim() === ""
      ? null
      : !Number.isInteger(lifetime) || lifetime < 1
        ? "A whole number of days, at least one."
        : lifetime > TOKEN_MAX_DAYS
          ? `At most ${TOKEN_MAX_DAYS} days.`
          : null;
  const badDays = daysProblem !== null;
  const actsAs: ReactNode = self ? "you" : owner.login || "the account";

  async function submit() {
    if (minted || badDays) return;
    const answer = await write.run(
      {
        method: "POST",
        path: "/iam/credentials",
        ...(self ? {} : { query: { person: owner.id } }),
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
      title={self ? "New personal access token" : `Mint a token for ${owner.login || owner.id}`}
      icon={<KeyGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      closeDisabledReason={waiting.reason}
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
              {pressLabel(write, "Mint", "Minting", waiting.asked)}
            </Button>
          </>
        )
      }
    >
      {minted ? (
        <ShownOnce label="Token" value={token}>
          This token acts as {actsAs}, with what it carries, until {fmtDateTime(expires)}. It is
          shown only now and cannot be read back; present it as{" "}
          <span className="mono">Authorization: Bearer …</span>.
        </ShownOnce>
      ) : (
        <>
          {self && (
            <Text as="p" variant="body" tone="secondary">
              A token is for your own assistant or script: it acts as you, carrying only the grants
              you tick below and never more than you hold, and stops working when you change your
              password or sign out everywhere.
            </Text>
          )}
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
            helper={`Empty takes the engine's default of ${TOKEN_DEFAULT_DAYS} days; at most ${TOKEN_MAX_DAYS}.`}
            error={daysProblem ?? undefined}
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
          {/* A TOKEN CARRYING NO GRANT IS STILL A CREDENTIAL, and says what it
              reaches: KEEPING its owner's own record takes no grant, and
              READING anything — that record included — takes state:read
              (`authz`'s read rows). Unsaid, a person holding none was offered
              "Nothing to offer." beside an enabled Mint; said as "your own
              inbox, queue, pins and priorities", it promised a script reads
              every one of which was refused. */}
          {grants.length === 0 && (
            <Text as="p" variant="caption" tone="secondary">
              With no grant ticked, the token only keeps {self ? "your" : "the account's"} own
              record — marks {self ? "your" : "its"} inbox read, pins views and orders{" "}
              {self ? "your" : "its"} priorities — and reads nothing, {self ? "your" : "its"} own
              inbox and queue included: reading takes state:read.
            </Text>
          )}
        </>
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}
