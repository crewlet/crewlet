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
import { Button, Callout, EmptyState, Section as UiSection, Tag, cx } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { PlugGlyph, KeyGlyph, ClockGlyph, TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { PhaseTag } from "~/ui/primitives.tsx";
import { href } from "~/app/router.tsx";
import { fmtDateTime, fmtTime, humanize, relTime } from "~/lib/format.ts";
import { useClockReading } from "~/lib/clock.ts";
import { ClockText } from "~/app/frame/cells.tsx";
import { isLogRefusal } from "~/protocol/index.ts";
import { goSignIn } from "~/lib/session.ts";
import {
  activityOf,
  activityWord,
  handleLabel,
  ringOf,
  roundLabel,
  seatPath,
  stateLine,
  toneOf,
  type NameOf,
  type Seat,
  type SeatKind,
} from "~/lib/seats.ts";
import type {
  AgentRow,
  FeedRow,
  LogRefusal,
  QueryRefusal,
  ReadErrorCode,
} from "~/protocol/index.ts";

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
      style={{ gap: "var(--spacing-2)", minWidth: 0 }}
      href={href(["agents", "seats", target])}
    >
      {/* THE KIND IS THE OUTLINE: the kit draws a person as a circle and an
          agent as a squircle, and that is the one cue telling them apart. A
          kind the chart does not hold takes the kit's default, the agent's
          squircle — the engine runs agents, and a person is always declared.
          `decorative` because the name is printed immediately beside it —
          without it the row reads "Ada Lovelace avatar, Ada Lovelace". */}
      <SeatAvatar name={name} size={size} kind={kind === "human" ? "human" : "agent"} decorative />
      <span className="truncate">{name}</span>
    </a>
  );
}

/**
 * A seat's state as a pill: the engine's word and its ring's tone. An idle
 * seat is the neutral pill — a green one read as activity.
 */
export function StateBadge({ agent }: { agent: AgentRow | null | undefined }) {
  const state = activityOf(agent);
  return (
    <Tag variant={toneOf(state)} dot>
      {activityWord(state)}
    </Tag>
  );
}

export function SeatCard({
  seat,
  agent,
  nameOf,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  /** A person's name by their handle, off the chart the caller holds — see `NameOf`. */
  nameOf: NameOf;
}) {
  // THE RING'S TONE, OR NONE: a person is not run by the engine, and an idle
  // seat draws no edge, for the reason `ringOf` gives.
  const tone = seat.kind === "human" ? undefined : ringOf(activityOf(agent));
  const call = agent?.live_call;
  // Decoded ONCE, by the helper the attention queue also reads: the number on
  // this card and the sentence in that row are the same reading of one field.
  const round = call ? roundLabel(call) : null;
  return (
    // `seatPath`, not a handle spelled out again: a seat the engine reported no
    // handle for is addressed by NAME, and `#/agents/seats/` opens nothing.
    <a className="seat-card" data-tone={tone} href={href(seatPath(seat))}>
      <div className="row">
        {/* THE RING IS THE STATE, as on the chart's cards: one hue per seat,
            and it is what the seat is doing. */}
        <SeatAvatar
          name={seat.name}
          size="lg"
          kind={seat.kind === "human" ? "human" : "agent"}
          {...(tone ? { ring: tone } : {})}
          decorative
        />
        <div className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
          <strong className="truncate t-body">{seat.name}</strong>
          {seat.handle && (
            <span className="truncate t-caption mono">{handleLabel(seat.handle)}</span>
          )}
        </div>
        {seat.kind === "human" ? (
          // THE CIRCLE SAYS IT. A tag reading "human" beside a person's
          // circle was the outline said twice; the word stays for a
          // screen reader, which does not see the outline.
          <span className="sr-only">human</span>
        ) : (
          <StateBadge agent={agent} />
        )}
      </div>
      <div className="seat-line truncate">
        {/* THE CLOCK IS READ IN THE LINE THAT SHOWS IT: a card subscribed to
            the second redrew its badge, its name and its phase once a second
            for the one part of it whose words move. */}
        <ClockText read={(now) => stateLine(agent, { now, seat, nameOf })} />
      </div>
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
              primitive this module already imports, so there was never a
              second spelling to avoid — only a second colour. */}
          <PhaseTag phase={call.phase} />
          {/* NOT A BARE DASH. "round —" on a seat that is plainly working reads
              as a field the engine failed to report; the engine reported it
              exactly — `-1` is the opening frame a phase publishes before its
              first provider call, so round 1 is in flight, and the hint says it
              has not come back. `t-num` keeps the digits from jittering as the
              round advances. */}
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
 * One row of the event log.
 *
 * Four fixed columns — when, who, what, where — so a run of rows scans as
 * columns rather than as prose. The category is a WORD, not a hue: eight
 * coloured category chips in one list, repeated on every row, is most of what
 * made the old feed unreadable.
 *
 * WHO IS SAID ONCE. An engine event's source and actor are routinely the same
 * name — a lifecycle event is published BY the node and ABOUT the node — and
 * drawing both put `harness-0` in two adjacent columns while the sentence
 * between them was cut to "Organization…". The tail names the source only
 * where it differs from the actor.
 *
 * `compact` is the row a CARD draws, beside other cards: the time and one
 * line that is the actor followed by what happened, with no tail — the
 * source and the category are the log's columns, one click away, and at half
 * a page's width they were what the sentence gave its room up to.
 */
export function EventRow({
  event,
  onOpen,
  compact = false,
}: {
  event: FeedRow;
  onOpen?: () => void;
  compact?: boolean;
}) {
  // THE ONLY THING ON THE ROW THE CLOCK MOVES is the relative half of its
  // title, and the row reads it as WORDS: subscribed to the second, every row
  // of a four-hundred-row log rendered once a second for a tooltip, where now
  // a row renders when "4m ago" becomes "5m ago".
  const ago = useClockReading((now) => relTime(event.timestamp, now));
  const actor = event.actor || "engine";
  const what = event.summary || event.type;
  const failedMark = event.failed && (
    <TriangleAlertGlyph
      size="xs"
      aria-label="failed"
      style={{ display: "inline", color: "var(--color-feedback-danger-ink)", marginRight: 4 }}
    />
  );
  const time = (
    <time
      className="feed-time"
      dateTime={event.timestamp}
      title={`${fmtDateTime(event.timestamp)} · ${ago}`}
    >
      {/* A WALL CLOCK, AND THE TRACK IS SIZED FOR ONE. The full instant is
          in the title; which DAY a row belongs to is a heading between days
          (`routes/live/Activity.tsx`), because a date is a property of
          the rows under it rather than of the first of them — and rendered
          here it put `fmtDateTime` in a 62px column and wrapped one row per
          day to three lines. */}
      {fmtTime(event.timestamp)}
    </time>
  );
  const source = event.source && event.source !== actor ? event.source : "";
  // THE ENGINE'S OWN SENTENCES OFTEN OPEN WITH THEIR ACTOR ("Agent PM
  // finished reflecting"), so a card's line prefixed with the actor read
  // "Agent PM Agent PM finished…". Where the sentence already names them
  // first, the actor IS its opening words, drawn in the actor's weight.
  const opens = what.startsWith(`${actor} `);
  const rest = opens ? what.slice(actor.length) : ` ${what}`;
  // THE HOVER TEXT IS THE DRAWN LINE, built from the same two pieces: it
  // spelled "actor — what" over a line reading "actor what", so the full
  // text of a cut line was not the text that was cut.
  const said = `${actor}${rest}`;
  const body = compact ? (
    <>
      {time}
      {/* ONE LINE, and its title is the whole of it: a sentence cut at the
          card's edge is still readable on hover. */}
      <span className="feed-what truncate" title={said}>
        {failedMark}
        <span className="feed-actor">{actor}</span>
        {rest}
      </span>
    </>
  ) : (
    <>
      {time}
      <span className="feed-actor truncate">{actor}</span>
      <span className="feed-what truncate" title={what}>
        {failedMark}
        {what}
      </span>
      <span className="feed-tail">
        {source && <span className="truncate">{source}</span>}
        <span className="muted">{humanize(event.category) || "system"}</span>
      </span>
    </>
  );
  return (
    <a
      className={cx("feed-row", compact && "compact", event.failed && "failed")}
      href={href(["live", "events", event.id])}
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
    <Callout variant="neutral" icon={<ClockGlyph size="md" />}>
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
    <Callout variant="warning" icon={<ClockGlyph size="md" />}>
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
    // A SOCKET, drawn as one: the kit's plug, a connection that went away.
    <Callout variant="neutral" icon={<PlugGlyph size="md" />}>
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
    <Callout variant="neutral" icon={<ClockGlyph size="md" />}>
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
    <Callout variant="warning" icon={<TriangleAlertGlyph size="md" />}>
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
  detail,
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
  /**
   * The engine's own sentence on a `bad_params` refusal (`useQuery`'s
   * `detail`). Given, the refusal is the ENGINE'S words about what to change
   * rather than this component's guess that the screen asked wrong — a window
   * past the spend history is the reader's to change, not a bug.
   */
  detail?: string;
  loading: boolean;
  /**
   * `hint` IS REQUIRED, which is uilet's `EmptyState` rule and the reason this
   * component exists at all: "No model calls" on a window nothing ran in and
   * on a node whose store could not be read are the same headline and
   * completely different problems, and only the second sentence separates
   * them. It was optional while one caller in the tree had no second sentence
   * (`routes/spend/Spend.tsx`); that one now says what to do about it, so the
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
  if (error === "bad_params" && detail) {
    return <Callout variant="warning">The engine refused this request: {detail}</Callout>;
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
