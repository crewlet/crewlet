/**
 * Live › Now running: what the company is doing this minute, and the phases
 * it just finished.
 *
 * FIVE CARDS, ALWAYS IN THE SAME PLACE, whether or not they hold anything —
 * an empty one says so — because a screen whose sections come and go with the
 * data cannot be read at a glance, which is the only way this one is read:
 *
 *  - RUNNING TURNS: one `LiveTurnRow` per seat the engine says is working —
 *    the row Home draws, with where the turn is, how long it has run, the call
 *    it is making and whether it has stopped moving;
 *  - WAITING ON A PERSON: every executor parked on a question, with who it
 *    asks, how long it has waited and how long its box is still held, and an
 *    Answer that sends the reply by the executor's turn;
 *  - IN A BOX: the rest of the executors in flight;
 *  - ACTIVITY: the engine's own count of events over the window, and the
 *    latest few;
 *  - RECENT PHASES: the settled model calls, paged (`RecentPhases.tsx`).
 *
 * THE FIRST THREE ARE THE OVERVIEW and every signed-in reader is sent them —
 * a running turn's STATUS, and an executor's, its body drawn as not shown
 * where the engine withheld it. THE LAST TWO ARE THE COMPANY-WIDE TRAIL,
 * Admin's: for anybody else they are drawn locked and asked nothing, because
 * a feed the engine never pushes them reads as a quiet company.
 *
 * The spend panels and the onboarding cards that used to sit here went: the
 * spend is Spend's, on the replicated usage domain, and the onboarding cards
 * pointed at screens the sidebar already names.
 *
 * # The two conditions whose home is here
 *
 * `lib/attention.ts` gives Live two subjects — a ROUND that has stopped moving
 * and an EXECUTOR parked on a question. Neither is a card of its own any
 * more: a quiet round is marked on its own running row, and a parked executor
 * is a row of Waiting on a person, each beside what it is about rather than in a
 * list of alarms that restated the rows around it.
 *
 * # One set of filters for the whole screen
 *
 * `seat=` (a handle), `phase=` and `failed=` narrow every card that can honour
 * them, and the window (`15m`, `1h`, `6h`) is the activity strip's. The seat
 * is the seat's HANDLE: the engine resolves it to the id every node derives,
 * so two unit seats sharing a role name are two filters.
 */

import { useMemo } from "react";
import {
  ActivityStrip,
  Button,
  Card,
  EmptyState,
  FilterChip,
  FilterChipGroup,
  Select,
  Skeleton,
  peekClickHandler,
  TimeRangePicker,
} from "@crewlethq/ui";
import {
  BrainGlyph,
  ChartNoAxesGanttGlyph,
  CircleQuestionMarkGlyph,
  ClockGlyph,
  SquareTerminalGlyph,
  XGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useAssistantContext } from "~/app/assistant/useAssistantContext.ts";
import { choiceKey, urlAction, windowKey } from "~/app/assistant/urlAction.ts";
import { EventRow, QueryState } from "~/components/common.tsx";
import { CoverageNote } from "~/components/CoverageNote.tsx";
import { DecisionRow } from "~/components/DecisionRow.tsx";
import { LiveDot, LiveTurnRow } from "~/components/LiveTurnRow.tsx";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { mergeRuns, RunStatus, runTitle } from "./Executors.tsx";
import { RecentPhases } from "./RecentPhases.tsx";
import { useAgents, useEngineHealth, useEvents, useOrg, useExecutors } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { ADMIN } from "~/lib/access.ts";
import { GrantRequired } from "~/app/frame/GrantRequired.tsx";
import { awaitingPerson, indexOrg, workingLongestFirst } from "~/lib/seats.ts";
import { phaseKey, type PhaseRecord } from "~/lib/phases.ts";
import { plural, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { cutInto, spanOf, useTimeRange, windowLabel } from "~/lib/range.ts";
import type { Offer } from "~/lib/range.ts";
import { rangePickerProps } from "~/lib/rangePicker.ts";
import type { AgentRow, EventSeries, ExecutorRow } from "~/protocol/index.ts";

/**
 * WHICH WINDOWS THE STRIP HAS: the short end of the shared vocabulary, since
 * this screen is about now and the event log owns every longer question.
 *
 * MINUTES AT EVERY WIDTH, asked of the engine and then summed into the
 * strip's cells: six hours is 360 minute bars, well inside the engine's cap,
 * and a sum of whole bars into a cell is exact where re-bucketing hours into
 * a sixty-cell strip would not be.
 */
const STRIP_OFFER: Offer = {
  ranges: ["15m", "1h", "6h"],
  custom: false,
  fallback: "1h",
  buckets: ["minute"],
};

/**
 * The most cells the strip is ever cut into: sixty is an hour of minute
 * cells, and a wider window widens the cell rather than adding slivers.
 */
const STRIP_CELLS = 60;

/**
 * How many events the Activity card lists under its strip: seven rows is the
 * height of the In a box card beside it at eight, so the pair ends level.
 */
export const LATEST_EVENTS = 7;

/**
 * How many executors In a box draws before it says how many more.
 *
 * EIGHT: a row is `--size-row-md` tall, so eight is a screenful rather than a
 * scroll. The set is not bounded by the seat count the way the running turns
 * are — a seat can run several boxes — so the full list is one click away and
 * the card says how much of it is not here.
 */
const IN_BOX_ROWS = 8;

/** How often the executors' durable rows are read again. */
const RUNS_POLL_MS = 30_000;

/** How often a seat's own latest events are read again, when one is chosen. */
const SEAT_EVENTS_POLL_MS = 15_000;

/**
 * The phases the engine emits, the turn's own two first.
 *
 * NOT `auxiliary`: no phase record carries it. The auxiliary model's spend is
 * an `auxiliary_spend` record, which is no phase and runs nothing live, so a
 * filter offering it matched no row and read as a quiet model. Nor is a launch
 * of the executor a phase of its own: it is its round's work, filed and
 * filtered as `execute`.
 */
const PHASES = ["execute", "review", "onboarding", "subagent", "judge"] as const;

/**
 * The phase a running turn is in, in the vocabulary the phase filter uses. A
 * PARKED turn is in its executor's round — the executor, out in its box, is
 * that round's work — so the filter that keeps `execute` keeps it.
 */
function runningPhase(row: AgentRow): string {
  if (row.turn?.stage === "parked") return "execute";
  return row.live_call?.phase ?? row.current_phase ?? "context";
}

export function LiveNow() {
  const now = useNow();
  const agents = useAgents();
  const executors = useExecutors();
  const pushed = useEvents();
  const engine = useEngineHealth();
  const org = useOrg();
  const viewer = useViewer();
  const index = useMemo(() => indexOrg(org), [org]);
  const { open: openPeek } = usePeekControls();

  // EVERY FILTER IS A FILTER, so it replaces the history entry.
  const [seat, setSeat] = useParam("seat", "", "filter");
  const [phase, setPhase] = useParam("phase", "", "filter");
  const [failed, setFailed] = useParam("failed", "", "filter");
  // NOT ALIGNED to a bucket: the strip's newest cell is the minute in progress.
  const range = useTimeRange(now, STRIP_OFFER, false);

  const seatRow = agents.find((a) => a.handle === seat);
  const agentSeats = useMemo(() => index.seats.filter((s) => s.kind === "agent"), [index.seats]);
  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;

  // --- assistant page actions (WP12) ---
  // THE CONTROLS' OWN VOCABULARY: a seat from the menu's agent seats, a phase
  // from its picker, the one failures switch, and the window from this
  // screen's offer.
  const nav = useNavigator();
  useAssistantContext({
    actions: urlAction("live.filter.apply", {
      scope: "Live filters and activity window (view only)",
      keys: {
        window: windowKey(STRIP_OFFER, () => now),
        seat: choiceKey(
          "Seat",
          agentSeats.map((s) => ({ value: s.handle, label: s.name })),
          "filter",
        ),
        phase: choiceKey(
          "Phase",
          PHASES.map((p) => ({ value: p, label: p })),
          "filter",
        ),
        failed: choiceKey("Failures", [{ value: "true", label: "Failed phases" }], "filter"),
      },
      nav,
    }),
  });
  // --- end assistant page actions (WP12) ---

  // --- running turns -----------------------------------------------------
  const running = useMemo(
    () =>
      workingLongestFirst(agents).filter(
        (a) => (!seat || a.handle === seat) && (!phase || runningPhase(a) === phase),
      ),
    [agents, seat, phase],
  );
  // THE PHASES THE RUNNING ROWS ARE DRAWING, so a phase that completes lands
  // in Recent phases at once instead of behind the "new rows" button.
  const runningKeys = useMemo(
    () =>
      agents.flatMap((a) =>
        a.live_call
          ? [phaseKey(a.live_call.turn_id, a.live_call.phase, a.live_call.iteration)]
          : [],
      ),
    [agents],
  );

  // --- executors ---------------------------------------------------------
  // THE DURABLE ROWS AND THE PUSH, folded by the Executors screen's own function:
  // the push is a reconcile behind a box that came up two seconds ago, the
  // store a poll behind it, and neither alone is "right now".
  const runs = useQuery("executors", undefined, { pollMs: RUNS_POLL_MS });
  const inFlight = useMemo(
    () =>
      mergeRuns(runs.data?.runs ?? [], executors).filter((r) => !seat || r.agent_handle === seat),
    [runs.data, executors, seat],
  );
  const waiting = inFlight.filter((r) => awaitingPerson(r.status));
  const boxed = inFlight.filter((r) => !awaitingPerson(r.status));

  // --- activity ----------------------------------------------------------
  const { cell, cells } = cutInto(spanOf(range.window), STRIP_CELLS);
  // THE FEED'S ROWS, on the strip and on a seat's latest events alike: the
  // unselected card is the live ring, which holds no accounting row, so a
  // strip that counted them — or a seat's card listing its spend records,
  // several after every turn — would be drawing a different stream.
  //
  // ADMIN'S, the company-wide trail — see the file's doc: asked of nobody
  // else.
  const series = useQuery(
    "event_series",
    {
      since: range.since,
      until: range.until,
      bucket: "minute",
      feed_only: "true",
      ...(seat ? { seat } : {}),
    },
    { pollMs: 30_000, enabled: viewer.admin },
  );
  const strip = useMemo(
    () => stripOf(series.data, now, cell, cells),
    [series.data, now, cell, cells],
  );
  // A SEAT'S OWN LATEST EVENTS come from the engine, narrowed by its id: the
  // push names no seat on an event, so filtering it here would be a guess.
  const seatEvents = useQuery(
    "events",
    { seat, limit: LATEST_EVENTS, feed_only: "true" },
    { enabled: viewer.admin && seat !== "", pollMs: SEAT_EVENTS_POLL_MS },
  );
  const latest = seat ? (seatEvents.data?.events ?? []) : pushed.slice(0, LATEST_EVENTS);

  // A recorded phase names its seat by id and by role; the chart names it.
  const nameOfPhase = useMemo(() => {
    const byId = new Map<string, string>();
    for (const a of agents) {
      if (a.agent_id && a.handle)
        byId.set(a.agent_id, index.byHandle.get(a.handle)?.name ?? a.role);
    }
    return (r: PhaseRecord) => byId.get(r.agentId) ?? r.role;
  }, [agents, index]);

  const filtering = !!(seat || phase || failed === "true");
  const eventLog = href(["live", "events"], seat ? { seat } : undefined);

  return (
    <>
      {/* THE WINDOW ALONE. The shell's header already carries how many seats
          are working, on every screen; a second count here drew the same
          number twice side by side and pushed the shell's chip off a phone's
          page bar. */}
      <PageActions>
        <TimeRangePicker {...rangePickerProps(range, "Activity window")} />
      </PageActions>

      <div className="live-now">
        <div className="toolbar live-filters">
          <Select
            width="auto"
            value={seat}
            onChange={(value) => setSeat(String(value))}
            options={[
              { value: "", label: "Every seat" },
              ...agentSeats.map((s) => ({ value: s.handle, label: s.name })),
            ]}
            ariaLabel="Seat"
            menuClassName="seat-filter-menu"
            placeholder="Every seat"
            active={seat !== ""}
          />
          {/* A PICKER, like the seat beside it: six phase chips wrapped to
              three rows on a phone, and this toolbar is sticky. */}
          <Select
            width="auto"
            value={phase}
            onChange={(value) => setPhase(String(value))}
            options={[
              { value: "", label: "Every phase" },
              ...PHASES.map((p) => ({ value: p, label: p })),
            ]}
            ariaLabel="Phase"
            placeholder="Every phase"
            active={phase !== ""}
          />
          <FilterChipGroup
            label="Failures"
            hideLabel
            semantics="radio"
            allowNone
            // `failed=true`, THE SPELLING Turns uses for the same filter, so
            // an address carried from one Live screen to the other means the
            // same thing on both.
            value={failed === "true" ? "true" : ""}
            onValueChange={(next) => setFailed(next ? "true" : "")}
          >
            <FilterChip value="true">failed phases</FilterChip>
          </FilterChipGroup>
          {filtering && (
            <Button
              size="small"
              variant="ghost"
              leadingIcon={<XGlyph size="xs" />}
              onClick={() => {
                setSeat("");
                setPhase("");
                setFailed("");
              }}
            >
              Clear
            </Button>
          )}
        </div>

        <div className="live-row-pair">
          <Card padding="none" className="live-card">
            <Card.Header
              icon={<LiveDot running={running.length} />}
              count={running.length}
              actions={
                <a className="t-link" href={href(["live", "turns"], seat ? { seat } : undefined)}>
                  Every turn
                </a>
              }
            >
              <Card.Title as="h3">Running turns</Card.Title>
            </Card.Header>
            {running.length > 0 ? (
              <ul className="live-list">
                {running.map((row) => (
                  <LiveTurnRow key={row.id} row={row} index={index} now={now} />
                ))}
              </ul>
            ) : (
              <EmptyState
                size="compact"
                icon={<ClockGlyph size={32} />}
                title={filtering ? "No running turn matches these filters" : "No seat is mid-turn"}
                description={
                  filtering
                    ? "Clear the filters to see every running turn."
                    : agentSeats.length
                      ? "Turns appear here when they start."
                      : "No agent seats are defined yet."
                }
              />
            )}
          </Card>

          <Card padding="none" className="live-card">
            <Card.Header icon={<CircleQuestionMarkGlyph size="sm" />} count={waiting.length}>
              <Card.Title as="h3">Waiting on a person</Card.Title>
            </Card.Header>
            {waiting.length > 0 ? (
              <ul className="decision-list">
                {waiting.map((run) => (
                  <DecisionRow
                    key={run.turn_id}
                    subject={{ kind: "run", at: run.paused_at || run.updated_at, run }}
                    now={now}
                  />
                ))}
              </ul>
            ) : (
              <EmptyState
                size="compact"
                icon={<CircleQuestionMarkGlyph size={32} />}
                title="No executor is waiting on anybody"
                description="Executors that need an answer appear here."
              />
            )}
          </Card>
        </div>

        <div className="live-row-pair">
          <Card padding="none" className="live-card">
            <Card.Header
              icon={<SquareTerminalGlyph size="sm" />}
              count={boxed.length}
              actions={
                <a className="t-link" href={href(["live", "executors"])}>
                  All executors
                </a>
              }
            >
              <Card.Title as="h3">In a box</Card.Title>
            </Card.Header>
            {runs.error && !runs.data ? (
              <QueryState error={runs.error} refusal={runs.refusal} loading={false} />
            ) : boxed.length > 0 ? (
              <div className="list">
                {boxed.slice(0, IN_BOX_ROWS).map((run) => (
                  <InBoxRow
                    key={run.turn_id}
                    run={run}
                    who={seatName(run.agent_handle) || run.role}
                    now={now}
                    onOpen={() => openPeek({ kind: "run", id: run.turn_id })}
                  />
                ))}
                {/* WHAT IS NOT SHOWN, said: a list cut at eight with no note
                    reads as a company with eight executors in flight. */}
                {boxed.length > IN_BOX_ROWS && (
                  <a className="list-row clickable" href={href(["live", "executors"])}>
                    <span className="t-caption">
                      {plural(boxed.length - IN_BOX_ROWS, "more executor")} · all executors
                    </span>
                  </a>
                )}
              </div>
            ) : (
              <EmptyState
                size="compact"
                icon={<SquareTerminalGlyph size={32} />}
                title="Nothing is running in a box"
                description="What a finished one did is on its turn’s trace."
              />
            )}
          </Card>

          <Card padding="none" className="live-card">
            <Card.Header
              icon={<ChartNoAxesGanttGlyph size="sm" />}
              subtitle={
                series.data
                  ? `${plural(series.data.total, "event")} in the last ${windowLabel(range.window)}`
                  : `the last ${windowLabel(range.window)}`
              }
              actions={
                <a className="t-link" href={eventLog}>
                  Event log
                </a>
              }
            >
              <Card.Title as="h3">Activity</Card.Title>
            </Card.Header>
            {viewer.loading ? (
              <Skeleton variant="text" rows={4} label="Checking your access" />
            ) : !viewer.admin ? (
              // THE COMPANY-WIDE TRAIL, Admin's — see the file's doc. Drawn
              // locked rather than empty: the engine pushes the feed to an
              // admin's socket alone, so "nothing has happened" would be a
              // claim about the company.
              <GrantRequired size="compact" grants={[ADMIN]} reader={viewer.login} />
            ) : (
              <>
                {/* EVERY READ THIS CARD MAKES OF THE FLEET: the count, and the
                    latest events — a seat's own, read at query time, or the
                    push, which this node seeded at boot from every node it
                    could reach (`seeded_from`) — each of which a node can be
                    missing from. */}
                <CoverageNote
                  coverage={[
                    series.data?.coverage,
                    seat ? seatEvents.data?.coverage : engine?.seeded_from,
                  ]}
                  what="this activity"
                />
                <div className="live-strip">
                  {series.error && !series.data ? (
                    <QueryState error={series.error} refusal={series.refusal} loading={false} />
                  ) : (
                    <ActivityStrip
                      buckets={strip}
                      label={`Events over the last ${windowLabel(range.window)}`}
                      summary={({ peak, total, buckets }) =>
                        `${plural(total, "event")} over ${buckets} buckets, ${peak} in the busiest.`
                      }
                    />
                  )}
                </div>
                {seat && seatEvents.error && !seatEvents.data ? (
                  // A SEAT'S EVENTS THAT COULD NOT BE READ are not a quiet seat.
                  <QueryState
                    error={seatEvents.error}
                    refusal={seatEvents.refusal}
                    loading={false}
                  />
                ) : latest.length > 0 ? (
                  <div className="live-feed">
                    {latest.map((ev) => (
                      <EventRow key={ev.id} event={ev} compact />
                    ))}
                  </div>
                ) : (
                  <EmptyState
                    size="compact"
                    icon={<ChartNoAxesGanttGlyph size={32} />}
                    title="Nothing has happened yet"
                    description="Events appear here as agents work."
                  />
                )}
              </>
            )}
          </Card>
        </div>

        {viewer.admin ? (
          <RecentPhases
            seat={seat}
            agentId={seatRow?.agent_id ?? ""}
            phase={phase}
            failed={failed === "true"}
            runningKeys={runningKeys}
            nameOf={nameOfPhase}
            now={now}
          />
        ) : (
          // EVERY SEAT'S SETTLED PHASES are the company-wide trail: asked of
          // nobody but an admin, and drawn locked for everybody else.
          <Card padding="none" className="live-card">
            <Card.Header icon={<BrainGlyph size="sm" />}>
              <Card.Title as="h3">Recent phases</Card.Title>
            </Card.Header>
            {viewer.loading ? (
              <Skeleton variant="text" rows={4} label="Checking your access" />
            ) : (
              <GrantRequired size="compact" grants={[ADMIN]} reader={viewer.login} />
            )}
          </Card>
        )}
      </div>
    </>
  );
}

/**
 * The strip's cells, summed from the engine's minute bars.
 *
 * KEYED BY THE CELL'S OWN INSTANT, never by position: keyed `p0..p59`, a
 * window recomputed from the clock shifts every cell's content one place left
 * on each roll and rewrites the lot. A bar is added to the cell its start falls
 * in, so a cell's value is exactly the engine's count over it.
 */
export function stripOf(
  series: EventSeries | null | undefined,
  now: number,
  cell: number,
  cells: number,
): { t: number; v: number }[] {
  const end = Math.floor(now / cell) * cell;
  const buckets = new Map<number, number>();
  for (let t = end - (cells - 1) * cell; t <= end; t += cell) buckets.set(t, 0);
  for (const bar of series?.bars ?? []) {
    const t = Math.floor(Date.parse(bar.at) / cell) * cell;
    if (buckets.has(t)) buckets.set(t, (buckets.get(t) ?? 0) + bar.count);
  }
  return [...buckets.entries()].map(([t, v]) => ({ t, v }));
}

/** One executor in flight, as a row that opens it beside the list. */
function InBoxRow({
  run,
  who,
  now,
  onOpen,
}: {
  run: ExecutorRow;
  who: string;
  now: number;
  onOpen: () => void;
}) {
  return (
    // A REAL LINK to the executor's page, peeking on a plain click — the rule
    // every row in the product follows, written once in `peekClickHandler`.
    <a
      className="list-row clickable"
      href={peekHref({ kind: "run", id: run.turn_id })}
      onClick={peekClickHandler(onOpen)}
    >
      <RunStatus status={run.status} />
      <span className="col live-box-body">
        <span className="truncate t-cell">{runTitle(run)}</span>
        <span className="truncate t-caption">
          {who}
          {run.harness ? ` · ${run.harness}` : ""}
        </span>
      </span>
      <span className="t-caption nowrap">{relTime(run.started_at, now)}</span>
    </a>
  );
}
