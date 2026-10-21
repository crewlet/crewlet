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
import {
  Button,
  Callout,
  CodeBlock,
  Disclosure,
  EmptyState,
  Section as UiSection,
  Tag,
  cx,
  InlineCode,
} from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import {
  PlugGlyph,
  KeyGlyph,
  ClockGlyph,
  RefreshCwGlyph,
  ShieldUserGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
// STILL OURS: an attention row's mark is named by `lib/attention.ts` as a
// value, and uilet's glyphs are components. The name -> drawing lookup stays
// in `~/ui/Icon.tsx`, which is the one place a port of it moves every caller
// at once - the same call `app/frame/cells.tsx` makes for the same reason.
import { PhaseTag } from "~/ui/primitives.tsx";
import { href } from "~/app/router.tsx";
import { fmtDateTime, fmtTime, humanize, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { confirmStepUp, isLogRefusal } from "~/protocol/index.ts";
import { goSignIn, page } from "~/lib/session.ts";
import { needsWords } from "~/lib/access.ts";
import { remedyFor, type AuthorityAction } from "~/lib/authorityRemedy.ts";
import {
  activityOf,
  shownStateOf,
  stateWord,
  handleLabel,
  statusOf,
  roundLabel,
  seatPath,
  stateLine,
  toneOf,
  type NameOf,
  type Seat,
  type SeatKind,
} from "~/lib/seats.ts";
import type { AgentAvatar } from "~/lib/avatar.ts";
import type { AgentRow, FeedRow, LogRefusal, QueryRefusal } from "~/protocol/index.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { REFUSAL_SENTENCES } from "~/contract/authority.ts";

/** A seat's name and handle, linked. The one way a person appears in a list. */
export function SeatChip({
  name,
  handle,
  kind,
  avatar,
  size = "sm",
}: {
  name: string;
  handle?: string;
  /**
   * The seat's kind, which decides the badge's one variant.
   *
   * THE SAME PROP [linkedSeat] TAKES, and it was a `human` boolean here: one
   * fact spelled two ways across two components drawing the same badge, so a
   * caller holding the chart's answer had to translate it at every site and
   * eighteen of the nineteen simply did not. Both take the pair [seatLookup]
   * returns now, which is one spread and cannot be half-applied.
   */
  kind?: SeatKind;
  /** How an agent is drawn, where the chart holds the seat: also off [seatLookup]. */
  avatar?: AgentAvatar | null | undefined;
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
      href={href(["company", "org", "seats", target])}
    >
      {/* THE KIND IS THE OUTLINE: the kit draws a person as a circle and an
          agent as a squircle, and that is the one cue telling them apart. A
          kind the chart does not hold takes the kit's default, the agent's
          squircle — the engine runs agents, and a person is always declared.
          `decorative` because the name is printed immediately beside it —
          without it the row reads "Ada Lovelace avatar, Ada Lovelace". */}
      <SeatAvatar
        name={name}
        size={size}
        kind={kind === "human" ? "human" : "agent"}
        avatar={avatar}
        decorative
      />
      <span className="truncate">{name}</span>
    </a>
  );
}

/**
 * A seat's state as a pill: the engine's word and its status badge's tone. An idle
 * seat is the neutral pill — a green one read as activity.
 */
export function StateBadge({ agent }: { agent: AgentRow | null | undefined }) {
  // A PAUSED SEAT READS "Paused", as its own page, the chart and the Inbox say.
  const state = shownStateOf(agent);
  return (
    // SENTENCE CASE, as the seat's own page and the chart write the state
    // (`stateWord`): the roster's "idle" beside the seat page's "Idle" was one
    // state in two spellings.
    <Tag variant={toneOf(state)} dot>
      {stateWord(state)}
    </Tag>
  );
}

/**
 * The engine's own words for a failure, folded behind the kit's disclosure.
 *
 * NEVER THE HEADLINE. A banner or a reading pane leads with the plain sentence
 * (`failureWords` in lib/seats.ts); the raw message ("llm chain exhausted
 * after 1 providers ...") is what an operator fixing it reads, so it stays one
 * press away, selectable, and is drawn nowhere else. The engine sends it to an
 * admin alone, since it names the company's model entries, so for every other
 * reader there is nothing to draw.
 */
export function TechnicalDetails({ text }: { text: string | null | undefined }) {
  if (!text) return null;
  return (
    <Disclosure title="Technical details" size="compact" headingLevel="none" lazy>
      <CodeBlock plain selectable label="The engine's message" code={text} />
    </Disclosure>
  );
}

export function SeatCard({
  seat,
  agent,
  nameOf,
  stateInHeading = false,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  /** A person's name by their handle, off the chart the caller holds — see `NameOf`. */
  nameOf: NameOf;
  /**
   * The list this card stands in is grouped by state, so its heading already
   * says the state: the pill is not drawn a second time on every card under
   * it. The badge's glyph and the line keep what the heading does not say.
   */
  stateInHeading?: boolean;
}) {
  const now = useNow();
  // THE STATUS, OR NONE: a person is not run by the engine, and an idle seat
  // draws none, for the reason `statusOf` gives.
  const status = seat.kind === "human" ? undefined : statusOf(activityOf(agent));
  const call = agent?.live_call;
  // Decoded ONCE, by the helper the attention queue also reads: the number on
  // this card and the sentence in that row are the same reading of one field.
  const round = call ? roundLabel(call) : null;
  return (
    // `seatPath`, not a path spelled out again: a seat's address is one rule,
    // kept in one place.
    <a className="seat-card" href={href(seatPath(seat))}>
      <div className="row">
        {/* THE STATE IS THE GLYPH ON THE BADGE and the pill beside the name,
            never a line round the badge or down the card's edge: a card in
            a state's colour beside a plate in the agent's own put two hues
            on one seat. */}
        <SeatAvatar
          name={seat.name}
          size="lg"
          kind={seat.kind === "human" ? "human" : "agent"}
          avatar={seat.avatar}
          status={status}
          decorative
        />
        <div className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
          <strong className="truncate t-body">{seat.name}</strong>
          <span className="truncate t-caption mono">{handleLabel(seat.handle)}</span>
        </div>
        {seat.kind === "human" ? (
          // THE CIRCLE SAYS IT. A tag reading "human" beside a person's
          // circle was the outline said twice; the word stays for a
          // screen reader, which does not see the outline.
          <span className="sr-only">human</span>
        ) : stateInHeading ? null : (
          <StateBadge agent={agent} />
        )}
      </div>
      <div className="seat-line truncate">{stateLine(agent, { now, seat, nameOf })}</div>
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
          <span className="t-caption">{relTime(call.updated_at, now)}</span>
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
  const now = useNow();
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
      title={`${fmtDateTime(event.timestamp)} · ${relTime(event.timestamp, now)}`}
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
 * `Record<QueryErrorCode, …>` is what makes that checkable: a code added to
 * the type without a sentence here is a compile error.
 */
const REFUSALS: Record<QueryErrorCode, ReactNode> = {
  unauthorized: (
    // `action` IS the spacer-then-control our markup spelled by hand: a
    // Callout puts its control at the trailing edge, so the `spacer` span
    // that used to push the button there has nothing left to do.
    <Callout
      variant="warning"
      icon={<KeyGlyph size="md" />}
      action={
        // A BANNER THAT NAMES A REPAIR OFFERS IT. The sign-in comes back to
        // this screen, so somebody who holds the access lands where the
        // refusal was.
        <Button size="small" variant="secondary" leadingIcon={<KeyGlyph />} onClick={goSignIn}>
          Sign in
        </Button>
      }
    >
      {/* ABOUT ACCESS, NOT A TOKEN: what a signed-in reader refused here
          lacks is access — Admin or an app's — and an answer that names it
          is drawn by `RefusedOnAuthority` below instead. */}
      You don’t have access to this.
    </Callout>
  ),
  // NO `icon` ON THIS ONE, OR ON THE THREE BELOW IT. A Callout draws its
  // variant's own mark, and for neutral that is the info glyph and for danger
  // the error glyph — which is exactly what these passed by hand. An icon prop
  // repeating the variant is a second place for the two to disagree.
  unknown_query: <Callout variant="neutral">Not available on this node.</Callout>,
  bad_params: <Callout variant="warning">The engine refused this request.</Callout>,
  unavailable: (
    <Callout variant="neutral" icon={<ClockGlyph size="md" />}>
      Still catching up. This screen retries on its own.
    </Callout>
  ),
  not_found: <Callout variant="neutral">Not found. It may have been removed.</Callout>,
  artifact_trashed: (
    <Callout variant="neutral">This artifact is in the trash. Its owner can restore it.</Callout>
  ),
  timeout: (
    <Callout variant="warning" icon={<ClockGlyph size="md" />}>
      The engine didn’t answer in time.
    </Callout>
  ),
  // THE TWO THAT HAD NO SENTENCE. Both used to render "the engine refused
  // this query", which is wrong about each of them in a different way.
  query_failed: (
    <Callout variant="danger">
      The engine failed to answer. You can check logs for the details.
    </Callout>
  ),
  closed: (
    // A SOCKET, drawn as one. `plug` was ours; `Cable` is the nearest thing
    // uilet vendors and says the same thing about a connection that went away.
    <Callout variant="neutral" icon={<PlugGlyph size="md" />}>
      Connection lost. The screen reloads when it’s back.
    </Callout>
  ),
};

/**
 * A refusal on authority that says WHY: the access any one of which would
 * have admitted the reader ("needs Worklet access"), or (where none was named,
 * or the rule is one no access changes) what the deciding rule asks, in its
 * own sentence ([REFUSAL_SENTENCES], keyed by the rule's reason).
 *
 * FROM THE ANSWER, NEVER WRITTEN HERE. The engine's error frame carries the
 * deciding rule's own reason and grants under the keys its REST envelope uses;
 * a sentence naming a permission typed into this file is a second statement
 * of the rule, and it is the one that goes stale the day the rule moves. One
 * sentence for every empty list was exactly that: it told a credential bound
 * to no seat, and a token on a gesture that needs the person, that what they
 * lacked was leading somebody. A reason this build has no sentence for says
 * only that no access would change it.
 *
 * AND IT OFFERS WHAT CLEARS THAT RULE ([remedyFor]), never one control for
 * all of them: Sign in for a reader short of access, which somebody holding it
 * clears; Confirm for a proof gone stale, which the reader clears in this tab;
 * Sign in as yourself for a credential acting as no seat; Try again once an
 * admin has activated an account; Reload for a request this engine's build has
 * no rule for — and nothing at all for a rule no reader can clear from here.
 * It offered Sign in to every one of them, so a reader whose account was not
 * active signed in again to the same refusal.
 */
function RefusedOnAuthority({ refusal }: { refusal: QueryRefusal }) {
  const remedy = remedyFor(refusal.reason, refusal.grants);
  return (
    <Callout
      variant="warning"
      icon={<KeyGlyph size="md" />}
      action={remedy.action ? <AuthorityRemedy action={remedy.action} /> : undefined}
    >
      {remedy.namesAccess
        ? needsWords("This answer", refusal.grants)
        : (REFUSAL_SENTENCES[refusal.reason] ??
          "You can’t see this, and no access would change that.")}{" "}
      <span className="t-caption">
        Refused as <InlineCode>{refusal.reason}</InlineCode>.
      </span>
    </Callout>
  );
}

/**
 * The one control a refusal on authority offers, each the move that clears its
 * rule. Signing in comes back to this screen, so somebody who holds the access
 * lands where the refusal was; a confirmed step-up replaces the session, which
 * re-dials the socket, and every read on the screen asks again on the
 * reconnect; asking again and reloading reload this screen.
 */
function AuthorityRemedy({ action }: { action: AuthorityAction }) {
  const controls: Record<AuthorityAction, [string, ReactNode, () => void]> = {
    sign_in: ["Sign in", <KeyGlyph />, goSignIn],
    sign_in_yourself: ["Sign in as yourself", <KeyGlyph />, goSignIn],
    confirm: ["Confirm it’s you", <ShieldUserGlyph />, () => void confirmStepUp()],
    try_again: ["Try again", <RefreshCwGlyph />, () => page.reloadInto(location.hash)],
    reload: ["Reload", <RefreshCwGlyph />, () => page.reloadInto(location.hash)],
  };
  const [label, icon, act] = controls[action];
  return (
    <Button size="small" variant="secondary" leadingIcon={icon} onClick={act}>
      {label}
    </Button>
  );
}

/**
 * A refusal by this node's STATE LOG that waiting will not clear — its log at
 * the byte ceiling, a record it cannot decode, a barrier its broker refused —
 * said as what it is rather than as the node catching up.
 *
 * `unavailable` covers both, and the generic banner promises "this screen asks
 * again on its own", which for these would be a promise of a loop: the same
 * read is refused until an admin acts or another node is asked. So the
 * banner says the one thing the reader can do: ask another node.
 */
function RefusedByTheLog() {
  return (
    <Callout variant="warning" icon={<TriangleAlertGlyph size="md" />}>
      Couldn’t load this on this node. Try another node.
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
   * Why the answer was refused — `useQuery`'s and `useRest`'s own `refusal`.
   * For an `unauthorized` answer the banner then says which grant would have
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
  empty?: {
    title: ReactNode;
    hint: ReactNode;
    /** The one way on from an empty answer, where the reader has one. */
    action?: ReactNode;
    /**
     * `"none"` where the card the claim sits in already names it with a
     * title of its own, as the kit's `EmptyState` asks of such a claim.
     */
    headingLevel?: "none";
  };
  children?: ReactNode;
}) {
  if (error === "unauthorized" && refusal && !isLogRefusal(refusal)) {
    return <RefusedOnAuthority refusal={refusal} />;
  }
  if (error === "unavailable" && refusal && isLogRefusal(refusal) && refusal.retryAfter === 0) {
    return <RefusedByTheLog />;
  }
  if (error === "bad_params" && detail) {
    return <Callout variant="warning">The engine refused this request: {detail}</Callout>;
  }
  const banner = error ? REFUSALS[error as QueryErrorCode] : undefined;
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
      <EmptyState
        size="compact"
        headingLevel={empty.headingLevel}
        title={empty.title}
        description={empty.hint}
        action={empty.action}
      />
    );
  }
  return <>{children}</>;
}
