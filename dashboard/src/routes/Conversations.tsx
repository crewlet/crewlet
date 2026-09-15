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
 */

import { useCallback, useMemo } from "react";
import { QueryState, SeatChip, Section } from "~/components/common.tsx";
import { useOrg } from "~/lib/store-hooks.ts";
import { useParam } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { indexOrg } from "~/lib/seats.ts";
import { tsKey } from "~/lib/format.ts";
import type { A2AChannel } from "~/protocol/index.ts";
import {
  Callout,
  DataView,
  type DataViewColumn,
  EmptyState,
  type FilterDef,
  type FilterValues,
  PageHeader,
  RelativeTime,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
  useNow,
} from "@crewlethq/ui";
import { ChatGlyph, GroupGlyph, InfoGlyph, LinkGlyph } from "@crewlethq/icons/glyphs";

export function Conversations() {
  const org = useOrg();
  const now = useNow();
  const channels = useQuery("a2a_channels", undefined, { pollMs: 30_000 });
  const index = useMemo(() => indexOrg(org), [org]);

  // A channel names its parties by HANDLE — see a2a.Channel.OtherParty — and
  // a handle is an address, not a label. Passed straight through it put
  // `agent-ai-systems-engineer` where the seat's name belongs and built the
  // avatar's monogram out of it. The handle still does the linking.
  const seatName = useCallback(
    (handle: string) => index.byHandle.get(handle)?.name || handle,
    [index],
  );

  const all = useMemo(() => channels.data?.channels ?? [], [channels.data]);

  /*
   * THE SCREEN OWNS THE NARROWING, so both axes are URL parameters. State is
   * the axis worth having: an open channel is one a seat is still waiting on,
   * and a record with a thousand closed rows in it buries them.
   */
  const [state, setState] = useParam("state", "");
  const [party, setParty] = useParam("party", "");

  const shown = useMemo(() => {
    const needle = party.trim().toLowerCase();
    return all
      .filter((c) => !state || (c.closed_at ? "closed" : "open") === state)
      .filter(
        (c) =>
          !needle ||
          seatName(c.requester).toLowerCase().includes(needle) ||
          seatName(c.target).toLowerCase().includes(needle) ||
          c.requester.toLowerCase().includes(needle) ||
          c.target.toLowerCase().includes(needle),
      );
  }, [all, state, party, seatName]);

  const filters = useMemo<FilterDef<A2AChannel>[]>(
    () => [
      {
        name: "party",
        label: "Party",
        role: "search",
        placeholder: "Either seat, by name or handle",
      },
      {
        name: "state",
        label: "State",
        kind: "select",
        options: [
          { value: "", label: "Any state" },
          { value: "open", label: "Open" },
          { value: "closed", label: "Closed" },
        ],
      },
    ],
    [],
  );

  const values: FilterValues = { party, state };

  const onValuesChange = useCallback(
    (next: FilterValues) => {
      setParty(String(next.party ?? ""));
      setState(String(next.state ?? ""));
    },
    [setParty, setState],
  );

  const columns = useMemo<DataViewColumn<A2AChannel>[]>(
    () => [
      {
        key: "state",
        header: "State",
        shrink: true,
        sortable: true,
        sortValue: (c) => (c.closed_at ? "closed" : "open"),
        render: (c) => (
          <Tag variant={c.closed_at ? "neutral" : "info"} dot>
            {c.closed_at ? "closed" : "open"}
          </Tag>
        ),
      },
      {
        key: "from",
        header: "Asked by",
        sortable: true,
        sortValue: (c) => seatName(c.requester),
        render: (c) => <SeatChip name={seatName(c.requester)} handle={c.requester} />,
      },
      {
        key: "to",
        header: "Asked",
        sortable: true,
        sortValue: (c) => seatName(c.target),
        render: (c) => <SeatChip name={seatName(c.target)} handle={c.target} />,
      },
      {
        key: "messages",
        header: "Messages",
        align: "right",
        shrink: true,
        sortable: true,
        firstDirection: "desc",
        sortValue: (c) => c.messages,
      },
      {
        key: "opened",
        header: "Opened",
        shrink: true,
        sortable: true,
        firstDirection: "desc",
        sortValue: (c) => tsKey(c.opened_at),
        render: (c) => <RelativeTime className="t-caption" value={c.opened_at} now={now} />,
      },
      {
        key: "last",
        header: "Last message",
        shrink: true,
        sortable: true,
        firstDirection: "desc",
        sortValue: (c) => tsKey(c.last_at),
        render: (c) => <RelativeTime className="t-caption" value={c.last_at} now={now} />,
      },
    ],
    [seatName, now],
  );

  return (
    <>
      <PageHeader
        title="Agent-to-agent"
        description="The private channels seats opened with each other. One ask, one answer, then closed. The channel is the authorization record, not the transport."
      />

      <StatGroup columns={3}>
        <StatCard
          icon={<LinkGlyph />}
          label="Open channels"
          value={all.filter((c) => !c.closed_at).length}
          sub="one ask, one answer, then closed"
        />
        <StatCard
          icon={<ChatGlyph />}
          label="Messages"
          value={all.reduce((n, c) => n + c.messages, 0)}
          sub="across every channel in the record"
        />
        <StatCard
          icon={<GroupGlyph />}
          label="Pairs"
          value={new Set(all.map((c) => `${c.requester}->${c.target}`)).size}
          sub="distinct requester/target pairs"
        />
      </StatGroup>

      {channels.loading && <Skeleton label="Loading the conversations" variant="text" rows={4} />}
      {channels.data?.available === false ? (
        <Callout variant="neutral" icon={<LinkGlyph size="sm" />}>
          <span>
            No agent-to-agent channel record is reachable from this node. Channels live in the
            fleet's coordination store; a node that cannot read it says so rather than drawing an
            empty list.
          </span>
        </Callout>
      ) : (
        <QueryState error={channels.error} loading={channels.loading}>
          <DataView<A2AChannel>
            framed
            columns={columns}
            rows={shown}
            totalCount={all.length}
            getRowKey={(c) => c.id}
            defaultSort={{ key: "last", direction: "desc" }}
            filters={filters}
            filterValues={values}
            onFilterValuesChange={onValuesChange}
            emptyMessage={
              <EmptyState
                size="compact"
                icon={<LinkGlyph />}
                title={
                  all.length ? "No channel matches these filters" : "No channels have been opened"
                }
                description={
                  all.length
                    ? "Clear them to see every channel in the record."
                    : "A seat opens one with a2a_ask, narrowly scoped to tight-loop sync between agents. Ordinary collaboration goes through chat and the tracker, where a human can see it."
                }
              />
            }
          />
        </QueryState>
      )}

      <Section title="What this surface is, and is not">
        <Callout variant="neutral" icon={<InfoGlyph size="sm" />}>
          <span className="col" style={{ gap: 4 }}>
            <span>
              A2A is deliberately narrow: one ask, one answer, then the channel closes. Both halves
              travel over the durable seat inbox, so a colleague owned by another node is an
              ordinary target.
            </span>
            <span className="t-caption">
              Anything a human teammate would reasonably want to see goes through the company's
              chat, tracker or code host instead — which is why those threads are on the other tab,
              and why every one of them links out to where it actually lives.
            </span>
          </span>
        </Callout>
      </Section>
    </>
  );
}
