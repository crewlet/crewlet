/**
 * Agent-to-agent — the private channels seats opened with each other.
 *
 * A channel is an AUTHORIZATION RECORD rather than a transport: one ask, one
 * answer, then closed. Nothing queues here — both the brief and the reply
 * travel over the durable seat inbox — so what is shown is the record itself:
 * who asked whom, how many messages crossed, and when.
 *
 * This screen used to carry a second lens over the conversation ledger: one
 * row per completed turn, keyed on the external thread it served. That was a
 * viewer for somebody ELSE's threads — a Slack channel, a Jira issue — which
 * is not what a conversations screen in this product is meant to be, and the
 * name promised a chat system the engine does not have. The ledger itself
 * stays exactly where it was: it is prior-turn context for the agent's prompt
 * (see internal/engine, `req.History`), not a display feature, and removing it
 * would make every threaded seat forget what it said last turn.
 *
 * # The whole record, not the open half
 *
 * `a2a_channels` answers OPEN channels unless asked otherwise, which is the
 * right default for a screen watching a working company and the wrong one for
 * this screen: it draws a State column, counts closed channels in its stats
 * and says "no channels have been opened" when the list is empty. Every one of
 * those is a claim about the record, so the record is what it asks for — and a
 * `peek=channel:` on a channel that has since closed has to resolve, or the
 * rail opens empty on the object the reader just clicked.
 */

import { useCallback, useMemo } from "react";
import { EventRow, QueryState } from "~/components/common.tsx";
import { CoverageNote } from "~/components/CoverageNote.tsx";
import {
  Callout,
  Card,
  EmptyState,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
  InlineCode,
  TableNumberCell,
  TableDateCell,
  TableEntityCell,
  type DataGridActivationEvent,
  peekClickHandler,
} from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { plainSeat } from "~/app/frame/cells.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { href } from "~/app/router.tsx";
import { MessageSquareGlyph, UsersGlyph, LinkGlyph } from "@crewlethq/icons/glyphs";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { indexOrg, useSeatBadgeOf } from "~/lib/seats.ts";
import {
  elapsedMs,
  fmtDateTime,
  fmtDuration,
  oldestFirst,
  plural,
  relTime,
  tsKey,
  fmtCount,
} from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { A2AChannel } from "~/protocol/index.ts";

/** The channel record has no push behind it; a channel's life is one exchange. */
const POLL_MS = 30_000;

/** Every channel the record holds, open and closed. See the file's own doc. */
const WHOLE_RECORD = { state: "all" };

/**
 * A handle resolved to the seat's name, or the handle itself.
 *
 * A channel names its parties by HANDLE — see a2a.Channel.OtherParty — and a
 * handle is an address, not a label. Passed straight through it put
 * `agent-ai-systems-engineer` where the seat's name belongs and built the
 * avatar's monogram out of it. The handle still does the linking.
 */
function useSeatName(): (handle: string) => string {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  return useCallback((handle: string) => index.byHandle.get(handle)?.name || handle, [index]);
}

/**
 * What an A2A channel is called: who asked whom.
 *
 * ONE FUNCTION for the page's header, the peek's and the label this screen
 * publishes, because a channel wearing two spellings of its own name in two
 * places is how the third one goes wrong. SEAT NAMES rather than handles, with
 * `seatName`'s own fallback behind them, so an unresolved roster degrades to
 * `alice → bob` rather than to an arrow with nothing either side of it.
 */
function channelTitle(channel: A2AChannel, seatName: (handle: string) => string): string {
  return `${seatName(channel.requester)} → ${seatName(channel.target)}`;
}

/** Open or closed — the only predicate a channel has. */
function ChannelState({ channel }: { channel: A2AChannel }) {
  return channel.closed_at ? (
    <Tag variant="neutral" dot>
      closed
    </Tag>
  ) : (
    <Tag variant="info" dot>
      open
    </Tag>
  );
}

/**
 * The facts a channel is recognised by, in the grid's own column order.
 *
 * ONE FUNCTION for the page and the rail, so a reader who peeks a channel and
 * then opens it reads the same five things in the same places. The STATE is
 * not among them — it is the header's own pill, and a state spelled in colour
 * and again in a list reads as two facts about one channel.
 */
function channelFacts(
  channel: A2AChannel,
  seatName: (handle: string) => string,
  now: number,
): Fact[] {
  return [
    {
      label: "Asked by",
      value: seatName(channel.requester),
      path: ["company", "org", "seats", channel.requester],
    },
    {
      label: "Asked",
      value: seatName(channel.target),
      path: ["company", "org", "seats", channel.target],
    },
    { label: "Messages", value: <TableNumberCell value={channel.messages} format={fmtCount} /> },
    { label: "Opened", value: <TableDateCell value={channel.opened_at} now={now} /> },
    { label: "Last message", value: <TableDateCell value={channel.last_at} now={now} /> },
  ];
}

/**
 * What crossed the channel, as far as the record can say.
 *
 * THE WORDS ARE NOT IN THE RECORD AND THEY ARE NOT MISSING. The channel record
 * is the authorization — the pair, the count and the window — and the brief and
 * the reply travel over the seat inbox, published as ordinary
 * `a2a_message_sent` events. So this says which half has happened, derived from
 * the count the record does keep, and links to the log NARROWED TO THIS CHANNEL
 * (`#/live/events?channel=`), which the engine answers by the channel id every
 * A2A event carries. The link used to carry the id as the log's text search,
 * which matched no summary the engine writes — so it landed on an empty log.
 */
function Exchange({
  channel,
  seatName,
  now,
}: {
  channel: A2AChannel;
  seatName: (handle: string) => string;
  now: number;
}) {
  const answered = channel.messages > 1;
  return (
    <div className="col gap-2">
      <div className="row gap-2">
        <Tag appearance="outline">ask</Tag>
        <span className="truncate t-cell">
          {seatName(channel.requester)} asked {seatName(channel.target)}
        </span>
        <span className="spacer" />
        <span className="t-caption nowrap">{relTime(channel.opened_at, now)}</span>
      </div>
      <div className="row gap-2">
        <Tag appearance="outline">answer</Tag>
        <span className="truncate t-cell">
          {answered
            ? `${seatName(channel.target)} answered`
            : "nothing has come back on this channel yet"}
        </span>
        <span className="spacer" />
        {answered && <span className="t-caption nowrap">{relTime(channel.last_at, now)}</span>}
      </div>
      <span className="t-caption">
        The messages are in the event log.{" "}
        <a className="t-link prose-link" href={channelEventsHref(channel.id)}>
          Read this channel's events ↗
        </a>
      </span>
    </div>
  );
}

/**
 * The event log narrowed to one channel — the one address every "read this
 * channel's events" link takes, so the peek, the page and the log agree.
 */
export function channelEventsHref(id: string): string {
  return href(["live", "events"], { channel: id });
}

/**
 * A section of the channel body, framed for the page or run on for the rail.
 *
 * AT MODULE SCOPE, which is the whole point of it being a component at all: a
 * component defined inside a render body is a NEW function on every render, so
 * `<Wrap>` is a different element type each time and React unmounts the whole
 * subtree and builds it again rather than reconciling it. Both callers of
 * [ChannelBody] pass a `now` that ticks once a second, so the body's DOM was
 * replaced sixty times a minute — and a reader dragging over the channel id to
 * copy it lost the selection within the second, because the node it anchored
 * to no longer existed.
 */
function Wrap({
  flush,
  title,
  children,
}: {
  flush?: boolean;
  title: string;
  children: React.ReactNode;
}) {
  if (flush) {
    return (
      <section className="col gap-2">
        <div className="t-label">{title}</div>
        {children}
      </section>
    );
  }
  return (
    <Card>
      <Card.Header>
        <Card.Title>{title}</Card.Title>
      </Card.Header>
      {children}
    </Card>
  );
}

/**
 * One channel, in the rail and on its own page.
 *
 * `flush` is the peek: the rail is the panel already, so the two sections run
 * on as plain blocks rather than growing a second frame inside the first —
 * the same split `ItemBody` makes between the work item's page and its peek.
 */
function ChannelBody({
  channel,
  seatName,
  now,
  flush,
}: {
  channel: A2AChannel;
  seatName: (handle: string) => string;
  now: number;
  flush?: boolean;
}) {
  // OPEN FOR HOW LONG, measured to the close where there is one and to the
  // last message where there is not: a channel that is still open has no end,
  // and measuring it to `now` would make the number move while nothing did.
  const span = elapsedMs(channel.opened_at, channel.closed_at || channel.last_at);

  return (
    <>
      <Wrap flush={flush} title="The exchange">
        <Exchange channel={channel} seatName={seatName} now={now} />
      </Wrap>
      <Wrap flush={flush} title="The record">
        <PropertiesRail
          groups={[
            {
              properties: [
                { label: "Opened", value: fmtDateTime(channel.opened_at) },
                { label: "Last message", value: fmtDateTime(channel.last_at) },
                {
                  label: "Closed",
                  // ABSENT IS NOT A DATE. An open channel has no close, and
                  // the rail renders a missing value as the dash that says so.
                  value: channel.closed_at ? fmtDateTime(channel.closed_at) : undefined,
                },
                {
                  label: "Open for",
                  value: span == null ? undefined : fmtDuration(span),
                  title: "From ask to close",
                },
                { label: "Channel", value: <InlineCode>{channel.id}</InlineCode> },
              ],
            },
          ]}
        />
      </Wrap>
    </>
  );
}

/**
 * Why a channel the reader was sent to is not in the listing, as far as the
 * answer can say.
 *
 * THE LISTING IS CUT, AND IT SAYS WHEN. The engine answers the most recently
 * active channels up to a limit and reports `truncated` when the record held
 * more, so a missing channel is either one older than every channel the page
 * holds — or, on a page that holds the whole record, one that is not in it.
 * This used to name the limit as a number, which is the engine's to change,
 * and to offer "older than the page" whether or not the page was cut.
 */
function missingChannel(truncated: boolean | undefined): string {
  return truncated
    ? "It may be older than this list, purged, or the id is wrong."
    : "It may have been purged, or the id is wrong.";
}

/**
 * One agent-to-agent channel, beside the list it was found in.
 *
 * # It asks for the whole record
 *
 * A peek is opened from a pasted URL as often as from a row, and the channel
 * it names has usually CLOSED by the time anybody reads it — one ask and one
 * answer is the whole life of one. So this reads every channel the record
 * holds and finds its own, rather than the open ones the default answers.
 *
 * # Three answers, not two
 *
 * "This node cannot reach the coordination store", "the record holds no such
 * channel" and "here it is" are three different facts, and the first two both
 * look like an empty rail if they are folded together. `available` is what
 * tells them apart — see `queries.a2aChannels`.
 */
export function ChannelPeek({ id }: { id: string }) {
  const now = useNow();
  const seatName = useSeatName();
  const { data, loading, error, refusal } = useQuery("a2a_channels", WHOLE_RECORD, {
    enabled: id !== "",
    pollMs: POLL_MS,
  });
  const channel = (data?.channels ?? []).find((c) => c.id === id) ?? null;

  return (
    <>
      {loading && !data && <Skeleton variant="text" rows={5} label="Loading the channel" />}
      <QueryState error={error} refusal={refusal} loading={loading}>
        {data?.available === false && (
          <Callout variant="neutral" icon={<LinkGlyph size="md" />}>
            Can’t read channels from this node right now.
          </Callout>
        )}
        {data?.available !== false && !channel && (
          <EmptyState
            size="compact"
            icon={<LinkGlyph size={32} />}
            title="No such channel in the record"
            description={missingChannel(data?.truncated)}
          />
        )}
        {channel && (
          <>
            <ObjectHeader
              size="peek"
              kind="A2A channel"
              icon="link"
              identifier={channel.id}
              title={channelTitle(channel, seatName)}
              status={<ChannelState channel={channel} />}
              facts={channelFacts(channel, seatName, now)}
            />
            <div className="col gap-3">
              <ChannelBody channel={channel} seatName={seatName} now={now} flush />
            </div>
          </>
        )}
      </QueryState>
    </>
  );
}

export function Conversations() {
  const seatBadge = useSeatBadgeOf();
  const now = useNow();
  const seatName = useSeatName();
  const channels = useQuery("a2a_channels", WHOLE_RECORD, { pollMs: POLL_MS });
  const rows = useMemo(() => channels.data?.channels ?? [], [channels.data]);
  const cut = channels.data?.truncated === true;

  // THE ORDER `[` AND `]` WALK. Published from the rows this screen holds, so
  // the stepper walks the record as the reader sorted it rather than the order
  // the coordination bucket happened to answer in.
  usePeekNeighbours(
    useMemo(() => rows.map((c) => ({ kind: "channel" as const, id: c.id })), [rows]),
  );

  const { open: openPeek } = usePeekControls();

  const openChannel = useCallback(
    (c: A2AChannel, e: DataGridActivationEvent) => {
      const go = () => openPeek({ kind: "channel", id: c.id });
      // The grid hands this both events. `peekClickHandler` is the frame's one
      // copy of "which clicks mean elsewhere" and reads a MOUSE event — every
      // modifier and the middle button belong to the browser, so the row stays
      // a real link to the channel's page. The `enter` chord carries no button
      // at all and is never "open elsewhere".
      if (!("button" in e)) {
        go();
        return;
      }
      peekClickHandler(go)?.(e);
    },
    [openPeek],
  );

  return (
    <>
      {/* The flush Panel this row sat in is gone: StatGroup draws that surface
          itself — the same hairline, radius and clip our `panel-flush` did —
          and the tiles inside it are flush, so wrapping it would be two
          surfaces around one row. */}
      <StatGroup columns={3}>
        {/* WHAT THE COUNTS COVER, said on each. A cut listing is the most
            recently active channels, so every number here is over that page
            rather than over the record — and an open channel nobody has
            touched in a week, the one this screen exists to surface, is the
            first to fall off the end. */}
        <StatCard
          icon={<LinkGlyph size="xs" />}
          label="Open channels"
          value={rows.filter((c) => !c.closed_at).length}
          sub={cut ? "of the most recent" : "one ask, one answer, then closed"}
        />
        <StatCard
          icon={<MessageSquareGlyph size="xs" />}
          label="Messages"
          value={rows.reduce((n, c) => n + c.messages, 0)}
          sub={cut ? "across the most recent channels only" : "across every channel in the record"}
        />
        <StatCard
          icon={<UsersGlyph size="xs" />}
          label="Pairs"
          value={new Set(rows.map((c) => `${c.requester}->${c.target}`)).size}
          sub={cut ? "among the most recent channels only" : "distinct requester/target pairs"}
        />
      </StatGroup>

      {channels.loading && <Skeleton variant="text" rows={4} label="Loading channels" />}
      {channels.data?.available === false ? (
        <Callout variant="neutral" icon={<LinkGlyph size="md" />}>
          Can’t read channels from this node right now.
        </Callout>
      ) : (
        <QueryState
          error={channels.error}
          refusal={channels.refusal}
          loading={channels.loading}
          empty={
            rows.length > 0
              ? undefined
              : {
                  title: "No channels have been opened",
                  hint: "Agents open these to ask each other quick questions.",
                }
          }
        >
          <Card padding="none">
            <DataGrid<A2AChannel>
              rows={rows}
              rowKey={(c) => c.id}
              onRowActivate={openChannel}
              rowHref={(c) => peekHref({ kind: "channel", id: c.id })}
              defaultSort="-last"
              columns={[
                {
                  key: "state",
                  header: "State",
                  shrink: true,
                  sortValue: (c) => (c.closed_at ? "closed" : "open"),
                  cell: (c) => <ChannelState channel={c} />,
                },
                {
                  key: "from",
                  header: "Asked by",
                  // WHO ASKED WHOM is what a row is; the counts and the dates
                  // give way first beside a peek (DataGrid's `fitColumns`).
                  floor: "9rem",
                  sortValue: (c) => seatName(c.requester),
                  // NOT `linkedSeat`, and not the `SeatChip` this column used to
                  // draw: both are anchors and every row here is one now, and
                  // an anchor inside an anchor is markup no browser agrees
                  // about. The seat is a link again in the peek's own facts.
                  cell: (c) => <TableEntityCell {...plainSeat({ ...seatBadge(c.requester) })} />,
                },
                {
                  key: "to",
                  header: "Asked",
                  floor: "9rem",
                  sortValue: (c) => seatName(c.target),
                  cell: (c) => <TableEntityCell {...plainSeat({ ...seatBadge(c.target) })} />,
                },
                {
                  key: "messages",
                  header: "Messages",
                  align: "right",
                  shrink: true,
                  drop: 2,
                  sortValue: (c) => c.messages,
                  // A CELL RATHER THAN THE BARE NUMBER it used to render: a
                  // channel with nothing on it yet is a real zero and must
                  // read as one, and the tabular face is what lets a column of
                  // counts be compared down the page.
                  cell: (c) => <TableNumberCell value={c.messages} format={fmtCount} />,
                },
                {
                  key: "opened",
                  align: "right",
                  header: "Opened",
                  shrink: true,
                  drop: 1,
                  sortValue: (c) => tsKey(c.opened_at),
                  cell: (c) => <TableDateCell value={c.opened_at} now={now} />,
                },
                {
                  key: "last",
                  align: "right",
                  header: "Last message",
                  shrink: true,
                  sortValue: (c) => tsKey(c.last_at),
                  cell: (c) => <TableDateCell value={c.last_at} now={now} />,
                },
              ]}
            />
          </Card>
        </QueryState>
      )}
    </>
  );
}

/** How many of a channel's events its page reads. A channel is one ask and
 *  one answer, so its events are a handful — the open, the two messages, the
 *  close, and the turns on either side; fifty is every channel with room. */
const CHANNEL_EVENTS = 50;

/**
 * One agent-to-agent channel, on its own page.
 *
 * `#/live/a2a/{id}` is what `Open ↗` from the rail lands on and what a
 * ⌘-click on a row opens. It used to be the whole list with the channel drawn
 * beneath it, so a reader who followed a link to one conversation got every
 * conversation and scrolled for the one they asked for.
 *
 * # The words, read here
 *
 * The record holds the pair, the count and the window; the brief and the reply
 * are `a2a_message_sent` events. The page asks the log for this channel's
 * events (`channel_id`, an index seek) and draws them in the order they
 * happened — a conversation reads top down — with the whole log one link away.
 */
export function ChannelScreen({ id }: { id: string }) {
  const now = useNow();
  const seatName = useSeatName();
  const channels = useQuery("a2a_channels", WHOLE_RECORD, { pollMs: POLL_MS });
  const events = useQuery("events", { channel_id: id, limit: CHANNEL_EVENTS });
  const channel = (channels.data?.channels ?? []).find((c) => c.id === id) ?? null;
  // AND THAT NAME IS THE CHANNEL'S EVERYWHERE, not just in the header below.
  // The breadcrumb, the browser tab and the palette's recents read the one
  // label a screen publishes and otherwise show the raw path segment — a
  // channel uuid, which says neither who nor what.
  const name = channel ? channelTitle(channel, seatName) : "";
  usePageLabels(name ? { [id]: name } : {});
  const rows = useMemo(() => [...(events.data?.events ?? [])].sort(oldestFirst), [events.data]);
  // NOT IN A RECORD THAT WAS READ. One fact, stated once: the not-found state
  // carries the way to the log itself, and the events card below is not drawn
  // beside it to say "none" a second time — unless the log still holds what
  // crossed the channel, which outlives the channel's own record (a purged
  // record's events are exactly what somebody following an old link wants).
  const missing = channels.data?.available === true && !channel;
  const eventsCard = !missing || rows.length > 0;

  return (
    <>
      <ObjectHeader
        kind="A2A channel"
        icon="link"
        identifier={id}
        title={name || (missing ? "No such channel" : "A2A channel")}
        status={channel ? <ChannelState channel={channel} /> : undefined}
        facts={channel ? channelFacts(channel, seatName, now) : []}
      />
      {channels.loading && !channels.data && (
        <Skeleton variant="text" rows={5} label="Loading the channel" />
      )}
      <QueryState error={channels.error} refusal={channels.refusal} loading={channels.loading}>
        {/* THREE ANSWERS, NOT TWO: "this node cannot reach the coordination
            store", "the record holds no such channel" and "here it is" — and
            the second is guarded on a record that was actually read. */}
        {channels.data?.available === false && (
          <Callout variant="neutral" icon={<LinkGlyph size="md" />}>
            Can’t read channels from this node right now.
          </Callout>
        )}
        {missing && !eventsCard && (
          <EmptyState
            icon={<LinkGlyph size={32} />}
            title="No such channel in the record"
            description={missingChannel(channels.data?.truncated)}
            action={
              <a className="t-link" href={channelEventsHref(id)}>
                Open in the event log
              </a>
            }
          />
        )}
        {missing && eventsCard && (
          <Callout variant="neutral" icon={<LinkGlyph size="md" />}>
            The channel is gone, but its messages are still in the log.
          </Callout>
        )}
        {channel && <ChannelBody channel={channel} seatName={seatName} now={now} />}
      </QueryState>

      {eventsCard && (
        <Card padding="none">
          <Card.Header
            icon={<MessageSquareGlyph size="sm" />}
            // THE SUBTITLE TAKES ITS OWN LINE, so on a phone the title and
            // the link keep theirs ("On this chann…" beside "wh…").
            className="card-head-stacked"
            count={events.data ? rows.length : undefined}
            subtitle="what crossed it, oldest first"
            actions={
              <a className="t-link" href={channelEventsHref(id)}>
                Open in the event log
              </a>
            }
          >
            <Card.Title>On this channel</Card.Title>
          </Card.Header>
          <CoverageNote coverage={[events.data?.coverage]} what="this channel's history" />
          {events.loading && !events.data && (
            <Skeleton variant="text" rows={3} label="Loading the channel's events" />
          )}
          <QueryState
            error={events.error}
            refusal={events.refusal}
            loading={events.loading}
            empty={
              events.data && rows.length === 0
                ? {
                    title: "No event names this channel",
                    hint: "Its events may be too old, or on a node that didn’t answer.",
                  }
                : undefined
            }
          >
            <div className="list">
              {rows.map((ev) => (
                <EventRow key={ev.id} event={ev} />
              ))}
            </div>
            {events.data?.next && (
              <footer className="panel-foot">
                <span>
                  Newest {plural(rows.length, "event")}. The rest are in{" "}
                  <a className="t-link prose-link" href={channelEventsHref(id)}>
                    the event log
                  </a>
                  .
                </span>
              </footer>
            )}
          </QueryState>
        </Card>
      )}
    </>
  );
}
