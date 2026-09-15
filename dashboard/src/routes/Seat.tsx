/**
 * One seat: who it is, what it is doing, what it remembers, what it costs.
 *
 * Tabs are SECTIONS — they push a history entry, because the reader called
 * them — and the tab is in the URL so a colleague can be sent the exact view.
 */

import { useMemo, useRef } from "react";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip, Section, StateBadge } from "~/components/common.tsx";
import { TurnCard } from "~/components/TurnCard.tsx";
import { useSettled } from "~/lib/settled.ts";
import {
  Avatar,
  Badge,
  Button,
  Empty,
  KeyValue,
  Meter,
  Panel,
  Skeleton,
  Stat,
  StatRow,
  Tabs,
} from "~/ui/primitives.tsx";
import { BarList, phaseColor } from "~/ui/charts.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useAgents, useOrg, usePhaseEvents, useSandboxes, useTokens } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { awaitingPerson, indexOrg, statusLine, afkReason, runState } from "~/lib/seats.ts";
import {
  fmtCount,
  fmtDateTime,
  fmtDuration,
  plural,
  relTime,
  splitConversationKey,
} from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import {
  fromLiveCall,
  fromPhaseEvent,
  groupTurns,
  mergePhases,
  streamedPhases,
  type PhaseRecord,
} from "~/lib/phases.ts";
import type { ConversationEntry, CounterpartyProfile, EventRecord } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

type Tab = "overview" | "model" | "threads" | "memory" | "cost" | "access";

const seatTurnKey = (g: { turnId: string }) => g.turnId;

/** A provider chain, in the order the fallback walks it. */
function ModelChain({ keys }: { keys: string[] }) {
  return (
    <span className="row gap-1" style={{ flexWrap: "wrap" }}>
      {keys.map((key, i) => (
        <span key={key} className="row gap-1">
          {i > 0 && (
            <span className="faint" title="falls back to">
              →
            </span>
          )}
          <code className="inline">{key}</code>
        </span>
      ))}
    </span>
  );
}

export function SeatScreen({ handle }: { handle: string }) {
  const nav = useNavigator();
  const org = useOrg();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const tokens = useTokens();
  const now = useNow();
  const [tab, setTab] = useParam("tab", "overview", "section");
  // WHICH THREAD IS OPEN, as a filter rather than a section: opening one
  // replaces the history entry, so Back leaves the seat rather than walking
  // every thread the reader glanced at.
  const [thread, setThread] = useParam("conversation", "", "filter");

  const phaseEvents = usePhaseEvents();

  const index = useMemo(() => indexOrg(org), [org]);
  const seat =
    index.byHandle.get(handle) ??
    index.byName.get(handle) ??
    [...index.byHandle.values()].find((s) => s.handle.toLowerCase() === handle.toLowerCase()) ??
    null;

  const agent = agents.find((a) => a.handle === handle || a.id === handle || a.role === seat?.name);
  const sandbox = sandboxes.find((s) => s.role === seat?.name) ?? null;
  // The ROLE NAME, which is what a phase record carries — the URL and every
  // link into this screen carry the handle. Empty for a handle that resolves to
  // nothing, and the stream filter below reads it as "match no phase" rather
  // than as "match every phase that named no role".
  const role = agent?.role ?? seat?.name ?? "";

  // The seat's own phase history. Its `live` half is deliberately NOT read:
  // the projection already pushes it onto the roster, and reading it here too
  // would give one screen two sources for one fact.
  const history = useQuery(
    "agent",
    { id: handle },
    { enabled: tab === "overview" || tab === "model" },
  );
  const memory = useQuery("agent_memory", { id: handle }, { enabled: tab === "memory" });
  // WHAT THIS SEAT HAS SAID ON A SURFACE THE ENGINE DOES NOT OWN. The ledger
  // is what stops it replying twice in one chat thread, and it has been
  // written since the runtime landed with nothing on any screen reading it.
  //
  // SCOPED, so a seat's threads are readable by that seat's own person and by
  // an operator — the same rule every other per-seat question follows. The
  // handle is sent explicitly because this screen is about somebody else's
  // seat as often as the reader's own.
  const threads = useQuery(
    "conversations",
    { handle, ...(thread ? { conversation: thread } : {}) },
    { enabled: tab === "threads" },
  );
  // A PERSON RECORD IS A HUMAN'S. A seat has a MAILBOX — the durable
  // subscription the engine attaches when it acquires the seat — and nothing
  // on a person's record describes one, so this is read only for a human.
  // AND ONLY WHERE THE READER MAY HAVE IT. A person record is somebody's
  // unread notices, the order they mean to work in and who set it — the
  // engine scopes it to the seat the caller's own credential is bound to, and
  // a colleague reading it needs an operator one. Asking anyway would put a
  // refusal on the screen where the honest answer is that this is theirs.
  const viewer = useViewer();
  const mayReadPerson = viewer.operator || (viewer.handle !== "" && viewer.handle === handle);
  const person = useQuery(
    "work_person",
    { handle },
    { enabled: tab === "overview" && seat?.kind === "human" && mayReadPerson, pollMs: 60_000 },
  );
  const spend = useQuery(
    "tokens",
    { agent_role: seat?.name ?? "", since_days: 7, recent_turns: 50 },
    { enabled: tab === "cost" && !!seat },
  );

  const phases = useMemo<PhaseRecord[]>(() => {
    const stored = (history.data?.llm_history ?? [])
      .map((ev) => fromPhaseEvent(ev as EventRecord))
      .filter((r): r is PhaseRecord => r !== null);
    // The query above is answered ONCE, at mount. Every phase that finishes
    // after it — which is every phase of the turn a reader opened this tab to
    // watch — reaches the tab only here.
    const streamed = streamedPhases(phaseEvents, (r) => role !== "" && r.role === role);
    const live = agent?.live_call ? [fromLiveCall(agent.live_call, agent.role)] : [];
    // Streamed FIRST so the query's own copy of the same phase wins the key:
    // both are the same durable record, and preferring the one that came
    // through the paged, authoritative answer keeps one source in charge.
    return mergePhases([...streamed, ...stored], live);
  }, [history.data, phaseEvents, agent, role]);

  const turns = useMemo(() => groupTurns(phases), [phases]);
  const liveTurns = useMemo(() => turns.filter((g) => g.live), [turns]);
  const doneTurns = useMemo(() => turns.filter((g) => !g.live), [turns]);
  const liveTurnKeys = useMemo(() => liveTurns.map(seatTurnKey), [liveTurns]);
  // The turns this reader has watched RUN.
  //
  // A turn card is REMOUNTED when it crosses from the live region into the
  // settled list — two different lists, so React builds a new component — and
  // its latched open state goes with the old one. `i === 0 && !liveTurns.length`
  // then closes it whenever the seat has already started another turn, which
  // collapsed the transcript the reader had open at the exact moment its last
  // phase landed. Accumulated in a ref, and written during render for the same
  // reason `useSettled` does it: adding to a set is idempotent, so a render
  // React discards and repeats leaves the same set behind.
  const watched = useRef<Set<string>>(new Set());
  for (const key of liveTurnKeys) watched.current.add(key);
  // A turn the reader has been watching run is not a NEW row when it finishes:
  // it moves out of the live list into this one, and holding it behind "1 new
  // turn finished while you were reading" is the same disappearance from the
  // reader's side.
  const settled = useSettled(doneTurns, seatTurnKey, liveTurnKeys);

  if (!seat) {
    return (
      <>
        <Empty
          icon="user"
          title={`No seat called “${handle}”`}
          hint="Seats are addressed by handle. If a company revision was just applied, this seat may have been renamed or removed."
          action={
            <Button variant="primary" onClick={() => nav.to(["people"])}>
              All seats
            </Button>
          }
        />
      </>
    );
  }

  const manager = index.managerOf.get(seat.name);
  const reports = index.reportsOf.get(seat.name) ?? [];
  const human = seat.kind === "human";

  // A HUMAN SEAT HAS NO RUNTIME TABS (see the strip below), so a URL naming
  // one — a bookmark from before the seat's kind changed, or a link between
  // two seats — must land somewhere real rather than on a blank page.
  const shown: Tab = human && tab !== "overview" && tab !== "access" ? "overview" : (tab as Tab);
  const state = runState(agent, sandboxes);
  const seatSpend = tokens?.by_agent?.find((a) => a.role === seat.name);

  return (
    <>
      <PageActions>
        {
          <>
            <Badge mono outline>
              @{seat.handle}
            </Badge>
            {human ? (
              <Badge outline>human seat</Badge>
            ) : (
              <StateBadge agent={agent} sandboxes={sandboxes} />
            )}
            {seat.unit && <Badge outline>{seat.unit.name}</Badge>}
          </>
        }
        {
          <Button
            icon="activity"
            size="sm"
            onClick={() => nav.to(["activity"], { actor: seat.name })}
          >
            Its events
          </Button>
        }
      </PageActions>
      <PageNote>{seat.goal || statusLine(agent, { sandbox, seat })}</PageNote>

      {agent?.last_error && (
        <div className="banner critical">
          <Icon name="alert" size="sm" />
          <span>
            <strong>{agent.last_error.kind || "error"}</strong> — {agent.last_error.message}
            {agent.last_error.phase && ` (during ${agent.last_error.phase})`}
            {agent.last_error.at && ` · ${relTime(agent.last_error.at, now)}`}
          </span>
          {agent.last_error.event_id && (
            <a className="t-link" href={href(["activity", "events", agent.last_error.event_id])}>
              event →
            </a>
          )}
        </div>
      )}
      {state === "afk" && (
        <div className="banner caution">
          <Icon name="pause" size="sm" />
          <span>This seat is AFK: {afkReason(agent?.afk_reason)}.</span>
        </div>
      )}
      {sandbox && awaitingPerson(sandbox.status) && (
        <div className="banner caution">
          <Icon name="help" size="sm" />
          <span>
            A coding run is paused on a question: {sandbox.question || "(no question recorded)"}
          </span>
          <Button size="sm" onClick={() => nav.to(["runs"], { run: sandbox.turn_id })}>
            The run
          </Button>
        </div>
      )}

      <Tabs<Tab>
        ariaLabel="Seat sections"
        value={shown}
        onChange={setTab}
        // THE KIND DECIDES THE SET. Model activity, Memory and Cost are
        // properties of a RUNTIME, and a human seat has none: it is
        // addressable and never spawned. All five were rendered
        // unconditionally, so a human teammate's Cost tab read "TOKENS · 7D —
        // 0 · INPUT / OUTPUT — 0 / 0 · CONFIGURED BUDGET — unlimited", which
        // is three measurements of a thing that cannot be measured, and their
        // Memory tab offered a diary nothing will ever write.
        //
        // Not disabled — absent. A tab that cannot have content is not an
        // empty state, it is a claim that the reader is missing something.
        options={
          human
            ? [
                { value: "overview" as const, label: "Overview", icon: "user" as const },
                { value: "access" as const, label: "Access", icon: "key" as const },
              ]
            : [
                { value: "overview" as const, label: "Overview", icon: "user" as const },
                { value: "model" as const, label: "Model activity", icon: "brain" as const },
                { value: "threads" as const, label: "Conversations", icon: "message" as const },
                { value: "memory" as const, label: "Memory", icon: "database" as const },
                { value: "cost" as const, label: "Cost", icon: "coin" as const },
                { value: "access" as const, label: "Access", icon: "key" as const },
              ]
        }
      >
        {/* WITHHELD, and said so. A panel that simply is not there reads as
            a person with nothing on their plate, which is the one thing it
            must not read as. */}
        {shown === "overview" && human && !mayReadPerson && (
          <Panel title="Their day" icon="check">
            <p className="t-body">
              Their inbox, their queue and their pinned views are theirs. Reading another person's
              record needs an operator credential.
            </p>
          </Panel>
        )}

        {shown === "overview" && human && person.data?.held && (
          <Panel
            title="Their day"
            icon="check"
            subtitle="Read-only here: an inbox is moved on by the person whose it is, through their own assistant."
          >
            <StatRow cols={3}>
              <Stat
                icon="inbox"
                label="Unread"
                value={person.data.unread?.length ?? 0}
                sub={
                  person.data.due?.length
                    ? `${person.data.due.length} snoozed and now due`
                    : "nothing snoozed is due"
                }
              />
              <Stat
                icon="layers"
                label="Queue"
                value={person.data.priorities?.length ?? 0}
                // WHO CHOSE IT is the one thing a queue cannot say for
                // itself. A lead may set what somebody in their line does
                // next, and a person who starts the day on work they did
                // not choose should be able to tell.
                // AND WHEN. A queue somebody else ordered three weeks ago
                // is a different fact from one they ordered this morning,
                // and the name alone cannot tell them apart — which is
                // what carrying the instant the whole way and rendering
                // nothing amounted to.
                sub={
                  person.data.priorities_set_by
                    ? `set by ${person.data.priorities_set_by}${
                        person.data.priorities_set_at
                          ? ` ${relTime(person.data.priorities_set_at, now)}`
                          : ""
                      }`
                    : "their own order"
                }
              />
              <Stat
                icon="flag"
                label="Pinned views"
                value={person.data.pinned_views?.length ?? 0}
                sub={`${person.data.favorites?.length ?? 0} starred`}
              />
            </StatRow>
          </Panel>
        )}

        {shown === "overview" && (
          <>
            <Panel padding="none">
              <StatRow cols={4}>
                <Stat
                  icon="zap"
                  label="State"
                  value={human ? "human" : state}
                  sub={statusLine(agent, { sandbox, seat })}
                />
                <Stat
                  icon="coin"
                  label="Tokens · 7d"
                  value={seatSpend ? fmtCount(seatSpend.total_tokens) : "—"}
                  sub={
                    seatSpend
                      ? `${seatSpend.calls.toLocaleString()} model calls`
                      : "nothing recorded"
                  }
                />
                <Stat
                  icon="layers"
                  label="Turns in the record"
                  // Zero is a MEASUREMENT — this seat has taken no turns — and
                  // an em dash would claim nobody looked.
                  value={turns.length}
                  sub="the phase history loaded below"
                />
                <Stat
                  icon="users"
                  label="Direct reports"
                  value={reports.length}
                  sub={manager ? `reports to ${manager.name}` : "no manager in the chart"}
                />
              </StatRow>
            </Panel>

            <div className="grid grid-auto-lg">
              <Panel title="Who this is" icon="user">
                <KeyValue
                  items={[
                    ["Role", seat.name],
                    [
                      "Handle",
                      <code key="h" className="inline">
                        @{seat.handle}
                      </code>,
                    ],
                    ["Kind", human ? "human teammate — never spawned by the engine" : "agent seat"],
                    ["Goal", seat.goal || <span className="faint">not set</span>],
                    ["Email", seat.email || <span className="faint">not set</span>],
                    [
                      "Unit",
                      seat.unitChain.length ? (
                        seat.unitChain.map((u) => u.name).join(" › ")
                      ) : (
                        <span className="faint">org-wide</span>
                      ),
                    ],
                    [
                      "Unit lead",
                      seat.unitLead ? (
                        <SeatChip
                          name={seat.unitLead}
                          handle={index.byName.get(seat.unitLead)?.handle}
                        />
                      ) : (
                        <span className="faint">none</span>
                      ),
                    ],
                    [
                      "Reports to",
                      manager ? (
                        <SeatChip name={manager.name} handle={manager.handle} />
                      ) : (
                        <span className="faint">nobody</span>
                      ),
                    ],
                    // A CHAIN, DRAWN AS ONE. `llm:` accepts a key, a list or a
                    // per-phase mapping, so this is the flattened order the
                    // provider chain actually walks — and an empty ARRAY is
                    // truthy, which is why the fallback is an explicit length
                    // test rather than `||`.
                    [
                      "Model",
                      seat.llm.length ? (
                        <ModelChain keys={seat.llm} />
                      ) : (
                        <span className="faint">default provider</span>
                      ),
                    ],
                    [
                      "Auxiliary model",
                      seat.llmAuxiliary.length ? (
                        <ModelChain keys={seat.llmAuxiliary} />
                      ) : (
                        <span className="faint">none — reflection uses the default</span>
                      ),
                    ],
                  ]}
                />
              </Panel>

              <Panel title="Profile" icon="book">
                <div className="col gap-3">
                  {seat.backstory && (
                    <div className="col gap-1">
                      <div className="t-label">Backstory</div>
                      <p className="t-body measure">{seat.backstory}</p>
                    </div>
                  )}
                  {seat.responsibilities.length > 0 && (
                    <div className="col gap-1">
                      <div className="t-label">Responsibilities</div>
                      <ul
                        className="col gap-1"
                        style={{ paddingLeft: "var(--space-4)", margin: 0 }}
                      >
                        {seat.responsibilities.map((r, i) => (
                          <li key={i} className="t-cell">
                            {r}
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}
                  {seat.guidelines.length > 0 && (
                    <div className="col gap-1">
                      <div className="t-label">Behavioural guidelines</div>
                      <ul
                        className="col gap-1"
                        style={{ paddingLeft: "var(--space-4)", margin: 0 }}
                      >
                        {seat.guidelines.map((r, i) => (
                          <li key={i} className="t-cell">
                            {r}
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}
                  {!seat.backstory && !seat.responsibilities.length && !seat.guidelines.length && (
                    <span className="t-caption faint">
                      No profile is set. Backstory, responsibilities and guidelines render straight
                      into this seat's executor prompt.
                    </span>
                  )}
                </div>
              </Panel>
            </div>

            {reports.length > 0 && (
              <Section title="Direct reports" hint={`${reports.length}`}>
                <div className="seat-grid">
                  {reports.map((r) => (
                    <a
                      key={r.handle}
                      className="seat-card"
                      href={href(["company", "people", r.handle])}
                    >
                      <div className="row">
                        <Avatar name={r.name} human={r.kind === "human"} />
                        <span className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
                          <span className="truncate t-cell">{r.name}</span>
                          <span className="truncate t-caption">{r.goal || r.unit?.name}</span>
                        </span>
                        {r.kind === "human" ? (
                          <Badge outline>human</Badge>
                        ) : (
                          <StateBadge
                            agent={agents.find((a) => a.role === r.name)}
                            sandboxes={sandboxes}
                          />
                        )}
                      </div>
                    </a>
                  ))}
                </div>
              </Section>
            )}

            {seat.schedules.length > 0 && (
              <Panel
                title="Recurring work"
                icon="calendar"
                count={seat.schedules.length}
                padding="none"
              >
                <DataTable
                  rows={seat.schedules}
                  rowKey={(s) => s.name}
                  columns={[
                    { key: "name", header: "Name", cell: (s) => s.name, sortValue: (s) => s.name },
                    {
                      key: "cron",
                      header: "Cron",
                      shrink: true,
                      cell: (s) => <code className="inline">{s.cron}</code>,
                    },
                    {
                      key: "task",
                      header: "Task",
                      cell: (s) => <span className="truncate">{s.task}</span>,
                    },
                  ]}
                />
              </Panel>
            )}
          </>
        )}

        {shown === "model" && (
          <>
            {history.loading && !turns.length && <Skeleton rows={4} height={44} />}
            {/* The QUERY'S OWN STATE, BESIDE THE TURNS RATHER THAN IN PLACE OF
              THEM. It used to wrap them, and `QueryState` renders NOTHING while
              a query is in flight and a banner INSTEAD of its children when one
              fails — so the turn happening right now was hidden until the event
              store answered, and hidden for good on a node that keeps no event
              log at all. Only the settled half of this screen comes from that
              query; the running half is pushed. */}
            {history.error && <QueryState error={history.error} loading={history.loading} />}
            {!history.loading && !history.error && !turns.length && (
              <Empty
                inline
                icon="brain"
                title="No phases in the record for this seat"
                hint="A phase is recorded when it completes. A seat that has not taken a turn has nothing here."
              />
            )}
            {/* The same split the Model screen makes, for the same reason:
              a running turn changes every couple of hundred milliseconds,
              and letting that churn sit inside the settled history reflowed
              whatever the reader was working through. Here it also answers
              "which of these is happening right now", which used to be
              readable only off a badge. */}
            {liveTurns.length > 0 && (
              <section className="col gap-1 live-region">
                <div className="t-label">
                  Running now
                  <span className="faint"> · updates as each round is written</span>
                </div>
                <div className="col gap-2">
                  {liveTurns.map((g) => (
                    <TurnCard key={g.turnId} group={g} defaultOpen />
                  ))}
                </div>
              </section>
            )}
            {settled.pending > 0 && (
              <button className="new-rows" onClick={settled.flush}>
                {plural(settled.pending, "new turn")} finished while you were reading — show
              </button>
            )}
            <div className="col gap-2">
              {settled.items.map((g, i) => (
                <TurnCard
                  key={g.turnId}
                  group={g}
                  defaultOpen={(i === 0 && !liveTurns.length) || watched.current.has(g.turnId)}
                />
              ))}
            </div>
            {turns.length > 0 && (
              <Panel padding="tight">
                <div className="row">
                  <span className="t-caption">
                    Showing the most recent phases the engine holds for this seat.
                  </span>
                  <span className="spacer" />
                  <Button size="sm" onClick={() => nav.to(["model"], { role: seat.name })}>
                    All model activity for {seat.name}
                  </Button>
                </div>
              </Panel>
            )}
          </>
        )}

        {shown === "threads" && (
          <>
            <PageNote>
              Every thread this seat holds a record in, and what it said there. The ledger is what
              stops it replying twice in one conversation — it is the engine&rsquo;s only account of
              what a seat said on a surface it does not own, and until now nothing read it.
            </PageNote>
            <QueryState error={threads.error} loading={threads.loading}>
              <div className="split">
                <Panel
                  title="Threads"
                  icon="message"
                  count={threads.data?.conversations?.length ?? 0}
                  padding="none"
                >
                  {threads.data?.conversations?.length ? (
                    <div className="list">
                      {threads.data.conversations.map((row) => (
                        <button
                          key={row.key}
                          type="button"
                          className={`thread-entry as-row${row.key === thread ? " selected" : ""}`}
                          onClick={() => setThread(row.key === thread ? "" : row.key)}
                        >
                          <span className="row gap-1">
                            <span className="mono truncate t-cell" style={{ flex: 1 }}>
                              {row.key}
                            </span>
                            <Badge outline>
                              {row.turns} {row.turns === 1 ? "turn" : "turns"}
                            </Badge>
                            <span className="t-caption faint">{fmtDateTime(row.last_at)}</span>
                          </span>
                        </button>
                      ))}
                    </div>
                  ) : (
                    <Empty
                      inline
                      icon="message"
                      title="No conversations recorded"
                      hint="A seat writes one entry per turn that took part in a thread — a chat message, an issue comment, a page discussion."
                    />
                  )}
                </Panel>

                <Panel
                  title={thread ? "In this thread" : "Pick a thread"}
                  icon="clock"
                  count={threads.data?.entries?.length ?? 0}
                  padding="none"
                >
                  {!thread ? (
                    <Empty
                      inline
                      icon="clock"
                      title="Nothing selected"
                      hint="Choose a thread to see the turns this seat recorded in it."
                    />
                  ) : threads.data?.entries?.length ? (
                    <div className="list">
                      {threads.data.entries.map((entry, i) => (
                        <ThreadTurn key={entry.turn_id || i} entry={entry} />
                      ))}
                    </div>
                  ) : (
                    <Empty
                      inline
                      icon="clock"
                      title="No turns in this thread"
                      hint="The ledger is trimmed per conversation, so an old thread can list a count it no longer carries the turns for."
                    />
                  )}
                </Panel>
              </div>
            </QueryState>
          </>
        )}

        {shown === "memory" && (
          <>
            {memory.loading && <Skeleton rows={5} />}
            <QueryState error={memory.error} loading={memory.loading}>
              <div className="col gap-4">
                <Panel
                  title="Private diary"
                  icon="book"
                  count={memory.data?.diary?.length ?? 0}
                  subtitle="what this seat chose to remember"
                  padding="none"
                >
                  {memory.data?.diary?.length ? (
                    <div className="list">
                      {memory.data.diary.map((d, i) => (
                        <div key={d.id ?? i} className="thread-entry">
                          <div className="row gap-1">
                            <Badge outline>{d.retention || d.scope || "note"}</Badge>
                            <span className="spacer" />
                            <span className="t-caption">{fmtDateTime(d.created_at)}</span>
                          </div>
                          <p className="t-body">{d.content}</p>
                        </div>
                      ))}
                    </div>
                  ) : (
                    <Empty
                      inline
                      icon="book"
                      title="Nothing written yet"
                      hint="A seat writes here by calling reflect_and_persist during a turn."
                    />
                  )}
                </Panel>

                <Panel
                  title="Past turns"
                  icon="layers"
                  count={memory.data?.episodes?.length ?? 0}
                  subtitle="one row per completed turn, searched by similarity at turn start"
                  padding="none"
                >
                  <DataTable
                    rows={memory.data?.episodes ?? []}
                    rowKey={(e) => e.id ?? e.turn_id ?? e.created_at}
                    defaultSort={{ key: "at", dir: "desc" }}
                    empty={{
                      title: "No episodes recorded",
                      hint: "An episode is written when a turn completes.",
                    }}
                    columns={[
                      {
                        key: "at",
                        header: "When",
                        shrink: true,
                        sortValue: (e) => e.created_at,
                        cell: (e) => <span className="t-caption">{fmtDateTime(e.created_at)}</span>,
                      },
                      {
                        key: "task",
                        header: "What it did",
                        cell: (e) => (
                          <span className="truncate">{e.task_summary || e.content || "—"}</span>
                        ),
                      },
                      {
                        key: "outcome",
                        header: "Outcome",
                        shrink: true,
                        sortValue: (e) => e.review_outcome ?? e.outcome ?? "",
                        cell: (e) =>
                          e.review_outcome || e.outcome ? (
                            <Badge
                              tone={
                                (e.review_outcome ?? e.outcome) === "done" ? "positive" : "caution"
                              }
                            >
                              {e.review_outcome ?? e.outcome}
                            </Badge>
                          ) : (
                            <span className="faint">—</span>
                          ),
                      },
                      {
                        key: "dur",
                        header: "Took",
                        align: "right",
                        shrink: true,
                        sortValue: (e) => e.duration_ms ?? 0,
                        cell: (e) => fmtDuration(e.duration_ms ?? null),
                      },
                      {
                        key: "conv",
                        header: "Conversation",
                        cell: (e) =>
                          e.conversation_key ? (
                            <span className="mono t-caption">{e.conversation_key}</span>
                          ) : (
                            <span className="faint">—</span>
                          ),
                      },
                    ]}
                  />
                </Panel>

                <Panel
                  title="Skills it taught itself"
                  icon="zap"
                  // `?? 0` for the ANSWER, never for the field: `skills_total`
                  // is always sent, so falling back to `skills.length` would
                  // only ever substitute the page size for the total.
                  count={memory.data?.skills_total ?? 0}
                  subtitle="drafted from its own past work, loadable mid-turn"
                  padding="none"
                >
                  {memory.data?.skills?.length ? (
                    <div className="list">
                      {memory.data.skills.map((s, i) => (
                        <div key={s.id ?? s.key ?? i} className="thread-entry">
                          <div className="row gap-1">
                            <strong className="t-body">{s.title}</strong>
                            {s.version != null && <Badge outline>v{s.version}</Badge>}
                            <span className="spacer" />
                            {s.updated_at && (
                              <span className="t-caption">{fmtDateTime(s.updated_at)}</span>
                            )}
                          </div>
                          {s.summary && <p className="t-caption">{s.summary}</p>}
                        </div>
                      ))}
                      {memory.data.skills_total > memory.data.skills.length && (
                        // THE CUT, SAID. The count above is the seat's whole
                        // set and this list is a page of it, so without a line
                        // here the two silently disagree.
                        <div className="thread-entry t-caption faint">
                          {memory.data.skills.length} of {memory.data.skills_total} shown
                        </div>
                      )}
                    </div>
                  ) : (
                    <Empty
                      inline
                      icon="zap"
                      title="No synthesised skills"
                      hint="The learning loop drafts these from repeated work. A young company has none."
                    />
                  )}
                </Panel>

                <Panel
                  title="Who it has worked with"
                  icon="users"
                  count={memory.data?.counterparties?.length ?? 0}
                  padding="none"
                >
                  {memory.data?.counterparties?.length ? (
                    <div className="list">
                      {memory.data.counterparties.map((c, i) => (
                        <CounterpartyRow key={`${counterpartyKey(c)}-${i}`} profile={c} />
                      ))}
                    </div>
                  ) : (
                    <Empty
                      inline
                      icon="users"
                      title="No counterparty profiles"
                      hint="Built up from observed interactions. Nothing read these until now — the key was on the answer and the store behind it was never asked."
                    />
                  )}
                </Panel>
              </div>
            </QueryState>
          </>
        )}

        {shown === "cost" && (
          <>
            <Panel padding="none">
              <StatRow cols={3}>
                <Stat
                  icon="coin"
                  label="Tokens · 7d"
                  value={spend.data ? fmtCount(spend.data.totals.total_tokens) : "—"}
                  sub={spend.data ? `${spend.data.totals.calls.toLocaleString()} model calls` : ""}
                />
                <Stat
                  icon="arrowRight"
                  label="Input / output"
                  value={
                    spend.data
                      ? `${fmtCount(spend.data.totals.input_tokens)} / ${fmtCount(spend.data.totals.output_tokens)}`
                      : "—"
                  }
                  sub="input includes any cached prefix, as the provider reports it"
                />
                <Stat
                  icon="target"
                  label="Configured budget"
                  value={seat.tokenBudget ? fmtCount(seat.tokenBudget) : "unlimited"}
                  sub={
                    seat.tokenBudget
                      ? "token_budget on this role in the company config"
                      : "token_budget is 0 or unset on this role"
                  }
                />
              </StatRow>
            </Panel>

            {/* The live meter and the configured budget are DIFFERENT facts and
              the screen says so. The previous seat page printed "no budget is
              set" in one tab while another printed the budget from the same
              config, because one read a field the server never sent. */}
            {agent?.budget ? (
              <Panel
                title="Live budget meter"
                icon="target"
                subtitle="process-lifetime, not the 7-day window"
              >
                <Meter
                  fullMeans="spent"
                  used={agent.budget.used}
                  max={agent.budget.max}
                  ariaLabel={`${agent.role}'s token budget`}
                  label="Used"
                  right={`${fmtCount(agent.budget.used)} / ${fmtCount(agent.budget.max)}`}
                  tone={agent.budget.used >= agent.budget.max ? "critical" : undefined}
                />
                {agent.budget.used >= agent.budget.max && (
                  <p className="t-caption" style={{ marginTop: "var(--space-2)" }}>
                    This seat&rsquo;s meter is at its cap, so its turns are being declined at the
                    gate.
                  </p>
                )}
              </Panel>
            ) : (
              <div className="banner neutral">
                <Icon name="info" size="sm" />
                <span>
                  {seat.tokenBudget
                    ? "This role has a token_budget in the config, but no engine is currently reporting a meter for it — so there is nothing measured to draw."
                    : "No per-seat budget meter. This role has no token_budget, so its spend is bounded only by the company-wide one."}
                </span>
              </div>
            )}

            {spend.loading && <Skeleton rows={4} />}
            <QueryState error={spend.error} loading={spend.loading}>
              <div className="grid grid-auto-lg">
                <Panel title="By phase" icon="layers">
                  <BarList
                    data={(spend.data?.by_phase ?? []).map((p) => ({
                      label: p.phase,
                      value: p.total_tokens,
                      display: fmtCount(p.total_tokens),
                      color: phaseColor(p.phase),
                      sub: `${p.calls} calls`,
                    }))}
                    emptyLabel="No calls in the window."
                  />
                </Panel>
                <Panel title="By model" icon="cpu">
                  <BarList
                    data={(spend.data?.by_model ?? []).map((m) => ({
                      label: m.model,
                      value: m.total_tokens,
                      display: fmtCount(m.total_tokens),
                      sub: `${m.calls} calls`,
                    }))}
                    emptyLabel="No calls in the window."
                  />
                </Panel>
              </div>

              <Panel title="Recent turns" icon="layers" padding="none">
                <DataTable
                  rows={spend.data?.by_turn ?? []}
                  rowKey={(t) => t.turn_id}
                  defaultSort={{ key: "started", dir: "desc" }}
                  onRowClick={(t) => nav.to(["turns", t.turn_id])}
                  empty={{ title: "No turns in the window" }}
                  columns={[
                    {
                      key: "started",
                      header: "Started",
                      shrink: true,
                      sortValue: (t) => t.started_at,
                      cell: (t) => <span className="t-caption">{fmtDateTime(t.started_at)}</span>,
                    },
                    {
                      key: "id",
                      header: "Turn",
                      cell: (t) => <code className="inline">{t.turn_id.slice(0, 8)}</code>,
                    },
                    {
                      key: "tokens",
                      header: "Tokens",
                      align: "right",
                      sortValue: (t) => t.total_tokens,
                      cell: (t) => fmtCount(t.total_tokens),
                    },
                    {
                      key: "calls",
                      header: "Calls",
                      align: "right",
                      sortValue: (t) => t.calls,
                      cell: (t) => t.calls,
                    },
                  ]}
                />
              </Panel>
            </QueryState>
          </>
        )}

        {shown === "access" && (
          <div className="col gap-4">
            <Panel title="Identity on other surfaces" icon="link">
              {Object.keys(seat.contact).length ? (
                <KeyValue
                  items={Object.entries(seat.contact).map(([k, v]) => [
                    k.replace(/_/g, " "),
                    <code key={k} className="inline">
                      {v}
                    </code>,
                  ])}
                />
              ) : (
                <Empty
                  inline
                  icon="link"
                  title="No contact identities"
                  hint="A human seat needs at least one so inbound activity can be attributed to them. An agent seat's identities are derived from its handle and email."
                />
              )}
            </Panel>

            <Panel
              title="Tool credentials"
              icon="key"
              subtitle="merged down the unit chain, this seat's own entries winning"
              count={Object.keys(seat.mcpEnv).length}
            >
              {Object.keys(seat.mcpEnv).length ? (
                <div className="col gap-3">
                  {Object.entries(seat.mcpEnv).map(([server, vars]) => (
                    <div key={server} className="col gap-1">
                      <div className="t-label">{server}</div>
                      <KeyValue
                        items={Object.entries(vars).map(([k, v]) => [
                          <code key={k} className="inline">
                            {k}
                          </code>,
                          // Values are `${VAR}` POINTERS in the config and are
                          // stored verbatim; the engine resolves them only where
                          // a transport is constructed. A literal here would be a
                          // secret in a config, which the API redacts server-side.
                          <code key={`${k}v`} className="inline">
                            {v}
                          </code>,
                        ])}
                      />
                    </div>
                  ))}
                  <p className="t-caption">
                    These are the <code className="inline">${"{VAR}"}</code> references the config
                    carries, not resolved values — the engine resolves them when it builds this
                    seat's MCP children, and the API redacts anything literal.
                  </p>
                </div>
              ) : (
                <Empty
                  inline
                  icon="key"
                  title="No per-seat tool credentials"
                  hint="This seat uses whatever the shared MCP servers were configured with."
                />
              )}
            </Panel>
          </div>
        )}
      </Tabs>
    </>
  );
}

/** A counterparty's stable identity, for a key and for a link.
 *
 *  THE NAME IS NOT IT. A profile's identity is the seat handle, or the
 *  platform and external id for somebody this company has not mapped —
 *  the display name is deliberately excluded, because a person renaming
 *  themselves on a chat surface must not look like a different colleague.
 */
export function counterpartyKey(profile: CounterpartyProfile): string {
  const { handle, platform, external_id } = profile.subject;
  return handle || `${platform ?? "?"}:${external_id ?? "?"}`;
}

/** One colleague this seat has learned about.
 *
 *  # The two instants are both here, and that is the point
 *
 *  `last_updated_at` moves on every interaction; `last_corroborated_at` only
 *  when the traits actually changed. A colleague seen daily whose profile has
 *  not moved in months is one this seat has STOPPED learning about, and the
 *  Plan phase's own prefetch demotes stale traits on exactly that gap — so a
 *  panel carrying one number would disagree with the prompt the agent reads.
 *
 *  # The traits are a bag, not a schema
 *
 *  The model invents the keys. Rendering them as a fixed set of fields would
 *  show whichever three this company happened to produce first and silently
 *  drop the rest, so they are listed as they come.
 */
export function CounterpartyRow({ profile }: { profile: CounterpartyProfile }) {
  const traits = Object.entries(profile.traits ?? {});
  // THE GAP IS DERIVED, not rendered as two dates a reader has to subtract.
  const stale =
    profile.last_corroborated_at &&
    profile.last_updated_at &&
    new Date(profile.last_updated_at).getTime() - new Date(profile.last_corroborated_at).getTime() >
      STALE_TRAIT_MS;
  return (
    <div className="thread-entry">
      <div className="row gap-1">
        {profile.subject.handle ? (
          <a className="t-cell" href={href(["company", "people", profile.subject.handle])}>
            <strong>{profile.subject.name || profile.subject.handle}</strong>
          </a>
        ) : (
          <strong className="t-cell">{profile.subject.name || counterpartyKey(profile)}</strong>
        )}
        {!profile.resolved && (
          <Badge outline title="not mapped to a seat in this company">
            {profile.subject.platform || "external"}
          </Badge>
        )}
        <span className="spacer" />
        <span className="t-caption">
          {profile.interactions} {profile.interactions === 1 ? "interaction" : "interactions"}
        </span>
        <span className="t-caption faint">{fmtDateTime(profile.last_updated_at)}</span>
      </div>
      {traits.length > 0 ? (
        <div className="row gap-1 wrap">
          {traits.map(([key, value]) => (
            <Badge key={key} outline title={key}>
              {key}: {typeof value === "string" ? value : JSON.stringify(value)}
            </Badge>
          ))}
        </div>
      ) : (
        <p className="t-caption faint">Seen, and nothing believed about them yet.</p>
      )}
      {stale && (
        <p className="t-caption faint">
          Last corroborated {fmtDateTime(profile.last_corroborated_at)} — this seat is still working
          with them and has stopped learning about them.
        </p>
      )}
    </div>
  );
}

/** How far `last_updated_at` may run ahead of `last_corroborated_at` before
 *  the profile is called stale.
 *
 *  THIRTY DAYS, which is the shortest inbox retention this engine allows and
 *  therefore the shortest span over which "still working together" is a fact
 *  the company still holds evidence for. Shorter and every colleague seen
 *  twice in a week reads as stale; longer and a profile nobody has corroborated
 *  since last quarter looks current. */
const STALE_TRAIT_MS = 30 * 24 * 60 * 60 * 1000;

/** One recorded turn in one conversation.
 *
 *  # `reply` and `unsent` are NOT the same field rendered twice
 *
 *  Both carry the turn's final artifact, and which one holds it is the whole
 *  record of whether anybody received it. A turn can end with real work done
 *  and no way to say so — the round budget ran out, the loop broke, the
 *  reviewer closed it — and a panel that rendered the two alike would show
 *  work announced to nobody as announced. That is not a cosmetic difference:
 *  it is the exact confusion that made a seat answer a follow-up against a
 *  message it had never sent.
 */
export function ThreadTurn({ entry }: { entry: ConversationEntry }) {
  return (
    <div className="thread-entry">
      <div className="row gap-1">
        {entry.trigger && <Badge outline>{entry.trigger}</Badge>}
        {entry.decision && <Badge outline>{entry.decision}</Badge>}
        <span className="spacer" />
        {entry.turn_id && (
          <a className="t-link mono t-caption" href={href(["activity", "turns", entry.turn_id])}>
            turn
          </a>
        )}
        <span className="t-caption faint">{entry.at ? fmtDateTime(entry.at) : ""}</span>
      </div>
      {entry.intent && <p className="t-body">{entry.intent}</p>}
      {entry.reply && <p className="t-caption">{entry.reply}</p>}
      {entry.unsent && (
        // THE ONE THAT REACHED NOBODY, marked. See the doc above.
        <div className="banner caution">
          <Icon name="alert" size="sm" />
          <span>
            <strong>Nothing was delivered.</strong> {entry.unsent}
          </span>
        </div>
      )}
      {entry.completed_work && <p className="t-caption faint">{entry.completed_work}</p>}
      {entry.tool_calls && <pre className="code">{entry.tool_calls}</pre>}
    </div>
  );
}
