/**
 * The pieces more than one screen draws.
 *
 * Each of these existed two or three times in the dashboard this replaces, and
 * the copies had drifted: two different seat-tone maps, two budget bars at
 * different heights with different colours (one of them winning globally
 * because its room stylesheet loaded last), and one activity row renderer with
 * zero callers beside another with all of them.
 */

import type { ReactNode } from "react";
import { Avatar, Button, Callout, EmptyState, Section as UiSection, Tag, cx } from "@crewlethq/ui";
import { CableGlyph, KeyGlyph, ScheduleGlyph, WarningGlyph } from "@crewlethq/icons/glyphs";
// STILL OURS: an attention row's mark is named by `lib/attention.ts` as a
// value, and uilet's glyphs are components. The name -> drawing lookup stays
// in `~/ui/Icon.tsx`, which is the one place a port of it moves every caller
// at once - the same call `app/frame/cells.tsx` makes for the same reason.
import { Mark } from "~/ui/glyph.tsx";
import { PhaseTag, uiletTone } from "~/ui/primitives.tsx";
import { href } from "~/app/router.tsx";
import { fmtDateTime, fmtTime, humanize, relTime } from "~/lib/format.ts";
import { useClockReading } from "~/lib/clock.ts";
import { ClockText } from "~/app/frame/cells.tsx";
import { isLogRefusal } from "~/protocol/index.ts";
import { goSignIn } from "~/lib/session.ts";
import {
  roundLabel,
  runState,
  sandboxFor,
  seatPath,
  seatTone,
  stateLabel,
  statusLine,
  toneOf,
  type Seat,
  type SeatKind,
} from "~/lib/seats.ts";
import type {
  AgentRow,
  FeedRow,
  LogRefusal,
  QueryRefusal,
  ReadErrorCode,
  SandboxEntry,
} from "~/protocol/index.ts";
import type { Attention } from "~/lib/attention.ts";

/**
 * How tall a block of machine text grows before it scrolls itself, in px.
 *
 * 460px, which was the block stylesheet's own `max-height` and therefore the
 * height every one of these blocks has had since they were written — not a new
 * number. That rule is gone with the block it dressed, so this constant is now
 * the only place the ceiling is written down at all, which is where it belongs:
 * a height a caller passes cannot also be a height a stylesheet imposes, and
 * the two would drift. It has to be STATED at each call because uilet's
 * CodeBlock is unbounded unless a caller says otherwise, and unbounded is
 * wrong for every record this dashboard shows: a phase's verbatim system
 * prompt runs to tens of kilobytes, an event payload and a whole configuration
 * to hundreds of lines, and one of them at nine hundred lines pushes
 * everything under it off the screen.
 *
 * ONE CONSTANT, because it was five: a named one on the phase card and the
 * bare literal `460` at four call sites on the event and turn screens, which
 * is exactly how the phase card's ceiling and the event screen's come to
 * disagree with nothing to say so.
 */
export const RECORD_MAX_HEIGHT = 460;

/** A seat's name and handle, linked. The one way a person appears in a list. */
export function SeatChip({
  name,
  handle,
  kind,
  size = "sm",
}: {
  name: string;
  handle?: string;
  /**
   * The seat's kind, which decides the badge's one variant.
   *
   * THE SAME PROP [SeatCell] TAKES, and it was a `human` boolean here — one
   * fact spelled two ways across two components drawing the same badge, so a
   * caller holding the chart's answer had to translate it at every site and
   * eighteen of the nineteen simply did not. Both take the pair [seatLookup]
   * returns now, which is one spread and cannot be half-applied.
   */
  kind?: SeatKind;
  size?: "sm" | "md";
}) {
  const target = handle || name;
  return (
    <a
      // `seat-chip`, not a bare link: a seat's name is IDENTITY, and the
      // accent is reserved for saying where the reader is. A name rendered in
      // the accent everywhere it appears is identity-colouring by accident.
      // The affordance is the hover state and the cursor.
      className="row seat-chip"
      style={{ gap: "var(--space-2)", minWidth: 0 }}
      href={href(["company", "people", target])}
    >
      {/* `dashed` IS A HUMAN SEAT, in uilet's own words: its Avatar doc calls
          the drawn edge "a HUMAN seat: the engine does not run it", which is
          the structural fact ours carried. A kind the chart does not hold
          draws the neutral disc rather than claiming the seat is an agent.
          `decorative` because the name is printed immediately beside it —
          without it the row reads "Ada Lovelace avatar, Ada Lovelace". */}
      <Avatar name={name} size={size} variant={kind === "human" ? "dashed" : "solid"} decorative />
      <span className="truncate">{name}</span>
    </a>
  );
}

export function StateBadge({
  agent,
  sandboxes,
}: {
  agent: AgentRow | null | undefined;
  sandboxes: SandboxEntry[];
}) {
  const state = runState(agent, sandboxes);
  return (
    <Tag variant={uiletTone(toneOf(state))} dot>
      {stateLabel(state)}
    </Tag>
  );
}

export function SeatCard({
  seat,
  agent,
  sandboxes,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  sandboxes: SandboxEntry[];
}) {
  const sandbox = sandboxFor(sandboxes, agent);
  const tone = seat.kind === "human" ? "quiet" : seatTone(agent, sandboxes);
  const call = agent?.live_call;
  // Decoded ONCE, by the helper the attention queue also reads: the number on
  // this card and the sentence in that row are the same reading of one field.
  const round = call ? roundLabel(call.round_num) : null;
  return (
    // `seatPath`, not a handle spelled out again: a seat the engine reported no
    // handle for is addressed by NAME, and `#/company/people/` opens nothing.
    <a className="seat-card" data-tone={tone} href={href(seatPath(seat))}>
      <div className="row">
        <Avatar
          name={seat.name}
          size="lg"
          variant={seat.kind === "human" ? "dashed" : "solid"}
          decorative
        />
        <div className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
          <strong className="truncate t-body">{seat.name}</strong>
          <span className="truncate t-caption mono">@{seat.handle}</span>
        </div>
        {seat.kind === "human" ? (
          <Tag appearance="outline">human</Tag>
        ) : (
          <StateBadge agent={agent} sandboxes={sandboxes} />
        )}
      </div>
      <div className="seat-line truncate">{statusLine(agent, { sandbox, seat })}</div>
      {call?.in_progress && (
        <div className="row gap-1">
          {/* THE PHASE IN THE PHASE'S OWN HUE, drawn by the one component that
              holds the table. Phase is the single categorical identity this
              product spends colour on outside a chart, and the whole reason it
              spends it is that a reader FOLLOWS a phase across screens — so one
              `info` pill for every phase cost twice: `execute` and `review` were
              the same fill side by side on the roster, and the same phase was a
              different colour one screen over from the phase card, the turn card
              and the Trace, Seat and Model screens. `PhaseTag` also lowercases
              (the value is a store column and nothing normalises its case on the
              way out) and names an absent phase, where this drew a coloured gap.
              The note it replaces called the miss deliberate because `PhaseTag`
              was somebody else's file during the uilet port; it is a `~/ui`
              primitive this module already imports for `uiletTone`, so there was
              never a second spelling to avoid — only a second colour. */}
          <PhaseTag phase={call.phase} />
          {/* NOT A BARE DASH. "round —" on a seat that is plainly working reads
              as a field the engine failed to report; the engine reported it
              exactly — `-1` is the opening frame a phase publishes before its
              first provider call, so the phase has started and its first model
              round has not come back. `t-num` stays: it is what keeps the digits
              from jittering as the round advances, and it does nothing to a
              word. */}
          <span className="t-caption t-num" title={round?.hint}>
            {round?.text}
          </span>
          <span className="spacer" />
          <span className="t-caption">
            <ClockText read={(now) => relTime(call.updated_at, now)} />
          </span>
        </div>
      )}
      {seat.unit && <div className="t-caption truncate">{seat.unit.name}</div>}
    </a>
  );
}

/**
 * One obligation.
 *
 * Every row says WHAT happened and WHAT IT COSTS to leave it — the second half
 * is the part a list of conditions usually omits, and it is the half that lets
 * a reader decide whether to act now.
 */
export function AttentionRow({ item }: { item: Attention }) {
  // THE WORDS, NOT THE SECOND: a row renders when "4m ago" becomes "5m ago".
  const ago = useClockReading((now) => relTime(item.at, now));
  const inner = (
    <>
      <span className="attention-icon">
        <Mark name={item.icon} size="sm" />
      </span>
      <span className="col" style={{ gap: 2, flex: 1, minWidth: 0 }}>
        <span className="t-body" style={{ fontWeight: "var(--fw-medium)" }}>
          {item.title}
        </span>
        <span className="t-caption">{item.detail}</span>
      </span>
      {item.at && (
        <time className="t-caption nowrap" dateTime={item.at} title={fmtDateTime(item.at)}>
          {ago}
        </time>
      )}
    </>
  );
  if (!item.path) {
    return (
      <div className="attention-row" data-severity={item.severity}>
        {inner}
      </div>
    );
  }
  return (
    <a
      className="attention-row clickable"
      data-severity={item.severity}
      href={href(item.path, item.query)}
    >
      {inner}
    </a>
  );
}

/**
 * One row of the event log.
 *
 * Four fixed columns — when, who, what, where — so a run of rows scans as
 * columns rather than as prose. The category is a WORD, not a hue: eight
 * coloured category chips in one list, repeated on every row, is most of what
 * made the old feed unreadable.
 */
export function EventRow({ event, onOpen }: { event: FeedRow; onOpen?: () => void }) {
  // THE ONLY THING ON THE ROW THE CLOCK MOVES is the relative half of its
  // title, and the row reads it as WORDS: subscribed to the second, every row
  // of a four-hundred-row log rendered once a second for a tooltip, where now
  // a row renders when "4m ago" becomes "5m ago".
  const ago = useClockReading((now) => relTime(event.timestamp, now));
  const body = (
    <>
      <time
        className="feed-time"
        dateTime={event.timestamp}
        title={`${fmtDateTime(event.timestamp)} · ${ago}`}
      >
        {/* A WALL CLOCK, AND THE TRACK IS SIZED FOR ONE. The full instant is
            in the title; which DAY a row belongs to is a heading between days
            (`routes/activity/Activity.tsx`), because a date is a property of
            the rows under it rather than of the first of them — and rendered
            here it put `fmtDateTime` in a 62px column and wrapped one row per
            day to three lines. */}
        {fmtTime(event.timestamp)}
      </time>
      <span className="feed-actor truncate">{event.actor || "engine"}</span>
      <span className="feed-what truncate">
        {event.failed && (
          <WarningGlyph
            size="xs"
            style={{ display: "inline", color: "var(--critical-ink)", marginRight: 4 }}
          />
        )}
        {event.summary || event.type}
      </span>
      <span className="feed-tail">
        {event.source && <span className="truncate">{event.source}</span>}
        <span className="muted">{humanize(event.category) || "system"}</span>
      </span>
    </>
  );
  return (
    <a
      className={cx("feed-row", event.failed && "failed")}
      href={href(["activity", "events", event.id])}
      onClick={onOpen}
    >
      {body}
    </a>
  );
}

/**
 * A named band of a screen.
 *
 * uilet's `Section` is this, and it brings the half ours never had: the
 * heading is a REAL heading at the level the surrounding surface declares, so
 * a screen's outline has no gaps and nothing inside it counts levels by hand.
 * Ours rendered a `t-heading` span, which is a heading to a reader who can see
 * one and nothing at all to a reader navigating by them.
 *
 * The props keep our names so no caller changes: `hint` is its `description`.
 */
export function Section({
  title,
  hint,
  actions,
  children,
}: {
  title: ReactNode;
  hint?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <UiSection title={title} description={hint} actions={actions}>
      {children}
    </UiSection>
  );
}

/**
 * What each refusal MEANS, in the reader's terms.
 *
 * A TABLE OVER THE TYPE, not a chain of string comparisons, and the type is
 * the point: `QueryErrorCode` names the nine codes the engine can send and was
 * declared, documented and never used to type anything — so a branch comparing
 * against `"unavaliable"` compiled cleanly and quietly rendered the generic
 * failure for a projection that was merely catching up. Two of the nine had no
 * branch at all for the same reason: `closed` told the reader "the engine
 * refused this query", which it did not — the socket went away.
 *
 * `Record<ReadErrorCode, …>` is what makes that checkable: a code added to
 * the type without a sentence here is a compile error. The union is the
 * socket's `QueryErrorCode` and the one code a REST read adds, `unanswered`,
 * because a screen reading over REST draws its failure through this same
 * table and its own failure needs its own true sentence.
 */
const REFUSALS: Record<ReadErrorCode, ReactNode> = {
  unauthorized: (
    // `action` IS the spacer-then-control our markup spelled by hand: a
    // Callout puts its control at the trailing edge, so the `spacer` span
    // that used to push the button there has nothing left to do.
    <Callout
      variant="warning"
      icon={<KeyGlyph size="md" />}
      action={
        // A BANNER THAT NAMES A REPAIR OFFERS IT. The sign-in comes back to
        // this screen, so somebody who holds the grant lands where the
        // refusal was.
        <Button size="small" variant="secondary" leadingIcon={<KeyGlyph />} onClick={goSignIn}>
          Sign in
        </Button>
      }
    >
      {/* ABOUT THE GRANT, NOT A TOKEN. It said this needed "an API token
          matching one of your api.auth.tokens entries", which was the whole
          of authority while a Tier A token was the only credential; a person
          signed in with a session and refused one grant was sent to find a
          token they have no use for. */}
      The credential you presented does not carry the grant this answer needs, or none was
      presented. Sign in as somebody who holds it.
    </Callout>
  ),
  // NO `icon` ON THIS ONE, OR ON THE THREE BELOW IT. A Callout draws its
  // variant's own mark, and for neutral that is the info glyph and for danger
  // the error glyph — which is exactly what these passed by hand. An icon prop
  // repeating the variant is a second place for the two to disagree.
  unknown_query: (
    <Callout variant="neutral">
      The engine does not serve this answer — the subsystem behind it is not running on this node.
    </Callout>
  ),
  bad_params: (
    <Callout variant="warning">
      The engine refused this request: something it needs was missing or not a value it accepts.
      Retrying sends the same request — this is the screen&rsquo;s bug to fix, not a fault on the
      node.
    </Callout>
  ),
  unavailable: (
    <Callout variant="neutral" icon={<ScheduleGlyph size="md" />}>
      This node cannot answer yet: its copy of the company&rsquo;s records is still catching up, or
      it could not reach the coordination store for a moment. Nothing is lost, and this screen asks
      again on its own.
      <strong> This is not an empty company.</strong>
    </Callout>
  ),
  not_found: (
    <Callout variant="neutral">
      There is no such record. The link may point at something that was removed.
    </Callout>
  ),
  timeout: (
    <Callout variant="warning" icon={<ScheduleGlyph size="md" />}>
      The engine did not answer within 10 seconds. It may be under load.
    </Callout>
  ),
  // THE TWO THAT HAD NO SENTENCE. Both used to render "the engine refused
  // this query", which is wrong about each of them in a different way.
  query_failed: (
    <Callout variant="danger">
      The engine tried to answer and failed. This is a fault on the node rather than a refusal — its
      log says what went wrong.
    </Callout>
  ),
  closed: (
    // A SOCKET, drawn as one. `plug` was ours; `Cable` is the nearest thing
    // uilet vendors and says the same thing about a connection that went away.
    <Callout variant="neutral" icon={<CableGlyph size="md" />}>
      The connection went away before this answered. Nothing refused it; the screen reads again once
      the socket is back.
    </Callout>
  ),
  // A REST READ NO ANSWER CAME BACK TO, which is not `closed`: the socket can
  // be up the whole time — one request past its deadline on a slow engine, or
  // dropped on the way — and a banner promising a read "once the socket is
  // back" was waiting on an event that never came. Nothing here knows what the
  // engine would have said, so the sentence says only what is true of all of
  // it, and the read backs off on its own (`restRetryMs`).
  unanswered: (
    <Callout variant="neutral" icon={<ScheduleGlyph size="md" />}>
      No answer from the engine reached this page: it took too long, the request was lost on the
      way, or something in front of the engine answered in its place. Nothing refused it, and this
      screen asks again on its own.
    </Callout>
  ),
};

/**
 * A refusal on authority that says WHY: the grants any one of which would have
 * admitted the reader, or — an empty list — that none would, because what is
 * missing is a relation the org chart does not hold.
 *
 * FROM THE ANSWER, NEVER WRITTEN HERE. The engine's error frame carries the
 * deciding rule's own reason and grants under the keys its REST envelope uses;
 * a sentence naming a grant typed into this file is a second statement of the
 * rule, and it is the one that goes stale the day the rule's grant moves.
 */
function RefusedOnAuthority({ refusal }: { refusal: QueryRefusal }) {
  return (
    <Callout
      variant="warning"
      icon={<KeyGlyph size="md" />}
      action={
        <Button size="small" variant="secondary" leadingIcon={<KeyGlyph />} onClick={goSignIn}>
          Sign in
        </Button>
      }
    >
      {refusal.grants.length > 0 ? (
        <>
          You may not read this: the answer needs{" "}
          {refusal.grants.map((grant, i) => (
            <span key={grant}>
              {i > 0 ? " or " : ""}
              <code className="inline">{grant}</code>
            </span>
          ))}
          , and the credential you presented carries none of them. Sign in as somebody who holds
          one.
        </>
      ) : (
        <>
          You may not read this, and no grant would change that: the rule that decided asks a
          relation the org chart does not hold — leading this person, or the record being your own.
        </>
      )}{" "}
      <span className="t-caption">
        Refused as <code className="inline">{refusal.reason}</code>.
      </span>
    </Callout>
  );
}

/**
 * A refusal by this node's STATE LOG that waiting will not clear — its log at
 * the byte ceiling, a record it cannot decode, a barrier its broker refused —
 * said as what it is rather than as the node catching up.
 *
 * `unavailable` covers both, and the generic banner promises "this screen asks
 * again on its own", which for these would be a promise of a loop: the same
 * read is refused until an operator acts or another node is asked. So the
 * banner names the refusal and its own words, the remedy, and says the screen
 * is NOT asking again — nothing re-asks it on a timer, a poll included, so it
 * says what does: a reload once somebody has acted (a reconnect asks again
 * too, when the fix restarted the node). FROM THE ANSWER, NEVER WRITTEN HERE,
 * for the reason `RefusedOnAuthority` gives.
 */
function RefusedByTheLog({ refusal }: { refusal: LogRefusal }) {
  return (
    <Callout variant="warning" icon={<WarningGlyph size="md" />}>
      This node refused the read, and asking it again will not change that
      {refusal.detail ? <>: {refusal.detail}</> : null}. Another node can answer it, or an operator
      has to act on this one; the screen does not keep asking it, so reload it once they have.
      <strong> This is not an empty company.</strong>
      {refusal.code ? (
        <>
          {" "}
          <span className="t-caption">
            Refused as <code className="inline">{refusal.code}</code>.
          </span>
        </>
      ) : null}
    </Callout>
  );
}

/**
 * What an empty or failed answer means, said precisely.
 *
 * `unauthorized` and a company with no seats are the same empty list and
 * completely different problems; so are `unknown_query` and "nothing has
 * happened yet". Every screen routes its failure through here so the
 * distinction is made once.
 */
export function QueryState({
  error,
  refusal,
  loading,
  empty,
  children,
}: {
  error: string | null;
  /**
   * Why the answer was refused — `useQuery`'s own `refusal`. For an
   * `unauthorized` answer the banner then says which grant would have
   * admitted the reader, or that none would; for an `unavailable` one the
   * state log refused and waiting will not change, it says so and what the
   * refusal names, rather than that the node is catching up.
   */
  refusal?: QueryRefusal | LogRefusal | null;
  loading: boolean;
  /**
   * `hint` IS REQUIRED, which is uilet's `EmptyState` rule and the reason this
   * component exists at all: "No model calls" on a window nothing ran in and
   * on a node whose store could not be read are the same headline and
   * completely different problems, and only the second sentence separates
   * them. It was optional while one caller in the tree had no second sentence
   * (`routes/cost/Spend.tsx`); that one now says what to do about it, so the
   * type says what the component always meant.
   */
  empty?: { title: ReactNode; hint: ReactNode };
  children?: ReactNode;
}) {
  if (error === "unauthorized" && refusal && !isLogRefusal(refusal)) {
    return <RefusedOnAuthority refusal={refusal} />;
  }
  if (error === "unavailable" && refusal && isLogRefusal(refusal) && refusal.retryAfter === 0) {
    return <RefusedByTheLog refusal={refusal} />;
  }
  const banner = error ? REFUSALS[error as ReadErrorCode] : undefined;
  if (banner) return <>{banner}</>;
  // A CODE THIS BUILD DOES NOT KNOW. A newer node may send one — the wire
  // evolves additively — and naming it is more use than calling it a refusal.
  if (error) {
    return (
      <Callout variant="danger">
        The engine answered with a code this build does not know ({error}).
      </Callout>
    );
  }
  if (loading) return null;
  if (empty) {
    return (
      // `size="compact"` IS our `inline`, and the default mark is the inbox
      // glyph ours passed by hand — so both props this used to spell are the
      // component's own defaults now.
      <EmptyState size="compact" title={empty.title} description={empty.hint} />
    );
  }
  return <>{children}</>;
}
