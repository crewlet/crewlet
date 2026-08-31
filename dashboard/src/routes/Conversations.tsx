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
import { ScreenHead } from "~/app/Shell.tsx";
import { QueryState, SeatChip, Section } from "~/components/common.tsx";
import { Badge, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { indexOrg } from "~/lib/seats.ts";
import { relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";

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

  return (
    <>
      <ScreenHead
        title="Agent-to-agent"
        sub="The private channels seats opened with each other. One ask, one answer, then closed — the channel is the authorization record, not the transport."
      />

      <Panel padding="none">
        <StatRow cols={3}>
          <Stat
            icon="link"
            label="Open channels"
            value={(channels.data?.channels ?? []).filter((c) => !c.closed_at).length}
            sub="one ask, one answer, then closed"
          />
          <Stat
            icon="message"
            label="Messages"
            value={(channels.data?.channels ?? []).reduce((n, c) => n + c.messages, 0)}
            sub="across every channel in the record"
          />
          <Stat
            icon="users"
            label="Pairs"
            value={
              new Set((channels.data?.channels ?? []).map((c) => `${c.requester}->${c.target}`))
                .size
            }
            sub="distinct requester/target pairs"
          />
        </StatRow>
      </Panel>

      {channels.loading && <Skeleton rows={4} />}
      {channels.data?.available === false ? (
        <div className="banner neutral">
          <Icon name="link" size="sm" />
          <span>
            No agent-to-agent channel record is reachable from this node. Channels live in the
            fleet's coordination store; a node that cannot read it says so rather than drawing an
            empty list.
          </span>
        </div>
      ) : (
        <QueryState
          error={channels.error}
          loading={channels.loading}
          empty={
            channels.data?.channels?.length
              ? undefined
              : {
                  title: "No channels have been opened",
                  hint: "A seat opens one with a2a_ask — narrowly scoped to tight-loop sync between agents. Ordinary collaboration goes through chat and the tracker, where a human can see it.",
                }
          }
        >
          <Panel padding="none">
            <DataTable
              rows={channels.data?.channels ?? []}
              rowKey={(c) => c.id}
              defaultSort={{ key: "last", dir: "desc" }}
              columns={[
                {
                  key: "state",
                  header: "State",
                  shrink: true,
                  sortValue: (c) => (c.closed_at ? "closed" : "open"),
                  cell: (c) => (
                    <Badge tone={c.closed_at ? "neutral" : "info"} dot>
                      {c.closed_at ? "closed" : "open"}
                    </Badge>
                  ),
                },
                {
                  key: "from",
                  header: "Asked by",
                  sortValue: (c) => seatName(c.requester),
                  cell: (c) => <SeatChip name={seatName(c.requester)} handle={c.requester} />,
                },
                {
                  key: "to",
                  header: "Asked",
                  sortValue: (c) => seatName(c.target),
                  cell: (c) => <SeatChip name={seatName(c.target)} handle={c.target} />,
                },
                {
                  key: "messages",
                  header: "Messages",
                  align: "right",
                  shrink: true,
                  sortValue: (c) => c.messages,
                  cell: (c) => c.messages,
                },
                {
                  key: "opened",
                  header: "Opened",
                  shrink: true,
                  sortValue: (c) => tsKey(c.opened_at),
                  cell: (c) => <span className="t-caption">{relTime(c.opened_at, now)}</span>,
                },
                {
                  key: "last",
                  header: "Last message",
                  shrink: true,
                  sortValue: (c) => tsKey(c.last_at),
                  cell: (c) => <span className="t-caption">{relTime(c.last_at, now)}</span>,
                },
              ]}
            />
          </Panel>
        </QueryState>
      )}

      <Section title="What this surface is, and is not">
        <div className="banner neutral">
          <Icon name="info" size="sm" />
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
        </div>
      </Section>
    </>
  );
}
