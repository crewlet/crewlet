/**
 * The identity directory as more than one screen offers it: who holds a human
 * seat, and the writes made about one — inviting a person onto a vacant seat,
 * creating one there, cancelling the invitation that holds it — plus minting a
 * token (a service account's on People & access, your own on the Account
 * page), the grants a write confers, a value shown once, and what a write came
 * to.
 *
 * # A person is invited or created ON A SEAT, and only there
 *
 * Every person holds exactly one human seat for as long as they exist (the
 * engine refuses a person with none, ADR-0026), so the place a person comes
 * from is a vacant human seat: its card's menu on the org chart, the seat's
 * peek, and its page — each through [SeatHolding]'s three states (held,
 * invited, vacant) and the dialogs [SeatGestureDialog] opens. People & access
 * lists, edits, suspends and removes people; it no longer invites anybody, and
 * points at the chart instead.
 *
 * # Read where it is drawn, and heard everywhere it is
 *
 * The chart, the peek beside it and a seat's page each read the seat listing
 * for themselves ([useHumanSeats]), because each is drawn without the others;
 * a write made from any one of them is announced ([directoryMoved]) and every
 * listing on the page reads again.
 *
 * Every write goes through `lib/iamWrite.ts`, so the step-up, the operation key
 * a retry sends and the four answers are decided once. And they are offered to
 * a reader holding `people:manage` only — the grant every one of them is
 * refused without ([canManagePeople]). The directory is read by auditors
 * (`audit:read`) as well, and a control disabled with one reason on every seat
 * and every row is noise on the record they came to read; the engine refuses
 * whatever reaches it all the same.
 */

import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
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
  Tag,
  Text,
  type SelectOption,
} from "@crewlethq/ui";
import { BanGlyph, KeyGlyph, SendGlyph, UserPlusGlyph } from "@crewlethq/icons/glyphs";
import { DateCell } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import {
  GRANTS,
  TOKEN_DEFAULT_DAYS,
  TOKEN_MAX_DAYS,
  TOKEN_WITHHELD_GRANTS,
  type HumanSeat,
  type HumanSeatsAnswer,
  type SeatInvitation,
} from "~/contract/identity.ts";
import { fmtDateTime, tsKey } from "~/lib/format.ts";
import { loginKind, loginProblem } from "~/lib/login.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useIamGesture, type IamAnswer, type IamGesture } from "~/lib/iamWrite.ts";
import { useWaiting } from "~/lib/waiting.ts";
import { useRest, type RestOptions, type RestResult } from "~/lib/useRest.ts";
import { rest } from "~/protocol/index.ts";

/** The grant every directory write is decided on. */
export const PEOPLE_MANAGE = "people:manage";

/** The grant the directory is READ on beside it (`authz.ActionDirectoryRead`). */
const AUDIT_READ = "audit:read";

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

/**
 * Whether a reader's grants reach the directory's READS — `people:manage` or
 * `audit:read`, the engine's `authz.ActionDirectoryRead`. A surface that is
 * not the directory's own (the chart, a seat's peek and page) asks it only of
 * such a reader: anybody else would be handed a refusal on a screen they came
 * to for something else.
 */
export function canReadDirectory(grants: readonly string[]): boolean {
  return grants.includes(PEOPLE_MANAGE) || grants.includes(AUDIT_READ);
}

/**
 * How every read of the directory is kept current.
 *
 * The directory moves on an administrator's gesture and a node's Tier A at a
 * restart: a minute is soon enough to see a `crewlet iam` run in another
 * terminal land, and these surfaces are read, not watched. The tab coming back
 * asks at once, which is where somebody who just ran the command is looking;
 * a gesture made on this page is heard at once ([directoryMoved]).
 */
export const DIRECTORY_READ: RestOptions = { pollMs: 60_000, refetchOnFocus: true };

/**
 * Where a directory write is announced to every seat listing on the page.
 *
 * ONE ANSWER, SEVERAL READERS: the chart and the peek beside it each hold a
 * reading of `/iam/seats`, and an invitation sent from the peek re-read by the
 * peek alone said "Invited" beside a card still saying "Vacant" until the
 * chart's next poll.
 */
const directoryWrites = new EventTarget();

/** Tell every seat listing on the page that the directory may have moved. */
export function directoryMoved(): void {
  directoryWrites.dispatchEvent(new Event("moved"));
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
export function ReachNote({
  grants,
  you = false,
}: {
  grants: readonly string[];
  /** The grants are the reader's own, and the note speaks to them. */
  you?: boolean;
}) {
  if (grants.includes("state:read")) return null;
  return (
    <Callout variant="warning">
      Without <span className="mono">state:read</span>{" "}
      {you ? "you can open nothing but your own" : "they can open nothing but their own"} Account:
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

/**
 * What a credential the directory binds to no seat lacks, and who gives it.
 *
 * TWO DIFFERENT FACTS by whose login it is. A PERSON holds a human seat for as
 * long as they exist, so one with none was recorded before that held — a
 * fault an administrator mends (`person_without_seat` in the directory's
 * report). A service account or a Tier A token's session acting as itself is
 * ordinary, and binding its row to a human seat is what would add the seat's
 * work. Worded alike, the first read as a choice and the second as a fault.
 */
export function UnboundRemedy({ login }: { login: string }) {
  if (loginKind(login) === "person") {
    return (
      <>
        Every person holds a human seat and you hold none — you were recorded before that held — so
        ask whoever manages people to give you one: work the org chart hands to a seat reaches you
        once they do.
      </>
    );
  }
  return (
    <>
      Work the org chart hands to a seat reaches this credential once the directory binds its row to
      a human seat nothing else holds: <code className="inline">crewlet iam bind ID SEAT</code>, or
      — for a token no row holds yet —{" "}
      <code className="inline">crewlet iam create -kind machine -login {login} -seat SEAT</code>.
    </>
  );
}

/**
 * Every human seat of the running company and what holds it (`GET /iam/seats`),
 * or — `enabled` false — nothing asked at all.
 *
 * A caller passes `enabled` as `canReadDirectory(viewer.grants)` once the
 * viewer has answered (`!viewer.asking`), so nobody is asked a question they
 * would be refused. Every listing hears a write made anywhere on the page
 * ([directoryMoved]) and reads again, quietly.
 */
export function useHumanSeats(enabled: boolean): RestResult<HumanSeat[]> {
  const seats = useRest(
    enabled ? "/iam/seats" : null,
    async (signal) => {
      const answer = (await rest.get("/iam/seats", signal)) as HumanSeatsAnswer | null;
      return answer?.seats ?? [];
    },
    DIRECTORY_READ,
  );
  const { reload } = seats;
  useEffect(() => {
    if (!enabled) return;
    const moved = () => void reload({ quiet: true });
    directoryWrites.addEventListener("moved", moved);
    return () => directoryWrites.removeEventListener("moved", moved);
  }, [enabled, reload]);
  return seats;
}

/**
 * Whether the seat listing was refused because this node runs NO COMPANY
 * (`409 no_active_revision`): not a failure to report but the first step of a
 * first run, since there is no human seat until a company declares one. Read
 * through the generic refusal it was "the engine tried to answer and failed",
 * with nothing to do about it.
 */
export function listsNoCompany(seats: RestResult<unknown>): boolean {
  return !seats.data && seats.error?.code === "no_active_revision";
}

/**
 * The seats a bind may name: those NOTHING holds — no person, no service
 * account and no open invitation, which the engine's `?unheld=true` leaves
 * out alike, so the vacancies offered are exactly the ones a bind can take.
 */
export function useUnheldSeats() {
  return useRest("/iam/seats?unheld=true", async (signal) => {
    const answer = (await rest.get("/iam/seats?unheld=true", signal)) as HumanSeatsAnswer | null;
    return answer?.seats ?? [];
  });
}

/** A human seat as a dialog names it: its handle and the name the chart gives it. */
export type SeatName = Pick<HumanSeat, "handle" | "name">;

/**
 * A seat the vacancy list leaves out — the one a person holds now, or one a
 * dialog proposes before the list has arrived — named as the running org chart
 * names it. Named by its handle, it was offered as "jane-founder / jane-founder"
 * beside every vacancy's name.
 */
export function useSeatEntry(): (handle: string) => SeatName {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  return useCallback(
    (handle: string) => ({ handle, name: index.byHandle.get(handle)?.name ?? handle }),
    [index],
  );
}

/** The value of the "nobody" option in a seat select. */
export const NO_SEAT = "";

/**
 * A seat select's options: each seat by name with its handle — and, for a
 * SERVICE ACCOUNT alone (`none`), nobody first.
 *
 * NO SEAT IS A MACHINE'S ANSWER ONLY. A person holds a human seat for as long
 * as they exist, so the engine refuses a person's write that clears one
 * (`seat_required`); offered to a person, "No seat" was a choice whose every
 * save came back refused.
 */
export function seatOptions(
  seats: readonly SeatName[],
  { none = false }: { none?: boolean } = {},
): SelectOption[] {
  return [
    ...(none
      ? [{ value: NO_SEAT, label: "No seat", description: "Acts as itself, under its own login" }]
      : []),
    ...seats.map((s) => ({
      value: s.handle,
      label: s.name || s.handle,
      description: s.handle,
      text: `${s.name} ${s.handle}`,
    })),
  ];
}

/** What may be done about a human seat, wherever it is drawn. */
export type SeatGesture = "invite" | "create" | "cancel";

/**
 * The gestures a seat offers a reader holding `people:manage`: a VACANT seat
 * is invited onto or has a person created on it; a seat an open invitation
 * holds offers that invitation's cancellation; a HELD seat offers nothing
 * here — its holder is moved or removed on their row in People & access.
 *
 * NOTHING ON A SEAT THE LISTING DID NOT ANSWER FOR: a read that failed or has
 * not arrived says nothing about whether the seat is free, and an invitation
 * offered onto a seat somebody holds is a refusal the person pressing it
 * could have been spared.
 */
export function seatGestures(row: HumanSeat | undefined): SeatGesture[] {
  if (!row || row.holder) return [];
  return row.invitation ? ["cancel"] : ["invite", "create"];
}

/** Each gesture's words: as a menu item on a card, and as a button. */
export const SEAT_GESTURE_WORDS: Record<
  SeatGesture,
  { menu: string; button: string; icon: ReactNode }
> = {
  invite: { menu: "Invite to this seat…", button: "Invite", icon: <SendGlyph size="sm" /> },
  create: {
    menu: "Create a person on this seat…",
    button: "Create person",
    icon: <UserPlusGlyph size="sm" />,
  },
  cancel: { menu: "Cancel invitation", button: "Cancel invitation", icon: <BanGlyph size="sm" /> },
};

/**
 * What holds a seat, in the few words a card has room for: "Held by jane.doe",
 * "Invited · sam@example.com", or "Vacant". NEUTRAL WORDS for a card whose one
 * hue is a seat's state — who holds a seat is not something it is doing.
 */
export function holdingLine(row: HumanSeat): string {
  if (row.holder) return `Held by ${row.holder.login || row.holder.person}`;
  if (row.invitation) {
    const { email, sealed } = row.invitation;
    return email && !sealed ? `Invited · ${email}` : "Invited";
  }
  return "Vacant";
}

/**
 * The dialog a seat gesture opens, about the seat as it was when the gesture
 * started.
 *
 * A SNAPSHOT, NEVER THE LIVE ROW: the listing is read again the moment the
 * write lands ([directoryMoved]), and a cancellation reading the live row
 * would lose the invitation it is about — and its own dialog with it — before
 * a `202` could say it was recorded.
 */
export function SeatGestureDialog({
  gesture,
  seat,
  held,
  onClose,
}: {
  gesture: SeatGesture;
  seat: HumanSeat;
  /** The viewer's own grants. */
  held: readonly string[];
  onClose: () => void;
}) {
  switch (gesture) {
    case "invite":
      return <InviteDialog seat={seat} held={held} onClose={onClose} onDone={directoryMoved} />;
    case "create":
      return (
        <CreatePersonDialog seat={seat} held={held} onClose={onClose} onDone={directoryMoved} />
      );
    case "cancel":
      return seat.invitation ? (
        <CancelInvitation
          row={{ ...seat.invitation, seat: seat.handle }}
          seatName={seat.name}
          onClose={onClose}
          onChanged={directoryMoved}
        />
      ) : null;
  }
}

/**
 * Who holds a human seat — said on the seat's own page, where a held seat
 * names its holder, an invited one its invitation (with its cancellation), and
 * a vacant one the two ways to fill it.
 *
 * A READ THAT FAILED IS SAID: drawn as nothing, a refused or unreachable
 * listing removed the only way to fill the seat with no word why.
 *
 * THE DIALOG IS NOT THE LISTING'S: every write is announced
 * ([directoryMoved]) and the listing reads again — and a read refused then
 * (a node behind its identity log, a poll, the refetch as the tab comes back
 * from the email the link was pasted into) leaves no data. Drawn inside the
 * branch that needs the listing, the open dialog went with it, and so did the
 * invitation link or first password link the engine shows only once. So the
 * gesture is held here and its dialog drawn beside whatever the listing says,
 * as the chart and the peek draw theirs.
 */
export function SeatHolding({
  seat,
  seats,
  manages,
  held,
  you = false,
}: {
  seat: SeatName;
  /** The listing, as [useHumanSeats] read it. */
  seats: RestResult<HumanSeat[]>;
  /** The reader holds `people:manage`, so the gestures are drawn. */
  manages: boolean;
  /** The reader's own grants. */
  held: readonly string[];
  /** The seat is the reader's own. */
  you?: boolean;
}) {
  const [opening, setOpening] = useState<{ gesture: SeatGesture; row: HumanSeat } | null>(null);
  return (
    <>
      <SeatHoldingState
        seat={seat}
        seats={seats}
        manages={manages}
        you={you}
        onGesture={(gesture, row) => setOpening({ gesture, row })}
      />
      {opening && (
        <SeatGestureDialog
          gesture={opening.gesture}
          seat={opening.row}
          held={held}
          onClose={() => setOpening(null)}
        />
      )}
    </>
  );
}

/** What [SeatHolding] says about the listing: its failure, its absence, or the seat's holding. */
function SeatHoldingState({
  seat,
  seats,
  manages,
  you,
  onGesture,
}: {
  seat: SeatName;
  seats: RestResult<HumanSeat[]>;
  manages: boolean;
  you: boolean;
  onGesture: (gesture: SeatGesture, row: HumanSeat) => void;
}) {
  if (seats.code && !seats.data) {
    return (
      <QueryState
        error={seats.code}
        refusal={seats.refusal}
        detail={seats.error?.detail || undefined}
        loading={false}
      />
    );
  }
  if (!seats.data) return null;
  const row = seats.data.find((s) => s.handle === seat.handle);
  if (!row) {
    // THE LISTING IS THE RUNNING COMPANY'S, so a seat the chart draws and it
    // does not hold is one this node has not applied yet.
    return (
      <Callout variant="neutral">
        The identity directory does not list this seat yet: this node has not applied the revision
        that added it.
      </Callout>
    );
  }
  const actions = manages
    ? seatGestures(row).map((gesture) => (
        <Button
          key={gesture}
          size="small"
          variant={gesture === "cancel" ? "ghost" : "secondary"}
          leadingIcon={SEAT_GESTURE_WORDS[gesture].icon}
          onClick={() => onGesture(gesture, row)}
        >
          {SEAT_GESTURE_WORDS[gesture].button}
        </Button>
      ))
    : [];
  return (
    <Callout
      variant="neutral"
      action={actions.length > 0 ? <span className="row gap-2">{actions}</span> : undefined}
    >
      <HoldingSentence row={row} you={you} />
    </Callout>
  );
}

/** What holds a seat, said whole: its holder and their stage, its invitation, or nobody. */
export function HoldingSentence({ row, you = false }: { row: HumanSeat; you?: boolean }) {
  const { holder, invitation } = row;
  if (holder) {
    const stage = holder.stage && holder.stage !== "active" ? ` (${holder.stage})` : "";
    return (
      <>
        Held by {you && "you, as "}
        {holder.kind === "machine" && "the service account "}
        <code className="inline">{holder.login || holder.person}</code>
        {stage}.
      </>
    );
  }
  if (invitation) {
    return (
      <>
        <span>Invited: </span>
        {invitation.sealed || !invitation.email ? (
          "an address this node's keyring cannot open"
        ) : (
          <strong>{invitation.email}</strong>
        )}
        , until {fmtDateTime(invitation.expires_at)}. Redeeming the link binds them to this seat;
        until it is redeemed, cancelled or lapses, nobody else can be invited or bound to it.
      </>
    );
  }
  return <span>Nobody holds this seat.</span>;
}

/** The seat a person-dialog is about, said as a fact rather than offered as a choice. */
function SeatFact({ seat, children }: { seat: SeatName; children: ReactNode }) {
  return (
    <Text as="p" variant="body">
      For the seat <strong>{seat.name || seat.handle}</strong>{" "}
      <InlineCode>{seat.handle}</InlineCode>: {children}
    </Text>
  );
}

/**
 * Invite somebody onto a vacant human seat: an address and the grants it
 * confers. On success the link is shown ONCE — it is the credential, and the
 * engine keeps only a hash of its secret.
 *
 * THE SEAT IS FIXED, by where the dialog was opened from. An invitation names
 * the seat its redemption binds, always: a person holds one for as long as
 * they exist, and an open invitation holds it until then.
 */
export function InviteDialog({
  seat,
  held,
  onClose,
  onDone,
}: {
  /** The vacant seat the invitation is for. */
  seat: SeatName;
  /** The viewer's own grants. */
  held: readonly string[];
  onClose: () => void;
  /** After every answer, so the lists are read again. */
  onDone: () => void;
}) {
  const write = useIamGesture();
  const waiting = useWaiting();
  const [email, setEmail] = useState("");
  const [grants, setGrants] = useState<string[]>([]);

  const done = write.answer?.kind === "done" ? write.answer.body : null;
  const url = typeof done?.url === "string" ? done.url : "";
  const expires = typeof done?.expires_at === "string" ? done.expires_at : "";
  const named = seat.name || seat.handle;

  async function submit() {
    if (email.trim() === "" || done) return;
    const answer = await write.run({
      method: "POST",
      path: "/iam/invitations",
      body: { email: email.trim(), grants, seat: seat.handle },
    });
    if (answer) onDone();
  }

  return (
    <Modal
      open
      size="md"
      title={`Invite a person to ${named}`}
      icon={<SendGlyph />}
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
        url ? (
          <ShownOnce label="Invitation link" value={url}>
            This link is the credential: it works once, for {email.trim()}, and expires{" "}
            {fmtDateTime(expires)}. It is shown only now — send it yourself; this engine sends no
            mail. Opening it is where they choose a login and a password, and redeeming it binds
            them to {named}.
          </ShownOnce>
        ) : (
          <Text as="p" variant="body">
            The invitation is recorded, and the answer carried no link to show: cancel it on the
            seat and invite them again.
          </Text>
        )
      ) : (
        <>
          <SeatFact seat={seat}>
            redeeming the invitation binds them to it, so they act as that seat, and until then the
            invitation holds it.
          </SeatFact>
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
          <GrantPicker value={grants} onChange={setGrants} held={held} />
          <ReachNote grants={grants} />
        </>
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}

/**
 * Create a person on a vacant human seat, active at once, and hand back the
 * one-time link that sets their FIRST password.
 *
 * THE LOGIN IS THE ENGINE'S TO PROPOSE: left empty, the engine takes one from
 * the address (the same proposal an invitation's screen makes) and answers the
 * login it took, which the done screen says. One typed here is held to the
 * person grammar under the field before anything is posted.
 *
 * KEYED, and the key is what makes the link survive a dropped answer: the
 * engine derives the person and their link from the operation, so the retry
 * an unknown answer asks for — the same request, under the same key — hands
 * back the same link rather than a second person. A retry that finds the link
 * already spent, revoked or lapsed answers no link and says so (`detail`).
 */
export function CreatePersonDialog({
  seat,
  held,
  onClose,
  onDone,
}: {
  /** The vacant seat the person is created on. */
  seat: SeatName;
  /** The viewer's own grants. */
  held: readonly string[];
  onClose: () => void;
  /** After every answer, so the lists are read again. */
  onDone: () => void;
}) {
  const write = useIamGesture();
  const waiting = useWaiting();
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [login, setLogin] = useState("");
  const [grants, setGrants] = useState<string[]>([]);
  const [tried, setTried] = useState(false);
  const typed = login.trim();
  const loginWrong = loginProblem("person", typed);

  const done = write.answer?.kind === "done" ? write.answer.body : null;
  const text = (key: string) => (typeof done?.[key] === "string" ? (done[key] as string) : "");
  const url = text("url");
  const took = text("login") || typed;
  const named = seat.name || seat.handle;

  async function submit() {
    setTried(true);
    if (email.trim() === "" || loginWrong || done) return;
    const answer = await write.run({
      method: "POST",
      path: "/iam/people",
      body: {
        kind: "person",
        seat: seat.handle,
        email: email.trim(),
        ...(typed ? { login: typed } : {}),
        ...(name.trim() ? { name: name.trim() } : {}),
        grants,
      },
    });
    if (answer) onDone();
  }

  return (
    <Modal
      open
      size="md"
      title={`Create a person on ${named}`}
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
              {pressLabel(write, "Create", "Creating", waiting.asked)}
            </Button>
          </>
        )
      }
    >
      {done ? (
        <>
          <Text as="p" variant="body">
            {took ? (
              <>
                <InlineCode>{took}</InlineCode> is
              </>
            ) : (
              "They are"
            )}{" "}
            on the seat {named}, and signs in with that login or {email.trim()}.
          </Text>
          {url ? (
            <ShownOnce label="Password link" value={url}>
              This link sets their first password, once, and expires{" "}
              {fmtDateTime(text("expires_at"))}. It is shown only now — send it to them yourself;
              this engine sends no mail. It signs nobody in, and until it is used nobody can sign in
              as them.
            </ShownOnce>
          ) : text("detail") ? (
            // A RETRY THAT FOUND THE LINK CLOSED — spent, revoked or lapsed
            // since its first answer — in the engine's own words, which name
            // the way to a new one.
            <Callout variant="warning" title="No link to show">
              {sentenceOf(text("detail"))}
            </Callout>
          ) : null}
        </>
      ) : (
        <>
          <SeatFact seat={seat}>
            they are created on it, active at once, and act as that seat once they have set their
            password.
          </SeatFact>
          <FormField
            label="Email address"
            helper="Where they can be reached, and one way to sign in."
          >
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
          <FormField
            label="Login"
            optional
            helper="Lowercase words joined by dots, such as jane.doe. Left empty, the engine proposes one from the address and says which it took."
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
          <GrantPicker value={grants} onChange={setGrants} held={held} />
          <ReachNote grants={grants} />
        </>
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}

/** The engine writes a detail in its own lower case; drawn, it is a sentence. */
function sentenceOf(text: string): string {
  const trimmed = text.trim();
  if (trimmed === "") return "";
  const opened = trimmed[0]!.toUpperCase() + trimmed.slice(1);
  return /[.!?]$/.test(opened) ? opened : `${opened}.`;
}

/**
 * Withdraw an invitation nobody redeemed: its link stops working, and the
 * address and the seat it holds are free again.
 */
export function CancelInvitation({
  row,
  seatName,
  onClose,
  onChanged,
}: {
  row: Pick<SeatInvitation, "id" | "email" | "sealed"> & {
    /** The seat it names, by handle — absent on one issued before every invitation named one. */
    seat?: string;
  };
  /** The seat's name as the chart says it, where the caller has it. */
  seatName?: string;
  onClose: () => void;
  onChanged: () => void;
}) {
  const write = useIamGesture();
  const frees = row.seat
    ? `the address and the seat ${seatName || row.seat} are free again`
    : "the address is free to invite again";
  return (
    <ConfirmDialog
      title="Cancel this invitation?"
      confirm="Cancel invitation"
      dismiss="Keep it"
      danger
      write={write}
      onClose={onClose}
      onConfirm={async () => {
        const answer = await write.run({
          method: "DELETE",
          path: `/iam/invitations/${encodeURIComponent(row.id)}`,
        });
        if (!answer) return;
        onChanged();
        if (answer.kind === "done" && !answer.pending) onClose();
      }}
    >
      The link sent to {row.sealed || !row.email ? "this address" : <strong>{row.email}</strong>}{" "}
      stops working at once, as one nobody issued, and {frees}.
    </ConfirmDialog>
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
  // AND DONE IS DONE, a 202 included: the gesture is recorded and this node
  // has yet to apply it, which the outcome says. Its dismissal read as undoing
  // a cancellation already durable ("Keep it"), and the confirm sent the same
  // gesture again under a new key.
  const done = write.answer?.kind === "done";
  const settled = stale || done;
  return (
    <Modal
      open
      size="sm"
      title={title}
      onClose={onClose}
      dismissable={!write.busy}
      closeDisabledReason={waiting.reason}
      stackBody
      onSubmit={() => ready && !settled && onConfirm()}
      footer={
        settled ? (
          <Button variant="primary" onClick={onClose}>
            {done ? "Done" : "Close"}
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
