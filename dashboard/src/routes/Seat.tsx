/**
 * One seat: who it is, what it is doing, what it remembers, what it costs.
 *
 * Tabs are SECTIONS — they push a history entry, because the reader called
 * them — and the tab is in the URL so a colleague can be sent the exact view.
 *
 * TWO SOURCES, AND THE PAGE SAYS WHICH IS WHICH. Who a seat is and where it
 * sits in the hierarchy come from the anonymous org projection, whose
 * reporting lines the engine derived. How it is configured (email, model,
 * token budget, schedules, contact identities, integrations and tool
 * credential names) is in the operator-gated company document, read through
 * the `config` query: the projection stopped carrying those fields because
 * they include identities and credential references an anonymous reader has
 * no business seeing. Without a token that half of the page is the guarded
 * banner rather than a blank, and a credential is never on the wire at all.
 */

import { useId, useMemo, useRef, type ReactNode } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip, Section, StateBadge } from "~/components/common.tsx";
import { TurnCard } from "~/components/TurnCard.tsx";
import { useSettled } from "~/lib/settled.ts";
import {
  Avatar,
  Badge,
  Button,
  KeyValue,
  Panel,
  Stat,
  StatRow,
  TabPanel,
  Tabs,
} from "~/ui/primitives.tsx";
import { BarList, phaseColor } from "~/ui/charts.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { useAgents, useOrg, usePhaseEvents, useSandboxes, useTokens } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import {
  indexOrg,
  seatPath,
  seatSettings,
  statusLine,
  afkReason,
  runState,
  type Seat,
  type SeatSettings,
} from "~/lib/seats.ts";
import {
  configValueKind,
  fmtCount,
  fmtDateTime,
  fmtDuration,
  formatPhaseLLM,
  humanize,
  plural,
  splitConversationKey,
} from "~/lib/format.ts";
import {
  fromLiveCall,
  fromPhaseEvent,
  groupTurns,
  mergePhases,
  streamedPhases,
  type PhaseRecord,
} from "~/lib/phases.ts";
import type { CompanyDocument, ConfigRole, EventRecord } from "~/protocol/index.ts";
import { EmptyState, Meter, RelativeTime, Skeleton, useNow } from "@crewlethq/ui";
import {
  AccountTreeGlyph,
  ArrowForwardGlyph,
  BoltGlyph,
  Book2Glyph,
  CableGlyph,
  CalendarClockGlyph,
  DatabaseGlyph,
  ErrorGlyph,
  GroupGlyph,
  HelpGlyph,
  InfoGlyph,
  KeyGlyph,
  LayersGlyph,
  LinkGlyph,
  ManufacturingGlyph,
  MemoryGlyph,
  NeurologyGlyph,
  PauseGlyph,
  PersonGlyph,
  TargetGlyph,
  TimelineGlyph,
  TokenGlyph,
} from "@crewlethq/icons/glyphs";

type Tab = "overview" | "model" | "memory" | "cost" | "access";

const seatTurnKey = (g: { turnId: string }) => g.turnId;

export function SeatScreen({ handle }: { handle: string }) {
  const nav = useNavigator();
  const org = useOrg();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const tokens = useTokens();
  const now = useNow();
  const [tab, setTab] = useParam("tab", "overview", "section");

  const phaseEvents = usePhaseEvents();
  const panel = useId();

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
  const spend = useQuery(
    "tokens",
    { agent_role: seat?.name ?? "", since_days: 7, recent_turns: 50 },
    { enabled: tab === "cost" && !!seat },
  );
  // The operator-gated half of the page. Only on the tabs that render it:
  // the whole company document is not something to ask for while a reader
  // watches a turn run.
  const config = useQuery("config", undefined, {
    enabled: !!seat && (tab === "overview" || tab === "cost" || tab === "access"),
  });
  // NOTHING FROM THE DOCUMENT BESIDE A REFUSAL. useQuery keeps its last good
  // answer through a failed ask, which suits a poll and is wrong for a guarded
  // read: once a token is cleared or refused, the email, model, budget and
  // schedules it had been allowed to read stayed on the overview and the cost
  // tab, next to a banner saying the answer needs a token.
  const settings = useMemo<SeatSettings | null>(
    () => (seat && config.data && !config.error ? seatSettings(config.data, seat) : null),
    [seat, config.data, config.error],
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
        <ScreenHead title={handle} />
        <EmptyState
          icon={<PersonGlyph />}
          title={`No seat called “${handle}”`}
          description="Seats are addressed by handle. If a company revision was just applied, this seat may have been renamed or removed."
          action={
            <Button variant="primary" onClick={() => nav.to(["people"])}>
              All seats
            </Button>
          }
        />
      </>
    );
  }

  const manager = seat.manager;
  const reports = seat.reports;
  const hierarchy = index.hierarchy;
  const human = seat.kind === "human";
  const configRole = settings?.state === "found" ? settings.role : null;
  const state = runState(agent, sandboxes);
  const seatSpend = tokens?.by_agent?.find((a) => a.role === seat.name);

  return (
    <>
      <ScreenHead
        title={
          <span className="row" style={{ gap: "var(--spacing-3)" }}>
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
          <>
            {/* Back into the chart with this seat revealed and ringed, which
                is where "who is around this seat" is answered. A seat whose
                handle this engine did not report cannot be named there. */}
            {seat.handle && (
              <Button
                icon={AccountTreeGlyph}
                size="sm"
                onClick={() => nav.to(["org"], { seat: seat.handle })}
              >
                In the org chart
              </Button>
            )}
            <Button
              icon={TimelineGlyph}
              size="sm"
              onClick={() => nav.to(["activity"], { actor: seat.name })}
            >
              Its events
            </Button>
          </>
        }
      />

      {agent?.last_error && (
        <div className="banner critical">
          <ErrorGlyph size="sm" />
          <span>
            <strong>{agent.last_error.kind || "error"}</strong> — {agent.last_error.message}
            {agent.last_error.phase && ` (during ${agent.last_error.phase})`}
            {agent.last_error.at && (
              <>
                {" · "}
                <RelativeTime value={agent.last_error.at} now={now} />
              </>
            )}
          </span>
          {agent.last_error.event_id && (
            <a className="t-link" href={href(["events", agent.last_error.event_id])}>
              event →
            </a>
          )}
        </div>
      )}
      {state === "afk" && (
        <div className="banner caution">
          <PauseGlyph size="sm" />
          <span>This seat is AFK: {afkReason(agent?.afk_reason)}.</span>
        </div>
      )}
      {sandbox?.status === "awaiting_input" && (
        <div className="banner caution">
          <HelpGlyph size="sm" />
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
        panelId={panel}
        value={tab as Tab}
        onChange={setTab}
        options={[
          { value: "overview", label: "Overview", icon: PersonGlyph },
          { value: "model", label: "Model activity", icon: NeurologyGlyph },
          { value: "memory", label: "Memory", icon: DatabaseGlyph },
          { value: "cost", label: "Cost", icon: TokenGlyph },
          { value: "access", label: "Access", icon: KeyGlyph },
        ]}
      />

      <TabPanel id={panel} value={tab}>
        {tab === "overview" && (
          <>
            <Panel padding="none">
              <StatRow cols={4}>
                <Stat
                  icon={BoltGlyph}
                  label="State"
                  value={human ? "human" : state}
                  sub={statusLine(agent, { sandbox, seat })}
                />
                <Stat
                  icon={TokenGlyph}
                  label="Tokens · 7d"
                  value={seatSpend ? fmtCount(seatSpend.total_tokens) : "—"}
                  sub={
                    seatSpend
                      ? `${seatSpend.calls.toLocaleString()} model calls`
                      : "nothing recorded"
                  }
                />
                <Stat
                  icon={LayersGlyph}
                  label="Turns in the record"
                  // Zero is a MEASUREMENT — this seat has taken no turns — and
                  // an em dash would claim nobody looked.
                  value={turns.length}
                  sub="the phase history loaded below"
                />
                <Stat
                  icon={GroupGlyph}
                  label="Direct reports"
                  value={hierarchy ? reports.length : "Not reported"}
                  sub={
                    !hierarchy
                      ? "this engine did not report the hierarchy"
                      : manager
                        ? `reports to ${manager.name}`
                        : "no manager in the chart"
                  }
                />
              </StatRow>
            </Panel>

            <div className="grid grid-auto-lg">
              <Panel title="Who this is" icon={PersonGlyph}>
                <KeyValue
                  items={[
                    ["Role", seat.name],
                    [
                      "Handle",
                      seat.handle ? (
                        <code key="h" className="inline">
                          @{seat.handle}
                        </code>
                      ) : (
                        <span className="faint">not reported by this engine</span>
                      ),
                    ],
                    ["Kind", human ? "Human teammate, never run by the engine" : "Agent seat"],
                    ["Goal", seat.goal || <span className="faint">not set</span>],
                    [
                      "Unit",
                      seat.unitChain.length ? (
                        <span key="u" className="col" style={{ gap: 2 }}>
                          <span>
                            {seat.unitChain.map((u, i) => (
                              <span key={u.key}>
                                {i > 0 && " › "}
                                <a className="t-link" href={href(["org"], { unit: u.name })}>
                                  {u.name}
                                </a>
                              </span>
                            ))}
                          </span>
                          {seat.placedByRef && (
                            <span className="t-caption">
                              Placed by its <code className="inline">unit</code> reference
                            </span>
                          )}
                        </span>
                      ) : (
                        <span className="faint">org-wide</span>
                      ),
                    ],
                    [
                      "Unit lead",
                      !seat.unit ? (
                        <span className="faint">none</span>
                      ) : seat.unit.lead ? (
                        <span key="l" className="row gap-1">
                          <SeatChip name={seat.unit.lead.name} handle={seat.unit.lead.handle} />
                          {seat.unit.leadInherited && (
                            <span className="t-caption">inherited from a parent unit</span>
                          )}
                        </span>
                      ) : hierarchy ? (
                        <span className="faint">none</span>
                      ) : (
                        <span className="faint">not reported by this engine</span>
                      ),
                    ],
                    [
                      "Reports to",
                      manager ? (
                        <SeatChip name={manager.name} handle={manager.handle} />
                      ) : hierarchy ? (
                        <span className="faint">nobody</span>
                      ) : (
                        <span className="faint">not reported by this engine</span>
                      ),
                    ],
                  ]}
                />
              </Panel>

              <Panel
                title="Configuration"
                icon={ManufacturingGlyph}
                subtitle="from the company document"
              >
                <SettingsState
                  error={config.error}
                  loading={config.loading}
                  doc={config.data}
                  settings={settings}
                  seat={seat}
                >
                  {configRole && (
                    <KeyValue
                      items={[
                        ["Email", configRole.email || <span className="faint">not set</span>],
                        ["Model", <Model key="m" llm={configRole.llm} />],
                        ...phaseOverrides(configRole),
                        [
                          "Token budget",
                          configRole.token_budget ? fmtCount(configRole.token_budget) : "Unlimited",
                        ],
                      ]}
                    />
                  )}
                </SettingsState>
              </Panel>

              <Panel title="Profile" icon={Book2Glyph}>
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
                        style={{ paddingLeft: "var(--spacing-4)", margin: 0 }}
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
                        style={{ paddingLeft: "var(--spacing-4)", margin: 0 }}
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
                    <a key={r.key} className="seat-card" href={href(seatPath(r))}>
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

            {(configRole?.schedules?.length ?? 0) > 0 && (
              <Panel
                title="Recurring work"
                icon={CalendarClockGlyph}
                count={configRole?.schedules?.length ?? 0}
                padding="none"
              >
                <DataTable
                  rows={configRole?.schedules ?? []}
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
            {history.loading && !turns.length && (
              <Skeleton label="Loading this seat's turns" variant="text" rows={4} rowHeight={44} />
            )}
            {/* The QUERY'S OWN STATE, BESIDE THE TURNS RATHER THAN IN PLACE OF
              THEM. It used to wrap them, and `QueryState` renders NOTHING while
              a query is in flight and a banner INSTEAD of its children when one
              fails — so the turn happening right now was hidden until the event
              store answered, and hidden for good on a node that keeps no event
              log at all. Only the settled half of this screen comes from that
              query; the running half is pushed. */}
            {history.error && <QueryState error={history.error} loading={history.loading} />}
            {!history.loading && !history.error && !turns.length && (
              <EmptyState
                size="compact"
                icon={<NeurologyGlyph />}
                title="No phases in the record for this seat"
                description="A phase is recorded when it completes. A seat that has not taken a turn has nothing here."
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

        {tab === "memory" && (
          <>
            {memory.loading && (
              <Skeleton label="Loading this seat's memory" variant="text" rows={5} />
            )}
            <QueryState error={memory.error} loading={memory.loading}>
              <div className="col gap-4">
                <Panel
                  title="Private diary"
                  icon={Book2Glyph}
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
                    <EmptyState
                      size="compact"
                      icon={<Book2Glyph />}
                      title="Nothing written yet"
                      description="A seat writes here by calling reflect_and_persist during a turn."
                    />
                  )}
                </Panel>

                <Panel
                  title="Past turns"
                  icon={LayersGlyph}
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
                  icon={BoltGlyph}
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
                    <EmptyState
                      size="compact"
                      icon={<BoltGlyph />}
                      title="No synthesised skills"
                      description="The learning loop drafts these from repeated work. A young company has none."
                    />
                  )}
                </Panel>

                <Panel
                  title="Who it has worked with"
                  icon={GroupGlyph}
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
                    <EmptyState
                      size="compact"
                      icon={<GroupGlyph />}
                      title="No counterparty profiles"
                      description="Built up from observed interactions."
                    />
                  )}
                </Panel>
              </div>
            </QueryState>
          </>
        )}

        {tab === "cost" && (
          <>
            <Panel padding="none">
              <StatRow cols={3}>
                <Stat
                  icon={TokenGlyph}
                  label="Tokens · 7d"
                  value={spend.data ? fmtCount(spend.data.totals.total_tokens) : "—"}
                  sub={spend.data ? `${spend.data.totals.calls.toLocaleString()} model calls` : ""}
                />
                <Stat
                  icon={ArrowForwardGlyph}
                  label="Input / output"
                  value={
                    spend.data
                      ? `${fmtCount(spend.data.totals.input_tokens)} / ${fmtCount(spend.data.totals.output_tokens)}`
                      : "—"
                  }
                  sub="input includes any cached prefix, as the provider reports it"
                />
                <Stat
                  icon={TargetGlyph}
                  label="Configured budget"
                  value={
                    configRole
                      ? configRole.token_budget
                        ? fmtCount(configRole.token_budget)
                        : "unlimited"
                      : "Unknown"
                  }
                  sub={
                    configRole
                      ? configRole.token_budget
                        ? "token_budget on this role in the company config"
                        : "token_budget is 0 or unset on this role"
                      : config.error === "unauthorized"
                        ? "reading the company config needs an operator token"
                        : "the company config does not say for this seat"
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
                icon={TargetGlyph}
                subtitle="process-lifetime, not the 7-day window"
              >
                <Meter
                  value={agent.budget.used}
                  max={agent.budget.max}
                  label={agent.budget.refused_at ? "Refusing charges" : "Used"}
                  valueText={`${fmtCount(agent.budget.used)} / ${fmtCount(agent.budget.max)}`}
                  tone={agent.budget.refused_at ? "danger" : undefined}
                />
                {agent.budget.refused_at && (
                  <p className="t-caption" style={{ marginTop: "var(--spacing-2)" }}>
                    Turns for this seat are being declined at the budget gate. Last refusal{" "}
                    {fmtDateTime(agent.budget.refused_at)}.
                  </p>
                )}
              </Panel>
            ) : (
              <div className="banner neutral">
                <InfoGlyph size="sm" />
                <span>
                  {!configRole
                    ? "No engine is reporting a budget meter for this seat, so there is nothing measured to draw."
                    : configRole.token_budget
                      ? "This role has a token_budget in the config, but no engine is currently reporting a meter for it, so there is nothing measured to draw."
                      : "No per-seat budget meter. This role has no token_budget, so its spend is bounded only by the company-wide one."}
                </span>
              </div>
            )}

            {spend.loading && (
              <Skeleton label="Loading this seat's spend" variant="text" rows={4} />
            )}
            <QueryState error={spend.error} loading={spend.loading}>
              <div className="grid grid-auto-lg">
                <Panel title="By phase" icon={LayersGlyph}>
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
                <Panel title="By model" icon={MemoryGlyph}>
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

              <Panel title="Recent turns" icon={LayersGlyph} padding="none">
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
          <SettingsState
            error={config.error}
            loading={config.loading}
            doc={config.data}
            settings={settings}
            seat={seat}
          >
            {configRole && settings?.state === "found" && (
              <div className="col gap-4">
                <Panel title="Identity on other surfaces" icon={LinkGlyph}>
                  {Object.keys(configRole.contact ?? {}).length ? (
                    <KeyValue
                      items={Object.entries(configRole.contact ?? {}).map(([k, v]) => [
                        humanize(k),
                        <ConfigValue key={k} value={v} />,
                      ])}
                    />
                  ) : (
                    <EmptyState
                      size="compact"
                      icon={<LinkGlyph />}
                      title="No contact identities"
                      description="A human seat needs at least one so inbound activity can be attributed to them. An agent seat's identities are derived from its handle and email."
                    />
                  )}
                </Panel>

                <Panel title="Integrations" icon={CableGlyph} subtitle="this seat's own settings">
                  <SeatIntegrations role={configRole} />
                </Panel>

                <Panel
                  title="Tool credentials"
                  icon={KeyGlyph}
                  subtitle="names only: a credential value never reaches this page"
                >
                  <ToolCredentials seat={seat} role={configRole} unit={settings.unit} />
                </Panel>
              </div>
            )}
          </SettingsState>
        )}
      </TabPanel>
    </>
  );
}

/**
 * The operator-gated half of a seat, said precisely when it cannot be shown.
 *
 * `QueryState` covers a refused or failed read, which includes the guarded
 * banner with its Set token button. The three states after it are this
 * page's own: no configuration is active, the document has no seat by this
 * name (the projection and the document can disagree for a moment either
 * side of an apply), and a name held by two seats in a revision stored before
 * names had to be unique.
 */
function SettingsState({
  error,
  loading,
  doc,
  settings,
  seat,
  children,
}: {
  error: string | null;
  loading: boolean;
  doc: CompanyDocument | null;
  settings: SeatSettings | null;
  seat: Seat;
  children: ReactNode;
}) {
  if (loading && !doc && !error)
    return <Skeleton label="Loading the document" variant="text" rows={3} />;
  if (error) return <QueryState error={error} loading={loading} />;
  if (!doc) {
    return (
      <EmptyState
        size="compact"
        icon={<ManufacturingGlyph />}
        title="No company configuration is active"
        description="This seat's settings live in the company document, and none is active on this engine."
      />
    );
  }
  if (settings?.state === "missing") {
    return (
      <EmptyState
        size="compact"
        icon={<ManufacturingGlyph />}
        title={`The active configuration has no seat named ${seat.name}`}
        description="The org chart and the configuration can disagree for a moment while a new revision is applied."
      />
    );
  }
  if (settings?.state === "ambiguous") {
    return (
      <EmptyState
        size="compact"
        icon={<ManufacturingGlyph />}
        title={`More than one seat is named ${seat.name}`}
        description="This revision was stored before seat names had to be unique, so its settings cannot be attributed to one of them. Rename one of the seats to fix it."
      />
    );
  }
  return <>{children}</>;
}

/**
 * A value from the redacted document, in the form it may be shown.
 *
 * NEVER A CREDENTIAL: a literal in a credential field arrives as the mask and
 * says only that something is set, and a whole `${VAR}` names an entry in the
 * secret store. `secret` marks a credential field, where anything that is not
 * one whole reference is hidden here too, whatever the engine sent: see
 * [configValueKind]. A plain literal is shown only in a field that is not a
 * credential, such as a contact identity.
 */
function ConfigValue({ value, secret = false }: { value: string | undefined; secret?: boolean }) {
  switch (configValueKind(value, { secret })) {
    case "hidden":
      return <span className="t-caption">A literal value is set (hidden)</span>;
    case "reference":
      return <code className="inline is-reference">{value}</code>;
    case "literal":
      return <code className="inline">{value}</code>;
    default:
      return <span className="faint">not set</span>;
  }
}

/** A seat's `llm:` field, as a chain or as one chain per phase. */
function Model({ llm }: { llm: unknown }) {
  const rows = formatPhaseLLM(llm);
  if (!rows.length) return <span className="faint">the company default provider</span>;
  const only = rows[0];
  if (rows.length === 1 && only && only.phase === "")
    return <code className="inline">{only.chain}</code>;
  return (
    <span className="col" style={{ gap: 2 }}>
      {rows.map((row) => (
        <span key={row.phase}>
          {humanize(row.phase)}: <code className="inline">{row.chain}</code>
        </span>
      ))}
    </span>
  );
}

/**
 * The flat `llm_<phase>` fields a seat sets, each of which wins over `llm:`
 * for its phase. Listed as written rather than resolved against `llm:`, since
 * which chain a phase finally runs on is the engine's decision.
 */
function phaseOverrides(role: ConfigRole): [ReactNode, ReactNode][] {
  const fields = [
    "llm_review",
    "llm_subagent",
    "llm_auxiliary",
    "llm_judge",
    "llm_sandbox",
  ] as const;
  return fields.flatMap((field): [ReactNode, ReactNode][] => {
    const rows = formatPhaseLLM(role[field]);
    if (!rows.length) return [];
    return [
      [
        <code key={field} className="inline">
          {field}
        </code>,
        <code key={`${field}-v`} className="inline">
          {rows.map((r) => r.chain).join("; ")}
        </code>,
      ],
    ];
  });
}

/** The integration blocks written on this seat, and nothing it does not have. */
function SeatIntegrations({ role }: { role: ConfigRole }) {
  const i = role.integrations ?? {};
  const text = (value: string | undefined) =>
    value ? <code className="inline">{value}</code> : <span className="faint">not set</span>;
  const items: [ReactNode, ReactNode][] = [];
  if (i.github) {
    items.push(
      ["GitHub tier", text(i.github.tier)],
      [
        "GitHub repositories",
        i.github.repos?.length ? (
          <span className="col" style={{ gap: 2 }}>
            {i.github.repos.map((repo) => (
              <code key={repo} className="inline">
                {repo}
              </code>
            ))}
          </span>
        ) : (
          <span className="faint">every repository the installation covers</span>
        ),
      ],
      ["GitHub App", text(i.github.app_slug)],
      ["GitHub App key", <ConfigValue key="gk" secret value={i.github.private_key} />],
      ["GitHub webhook secret", <ConfigValue key="gw" secret value={i.github.webhook_secret} />],
    );
  }
  if (i.slack) {
    items.push(
      ["Slack channel", text(i.slack.channel)],
      ["Slack bot token", <ConfigValue key="sb" secret value={i.slack.bot_token} />],
      ["Slack signing secret", <ConfigValue key="ss" secret value={i.slack.signing_secret} />],
    );
  }
  if (i.mattermost) {
    items.push(
      ["Mattermost username", text(i.mattermost.username)],
      ["Mattermost channel", text(i.mattermost.channel)],
      ["Mattermost bot token", <ConfigValue key="mb" secret value={i.mattermost.bot_token} />],
    );
  }
  if (i.jira) items.push(["Owns Jira project", text(i.jira.project)]);
  if (i.confluence) items.push(["Owns Confluence space", text(i.confluence.space)]);
  if (!items.length) {
    return (
      <EmptyState
        size="compact"
        icon={<CableGlyph />}
        title="No per-seat integration settings"
        description="This seat uses the company's integrations as they are configured on the Integrations screen."
      />
    );
  }
  return <KeyValue items={items} />;
}

/**
 * The tool credentials a seat names, and the ones its home unit gives it.
 *
 * NOT MERGED HERE. The unit's entries reach its direct agent members with a
 * seat's own entries winning, and that is the engine's rule to apply: this
 * lists both as the document writes them, each under where it is written.
 */
function ToolCredentials({
  seat,
  role,
  unit,
}: {
  seat: Seat;
  role: ConfigRole;
  unit: { name: string; mcp_env?: Record<string, Record<string, string>> } | null;
}) {
  const own = Object.entries(role.mcp_env ?? {});
  const inherited = seat.kind === "agent" && unit ? Object.entries(unit.mcp_env ?? {}) : [];
  const group = (entries: [string, Record<string, string>][]) => (
    <div className="col gap-3">
      {entries.map(([server, vars]) => (
        <div key={server} className="col gap-1">
          <div className="t-label">{server}</div>
          <KeyValue
            items={Object.entries(vars).map(([name, value]) => [
              <code key={name} className="inline">
                {name}
              </code>,
              <ConfigValue key={`${name}-v`} secret value={value} />,
            ])}
          />
        </div>
      ))}
    </div>
  );
  if (!own.length && !inherited.length) {
    return (
      <EmptyState
        size="compact"
        icon={<KeyGlyph />}
        title="No per-seat tool credentials"
        description="This seat uses whatever the shared MCP servers were configured with."
      />
    );
  }
  return (
    <div className="col gap-4">
      {own.length > 0 && (
        <div className="col gap-2">
          <span className="t-caption">Set on this seat</span>
          {group(own)}
        </div>
      )}
      {inherited.length > 0 && (
        <div className="col gap-2">
          <span className="t-caption">
            Set on its unit, {unit?.name}, for every agent seat directly in it. An entry this seat
            sets itself wins.
          </span>
          {group(inherited)}
        </div>
      )}
      <p className="t-caption">
        A reference names an entry in the secret store and is resolved only when the engine starts
        this seat's tool servers.
      </p>
    </div>
  );
}
