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
import { QueryState, Section } from "~/components/common.tsx";
import {
  Button,
  Callout,
  Card,
  EmptyState,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, NumberCell, TextCell } from "~/app/frame/cells.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { peekHref, rowPeekHandler, usePeek, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { href } from "~/app/router.tsx";
import { ChatGlyph, GroupGlyph, InfoGlyph, LinkGlyph } from "@crewlethq/icons/glyphs";
import { useClient, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useOlderPages } from "~/lib/paging.ts";
import { indexOrg } from "~/lib/seats.ts";
import { elapsedMs, fmtDateTime, fmtDuration, plural, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { A2AChannel, A2AChannelsParams } from "~/protocol/index.ts";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { WHOLE_LOG } from "./Activity.tsx";

/** The channel record has no push behind it; a channel's life is one exchange. */
const POLL_MS = 30_000;

/** Every channel the record holds, open and closed. See the file's own doc. */
const WHOLE_RECORD: A2AChannelsParams = { state: "all" };

/**
 * One channel by its id, wherever it falls in the record's order.
 *
 * NOT A SEARCH OF A PAGE. The listing is one page of the most recently active
 * channels, so a channel looked up in it was found only while it was recent —
 * and one older than the page drew "no such channel" on a channel the record
 * holds. `id` narrows the read itself, and it names `all` because a lookup
 * must find a closed channel too.
 */
function oneChannel(id: string): A2AChannelsParams {
  return { state: "all", id };
}

/** A page of channels resumes after this pair — see `queries.beforeCursor`. */
type ChannelCursor = { before_time: string; before_id: string };

/**
 * The pair a listing's `next` carries, or null where it carries none: the last
 * page's `next` is an empty object, and half a pair is refused by the engine.
 */
function cursorOf(next: { before_time?: string; before_id?: string } | undefined) {
  return next?.before_time && next.before_id
    ? { before_time: next.before_time, before_id: next.before_id }
    : null;
}

/** A channel's identity, for the pager. */
const channelKey = (channel: A2AChannel) => channel.id;

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
      path: ["company", "people", channel.requester],
    },
    {
      label: "Asked",
      value: seatName(channel.target),
      path: ["company", "people", channel.target],
    },
    { label: "Messages", value: <NumberCell value={channel.messages} /> },
    { label: "Opened", value: <DateCell at={channel.opened_at} now={now} /> },
    { label: "Last message", value: <DateCell at={channel.last_at} now={now} /> },
  ];
}

/**
 * What crossed the channel, as far as the record can say.
 *
 * THE WORDS ARE NOT HERE AND THEY ARE NOT MISSING. The channel record is the
 * authorization — the pair, the count and the window — and the brief and the
 * reply travel over the seat inbox, published as ordinary `a2a_message_sent`
 * events. So this says which half has happened, derived from the count the
 * record does keep, and links to the log that holds the text rather than
 * inventing a body from a message counter.
 *
 * (The event log promotes a `channel_id` column and `store.ListQuery` has no
 * filter for it, so the link carries the id as the log's own text search. A
 * `channel` filter on the `events` query is what would make this an exact
 * read; matching the id against a rendered summary here instead would be the
 * same guess wearing a filter's clothes.)
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
        The words themselves are not in the channel record: both halves travel over the seat inbox
        and are published as events.{" "}
        <a
          className="t-link prose-link"
          // ON THE WINDOW THAT REACHES THE WHOLE LOG: a channel's events are as
          // old as the channel, and the log's own fallback is a day.
          href={href(["activity", "events"], { category: "a2a", q: channel.id, window: WHOLE_LOG })}
        >
          Read this channel's events ↗
        </a>
      </span>
    </div>
  );
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
                  title: "a closed channel is a finished exchange; re-opening is a new ask",
                },
                {
                  label: "Open for",
                  value: span == null ? undefined : fmtDuration(span),
                  title: "from the ask to the close, or to the last message on an open channel",
                },
                { label: "Channel", value: <code className="inline">{channel.id}</code> },
              ],
            },
          ]}
        />
      </Wrap>
    </>
  );
}

/**
 * One agent-to-agent channel, beside the list it was found in.
 *
 * # It asks for the whole record
 *
 * A peek is opened from a pasted URL as often as from a row, and the channel
 * it names has usually CLOSED by the time anybody reads it — one ask and one
 * answer is the whole life of one. So this asks for its own channel by id —
 * see [oneChannel] — rather than for the open ones the default answers.
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
  const { data, loading, error } = useQuery("a2a_channels", oneChannel(id), {
    enabled: id !== "",
    pollMs: POLL_MS,
  });
  const channel = (data?.channels ?? []).find((c) => c.id === id) ?? null;

  return (
    <>
      {loading && !data && <Skeleton variant="text" rows={5} label="Loading the channel" />}
      <QueryState error={error} loading={loading}>
        {data?.available === false && (
          <Callout variant="neutral" icon={<LinkGlyph size="md" />}>
            No channel record is reachable from this node, so this channel cannot be read here.
            Channels live in the fleet's coordination store.
          </Callout>
        )}
        {data?.available !== false && !channel && (
          <EmptyState
            size="compact"
            icon={<LinkGlyph size={32} />}
            title="No such channel in the record"
            description="A channel is kept until the retention horizon. It may have been purged, or the id may be wrong."
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

export function Conversations({ channelId }: { channelId?: string }) {
  const now = useNow();
  const seatName = useSeatName();
  const { socket } = useClient();

  // THE PAGES PAST THE FIRST, from the cursor each answer carries — and while
  // any are held the first page is frozen and stops polling; see
  // `lib/paging.ts` for why.
  const pager = useOlderPages<A2AChannel, ChannelCursor>(channelKey, WHOLE_RECORD);
  const channels = useQuery("a2a_channels", WHOLE_RECORD, {
    pollMs: pager.frozen ? undefined : POLL_MS,
  });
  const firstNext = channels.data?.truncated ? cursorOf(channels.data.next) : null;
  const rows = useMemo(() => pager.rows(channels.data?.channels), [pager.rows, channels.data]);
  const more = pager.more(firstNext);
  // THE ENGINE'S COUNTS, over every channel the listing matched — taken
  // before its cut, so they do not change as a reader pages. Absent when the
  // record could not be read at all, which the callout below says.
  const totals = channels.data?.totals;
  const loadOlder = () =>
    void pager.loadOlder(channels.data?.channels, firstNext, async (cursor) => {
      const page = await socket.query("a2a_channels", { ...WHOLE_RECORD, ...cursor });
      return { rows: page.channels, next: page.truncated ? cursorOf(page.next) : null };
    });
  const backToNewest = () => {
    pager.backToNewest();
    channels.refetch();
  };

  // THE CHANNEL THIS PATH NAMES, read by its id — see [oneChannel] — rather
  // than looked up in the page above, which holds it only while it is recent.
  const addressed = channelId ?? "";
  const lookup = useQuery("a2a_channels", oneChannel(addressed), {
    enabled: addressed !== "",
    pollMs: POLL_MS,
  });

  // THE ORDER `[` AND `]` WALK. Published from the rows this screen holds, so
  // the stepper walks the record as the reader sorted it rather than the order
  // the coordination bucket happened to answer in.
  usePeekNeighbours(
    useMemo(() => rows.map((c) => ({ kind: "channel" as const, id: c.id })), [rows]),
  );

  const { open: openPeek } = usePeekControls();
  const peek = usePeek();
  const focused = peek?.kind === "channel" ? peek.id : addressed;

  const openChannel = useCallback(
    (c: A2AChannel, e: React.MouseEvent | React.KeyboardEvent) => {
      const go = () => openPeek({ kind: "channel", id: c.id });
      // The grid hands this both events. `rowPeekHandler` is the frame's one
      // copy of "which clicks mean elsewhere" and reads a MOUSE event — every
      // modifier and the middle button belong to the browser, so the row stays
      // a real link to the channel's page. The `enter` chord carries no button
      // at all and is never "open elsewhere".
      if (!("button" in e)) {
        go();
        return;
      }
      rowPeekHandler(go)?.(e);
    },
    [openPeek],
  );

  const addressedChannel = (lookup.data?.channels ?? []).find((c) => c.id === addressed) ?? null;
  // AND THAT NAME IS THE CHANNEL'S EVERYWHERE, not just in the header below.
  // The breadcrumb, the browser tab and the palette's recents read the one
  // label a screen publishes and otherwise show the raw path segment — a
  // channel uuid, which says neither who nor what.
  const channelName = addressedChannel ? channelTitle(addressedChannel, seatName) : "";
  usePageLabels(addressed && channelName ? { [addressed]: channelName } : {});

  return (
    <>
      <PageNote>
        The private channels seats opened with each other. One ask, one answer, then closed — the
        channel is the authorization record, not the transport.
      </PageNote>

      {/* The flush Panel this row sat in is gone: StatGroup draws that surface
          itself — the same hairline, radius and clip our `panel-flush` did —
          and the tiles inside it are flush, so wrapping it would be two
          surfaces around one row. */}
      {/* THE ENGINE'S TOTALS, NOT A FOLD OVER THE ROWS. The rows are a page of
          the most recently active channels, so a count reduced over them
          described the page and changed as a reader loaded more. */}
      <StatGroup columns={3}>
        <StatCard
          icon={<LinkGlyph size="xs" />}
          label="Open channels"
          value={totals ? totals.open : ""}
          sub={totals ? `of ${plural(totals.channels, "channel")} in the record` : undefined}
        />
        <StatCard
          icon={<ChatGlyph size="xs" />}
          label="Messages"
          value={totals ? totals.messages : ""}
          sub="across every channel in the record"
        />
        <StatCard
          icon={<GroupGlyph size="xs" />}
          label="Pairs"
          value={totals ? totals.pairs : ""}
          sub="distinct requester/target pairs"
        />
      </StatGroup>

      {channels.loading && <Skeleton variant="text" rows={4} label="Loading channels" />}
      {channels.data?.available === false ? (
        <Callout variant="neutral" icon={<LinkGlyph size="md" />}>
          No agent-to-agent channel record is reachable from this node. Channels live in the fleet's
          coordination store; a node that cannot read it says so rather than drawing an empty list.
        </Callout>
      ) : (
        <QueryState
          error={channels.error}
          loading={channels.loading}
          empty={
            rows.length > 0
              ? undefined
              : {
                  title: "No channels have been opened",
                  hint: "A seat opens one with a2a_ask — narrowly scoped to tight-loop sync between agents. Ordinary collaboration goes through chat and the tracker, where a human can see it.",
                }
          }
        >
          <Card padding="none">
            <DataGrid<A2AChannel>
              rows={rows}
              rowKey={(c) => c.id}
              onRowActivate={openChannel}
              rowHref={(c) => peekHref({ kind: "channel", id: c.id })}
              isSelected={(c) => c.id === focused}
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
                  sortValue: (c) => seatName(c.requester),
                  // NOT `SeatCell`, and not the `SeatChip` this column used to
                  // draw: both are anchors and every row here is one now, and
                  // an anchor inside an anchor is markup no browser agrees
                  // about. The seat is a link again in the peek's own facts.
                  cell: (c) => <TextCell icon="memory">{seatName(c.requester)}</TextCell>,
                },
                {
                  key: "to",
                  header: "Asked",
                  sortValue: (c) => seatName(c.target),
                  cell: (c) => <TextCell icon="memory">{seatName(c.target)}</TextCell>,
                },
                {
                  key: "messages",
                  header: "Messages",
                  align: "right",
                  shrink: true,
                  sortValue: (c) => c.messages,
                  // A CELL RATHER THAN THE BARE NUMBER it used to render: a
                  // channel with nothing on it yet is a real zero and must
                  // read as one, and the tabular face is what lets a column of
                  // counts be compared down the page.
                  cell: (c) => <NumberCell value={c.messages} />,
                },
                {
                  key: "opened",
                  header: "Opened",
                  shrink: true,
                  sortValue: (c) => tsKey(c.opened_at),
                  cell: (c) => <DateCell at={c.opened_at} now={now} />,
                },
                {
                  key: "last",
                  header: "Last message",
                  shrink: true,
                  sortValue: (c) => tsKey(c.last_at),
                  cell: (c) => <DateCell at={c.last_at} now={now} />,
                },
              ]}
            />
            {/* WHERE THE REST ARE: the next page, read from the cursor the
                answer carries. Drawn only where there is one, or where the
                list is a paged snapshot that has to say so. */}
            {(more || pager.frozen || pager.error) && (
              <Card.Footer variant="meta">
                <span className="row gap-2 wrap">
                  {pager.error ? (
                    <QueryState error={pager.error} loading={false} />
                  ) : (
                    <span className="t-caption">
                      {totals
                        ? `${plural(rows.length, "channel")} of ${totals.channels.toLocaleString()}, most recently active first.`
                        : `${plural(rows.length, "channel")}, most recently active first.`}
                      {pager.frozen && " Updates are paused while older channels are loaded."}
                    </span>
                  )}
                  <span className="spacer" />
                  {more && (
                    <Button
                      size="small"
                      variant="secondary"
                      onClick={loadOlder}
                      disabled={pager.paging}
                      loading={pager.paging}
                    >
                      Load older channels
                    </Button>
                  )}
                  {pager.frozen && (
                    <Button size="small" variant="secondary" onClick={backToNewest}>
                      Back to the newest
                    </Button>
                  )}
                </span>
              </Card.Footer>
            )}
          </Card>
        </QueryState>
      )}

      {/* THE CHANNEL THIS PATH NAMES. `#/activity/a2a/{id}` is a channel's own
          page — it is where `Open ↗` from the rail lands and what a ⌘-click on
          a row opens — and it read as the plain list for as long as the id was
          accepted and ignored: a reader who followed a link to one channel got
          every channel and no sign of which one they had asked for. */}
      {addressedChannel && (
        <>
          <ObjectHeader
            kind="A2A channel"
            icon="link"
            identifier={addressedChannel.id}
            title={channelName}
            status={<ChannelState channel={addressedChannel} />}
            facts={channelFacts(addressedChannel, seatName, now)}
          />
          <ChannelBody channel={addressedChannel} seatName={seatName} now={now} />
        </>
      )}
      {/* ONLY OVER A RECORD THAT WAS ACTUALLY READ. "This node cannot reach the
          coordination store", "the read failed" and "the record holds no such
          channel" are three different facts, and this Empty states the third
          about somebody's company — so it is guarded on the answer rather than
          on `loading` alone. Unguarded, a timed-out socket or a node with no
          channel record drew the refusal banner above and, directly under it, a
          confident claim that the channel the reader followed a link to had
          been purged. `available` is the flag that tells the first two apart —
          see `queries.a2aChannels` — and a present answer is what tells a read
          that happened from one that did not; the sibling `ChannelPeek` gets
          both for free by rendering inside `QueryState`. */}
      {addressed !== "" && !addressedChannel && lookup.data?.available === true && (
        <EmptyState
          icon={<LinkGlyph size={32} />}
          title="No such channel in the record"
          description="A channel is kept until the retention horizon. It may have been purged, or the id may be wrong."
        />
      )}

      <Section title="What this surface is, and is not">
        <Callout variant="neutral" icon={<InfoGlyph size="md" />}>
          <span className="col" style={{ gap: 4 }}>
            <span>
              A2A is deliberately narrow: one ask, one answer, then the channel closes. Both halves
              travel over the durable seat inbox, so a colleague owned by another node is an
              ordinary target.
            </span>
            <span className="t-caption">
              Anything a human teammate would reasonably want to see goes through the company's
              chat, tracker or code host instead — so it is readable where it actually happens,
              which is why nothing here tries to be an inbox.
            </span>
          </span>
        </Callout>
      </Section>
    </>
  );
}
