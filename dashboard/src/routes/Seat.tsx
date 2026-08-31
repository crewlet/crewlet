/**
 * One seat: who it is, what it is doing, what it remembers, what it costs.
 *
 * Tabs are SECTIONS — they push a history entry, because the reader called
 * them — and the tab is in the URL so a colleague can be sent the exact view.
 */

import { useMemo } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
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
import { useAgents, useOrg, useSandboxes, useTokens } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { indexOrg, statusLine, afkReason, runState } from "~/lib/seats.ts";
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
  type PhaseRecord,
} from "~/lib/phases.ts";
import type { EventRecord } from "~/protocol/index.ts";

type Tab = "overview" | "model" | "memory" | "threads" | "cost" | "access";

const seatTurnKey = (g: { turnId: string }) => g.turnId;

export function SeatScreen({ handle }: { handle: string }) {
  const nav = useNavigator();
  const org = useOrg();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const tokens = useTokens();
  const now = useNow();
  const [tab, setTab] = useParam("tab", "overview", "section");

  const index = useMemo(() => indexOrg(org), [org]);
  const seat =
    index.byHandle.get(handle) ??
    index.byName.get(handle) ??
    [...index.byHandle.values()].find((s) => s.handle.toLowerCase() === handle.toLowerCase()) ??
    null;

  const agent = agents.find((a) => a.handle === handle || a.id === handle || a.role === seat?.name);
  const sandbox = sandboxes.find((s) => s.role === seat?.name) ?? null;

  // The seat's own phase history. Its `live` half is deliberately NOT read:
  // the projection already pushes it onto the roster, and reading it here too
  // would give one screen two sources for one fact.
  const history = useQuery(
    "agent",
    { id: handle },
    { enabled: tab === "overview" || tab === "model" },
  );
  const memory = useQuery("agent_memory", { id: handle }, { enabled: tab === "memory" });
  const threads = useQuery("conversations", { handle }, { enabled: tab === "threads" });
  const spend = useQuery(
    "tokens",
    { agent_role: seat?.name ?? "", since_days: 7, recent_turns: 50 },
    { enabled: tab === "cost" && !!seat },
  );

  const phases = useMemo<PhaseRecord[]>(() => {
    const stored = (history.data?.llm_history ?? [])
      .map((ev) => fromPhaseEvent(ev as EventRecord))
      .filter((r): r is PhaseRecord => r !== null);
    const live = agent?.live_call ? [fromLiveCall(agent.live_call, agent.role)] : [];
    return mergePhases(stored, live);
  }, [history.data, agent]);

  const turns = useMemo(() => groupTurns(phases), [phases]);
  const liveTurns = useMemo(() => turns.filter((g) => g.live), [turns]);
  const doneTurns = useMemo(() => turns.filter((g) => !g.live), [turns]);
  const settled = useSettled(doneTurns, seatTurnKey);

  if (!seat) {
    return (
      <>
        <ScreenHead title={handle} />
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
  const state = runState(agent, sandboxes);
  const seatSpend = tokens?.by_agent?.find((a) => a.role === seat.name);

  return (
    <>
      <ScreenHead
        title={
          <span className="row" style={{ gap: "var(--space-3)" }}>
            <Avatar name={seat.name} size="lg" human={human} />
            {seat.name}
          </span>
        }
        sub={seat.goal || statusLine(agent, { sandbox, seat })}
        badges={
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
        actions={
          <Button
            icon="activity"
            size="sm"
            onClick={() => nav.to(["activity"], { actor: seat.name })}
          >
            Its events
          </Button>
        }
      />

      {agent?.last_error && (
        <div className="banner critical">
          <Icon name="alert" size="sm" />
          <span>
            <strong>{agent.last_error.kind || "error"}</strong> — {agent.last_error.message}
            {agent.last_error.phase && ` (during ${agent.last_error.phase})`}
            {agent.last_error.at && ` · ${relTime(agent.last_error.at, now)}`}
          </span>
          {agent.last_error.event_id && (
            <a className="t-caption" href={href(["events", agent.last_error.event_id])}>
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
      {sandbox?.status === "awaiting_input" && (
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
        value={tab as Tab}
        onChange={setTab}
        options={[
          { value: "overview", label: "Overview", icon: "user" },
          { value: "model", label: "Model activity", icon: "brain" },
          { value: "memory", label: "Memory", icon: "database" },
          { value: "threads", label: "Conversations", icon: "message" },
          { value: "cost", label: "Cost", icon: "coin" },
          { value: "access", label: "Access", icon: "key" },
        ]}
      />

      {tab === "overview" && (
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
                  seatSpend ? `${seatSpend.calls.toLocaleString()} model calls` : "nothing recorded"
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
                  ["Model", seat.llm || <span className="faint">default provider</span>],
                  [
                    "Auxiliary model",
                    seat.llmAuxiliary || (
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
                    <ul className="col gap-1" style={{ paddingLeft: "var(--space-4)", margin: 0 }}>
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
                    <ul className="col gap-1" style={{ paddingLeft: "var(--space-4)", margin: 0 }}>
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
                    into this seat's Plan-phase prompt.
                  </span>
                )}
              </div>
            </Panel>
          </div>

          {reports.length > 0 && (
            <Section title="Direct reports" hint={`${reports.length}`}>
              <div className="seat-grid">
                {reports.map((r) => (
                  <a key={r.handle} className="seat-card" href={href(["seats", r.handle])}>
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

      {tab === "model" && (
        <>
          {history.loading && !turns.length && <Skeleton rows={4} height={44} />}
          <QueryState
            error={history.error}
            loading={history.loading}
            empty={
              turns.length
                ? undefined
                : {
                    title: "No phases in the record for this seat",
                    hint: "A phase is recorded when it completes. A seat that has not taken a turn has nothing here.",
                  }
            }
          >
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
                <TurnCard key={g.turnId} group={g} defaultOpen={i === 0 && !liveTurns.length} />
              ))}
            </div>
          </QueryState>
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

      {tab === "memory" && (
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
                subtitle="one row per completed turn, searched by similarity at plan time"
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
                          <a
                            className="mono t-caption"
                            href={href(["conversations"], { key: e.conversation_key })}
                          >
                            {e.conversation_key}
                          </a>
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
                count={memory.data?.skills?.length ?? 0}
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
                      <div key={`${c.subject}-${i}`} className="thread-entry">
                        <div className="row gap-1">
                          <strong className="t-cell">{c.subject}</strong>
                          <span className="spacer" />
                          <span className="t-caption">{fmtDateTime(c.updated_at)}</span>
                        </div>
                        <p className="t-caption">{c.summary}</p>
                      </div>
                    ))}
                  </div>
                ) : (
                  <Empty
                    inline
                    icon="users"
                    title="No counterparty profiles"
                    hint="Built up from observed interactions."
                  />
                )}
              </Panel>
            </div>
          </QueryState>
        </>
      )}

      {tab === "threads" && (
        <>
          {threads.loading && <Skeleton rows={4} />}
          {threads.data && threads.data.available === false ? (
            <div className="banner neutral">
              <Icon name="database" size="sm" />
              <span>
                This node holds no conversation ledger, so it cannot say which threads this seat is
                carrying. The ledger is written by the node that ran the turn.
              </span>
            </div>
          ) : (
            <QueryState
              error={threads.error}
              loading={threads.loading}
              empty={
                threads.data?.conversations?.length
                  ? undefined
                  : {
                      title: "No conversations recorded",
                      hint: "A conversation is recorded when this seat completes a turn that served one — a Slack thread, a Jira issue, a pull request.",
                    }
              }
            >
              <Panel padding="none">
                <DataTable
                  rows={threads.data?.conversations ?? []}
                  rowKey={(c) => c.key}
                  defaultSort={{ key: "last", dir: "desc" }}
                  onRowClick={(c) => nav.to(["conversations"], { handle, key: c.key })}
                  columns={[
                    {
                      key: "source",
                      header: "Surface",
                      shrink: true,
                      sortValue: (c) => splitConversationKey(c.key).source,
                      cell: (c) => (
                        <Badge outline>{splitConversationKey(c.key).source || "—"}</Badge>
                      ),
                    },
                    {
                      key: "key",
                      header: "Conversation",
                      sortValue: (c) => c.key,
                      cell: (c) => (
                        <code className="inline">{splitConversationKey(c.key).local}</code>
                      ),
                    },
                    {
                      key: "turns",
                      header: "Turns",
                      align: "right",
                      shrink: true,
                      sortValue: (c) => c.turns,
                      cell: (c) => c.turns,
                    },
                    {
                      key: "last",
                      header: "Last",
                      shrink: true,
                      sortValue: (c) => c.last_at,
                      cell: (c) => <span className="t-caption">{relTime(c.last_at, now)}</span>,
                    },
                  ]}
                />
              </Panel>
            </QueryState>
          )}
        </>
      )}

      {tab === "cost" && (
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
                used={agent.budget.used}
                max={agent.budget.max}
                label={agent.budget.refused_at ? "Refusing charges" : "Used"}
                right={`${fmtCount(agent.budget.used)} / ${fmtCount(agent.budget.max)}`}
                tone={agent.budget.refused_at ? "critical" : undefined}
              />
              {agent.budget.refused_at && (
                <p className="t-caption" style={{ marginTop: "var(--space-2)" }}>
                  Turns for this seat are being declined at the budget gate. Last refusal{" "}
                  {fmtDateTime(agent.budget.refused_at)}.
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

      {tab === "access" && (
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
                  carries, not resolved values — the engine resolves them when it builds this seat's
                  MCP children, and the API redacts anything literal.
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
    </>
  );
}
