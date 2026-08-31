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
import { Avatar, Badge, cx } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { href } from "~/app/router.tsx";
import { fmtDateTime, fmtTime, humanize, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { runState, seatTone, stateLabel, statusLine, toneOf, type Seat } from "~/lib/seats.ts";
import type { AgentRow, FeedRow, SandboxEntry } from "~/protocol/index.ts";
import type { Attention } from "~/lib/attention.ts";

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
      href={href(["seats", target])}
    >
      <Avatar name={name} size={size} human={human} />
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
    <Badge tone={toneOf(state)} dot>
      {stateLabel(state)}
    </Badge>
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
    <a className="seat-card" data-tone={tone} href={href(["seats", seat.handle])}>
      <div className="row">
        <Avatar name={seat.name} size="lg" human={seat.kind === "human"} />
        <div className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
          <strong className="truncate t-body">{seat.name}</strong>
          <span className="truncate t-caption mono">@{seat.handle}</span>
        </div>
        {seat.kind === "human" ? (
          <Badge outline>human</Badge>
        ) : (
          <StateBadge agent={agent} sandboxes={sandboxes} />
        )}
      </div>
      <div className="seat-line truncate">{statusLine(agent, { sandbox, seat })}</div>
      {call?.in_progress && (
        <div className="row gap-1">
          <Badge tone="info">{call.phase}</Badge>
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
        <Icon name={item.icon} size="sm" />
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
          <Icon
            name="alert"
            size="xs"
            style={{ display: "inline", color: "var(--critical-ink)", marginRight: 4 }}
          />
        )}
        {event.summary || event.type}
      </span>
      <span className="feed-tail">
        {event.source && <span className="truncate">{event.source}</span>}
        <span className="faint">{humanize(event.category) || "system"}</span>
      </span>
    </>
  );
  return (
    <a
      className={cx("feed-row", event.failed && "failed")}
      href={href(["events", event.id])}
      onClick={onOpen}
    >
      {body}
    </a>
  );
}

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
    <section className="col gap-3">
      <div className="row">
        <span className="t-heading">{title}</span>
        {hint && <span className="t-caption truncate">{hint}</span>}
        <span className="spacer" />
        {actions}
      </div>
      {children}
    </section>
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
      <div className="banner caution">
        <Icon name="key" size="sm" />
        <span>
          This answer is auth-gated. Set an API token matching one of your{" "}
          <code className="inline">api.auth.tokens</code> entries.
        </span>
      </div>
    );
  }
  if (error === "no_event_store") {
    return (
      <div className="banner neutral">
        <Icon name="database" size="sm" />
        <span>
          This node keeps no event log, so there is no history to read. Set{" "}
          <code className="inline">store.path</code> in <code className="inline">crewlet.yaml</code>{" "}
          to make it durable.
        </span>
      </div>
    );
  }
  if (error === "unknown_query") {
    return (
      <div className="banner neutral">
        <Icon name="info" size="sm" />
        <span>
          The engine does not serve this answer — the subsystem behind it is not running on this
          node.
        </span>
      </div>
    );
  }
  if (error === "timeout") {
    return (
      <div className="banner caution">
        <Icon name="clock" size="sm" />
        <span>The engine did not answer within 10 seconds. It may be under load.</span>
      </div>
    );
  }
  if (error) {
    return (
      <div className="banner critical">
        <Icon name="alert" size="sm" />
        <span>The engine refused this query ({error}).</span>
      </div>
    );
  }
  if (loading) return null;
  if (empty) {
    return (
      <div className="empty inline">
        <Icon name="inbox" size="xl" />
        <div className="empty-title">{empty.title}</div>
        {empty.hint && <div className="empty-sub">{empty.hint}</div>}
      </div>
    );
  }
  return <>{children}</>;
}
