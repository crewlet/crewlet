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
  Avatar,
  Button,
  Callout,
  EmptyState,
  InlineCode,
  Section as UiSection,
  Tag,
  cx,
} from "@crewlethq/ui";
import {
  CableGlyph,
  DatabaseGlyph,
  KeyGlyph,
  ScheduleGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
// STILL OURS: an attention row's mark is named by `lib/attention.ts` as a
// value, and uilet's glyphs are components. The name -> drawing lookup stays
// in `~/ui/Icon.tsx`, which is the one place a port of it moves every caller
// at once - the same call `app/frame/cells.tsx` makes for the same reason.
import { Mark } from "~/ui/glyph.tsx";
import { uiletTone } from "~/ui/primitives.tsx";
import { href } from "~/app/router.tsx";
import { fmtDateTime, fmtTime, humanize, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { requestToken } from "~/protocol/index.ts";
import { runState, seatTone, stateLabel, statusLine, toneOf, type Seat } from "~/lib/seats.ts";
import type { AgentRow, FeedRow, QueryErrorCode, SandboxEntry } from "~/protocol/index.ts";
import type { Attention } from "~/lib/attention.ts";

/**
 * How tall a block of machine text grows before it scrolls itself, in px.
 *
 * 460px, which is `.code`'s own ceiling in components.css and therefore the
 * height every one of these blocks has had since they were written — not a new
 * number. It has to be STATED at each call because uilet's CodeBlock is
 * unbounded unless a caller says otherwise, and unbounded is wrong for every
 * record this dashboard shows: a phase's verbatim system prompt runs to tens
 * of kilobytes, an event payload and a whole configuration to hundreds of
 * lines, and one of them at nine hundred lines pushes everything under it off
 * the screen.
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
  human,
  size = "sm",
}: {
  name: string;
  handle?: string;
  human?: boolean;
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
      {/* `dashed` IS our `human`, in uilet's own words: its Avatar doc calls
          the drawn edge "a HUMAN seat: the engine does not run it", which is
          the structural fact ours carried. `decorative` because the name is
          printed immediately beside it — without it the row reads "Ada
          Lovelace avatar, Ada Lovelace". */}
      <Avatar name={name} size={size} variant={human ? "dashed" : "solid"} decorative />
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
  const now = useNow();
  const sandbox = sandboxes.find((s) => s.role === seat.name) ?? null;
  const tone = seat.kind === "human" ? "quiet" : seatTone(agent, sandboxes);
  const call = agent?.live_call;
  return (
    <a className="seat-card" data-tone={tone} href={href(["company", "people", seat.handle])}>
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
          {/* `info`, NOT the phase vocabulary, deliberately. `TagVariant`
              carries `phase-execute` and the rest, but this pill is drawn
              beside a seat's run state rather than inside a turn, and our
              `PhaseTag` — which is where the phase hues belong — is a
              `~/ui` primitive another port owns. Two spellings of one
              phase in two colours is worse than the tone this already had. */}
          <Tag variant="info">{call.phase}</Tag>
          <span className="t-caption t-num">
            round {call.round_num >= 0 ? call.round_num + 1 : "—"}
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
 * One obligation.
 *
 * Every row says WHAT happened and WHAT IT COSTS to leave it — the second half
 * is the part a list of conditions usually omits, and it is the half that lets
 * a reader decide whether to act now.
 */
export function AttentionRow({ item }: { item: Attention }) {
  const now = useNow();
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
          {relTime(item.at, now)}
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
export function EventRow({
  event,
  onOpen,
  showDate,
}: {
  event: FeedRow;
  onOpen?: () => void;
  showDate?: boolean;
}) {
  const now = useNow();
  const body = (
    <>
      <time
        className="feed-time"
        dateTime={event.timestamp}
        title={`${fmtDateTime(event.timestamp)} · ${relTime(event.timestamp, now)}`}
      >
        {showDate ? fmtDateTime(event.timestamp) : fmtTime(event.timestamp)}
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
        // The banner used to say "set a token" and offer nothing that could.
        // With anonymous reads allowed the socket is never refused, so the
        // dialog's only other doors — a socket refusal, and the palette —
        // both stay shut on exactly the screen that needs it.
        <Button size="small" variant="secondary" leadingIcon={<KeyGlyph />} onClick={requestToken}>
          Set token
        </Button>
      }
    >
      This answer is auth-gated. It needs an API token matching one of your{" "}
      <InlineCode tone="inherit">api.auth.tokens</InlineCode> entries.
    </Callout>
  ),
  no_event_store: (
    <Callout variant="neutral" icon={<DatabaseGlyph size="md" />}>
      This node keeps no event log, so there is no history to read. Set{" "}
      <InlineCode tone="inherit">store.path</InlineCode> in{" "}
      <InlineCode tone="inherit">crewlet.yaml</InlineCode> to make it durable.
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
      This node has not finished reading the company&rsquo;s own records yet — its projection is
      still catching up. Nothing is wrong and nothing is lost; the screen fills in on its own.
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
};

/**
 * What an empty or failed answer means, said precisely.
 *
 * `no_event_store` and "nothing has happened yet" are the same empty list and
 * completely different problems; so are `unauthorized` and a company with no
 * seats. Every screen routes its failure through here so the distinction is
 * made once.
 */
export function QueryState({
  error,
  loading,
  empty,
  children,
}: {
  error: string | null;
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
  const refusal = error ? REFUSALS[error as QueryErrorCode] : undefined;
  if (refusal) return <>{refusal}</>;
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
