/**
 * Conversations — the threads the company is carrying.
 *
 * This is the messaging surface's foundation, and it is deliberately built on
 * what the engine ALREADY knows rather than on a new store:
 *
 *  - the **conversation ledger** (`conversation_sessions`) — one row per
 *    completed turn, keyed on the seat and the conversation it served, in the
 *    `{source}:{local}` grammar (`jira:POC-7`, `slack:C9:1718.001`,
 *    `github:acme/api#42`);
 *  - **agent-to-agent channels** — one ask, one answer, then closed. The
 *    channel is an authorization record rather than a transport, so what is
 *    shown is the record: who asked whom, how many messages, and when.
 *
 * What it does NOT do is claim to be a chat client. There is no operator→agent
 * write path in the engine that is not a vendor delivery, so a compose box here
 * would be a button that does nothing. Every thread deep-links out to the
 * surface it actually lives on.
 */

import { useMemo } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { useNavigator, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip, Section } from "~/components/common.tsx";
import {
  Badge,
  Button,
  Empty,
  Panel,
  SearchInput,
  Segmented,
  Skeleton,
  Stat,
  StatRow,
} from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtDateTime, relTime, splitConversationKey, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";

type Lens = "threads" | "a2a";

export function Conversations() {
  const nav = useNavigator();
  const org = useOrg();
  const agents = useAgents();
  const now = useNow();
  const [lens, setLens] = useParam("lens", "threads", "section");
  const [handle, setHandle] = useParam("handle", "");
  const [key, setKey] = useParam("key", "");

  const index = useMemo(() => indexOrg(org), [org]);
  const seats = index.seats.filter((s) => s.kind === "agent");
  // With no seat chosen the first agent seat is used, so the screen answers
  // something on arrival rather than showing a picker over an empty pane.
  const active = handle || seats[0]?.handle || "";

  const threads = useQuery(
    "conversations",
    { handle: active, ...(key ? { key } : {}) },
    { enabled: lens === "threads" && !!active },
  );
  const channels = useQuery("a2a_channels", undefined, { enabled: lens === "a2a", pollMs: 30_000 });

  const conversations = threads.data?.conversations ?? [];
  const entries = threads.data?.entries ?? [];

  return (
    <>
      <ScreenHead
        title="Conversations"
        sub="Every thread a seat is carrying across the surfaces it works on, and the private channels seats opened with each other."
        actions={
          <Segmented<Lens>
            ariaLabel="Conversation kind"
            value={lens as Lens}
            onChange={setLens}
            options={[
              { value: "threads", label: "Threads", icon: "message" },
              { value: "a2a", label: "Agent-to-agent", icon: "link" },
            ]}
          />
        }
      />

      {lens === "threads" && (
        <>
          <div className="toolbar">
            <span className="t-label">Seat</span>
            <div style={{ maxWidth: 220 }}>
              <SearchInput
                value={handle}
                onChange={setHandle}
                ariaLabel="Filter to a seat"
                placeholder={active || "handle"}
              />
            </div>
            <div className="row wrap gap-1">
              {seats.slice(0, 8).map((s) => (
                <Button
                  key={s.handle}
                  size="sm"
                  variant={s.handle === active ? "primary" : "ghost"}
                  onClick={() => setHandle(s.handle)}
                >
                  {s.name}
                </Button>
              ))}
            </div>
            <span className="spacer" />
            {key && (
              <Button size="sm" icon="x" onClick={() => setKey("")}>
                {key}
              </Button>
            )}
          </div>

          {threads.loading && <Skeleton rows={4} />}

          {threads.data?.available === false ? (
            <div className="banner neutral">
              <Icon name="database" size="sm" />
              <span>
                This node holds no conversation ledger. The ledger is written by whichever node ran
                the turn, so on a fleet another node may hold these rows.
              </span>
            </div>
          ) : (
            <QueryState error={threads.error} loading={threads.loading}>
              <div className="split">
                <Panel title="Threads" icon="message" count={conversations.length} padding="none">
                  {conversations.length ? (
                    <div className="list">
                      {conversations.map((c) => {
                        const { source, local } = splitConversationKey(c.key);
                        return (
                          <button
                            key={c.key}
                            className={`list-row clickable${key === c.key ? " selected" : ""}`}
                            onClick={() => setKey(key === c.key ? "" : c.key)}
                          >
                            <span className="col" style={{ gap: 2, minWidth: 0, flex: 1 }}>
                              <span className="row gap-1">
                                <Badge outline>{source || "—"}</Badge>
                                <span className="t-caption">{c.turns} turns</span>
                              </span>
                              <span className="truncate mono t-cell">{local}</span>
                              <span className="t-caption">{relTime(c.last_at, now)}</span>
                            </span>
                          </button>
                        );
                      })}
                    </div>
                  ) : (
                    <Empty
                      inline
                      icon="message"
                      title="No threads for this seat"
                      hint="A thread is recorded when a turn completes that served one. A seat that has only run schedules has none."
                    />
                  )}
                </Panel>

                <Panel
                  title={key ? splitConversationKey(key).local : "Recent turns"}
                  icon="layers"
                  subtitle={
                    key ? splitConversationKey(key).source : "across every thread this seat carries"
                  }
                  count={entries.length}
                  padding="none"
                >
                  {entries.length ? (
                    <div className="list">
                      {[...entries]
                        .sort((a, b) => tsKey(b.created_at) - tsKey(a.created_at))
                        .map((e, i) => (
                          <div key={e.turn_id ?? i} className="thread-entry">
                            <div className="row gap-1">
                              {e.outcome && (
                                <Badge tone={e.outcome === "done" ? "positive" : "caution"}>
                                  {e.outcome}
                                </Badge>
                              )}
                              <code className="inline">
                                {splitConversationKey(e.conversation_key).local}
                              </code>
                              <span className="spacer" />
                              <span className="t-caption" title={fmtDateTime(e.created_at)}>
                                {relTime(e.created_at, now)}
                              </span>
                            </div>
                            <p className="t-body">
                              {e.summary || <span className="faint">No summary recorded.</span>}
                            </p>
                            {e.turn_id && (
                              <div className="row">
                                <span className="spacer" />
                                <Button
                                  size="sm"
                                  variant="ghost"
                                  onClick={() => nav.to(["turns", String(e.turn_id)])}
                                >
                                  The turn →
                                </Button>
                              </div>
                            )}
                          </div>
                        ))}
                    </div>
                  ) : (
                    <Empty
                      inline
                      icon="layers"
                      title="Nothing recorded yet"
                      hint="The ledger keeps a structured entry per completed turn — never a transcript replay, because the thread has moved on by the next turn."
                    />
                  )}
                </Panel>
              </div>
            </QueryState>
          )}
        </>
      )}

      {lens === "a2a" && (
        <>
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
                fleet's coordination store; a node that cannot read it says so rather than drawing
                an empty list.
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
                      sortValue: (c) => c.requester,
                      cell: (c) => <SeatChip name={c.requester} handle={c.requester} />,
                    },
                    {
                      key: "to",
                      header: "Asked",
                      sortValue: (c) => c.target,
                      cell: (c) => <SeatChip name={c.target} handle={c.target} />,
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
                  A2A is deliberately narrow: one ask, one answer, then the channel closes. Both
                  halves travel over the durable seat inbox, so a colleague owned by another node is
                  an ordinary target.
                </span>
                <span className="t-caption">
                  Anything a human teammate would reasonably want to see goes through the company's
                  chat, tracker or code host instead — which is why those threads are on the other
                  tab, and why every one of them links out to where it actually lives.
                </span>
              </span>
            </div>
          </Section>
        </>
      )}
    </>
  );
}
