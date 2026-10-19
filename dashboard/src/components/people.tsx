/**
 * The identity directory as more than one screen offers it: who holds a human
 * seat, and the writes made about one — inviting a person onto a vacant seat,
 * creating one there, cancelling the invitation that holds it — plus minting a
 * token (a service account's on Service accounts, your own on the Account
 * page), the ACCESS a write confers (`lib/access.ts`: Admin, and one
 * permission per app), a value shown once, and what a write came to.
 *
 * # A person is invited or created ON A SEAT, and only there
 *
 * Every person holds exactly one human seat for as long as they exist (the
 * engine refuses a person with none, ADR-0032), so the place a person comes
 * from is a vacant human seat: its card's menu on the org chart, the seat's
 * peek, and its page — each through [SeatHolding]'s three states (held,
 * invited, vacant) and the dialogs [SeatGestureDialog] opens. People
 * lists, edits, suspends and removes people; it no longer invites anybody, and
 * points at the chart instead.
 *
 * # Read where it is drawn, and heard everywhere it is
 *
 * The chart, the peek beside it and a seat's page each read the seat listing
 * for themselves ([useHumanSeats]), because each is drawn without the others;
 * a write made from any one of them is announced ([directoryMoved]) and every
 * listing on the page reads again. EVERY SIGNED-IN READER reads the listing —
 * who holds each human seat is part of the org chart, the overview — and the
 * engine masks it per reader: the holder's name and kind to everybody, their
 * login, account and stage to an admin and to the holder, an open invitation
 * to an admin alone.
 *
 * Every write goes through `lib/iamWrite.ts`, so the step-up, the operation key
 * a retry sends and the four answers are decided once. And they are offered to
 * an ADMIN only — the access every one of them is refused without
 * ([canManagePeople]); a control disabled with one reason on every seat is
 * noise on a chart everybody else came to read, and the engine refuses
 * whatever reaches it all the same.
 */

import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  Checkbox,
  Copyable,
  EmptyState,
  EmptyValue,
  FormField,
  InlineCode,
  Input,
  Modal,
  Tag,
  Text,
  type SelectOption,
  CheckboxGroup,
  ConfirmField,
  confirmMatches,
  Inline,
  Stack,
  TableDateCell,
} from "@crewlethq/ui";
import { BanGlyph, KeyGlyph, SendGlyph, UserPlusGlyph } from "@crewlethq/icons/glyphs";

import { QueryState } from "~/components/common.tsx";
import {
  APPS,
  GRANTS,
  TOKEN_DEFAULT_DAYS,
  TOKEN_MAX_DAYS,
  type HumanSeat,
  type HumanSeatsAnswer,
  type SeatInvitation,
} from "~/contract/identity.ts";
import {
  ACCESS_WORDS,
  ADMIN,
  WORKLET,
  accessLabel,
  accessLine,
  expand,
  holds,
  isGrant,
} from "~/lib/access.ts";
import { asSentence, fmtDate, fmtDateCompact, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { loginProblem } from "~/lib/login.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useIamGesture, type IamAnswer, type IamGesture } from "~/lib/iamWrite.ts";
import { useWaiting } from "~/lib/waiting.ts";
import { useRest, type RestOptions, type RestResult } from "~/lib/useRest.ts";
import { rest } from "~/protocol/index.ts";

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
    <TableDateCell value={c.expires_at} now={now} />
  ) : (
    <EmptyValue label="Does not expire" />
  );
}

/**
 * Whether a reader's access reaches the directory — every write about a
 * person, and every read of somebody else's row: Admin, the engine's
 * `authz.ClassDirectory` and `iam.write` rows.
 */
export function canManagePeople(grants: readonly string[]): boolean {
  return holds(grants, ADMIN);
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

/** Tell every directory reading on the page that the directory may have moved. */
export function directoryMoved(): void {
  directoryWrites.dispatchEvent(new Event("moved"));
}

/**
 * Run `heard` whenever a write anywhere on the page may have moved the
 * directory ([directoryMoved]), while `enabled`. People and the
 * person peek beside it are two trees, and a removal made in the peek left
 * the list it was opened from naming the person until its next poll.
 */
export function useDirectoryMoved(heard: () => void, enabled = true): void {
  useEffect(() => {
    if (!enabled) return;
    directoryWrites.addEventListener("moved", heard);
    return () => directoryWrites.removeEventListener("moved", heard);
  }, [enabled, heard]);
}

/**
 * Access as words — a directory row's, an invitation's, or what a token
 * carries — one tag per part of its [accessLine]: "Admin", or the apps
 * ("Worklet", "Doclet"), or "Overview only". A name this build does not
 * know is drawn raw beside them, so a row never looks as if it held less than
 * it does.
 */
export function AccessTags({ grants }: { grants?: readonly string[] | null }) {
  const line = accessLine(grants ?? []);
  if (line === "Overview only") {
    return (
      <Tag size="sm" appearance="outline">
        Overview only
      </Tag>
    );
  }
  return (
    <Inline as="span" gap={1} wrap>
      {line.split(" · ").map((part) => (
        <Tag key={part} size="sm" appearance="outline">
          {part}
        </Tag>
      ))}
    </Inline>
  );
}

/**
 * The ACCESS PICKER: Admin, then one checkbox per app (`lib/access.ts`).
 *
 * ADMIN IS ONE SWITCH. It includes every app, so ticking it ticks every app
 * and LOCKS them — "Included with Admin" — and stores them too (the engine's
 * own CLI does the same with `-grants admin`), so taking Admin away later
 * leaves the person their apps rather than nothing. Unticking it unlocks the
 * apps and keeps them ticked.
 *
 * A PERMISSION THE WRITER DOES NOT HOLD is drawn disabled with why — the
 * engine refuses to confer it anyway (a caller may not confer what they do
 * not hold), and a box that only fails on submit is a guess. `held` is read
 * through `expand`, so an admin confers every app. `only` narrows the offer —
 * a token carries a subset of its owner's.
 *
 * A NAME THIS DASHBOARD DOES NOT KNOW that the row already holds is offered
 * as a box of its own, ticked: the engine keeps such a name and grants nothing
 * for it, and the writer may remove it but never add one — so unticking it
 * removes it, and a name nobody held is never offered.
 */
export function AccessPicker({
  value,
  onChange,
  held,
  only,
  kept = [],
  legend = "Access",
}: {
  value: readonly string[];
  onChange: (next: string[]) => void;
  /**
   * What this write may confer: the viewer's own access — and, on an edit,
   * what the person already holds, which keeping or taking away confers
   * nothing (the engine checks only what an edit ADDS).
   */
  held: readonly string[];
  only?: readonly string[];
  /** Names this dashboard does not know that the row held before the edit. */
  kept?: readonly string[];
  legend?: string;
}) {
  const may = expand(held);
  const offer = only ? expand(only) : null;
  const offered = GRANTS.filter((g) => !offer || offer.includes(g));
  const admin = value.includes(ADMIN);
  const unknown = kept.filter((g) => !isGrant(g));
  const toggle = (g: string, on: boolean): string[] => {
    if (g === ADMIN) {
      // TICKED, every app the build knows is stored beside it; UNTICKED, the
      // apps stay — see the doc.
      // A ROW STORED AS ADMIN ALONE (a Tier A file, an older CLI) is read
      // through `expand` as it is let go, so it keeps its apps too.
      return on
        ? [...new Set([...value, ADMIN, ...APPS])]
        : expand(value).filter((x) => x !== ADMIN);
    }
    return on ? [...value, g] : value.filter((x) => x !== g);
  };
  return (
    <CheckboxGroup legend={legend} emptyMessage="Nothing to offer.">
      {offered.map((g) => {
        const included = g !== ADMIN && admin;
        const refused = may.includes(g)
          ? null
          : `You can’t give ${ACCESS_WORDS[g].label} because you don’t have it.`;
        return (
          <Checkbox
            key={g}
            label={ACCESS_WORDS[g].label}
            description={
              refused ?? (included ? "Included with Admin." : ACCESS_WORDS[g].description)
            }
            checked={included || value.includes(g)}
            disabled={refused !== null || included}
            onCheckedChange={(on) => onChange(toggle(g, on))}
          />
        );
      })}
      {unknown.map((g) => (
        <Checkbox
          key={g}
          label={
            <Text variant="inherit" mono>
              {g}
            </Text>
          }
          description="Unknown access. It does nothing here."
          checked={value.includes(g)}
          onCheckedChange={(on) => onChange(toggle(g, on))}
        />
      ))}
    </CheckboxGroup>
  );
}

/**
 * What a PERSON given this access will find, said before the write is sent:
 *
 *   - NOTHING TICKED is the overview only — the org chart, the agents'
 *     status, spend and their own account — which every signed-in person
 *     reads, and nothing of the tracker, the knowledge base or the chat;
 *   - WITHOUT WORKLET, the work the company's agents send to people never
 *     reaches them: an agent asks a person, assigns them a task or waits on
 *     their answer through Worklet, and their inbox and My work are its.
 *     They can still open and answer a work item an agent asks them about.
 *
 * Only for a person: a service account is a credential somebody scripts, and
 * what it needs is the script's business.
 */
export function AppReachNote({
  grants,
  you = false,
}: {
  grants: readonly string[];
  /** The access is the reader's own, and the note speaks to them. */
  you?: boolean;
}) {
  const have = expand(grants);
  if (!APPS.some((app) => have.includes(app)) && !have.includes(ADMIN)) {
    return <Callout variant="warning">Overview only: no tasks, pages or chat.</Callout>;
  }
  if (!have.includes(WORKLET)) {
    return (
      <Callout variant="warning">
        Without {accessLabel(WORKLET)}, agents can’t send {you ? "you" : "them"} work.
      </Callout>
    );
  }
  return null;
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
    <Stack gap={2}>
      <FormField label={label}>
        {() => (
          <Copyable
            value={value}
            variant="block"
            monospace
            ariaLabel={`Copy ${label.toLowerCase()}`}
          />
        )}
      </FormField>
      <Callout variant="warning">{children}</Callout>
    </Stack>
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
        Saved. The list will update in a moment.
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
 * What a credential the directory binds to no seat lacks, and how it gets it.
 *
 * ONLY A CREDENTIAL IS EVER UNBOUND — a service account or a Tier A token's
 * session acting as itself, which is ordinary — since a person holds a human
 * seat for as long as they exist (ADR-0032). Binding its row to a human seat
 * is what would add the seat's work.
 */
export function UnboundRemedy() {
  return (
    <>
      To get work here, bind this account to an open seat:{" "}
      <InlineCode>crewlet iam bind ID SEAT</InlineCode>.
    </>
  );
}

/**
 * Every human seat of the running company and what holds it (`GET /iam/seats`),
 * or — `enabled` false — nothing asked at all.
 *
 * EVERY SIGNED-IN READER may ask it (`iam.seats.read`): the engine masks each
 * holder per reader — a name and a kind to everybody, the account to an admin
 * and the holder — so a caller passes `enabled` as "somebody has answered"
 * (`!viewer.asking && !viewer.anonymous`), never a permission. Every listing
 * hears a write made anywhere on the page ([directoryMoved]) and reads again,
 * quietly.
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
  useDirectoryMoved(
    useCallback(() => void reload({ quiet: true }), [reload]),
    enabled,
  );
  return seats;
}

/**
 * Whether the seat listing was refused because this node runs NO COMPANY
 * (`409 no_active_revision`): not a failure to report but the first step of a
 * first run, since there is no human seat until a company declares one. Read
 * through the generic refusal it was "the engine failed to answer",
 * with nothing to do about it.
 */
export function listsNoCompany(seats: RestResult<unknown>): boolean {
  return !seats.data && seats.error?.code === "no_active_revision";
}

/**
 * The seats a bind may name: those NOTHING holds — no person, no service
 * account and no open invitation, which the engine's `?unheld=true` leaves
 * out alike, so the vacancies offered are exactly the ones a bind can take.
 * An admin's question (the engine refuses it to anybody else), asked only by
 * the dialogs that move somebody.
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
    ...(none ? [{ value: NO_SEAT, label: "No seat", description: "Acts as itself" }] : []),
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
 * The gestures a seat offers an admin: a VACANT seat
 * is invited onto or has a person created on it; a seat an open invitation
 * holds offers that invitation's cancellation; a HELD seat offers nothing
 * here, its holder is moved or removed on their row in People.
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
  invite: { menu: "Invite someone…", button: "Invite", icon: <SendGlyph size="sm" /> },
  create: {
    menu: "Add a person…",
    button: "Add person",
    icon: <UserPlusGlyph size="sm" />,
  },
  cancel: { menu: "Cancel invitation", button: "Cancel invitation", icon: <BanGlyph size="sm" /> },
};

/**
 * Who holds a seat, by the name a reader of the chart knows them by: their
 * own name, which every reader is sent; else their login, which an admin and
 * the holder are sent; else that the engine's keyring could not open the
 * name, or — where nothing names them — that somebody does.
 */
export function holderName(holder: NonNullable<HumanSeat["holder"]>): string {
  if (holder.name) return holder.name;
  if (holder.login) return holder.login;
  if (holder.sealed) return "a person whose name this node can’t decrypt";
  return holder.kind === "machine" ? "a service account" : "a person";
}

/**
 * What holds a seat, in the few words a card has room for: "Held by Jane
 * Doe", "Invited · sam@example.com", or "Vacant". NEUTRAL WORDS for a card
 * whose one hue is a seat's state — who holds a seat is not something it is
 * doing. An invitation is an admin's to see, so to anybody else a seat it
 * holds reads as nobody's.
 */
export function holdingLine(row: HumanSeat): string {
  if (row.holder) return `Held by ${holderName(row.holder)}`;
  if (row.invitation) {
    const { email, sealed } = row.invitation;
    return email && !sealed ? `Invited · ${email}` : "Invited";
  }
  return "Open seat";
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
  form = "notice",
}: {
  seat: SeatName;
  /** The listing, as [useHumanSeats] read it. */
  seats: RestResult<HumanSeat[]>;
  /** The reader is an admin, so the gestures are drawn. */
  manages: boolean;
  /** The reader's own grants. */
  held: readonly string[];
  /** The seat is the reader's own. */
  you?: boolean;
  /**
   * How the holding is said. `notice` is a line above the seat's page, for a
   * seat somebody holds. `empty` is the page's own empty state, for a seat
   * nobody holds, whose Overview has no person's day or work to draw: a
   * notice above an empty column was the page saying "open" and then
   * nothing.
   */
  form?: "notice" | "empty";
}) {
  const [opening, setOpening] = useState<{ gesture: SeatGesture; row: HumanSeat } | null>(null);
  return (
    <>
      <SeatHoldingState
        seat={seat}
        seats={seats}
        manages={manages}
        you={you}
        form={form}
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
  form,
  onGesture,
}: {
  seat: SeatName;
  seats: RestResult<HumanSeat[]>;
  manages: boolean;
  you: boolean;
  form: "notice" | "empty";
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
    return <Callout variant="neutral">This seat isn’t ready yet. Try again in a moment.</Callout>;
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
  if (form === "empty" && !row.holder) {
    return (
      <EmptyState
        icon={<UserPlusGlyph size={32} />}
        title={row.invitation ? "An invitation holds this seat" : "This seat is open"}
        description={
          row.invitation ? (
            <HoldingSentence row={row} you={you} />
          ) : manages ? (
            "Nobody holds it yet. Invite someone to it, or add a person directly."
          ) : (
            "Nobody holds it yet. An admin invites someone to it."
          )
        }
        action={
          actions.length > 0 ? (
            <Inline as="span" gap={2}>
              {actions}
            </Inline>
          ) : undefined
        }
      />
    );
  }
  return (
    <Callout
      variant="neutral"
      action={
        actions.length > 0 ? (
          <Inline as="span" gap={2}>
            {actions}
          </Inline>
        ) : undefined
      }
    >
      <HoldingSentence row={row} you={you} />
    </Callout>
  );
}

/**
 * What holds a seat, said whole: its holder and their stage, its invitation,
 * or nobody. The holder by NAME, which every reader is sent, and by login
 * beside it where the reader is sent that too (an admin, the holder).
 */
export function HoldingSentence({ row, you = false }: { row: HumanSeat; you?: boolean }) {
  const now = useNow();
  const { holder, invitation } = row;
  if (holder) {
    const stage = holder.stage && holder.stage !== "active" ? ` (${holder.stage})` : "";
    const named = holder.name !== undefined && holder.name !== "";
    const login = holder.login ? <InlineCode>{holder.login}</InlineCode> : null;
    return (
      <>
        Held by {you && "you, as "}
        {holder.kind === "machine" && (named || login) && "the service account "}
        {named ? (
          <>
            <strong>{holder.name}</strong>
            {login && <> {login}</>}
          </>
        ) : (
          (login ?? holderName(holder))
        )}
        {stage}.
      </>
    );
  }
  if (invitation) {
    return (
      <>
        <span>Invited </span>
        {invitation.sealed || !invitation.email ? (
          "an address this node can’t decrypt"
        ) : (
          <strong>{invitation.email}</strong>
        )}
        . Expires {fmtDateCompact(invitation.expires_at, now)}.
      </>
    );
  }
  return <span>This seat is open.</span>;
}

/**
 * Invite somebody onto a vacant human seat: an address and the access it
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
  // EVERY APP, ADMIN UNTICKED: a person joins to use the company's apps, and
  // the agents send work to people through the tracker; admin is a decision
  // somebody makes on purpose.
  const [grants, setGrants] = useState<string[]>(() => [...APPS]);

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
      title={`Invite someone as ${named}`}
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
            Copy it now. You won’t see it again. Expires {fmtDate(expires)}.
          </ShownOnce>
        ) : (
          <Text as="p" variant="body">
            The invitation is recorded, and the answer carried no link to show: cancel it on the
            seat and invite them again.
          </Text>
        )
      ) : (
        <>
          <Text as="p" variant="body">
            The seat is held until they join.
          </Text>
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
          <AccessPicker value={grants} onChange={setGrants} held={held} />
          <AppReachNote grants={grants} />
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
  // EVERY APP, ADMIN UNTICKED, as an invitation's ([InviteDialog]).
  const [grants, setGrants] = useState<string[]>(() => [...APPS]);
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
      title={`Add someone as ${named}`}
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
              {pressLabel(write, "Add person", "Adding", waiting.asked)}
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
              "They’re"
            )}{" "}
            now {named}.
          </Text>
          {url ? (
            <ShownOnce label="Password link" value={url}>
              Copy it now. You won’t see it again. Expires {fmtDate(text("expires_at"))}.
            </ShownOnce>
          ) : text("detail") ? (
            // A RETRY THAT FOUND THE LINK CLOSED — spent, revoked or lapsed
            // since its first answer — in the engine's own words, which name
            // the way to a new one.
            <Callout variant="warning" title="No link to show">
              {asSentence(text("detail"))}
            </Callout>
          ) : null}
        </>
      ) : (
        <>
          <Text as="p" variant="body">
            They sign in once they set a password.
          </Text>
          <FormField label="Email address" helper="They can sign in with it.">
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
            helper="Leave empty to use their email."
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
          <AccessPicker value={grants} onChange={setGrants} held={held} />
          <AppReachNote grants={grants} />
        </>
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
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
    /** The seat it names, by handle. */
    seat?: string;
  };
  /** The seat's name as the chart says it, where the caller has it. */
  seatName?: string;
  onClose: () => void;
  onChanged: () => void;
}) {
  const write = useIamGesture();
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
      The link stops working.
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
  const ready = !typeToConfirm || confirmMatches(typed, typeToConfirm);
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
      <Text as="div" variant="body">
        {children}
      </Text>
      {typeToConfirm && (
        // A FORM FIELD LIKE EVERY OTHER. The value is typed exactly, so it is
        // shown exactly: the kit's label keeps a literal's case inside its
        // capitals, where ROBERT.SMITH kept the button disabled with no word why.
        <ConfirmField phrase={typeToConfirm} value={typed} onValueChange={setTyped} autoFocus />
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
 * Mint a token: a label, a lifetime, and access out of the owner's own — never
 * more than the minter holds. Admin may be ticked: the engine still refuses a
 * token every gesture that needs a person present, and every administrative
 * record about a person. The value is shown ONCE; the engine keeps a hash of
 * it.
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
  /** The minter's own access: a token carries nothing its minter does not hold. */
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
  // WHAT STARTS TICKED: the owner's own access that the minter holds too,
  // since the engine refuses a token carrying what its minter does not hold
  // (the picker locks such a box, so one ticked here would be a refusal
  // nobody could clear). A PERSON'S OWN starts with their access, the token
  // they almost always want: with nothing ticked, one pressed through could
  // not read its owner's own inbox. EXCEPT ADMIN, which a person ticks on
  // purpose: a bearer secret carrying Admin is host access for whatever
  // script holds it, so it is never the default.
  const carried = useMemo(() => {
    const may = expand(held);
    return expand(owner.grants ?? []).filter(
      (g) => isGrant(g) && may.includes(g) && !(self && g === ADMIN),
    );
  }, [self, owner.grants, held]);
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
        ? "Enter a whole number of days."
        : lifetime > TOKEN_MAX_DAYS
          ? `At most ${TOKEN_MAX_DAYS} days.`
          : null;
  const badDays = daysProblem !== null;

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
      title={self ? "New personal access token" : `New token for ${owner.login || owner.id}`}
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
              {pressLabel(write, "Create token", "Creating", waiting.asked)}
            </Button>
          </>
        )
      }
    >
      {minted ? (
        <ShownOnce label="Token" value={token}>
          Copy it now. You won’t see it again. Works until {fmtDate(expires)}. Send it as{" "}
          <Text variant="inherit" mono>
            Authorization: Bearer &lt;token&gt;
          </Text>
          .
        </ShownOnce>
      ) : (
        <>
          {self && (
            <Text as="p" variant="body" tone="secondary">
              For a script or assistant. Acts as you, with the access you pick.
            </Text>
          )}
          <FormField label="Label" optional helper="So you can tell tokens apart.">
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
            helper={`Leave empty for ${TOKEN_DEFAULT_DAYS}. Max ${TOKEN_MAX_DAYS}.`}
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
          <AccessPicker
            legend="Access the token carries"
            value={grants}
            onChange={setGrants}
            held={held}
            only={owner.grants ?? []}
          />
          {/* A TOKEN CARRYING NOTHING IS STILL A CREDENTIAL, and says what it
              reaches: the overview every signed-in principal reads, and
              nothing of an app — its owner's own inbox, queue and pins
              included, which are Worklet's. Unsaid, a person holding none
              was offered "Nothing to offer." beside an enabled Mint. */}
          {expand(grants).every((g) => !isGrant(g)) && (
            <Text as="p" variant="caption" tone="secondary">
              Nothing ticked: overview only. No tasks, pages or chat.
            </Text>
          )}
        </>
      )}
      <IamOutcome answer={write.answer} />
    </Modal>
  );
}
