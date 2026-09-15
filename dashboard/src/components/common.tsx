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
import { href } from "~/app/router.tsx";
import { fmtDateTime, fmtTime, humanize } from "~/lib/format.ts";
import { requestToken } from "~/protocol/index.ts";
import {
  runState,
  seatPath,
  seatTone,
  stateLabel,
  statusLine,
  toneOf,
  type Seat,
} from "~/lib/seats.ts";
import type { AgentRow, FeedRow, SandboxEntry } from "~/protocol/index.ts";
import type { Attention } from "~/lib/attention.ts";
import {
  Avatar,
  Button,
  Callout,
  Card,
  cx,
  EmptyState,
  formatRelative,
  InlineCode,
  RelativeTime,
  Stack,
  tableColumns,
  Tag,
  useNow,
  VisuallyHidden,
} from "@crewlethq/ui";
import type { DataViewColumn } from "@crewlethq/ui";
import {
  DatabaseGlyph,
  ErrorGlyph,
  InboxGlyph,
  InfoGlyph,
  KeyGlyph,
  ScheduleGlyph,
} from "@crewlethq/icons/glyphs";

/**
 * How tall a record block grows before it scrolls itself, in px.
 *
 * Twenty-five lines of the mono face at the size a block is set in, which is
 * about as much of a record as a reader takes in before scrolling anyway.
 * uilet's CodeBlock is unbounded without a ceiling, and the records these hold
 * are a whole turn, a whole configuration and a whole event payload: one of
 * them at nine hundred lines pushes everything under it off the screen.
 */
export const RECORD_MAX_HEIGHT = 460;

/**
 * What every record table in this dashboard is, as props.
 *
 * A panel table here shows a whole answer the engine already sliced, so it
 * PAGES NOWHERE: the caller holds the rows and the footer of the screen holds
 * whatever fetches more. It RESIZES NOTHING and STORES NOTHING either, because
 * every choice a reader makes on one of these screens lives in the URL, where
 * it can be shared and gone back to, rather than in this browser's own storage
 * where the next reader inherits it invisibly.
 *
 * It is a bundle of props rather than a component: the design system draws the
 * table, and what the engine has to say about one is only this. The rows come
 * in with the columns so that one call names the row type for both, which is
 * what lets a column's own accessors stay untyped at the call site.
 */
export function recordTable<T>(rows: readonly T[], columns: readonly DataViewColumn<T>[]) {
  const { columns: byKey, order } = tableColumns(columns);
  return {
    data: [...rows],
    columns: byKey,
    defaultColumnOrder: order,
    variant: "compact" as const,
    paginated: false,
    resizable: false,
    showSettings: false,
  };
}

/**
 * The rail a seat tile takes for what it is doing.
 *
 * Three, and `quiet` is deliberately absent rather than neutral: a seat with
 * nothing to say draws no mark, where a neutral rail would be a line every
 * idle seat carried and nobody could read past.
 */
const SEAT_RAIL: Record<string, "info" | "warning" | "danger" | undefined> = {
  working: "info",
  needs: "warning",
  broken: "danger",
};

/** A seat's name and handle, linked. The one way a person appears in a list. */
export function SeatChip({
  name,
  handle,
  human,
  size = "xs",
}: {
  name: string;
  handle?: string;
  human?: boolean;
  size?: "xs" | "sm";
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
      href={href(["seats", target])}
    >
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
    <Tag variant={toneOf(state)} dot>
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
    <Card
      href={href(seatPath(seat))}
      variant="subtle"

      /* The rail is what the seat is DOING, never who it is, and `quiet`
         takes none at all: an idle seat used to draw a glowing tile that
         read as activity. */
      {...(SEAT_RAIL[tone] ? { rail: SEAT_RAIL[tone] } : {})}
    >
      <Stack gap={2}>
        <div className="row">
          <Avatar
            name={seat.name}
            size="lg"
            variant={seat.kind === "human" ? "dashed" : "solid"}
            decorative
          />
          <div className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
            <strong className="truncate t-body">{seat.name}</strong>
            {seat.handle && <span className="truncate t-caption mono">@{seat.handle}</span>}
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
            <Tag variant="info">{call.phase}</Tag>
            <span className="t-caption t-num">
              round {call.round_num >= 0 ? call.round_num + 1 : "—"}
            </span>
            <span className="spacer" />
            <RelativeTime className="t-caption" value={call.updated_at} now={now} />
          </div>
        )}
        {seat.unit && <div className="t-caption truncate">{seat.unit.name}</div>}
      </Stack>
    </Card>
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
        <item.icon size="sm" />
      </span>
      <span className="col" style={{ gap: 2, flex: 1, minWidth: 0 }}>
        <span className="t-body" style={{ fontWeight: "var(--font-weight-medium)" }}>
          {item.title}
        </span>
        <span className="t-caption">{item.detail}</span>
      </span>
      {item.at && <RelativeTime className="t-caption nowrap" value={item.at} now={now} />}
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
    <a className="attention-row" data-severity={item.severity} href={href(item.path, item.query)}>
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
  depth = 0,
  mark,
}: {
  event: FeedRow;
  onOpen?: () => void;
  showDate?: boolean;
  /**
   * How far in this row's actor sits, in nesting steps. A trace draws its
   * spans as a tree, and a row that carried its own copy of this grid was a
   * second four-column layout drifting from this one a column at a time.
   */
  depth?: number;
  /** Drawn under the summary: a trace's own span bar. */
  mark?: ReactNode;
}) {
  const now = useNow();
  return (
    <a
      className={cx("feed-row", event.failed && "failed")}
      href={href(["events", event.id])}
      onClick={onOpen}
    >
      <time
        className="feed-time"
        dateTime={event.timestamp}
        title={[fmtDateTime(event.timestamp), formatRelative(event.timestamp, now)]
          .filter(Boolean)
          .join(" · ")}
      >
        {showDate ? fmtDateTime(event.timestamp) : fmtTime(event.timestamp)}
      </time>
      <span className="feed-actor truncate" style={depth ? { paddingLeft: depth * 12 } : undefined}>
        {depth > 0 && <span className="muted">└ </span>}
        {event.actor || "engine"}
      </span>
      <span className="feed-what truncate">
        {event.failed && (
          <>
            <ErrorGlyph size="xs" className="feed-failed-mark" />
            <VisuallyHidden>Failed</VisuallyHidden>{" "}
          </>
        )}
        {event.summary || event.type}
        {mark}
      </span>
      <span className="feed-tail">
        {event.source && <span className="truncate">{event.source}</span>}
        <span className="muted">{humanize(event.category) || "system"}</span>
      </span>
    </a>
  );
}

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
  empty?: { title: ReactNode; hint?: ReactNode };
  children?: ReactNode;
}) {
  if (error === "unauthorized") {
    return (
      <Callout variant="warning" icon={<KeyGlyph size="sm" />}>
        <span>
          This answer is auth-gated. It needs an API token matching one of your{" "}
          <InlineCode>api.auth.tokens</InlineCode> entries.
        </span>
        <span className="spacer" />
        {/* The banner used to say "set a token" and offer nothing that could.
            With anonymous reads allowed the socket is never refused, so the
            dialog's only other doors — a refusal, and the engine panel — both
            stay shut on exactly the screen that needs it. */}
        <Button variant="secondary" size="small" leadingIcon={<KeyGlyph />} onClick={requestToken}>
          Set token
        </Button>
      </Callout>
    );
  }
  if (error === "no_event_store") {
    return (
      <Callout variant="neutral" icon={<DatabaseGlyph size="sm" />}>
        <span>
          This node keeps no event log, so there is no history to read. Set{" "}
          <InlineCode>store.path</InlineCode> in <InlineCode>crewlet.yaml</InlineCode> to make it
          durable.
        </span>
      </Callout>
    );
  }
  if (error === "unknown_query") {
    return (
      <Callout variant="neutral" icon={<InfoGlyph size="sm" />}>
        <span>
          The engine does not serve this answer — the subsystem behind it is not running on this
          node.
        </span>
      </Callout>
    );
  }
  if (error === "timeout") {
    return (
      <Callout variant="warning" icon={<ScheduleGlyph size="sm" />}>
        <span>The engine did not answer within 10 seconds. It may be under load.</span>
      </Callout>
    );
  }
  if (error) {
    return (
      <Callout variant="danger" icon={<ErrorGlyph size="sm" />}>
        <span>The engine refused this query ({error}).</span>
      </Callout>
    );
  }
  if (loading) return null;
  if (empty) {
    return (
      <EmptyState
        size="compact"
        icon={<InboxGlyph />}
        title={empty.title}
        // WHY it is empty, and what would fill it. A caller that says nothing
        // gets the honest sentence rather than a blank line: the query
        // answered, and this is what it answered with.
        description={empty.hint ?? "The engine answered this query with nothing."}
      />
    );
  }
  return <>{children}</>;
}
