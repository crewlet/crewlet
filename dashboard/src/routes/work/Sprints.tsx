/**
 * One project's sprints — what was committed, what arrived, what shipped.
 *
 * # Every figure here is derived from the STAYS, and none of them is stored
 *
 * A sprint's numbers are predicates over one table of memberships: committed
 * is what was in the sprint when it STARTED, added is what arrived after,
 * removed is what was pulled out, and remaining is what is still open at the
 * end. A counter maintained forward could not say any of it — "committed" is
 * a statement about a past instant, and a counter is a statement about now.
 *
 * # Done is a delivery, not a membership
 *
 * A task can sit in a sprint the whole way through and never be finished, so
 * `done` counts the tasks that ENTERED a delivered status inside the sprint's
 * own window — which is also why a sprint that closed last month does not
 * keep gaining velocity as its leftovers land.
 *
 * # The two charts answer the two questions a sprint has
 *
 * ACROSS sprints: is this team's delivery steady — a ranked comparison, in
 * sprint order, because the order is the content. WITHIN one: is this sprint
 * going to land — a series, drawn to today rather than to the sprint's end so
 * a running sprint is not a flat line into its own future.
 *
 * # Three screens behind one export
 *
 * `#/work/ENG/sprints/3` addresses ONE sprint: it is where the rail's
 * `Open ↗` goes and where a modifier-click on a sprint lands. The router has
 * always parsed that segment and this file DROPPED it, so the address drew
 * the whole report under a breadcrumb naming one sprint — the reader was told
 * where they were by the crumb and shown everywhere by the screen. It is the
 * same bug `#/goals/{id}` had beside it and `#/admin/fleet/{id}` had above.
 *
 * A sprint is an object like any other, so its page and its rail wear the
 * same [ObjectHeader] over the same [sprintFacts], and both then answer the
 * two questions a number cannot: what SHAPE the sprint is in, and what is
 * still open inside it.
 *
 * # Read-only, like every other work screen
 *
 * A sprint is started, closed and rolled over by the engine's own duty or by
 * a lead's tool call. A button here would write as "the dashboard", which is
 * not a person and cannot be asked why.
 */

import { useMemo, type ReactNode } from "react";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Coverage, RowList, type RowChrome } from "~/components/work.tsx";
import { Badge, Banner, Empty, Meter, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { BarList, Legend, TimeSeries } from "~/ui/charts.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { Dash, NumberCell, SeatCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, seatLookup } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { usePageLabels } from "~/app/Shell.tsx";
import { fmtCount, fmtDate, tsKey } from "~/lib/format.ts";
import type { WorkBurndown, WorkSprintRow } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

/** The measure's own word, because a bare number is points to one team and
 *  minutes to another. */
function measureLabel(measure: string): string {
  return measure === "estimate_min" ? "minutes" : "points";
}

const STATE_TONE: Record<string, "positive" | "caution" | "info" | "neutral"> = {
  future: "neutral",
  active: "info",
  closed: "positive",
};

/**
 * The frame's address for one sprint.
 *
 * `{KEY}/{n}`, which is what `objects.ts` splits back into the route — and it
 * is built HERE rather than at each call site so the rows this screen
 * publishes for `[` and `]`, the href each panel title carries and the id the
 * rail parses can never be three spellings of one thing.
 */
function sprintRef(sprint: { project: string; number: number }): ObjectRef {
  return { kind: "sprint", id: `${sprint.project}/${sprint.number}` };
}

/**
 * The flags a sprint wears beside its title, and nothing else.
 *
 * ONLY WHAT IS EXCEPTIONAL — a spillover nobody has settled, and an archived
 * record. The state itself is a FACT rather than a flag, because every sprint
 * has one and a badge drawn for every sprint says nothing; these two are
 * drawn only for the sprints they are true of, which is what makes them worth
 * the reader's eye.
 *
 * `undefined` rather than an empty element, for the reason `nodeFlags` gives:
 * an empty child still takes its gap in the header row, so an ordinary
 * running sprint would sit with a hole where a state it is not in would have
 * been.
 */
function sprintFlags(sprint: WorkSprintRow): ReactNode {
  if (!sprint.rollover_pending && !sprint.archived) return undefined;
  return (
    <span className="row gap-1">
      {sprint.rollover_pending && <Badge tone="caution">spillover pending</Badge>}
      {sprint.archived && <Badge tone="neutral">archived</Badge>}
    </span>
  );
}

/**
 * The six facts a sprint is read by, in one order, on its page and in the
 * rail.
 *
 * ONE FUNCTION rather than two lists that happen to agree today: a reader
 * scans the same facts in the same order wherever the object appears, and a
 * header written twice is two orders as soon as somebody adds a seventh fact.
 *
 * They are what [SprintPanel] draws below — the window, the state, what the
 * sprint is carrying, how that changed under it, what it has delivered and
 * what is left — so a reader who opens the rail from the report is not handed
 * a different sprint from the one they were looking at.
 *
 * THE SCOPE CHANGE IS A FACT OF ITS OWN rather than a note under the scope,
 * and it is the one the rail could not otherwise say: the figures panel is the
 * page's, so in a 420 px rail `added` and `removed` live here or nowhere — and
 * "34 points, 12 of which arrived after it started" is a different sprint from
 * "34 points, all of them committed on day one", told apart by nothing else on
 * this line.
 */
function sprintFacts(sprint: WorkSprintRow): Fact[] {
  const f = sprint.figures;
  const measure = measureLabel(f.measure);
  // WHAT THE SPRINT IS ACTUALLY CARRYING, which is not `committed`: work that
  // arrived after the start is work the team has to land, and a scope that
  // named only the commitment would describe a plan rather than a sprint.
  const scope = f.committed + f.added;
  return [
    {
      label: "Window",
      // THE CLOSE RATHER THAN THE END where a sprint has one: a sprint closed
      // early ran for the days it ran, and printing its planned end would
      // date a delivery to a day nothing happened on.
      value:
        `${fmtDate(sprint.start_at)} → ${fmtDate(sprint.closed_at ?? sprint.end_at)}` +
        (sprint.days_remaining !== undefined ? ` · ${sprint.days_remaining}d left` : ""),
    },
    {
      label: "State",
      // THE TONE IS ON THE STATE and nowhere else on this line: `future`,
      // `active` and `closed` are the one thing here that is a state rather
      // than a quantity, and colour in this product carries exactly that.
      value: <Badge tone={STATE_TONE[sprint.state] ?? "neutral"}>{sprint.state}</Badge>,
    },
    {
      label: "Scope",
      value: (
        <NumberCell
          value={scope}
          suffix={` ${measure}`}
          title="what it took on, plus what arrived after it started"
        />
      ),
    },
    {
      label: "Scope change",
      // A SPRINT WHOSE SCOPE HELD IS A MEASUREMENT, not a missing value, so it
      // is a word rather than a dash: `+0 / −0` is what a team that planned
      // its sprint and was left alone to run it looks like, and a dash there
      // would claim nobody recorded the movement.
      value:
        f.added > 0 || f.removed > 0 ? (
          <span
            className="t-num"
            title={`${f.added} ${measure} arrived after it started; ${f.removed} were pulled out`}
          >
            +{fmtCount(f.added)} / −{fmtCount(f.removed)}
          </span>
        ) : (
          <span className="muted">unchanged</span>
        ),
    },
    {
      label: "Delivered",
      // ZERO DELIVERED IS A REAL ANSWER and the one a lead is looking for, so
      // it must not be the value a `||` turns into a dash — which is the whole
      // reason [NumberCell] exists.
      value: (
        <NumberCell
          value={f.done}
          suffix={` of ${scope}`}
          title="delivered inside the sprint's own window"
        />
      ),
    },
    {
      label: "Still open",
      value: <NumberCell value={f.remaining} suffix={` ${measure}`} title={`${f.tasks} tasks`} />,
    },
  ];
}

/**
 * Which of the two sprint screens this address names.
 *
 * The same shape `Fleet` takes: a trailing segment is one object, and its
 * absence is the list. Written as a dispatcher rather than as a branch inside
 * the report, because the two ask DIFFERENT questions of the engine — the
 * report wants a window of sprints and the page wants exactly one, including
 * one the cadence duty has since archived.
 */
export function Sprints({ project, sprint }: { project: string; sprint?: string }) {
  const number = Number(sprint);
  return sprint && Number.isFinite(number) && number > 0 ? (
    <SprintScreen project={project} number={number} />
  ) : (
    <SprintReport project={project} />
  );
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

function SprintReport({ project }: { project: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const { open: openPeek } = usePeekControls();
  // THE PROJECT IS THE PATH (`#/work/ENG/sprints`), not a picker: a sprint is
  // numbered per project, so a report reached with no project named was a
  // screen that had to guess — and it guessed the alphabetically first one.
  const chosen = project;

  // NOT UNTIL ONE IS CHOSEN — see the same guard on the board's project
  // overview. `chosen` is empty while the catalogue is still loading and stays
  // empty for a company with no projects, and the engine refuses the question
  // without one, so the screen's own empty state is the honest rendering
  // rather than a `bad_params` banner for a project nobody named.
  const report = useQuery("work_sprints", chosen ? { project: chosen } : undefined, {
    enabled: chosen !== "",
    pollMs: 60_000,
  });
  const sprints = report.data?.sprints ?? [];
  const measure = measureLabel(report.data?.measure ?? "points");

  // NEWEST FIRST, which is the order the panels are drawn in below — and
  // therefore the order `[` and `]` have to walk. Published rather than handed
  // to the rail, because only the list knows what it is showing; publishing
  // the answer's own oldest-first order instead would give a stepper that
  // walked the screen backwards. See `PeekHost`.
  const shown = useMemo(() => [...sprints].reverse(), [sprints]);
  usePeekNeighbours(useMemo(() => shown.map(sprintRef), [shown]));

  // THE ONE SPRINT WORTH A SERIES is the running one: a closed sprint's shape
  // is history and its outcome is already the velocity bar above, where a
  // running one is the question somebody opened this screen to ask.
  const active = sprints.find((s) => s.state === "active");
  const burndown = useQuery(
    "work_burndown",
    // BOTH HALVES FROM ONE ANSWER. A sprint is numbered PER PROJECT, so a
    // number paired with a key from somewhere else names a sprint that need
    // not exist: `chosen` changes the instant somebody picks a project and
    // `active` is still the previous project's report until the new one
    // lands, so every switch asked for the old number under the new key. The
    // row carries its own project, which makes the pair consistent by
    // construction — the panel shows the report it belongs to until the
    // replacement arrives, exactly as the rest of this screen does.
    active ? { project: active.project, sprint: active.number } : undefined,
    // FIVE MINUTES: a burndown moves by the day, and its own points are day
    // boundaries — a faster poll would redraw an identical series.
    { enabled: Boolean(active), pollMs: 300_000 },
  );

  const seat = seatLookup(index);

  return (
    <>
      <PageActions>
        {
          <a className="t-link" href={href(["work", chosen])}>
            Board →
          </a>
        }
      </PageActions>
      <PageNote>
        What each sprint took on, what arrived after it started, and what actually shipped inside
        its own window.
      </PageNote>

      <div className="toolbar">
        <span className="spacer" />
        <Coverage answer={report.data} />
      </div>

      {chosen && (
        <QueryState error={report.error} loading={report.loading}>
          {sprints.length === 0 ? (
            <Empty
              title="No sprints"
              hint={`${chosen} does not run sprints — a lead turns them on with the project's sprint policy.`}
            />
          ) : (
            <>
              {/* VELOCITY IS ABSENT UNTIL A SPRINT HAS CLOSED, and the screen
                  says so rather than drawing a zero — which would read as a
                  team that delivers nothing. */}
              <StatRow cols={3}>
                <Stat
                  label="Velocity"
                  value={report.data?.velocity_avg ?? "—"}
                  sub={
                    report.data?.velocity_avg === undefined
                      ? "no sprint has closed yet"
                      : `mean ${measure} per closed sprint`
                  }
                />
                <Stat label="Sprints shown" value={sprints.length} />
                <Stat
                  label="Earlier"
                  value={report.data?.earlier_sprints_dropped ?? 0}
                  sub="closed sprints below this window"
                />
              </StatRow>

              {sprints.some((s) => s.rollover_pending) && (
                <Banner tone="caution">
                  A closed sprint's spillover is still undecided — its unfinished work is neither
                  carried forward nor dropped until a lead says which.
                </Banner>
              )}

              <Velocity sprints={sprints} measure={measure} />

              {active && (
                <Burndown
                  data={burndown.data}
                  loading={burndown.loading}
                  error={burndown.error}
                  sprint={active}
                />
              )}

              {shown.map((s) => (
                <SprintPanel key={s.number} sprint={s} seat={seat} peek={openPeek} />
              ))}
            </>
          )}
        </QueryState>
      )}
    </>
  );
}

// ---------------------------------------------------------------------------
// One sprint
// ---------------------------------------------------------------------------

/**
 * The read behind both of a sprint's frames.
 *
 * THERE IS NO PER-SPRINT QUERY and there does not need to be: `work_sprints`
 * takes the number as a narrowing, so asking for one is the same question
 * with one more key. Written once here so the page and the rail cannot ask
 * two different things and disagree about a sprint's figures.
 *
 * ARCHIVED SPRINTS ARE INCLUDED, which the report deliberately excludes: the
 * report is "the last few sprints" and an archived one has fallen out of that
 * window, but an ADDRESS names a particular sprint and a link that stopped
 * resolving the day the cadence duty archived it would rot every bookmark and
 * every `Open ↗` behind it.
 */
function useSprint(project: string, number: number) {
  const addressed = project !== "" && number > 0;
  const report = useQuery(
    "work_sprints",
    addressed ? { project, sprint: number, archived: true } : undefined,
    { enabled: addressed, pollMs: 60_000 },
  );
  const burndown = useQuery(
    "work_burndown",
    addressed ? { project, sprint: number } : undefined,
    // FIVE MINUTES, as on the report: a burndown's own points are day
    // boundaries, so a faster poll would redraw an identical series.
    { enabled: addressed, pollMs: 300_000 },
  );
  return { report, burndown, sprint: report.data?.sprints?.[0] };
}

/**
 * One sprint's own page.
 *
 * It answers from the same `work_sprints` document the report does, narrowed
 * to one number — so a figure here and the same figure in the report are one
 * computation rather than two that agree today.
 */
export function SprintScreen({ project, number }: { project: string; number: number }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const { open: openPeek } = usePeekControls();
  const { report, burndown, sprint } = useSprint(project, number);
  // THE SPRINT'S NAME IN THE CRUMB, not the bare number it is addressed by:
  // `ENG / sprints / 3` tells a reader where they are only if they already
  // knew what sprint 3 was called.
  usePageLabels(sprint ? { [String(number)]: `${number} · ${sprint.name}` } : {});

  return (
    <>
      <PageActions>
        {
          <>
            <a className="t-link" href={href(["work", project, "sprints"])}>
              All sprints →
            </a>
            <a className="t-link" href={href(["work", project])}>
              Board →
            </a>
          </>
        }
      </PageActions>
      <PageNote>
        What this sprint took on, what arrived after it started, and what it has actually shipped
        inside its own window. Every figure is computed from the memberships on each read — nothing
        stores one.
      </PageNote>

      <div className="toolbar">
        <span className="spacer" />
        <Coverage answer={report.data} />
      </div>

      {report.loading && !report.data && <Skeleton rows={6} />}
      <QueryState error={report.error} loading={report.loading}>
        {report.data &&
          (sprint ? (
            <>
              <ObjectHeader
                kind="Sprint"
                icon="calendar"
                identifier={`${project}/${number}`}
                title={sprint.name}
                status={sprintFlags(sprint)}
                facts={sprintFacts(sprint)}
              />
              <Burndown
                data={burndown.data}
                loading={burndown.loading}
                error={burndown.error}
                sprint={sprint}
              />
              {/* THE FIGURES WITHOUT THEIR OWN HEADING, because the header
                  above already says which sprint this is and what state it is
                  in — drawn twice it would spend the widest line on the page
                  saying one thing. */}
              <SprintPanel sprint={sprint} seat={seatLookup(index)} peek={openPeek} bare />
              <OpenInSprint
                project={project}
                number={number}
                state={sprint.state}
                chrome={{ seatName: (handle) => index.byHandle.get(handle)?.name ?? handle }}
              />
            </>
          ) : (
            <NoSuchSprint project={project} number={number} />
          ))}
      </QueryState>
    </>
  );
}

/**
 * One sprint, in the rail.
 *
 * # It reads its own sprint
 *
 * A peek is not a projection of the row that opened it: a report row carries
 * what the report needed, and this is opened from a pasted URL as often as
 * from a panel. So it asks — see [useSprint], which is the same read the page
 * makes, so the two frames of one sprint can never disagree.
 *
 * # Two panels, and neither is the report
 *
 * The header already carries every figure. What is left is the pair a number
 * cannot answer — what SHAPE the sprint is in, and WHICH work is still open
 * inside it — and that is where it stops: a rail that grew the assignee
 * breakdown and the velocity chart would be the report in a narrow column,
 * and the reason the rail exists is that the list behind it stays on screen.
 */
export function SprintPeek({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  // `ENG/3` — the project key and the number, joined by the one separator
  // `objects.ts` put there. A plain split is right because a project key is
  // `[A-Z][A-Z0-9_]*` and a sprint number is digits: neither can carry one.
  const [project = "", raw = ""] = id.split("/");
  const number = Number(raw);
  const addressed = project !== "" && Number.isFinite(number) && number > 0;
  const { report, burndown, sprint } = useSprint(project, addressed ? number : 0);

  // A TOKEN THAT IS NOT AN ADDRESS SAYS SO. `peek=sprint:` carries whatever
  // was in the URL, and a hand-edited or truncated one leaves the read
  // disabled — so without this the rail would open, ask nothing, and draw an
  // empty panel over the list, which is the one thing every empty state in
  // this product is written to avoid.
  if (!addressed) {
    return (
      <Empty
        inline
        icon="calendar"
        title={`“${id}” is not a sprint`}
        hint="A sprint is addressed as the project's key and its number joined by a slash — ENG/3. This one carries one or the other, so there is nothing to look up."
      />
    );
  }

  return (
    <>
      {report.loading && !report.data && <Skeleton rows={6} />}
      <QueryState error={report.error} loading={report.loading}>
        {report.data &&
          (sprint ? (
            <>
              <ObjectHeader
                size="peek"
                kind="Sprint"
                icon="calendar"
                identifier={`${project}/${number}`}
                title={sprint.name}
                status={sprintFlags(sprint)}
                facts={sprintFacts(sprint)}
              />
              <div className="col gap-3">
                <Burndown
                  data={burndown.data}
                  loading={burndown.loading}
                  error={burndown.error}
                  sprint={sprint}
                />
                <OpenInSprint
                  project={project}
                  number={number}
                  state={sprint.state}
                  chrome={{ seatName: (handle) => index.byHandle.get(handle)?.name ?? handle }}
                />
              </div>
            </>
          ) : (
            <NoSuchSprint project={project} number={number} inline />
          ))}
      </QueryState>
    </>
  );
}

/** How many open tasks a sprint's frames list before they say so.
 *
 *  FIFTY, which is the board's own column page: a sprint carrying more open
 *  work than one board column is a sprint nobody reads task by task, and the
 *  count in the panel head is the engine's own so the rest is never silent. */
const OPEN_LIMIT = 50;

/**
 * What is still open in a sprint, as rows rather than as a number.
 *
 * THE FIGURES SAY HOW MUCH AND THIS SAYS WHICH, and they are two different
 * questions: "15 points remaining" is what a lead plans against, and "these
 * four tasks" is what they act on.
 *
 * IT IS DRAWN FOR A CLOSED SPRINT TOO, and that is the case it matters most
 * in: work still open in a sprint that has closed is the spillover somebody
 * has to carry forward or drop, which is the one thing on this whole screen
 * a lead has to decide.
 */
function OpenInSprint({
  project,
  number,
  state,
  chrome,
}: {
  project: string;
  number: number;
  state: WorkSprintRow["state"];
  chrome: RowChrome;
}) {
  const now = useNow();
  const { open: openPeek } = usePeekControls();
  const addressed = project !== "" && number > 0;
  const { data, loading, error } = useQuery(
    "work_items",
    addressed
      ? {
          container: `project:${project}`,
          sprint: String(number),
          // THE ENGINE'S OWN WORD. `open` is this dashboard's name for two of
          // the four status GROUPS and the tracker's grammar has no such key
          // — it refuses a parameter it does not read rather than ignoring
          // one, which makes a leaked key fail the WHOLE read and render as a
          // sprint with nothing left in it.
          status_group: "not_started,active",
          // EVERY TASK ON ITS OWN ROW, which is the one mode that matches what
          // the figures above count. A sprint's membership is per TASK, so the
          // default `collapsed` — which filters ROOTS and lets their subtrees
          // ride along — would hide a subtask that is in this sprint under a
          // parent that is not, and the panel would list three rows beside a
          // header saying twelve tasks are open.
          subtasks: "separate",
          // THE BOARD'S OWN ORDER, so a lead reading the spillover here sees
          // it in the sequence they arranged it in rather than in whatever
          // order the rows were written.
          sort: "rank",
          limit: OPEN_LIMIT,
        }
      : undefined,
    { enabled: addressed, pollMs: 60_000 },
  );
  const rows = data?.items ?? [];
  // A BOOLEAN, never the count itself: `{n && <p/>}` renders a bare "0" when
  // the count is zero, which is the one JSX mistake with no error, no warning
  // and no type failure behind it.
  const truncated = data != null && data.total_hint > rows.length;

  return (
    <Panel
      title="Still open"
      icon="inbox"
      count={rows.length}
      subtitle={
        state === "closed"
          ? "the spillover — neither carried forward nor dropped until a lead says which"
          : "what has to land before this sprint ends"
      }
      padding="none"
    >
      {loading && !data ? (
        <Skeleton rows={3} />
      ) : (
        <QueryState
          error={error}
          loading={loading}
          empty={
            rows.length > 0
              ? undefined
              : {
                  title: "Nothing is open in this sprint",
                  hint: "Every task that was in it has reached a status in the done or closed group.",
                }
          }
        >
          <RowList
            rows={rows}
            now={now}
            chrome={chrome}
            hrefOf={(row) => href(["work", row.key])}
            onOpen={(row) => openPeek({ kind: "item", id: row.key })}
          />
          {/* WHICH OF THE COUNT IS DRAWN. A panel listing fifty rows out of a
              hundred and saying nothing reads as a sprint with fifty tasks
              left, which is the number a lead would plan against. */}
          {truncated && (
            <p className="t-caption faint" style={{ padding: "var(--space-3)" }}>
              {rows.length} of {data?.total_hint} open, in the board's own order — the board has the
              rest.
            </p>
          )}
        </QueryState>
      )}
    </Panel>
  );
}

/**
 * NOT AN EMPTY PANEL, and not a spinner that never resolves.
 *
 * A sprint address reaches this from a bookmark and from a pasted URL as often
 * as from a panel, and the two ways it resolves to nothing are different
 * repairs: a project key nobody minted, and a number this project has not
 * reached yet. The engine's own refusal separates them — `work_sprints`
 * answers `not_found` for the first and an empty list for the second — so
 * this is the second, said as what it is.
 */
function NoSuchSprint({
  project,
  number,
  inline,
}: {
  project: string;
  number: number;
  inline?: boolean;
}) {
  return (
    <Empty
      inline={inline}
      icon="calendar"
      title={`${project} has no sprint ${number}`}
      hint="Sprints are numbered per project and minted by the cadence duty, so a number above the last one it has reached names a sprint that does not exist yet."
    />
  );
}

/**
 * Delivery across the recent sprints.
 *
 * IN SPRINT ORDER, oldest first, and NOT sorted by size: this is a sequence
 * rather than a ranking, and re-ordering it by how much each sprint delivered
 * would destroy the one thing a reader is looking for — whether the team is
 * steady, climbing or falling.
 *
 * ONE SERIES WITH ONE MEMBER HIGHLIGHTED, which is the emphasis form: the
 * running sprint is a partial total and every closed one is final, so a single
 * hue across all of them would invite the reader to compare an unfinished
 * sprint against finished ones as though they were the same measurement. No
 * legend, because a legend for one series is a box saying what the title says.
 */
export function Velocity({ sprints, measure }: { sprints: WorkSprintRow[]; measure: string }) {
  // A SPRINT THAT HAS NOT STARTED HAS NO DELIVERY, and a bar reading "0 of 0"
  // in a delivery chart is not a slow sprint — it is a sprint that has not
  // happened. Every row used to be drawn, so a project with three planned
  // sprints ahead of it drew three empty bars beside its real ones and the
  // trend a reader came for was three-fifths fiction.
  //
  // `future` is the state the cadence duty mints ahead; `closed` and `active`
  // are the two that have a measurement.
  const run = sprints.filter((s) => s.state === "closed" || s.state === "active");
  if (run.length < 2) return null;
  sprints = run;
  // THE SCALE IS THE LARGEST SPRINT'S WHOLE COMMITMENT, so a bar reads as a
  // fraction of what that sprint took on rather than of what the best sprint
  // delivered — the second makes an ordinary sprint beside an exceptional one
  // look like a failure.
  const max = Math.max(1, ...sprints.map((s) => s.figures.committed + s.figures.added));
  return (
    <Panel
      title="Delivery by sprint"
      icon="activity"
      subtitle="the running sprint in colour; its total is still partial"
    >
      <BarList
        max={max}
        data={sprints.map((s) => ({
          label: `${s.number} · ${s.name}`,
          value: s.figures.done,
          display: `${s.figures.done} of ${s.figures.committed + s.figures.added} ${measure}`,
          color: s.state === "active" ? "var(--viz-1)" : "var(--viz-other)",
          sub:
            s.state === "closed"
              ? `${fmtDate(s.start_at)} — ${fmtDate(s.closed_at ?? s.end_at)}${
                  s.figures.remaining ? ` · ${s.figures.remaining} ${measure} carried out` : ""
                }`
              : `${fmtDate(s.start_at)} — ${fmtDate(s.end_at)}`,
        }))}
        emptyLabel="No sprint in this window has delivered anything yet."
      />
    </Panel>
  );
}

/**
 * The running sprint, day by day.
 *
 * THREE LINES AND ONLY ONE OF THEM IS A MEASUREMENT OF PROGRESS. Remaining is
 * the question; scope is what makes it answerable, because a falling line over
 * a rising scope is a team keeping up rather than a team finishing; and the
 * ideal is a dashed REFERENCE, drawn so it cannot be mistaken for a series
 * that happened to be linear.
 *
 * A SPRINT WITH NO LIVED DAY DRAWS NO CURVE AND SAYS WHY: its series is one
 * point, and a chart of one point invites a conclusion from nothing. It keeps
 * its PANEL, because a burndown simply absent from an active sprint's report
 * reads as a chart that failed to load.
 */
export function Burndown({
  data,
  loading,
  error,
  sprint,
}: {
  data?: WorkBurndown | null;
  loading?: boolean;
  error?: string | null;
  sprint: WorkSprintRow;
}) {
  // A REFUSED SERIES KEEPS ITS PANEL, for the same reason a sprint with no
  // lived day does: this panel is drawn only for a sprint that is RUNNING, so
  // its absence reads as "this sprint has no burndown" — a statement about the
  // work — when what happened is that the question failed. `QueryState` is
  // where every code is turned into a sentence, so the refusal says which.
  if (error) {
    return (
      <Panel title={`Sprint ${sprint.number} burndown`} icon="activity" subtitle={sprint.name}>
        <QueryState error={error} loading={false} />
      </Panel>
    );
  }
  if (loading && !data) return null;
  if (!data) return null;
  const measure = measureLabel(data.measure);
  // A SPRINT WITH NO LIVED DAY SAYS SO rather than vanishing. The series
  // stops at the reader's own instant, so one point means the clock is at or
  // before the sprint's start — and a chart of one point invites a
  // conclusion from nothing. Drawing NOTHING was the other mistake: a
  // burndown panel that is simply absent from an active sprint's report
  // reads as a chart that failed to load, which is what every empty state in
  // this product is written to avoid.
  if (data.points.length < 2) {
    return (
      <Panel title={`Sprint ${data.sprint} burndown`} icon="activity" subtitle={sprint.name}>
        <Empty
          title="Nothing to draw yet"
          hint={`This sprint's window opens on ${fmtDate(data.start_at)}. A burndown needs a day that has been lived through, so the first point arrives with it.`}
        />
      </Panel>
    );
  }
  const from = tsKey(data.start_at);
  const to = tsKey(data.end_at);
  const last = data.points[data.points.length - 1];

  return (
    <Panel
      title={`Sprint ${data.sprint} burndown`}
      icon="activity"
      subtitle={`${sprint.name} · in ${measure}`}
      actions={
        sprint.days_remaining !== undefined && <Badge outline>{sprint.days_remaining}d left</Badge>
      }
    >
      {/* UNESTIMATED WORK IS NAMED ABOVE THE CHART, never folded in: a series
          over a sprint half of whose tasks carry no value is a series about
          half a sprint, and a reader who cannot see that quotes the number. */}
      {data.unestimated > 0 && (
        <Banner tone="info">
          {data.unestimated} of {data.tasks} tasks carry no {measure}, so this chart describes only
          the rest.
        </Banner>
      )}
      <TimeSeries
        from={from}
        to={to}
        height={160}
        label={`Sprint ${data.sprint} burndown`}
        format={(n) => `${n} ${measure}`}
        series={[
          {
            name: "Scope",
            color: "var(--viz-other)",
            points: data.points.map((p) => ({ t: tsKey(p.at), v: p.scope })),
          },
          {
            name: "Ideal",
            color: "var(--border-strong)",
            dashed: true,
            points: [
              { t: from, v: data.ideal },
              { t: to, v: 0 },
            ],
          },
          {
            name: "Remaining",
            color: "var(--viz-1)",
            // THE ONE FILLED SERIES, because it is the one the chart is
            // about: an area under every line would blend three translucent
            // washes into a fourth colour nobody chose.
            fill: true,
            points: data.points.map((p) => ({ t: tsKey(p.at), v: p.remaining })),
          },
        ]}
      />
      <Legend
        items={[
          { label: "Remaining", color: "var(--viz-1)" },
          { label: "Scope", color: "var(--viz-other)" },
          { label: "Ideal", color: "var(--border-strong)" },
        ]}
      />
      {last && (
        <p className="t-caption">
          {last.remaining} {measure} still to do of {last.scope} in the sprint; {last.delivered}{" "}
          delivered.
          {/* THE GAP IS ABANDONED WORK, and it is the one quantity two lines
              cannot show: `cancelled` is finished and undelivered, so it
              leaves the remaining line without joining the delivered one. */}
          {last.scope - last.remaining - last.delivered > 0
            ? ` ${(last.scope - last.remaining - last.delivered).toLocaleString()} ${measure} were cancelled rather than delivered.`
            : ""}
        </p>
      )}
    </Panel>
  );
}

/** One sprint's figures, its breakdown and the two things a lead acts on.
 *
 *  Exported for its own tests: every number here has a wrong form that reads
 *  as a different fact rather than as a missing one. */
export function SprintPanel({
  sprint,
  seat,
  peek,
  bare,
}: {
  sprint: WorkSprintRow;
  /** The chart's word for a handle, and which kind of seat it is. */
  seat?: (handle: string) => { name: string; kind?: "agent" | "human" };
  /**
   * Open an object in the rail.
   *
   * ONE CALLBACK RATHER THAN A HOOK, and that is not a style choice: this
   * panel is rendered directly by its own suite, and `usePeekControls` reads
   * the navigator, so reaching for it here would make every case in that file
   * depend on a router it has no reason to stand up.
   */
  peek?: (ref: ObjectRef) => void;
  /** Under an [ObjectHeader] that already names this sprint and its state. */
  bare?: boolean;
}) {
  const f = sprint.figures;
  const measure = measureLabel(f.measure);
  const committed = f.committed + f.added;
  const ref = sprintRef(sprint);
  // A PANEL TITLE IS A REAL LINK EITHER WAY: its href is the sprint's own
  // page, so ⌘-click and the middle button open a tab and the status bar says
  // where it goes; a plain left click peeks instead, because the report is
  // where the reader is. `rowPeekHandler` is the frame's ONE copy of which
  // clicks mean "open elsewhere".
  const title =
    peek && !bare ? (
      <a className="t-link" href={peekHref(ref)} onClick={rowPeekHandler(() => peek(ref))}>
        {sprint.number} · {sprint.name}
      </a>
    ) : (
      `${sprint.number} · ${sprint.name}`
    );

  return (
    <Panel
      title={bare ? undefined : title}
      actions={
        bare ? undefined : (
          <>
            <Badge tone={STATE_TONE[sprint.state] ?? "neutral"}>{sprint.state}</Badge>
            {sprint.rollover_pending && <Badge tone="caution">spillover pending</Badge>}
            {sprint.archived && <Badge tone="neutral">archived</Badge>}
          </>
        )
      }
    >
      <p className="muted">
        {fmtDate(sprint.start_at)} →{" "}
        {sprint.closed_at ? fmtDate(sprint.closed_at) : fmtDate(sprint.end_at)}
        {sprint.days_remaining !== undefined && ` · ${sprint.days_remaining}d left`}
        {sprint.goal ? ` · ${sprint.goal}` : ""}
      </p>
      {/* THE SPRINT'S OWN PROGRESS AS A SHAPE, beside the five numbers that
          say what it is made of. The measure is on the bar's own right, so a
          reader never has to find it in a column heading. */}
      {committed > 0 && (
        <Meter
          used={f.done}
          max={committed}
          ariaLabel={`Sprint ${sprint.number} — delivered`}
          label="Delivered"
          right={`${f.done} of ${committed} ${measure}`}
          fullMeans="achieved"
        />
      )}
      <StatRow cols={5}>
        <Stat label="Committed" value={f.committed} sub={measure} />
        <Stat label="Added" value={f.added} sub="arrived after the start" />
        <Stat label="Removed" value={f.removed} sub="pulled out, not carried" />
        <Stat label="Done" value={f.done} sub="delivered inside the window" />
        <Stat label="Remaining" value={f.remaining} sub={`${f.tasks} tasks`} />
      </StatRow>
      {/* UNESTIMATED IS THE HONESTY COLUMN. A sprint reporting 8 of 34 points
          done where half its tasks carry no estimate is reporting on half a
          sprint. */}
      {f.unestimated > 0 && (
        <Banner tone="info">
          {f.unestimated} of {f.tasks} tasks carry no {measure}, so these figures describe only the
          rest.
        </Banner>
      )}
      {sprint.by_assignee && sprint.by_assignee.length > 0 && (
        <DataGrid
          rows={sprint.by_assignee}
          rowKey={(a) => a.handle}
          // PER SPRINT, because the report draws one of these grids per
          // sprint: unnamed they would all read the screen's ONE `sort` key,
          // so sorting the breakdown of sprint 4 would silently re-sort every
          // other sprint's — and a link to a sorted report would mean
          // something different depending on which table the reader touched.
          name={`sprint${sprint.number}`}
          // A PLAIN CLICK OPENS THE PERSON, because "who is carrying this" is
          // a question about them rather than about the sprint — and the
          // sprint stays on screen behind the rail, which is the whole point
          // of not navigating.
          onRowActivate={peek ? (a) => peek({ kind: "seat", id: a.handle }) : undefined}
          columns={[
            {
              key: "handle",
              header: "Who",
              // THE NAME, not the handle: the answer is keyed on the slug and
              // the column shows what the chart calls the person, so ordering
              // on the raw key would sort by a word nobody can see.
              sortValue: (a) => seat?.(a.handle).name ?? a.handle,
              cell: (a) => {
                const who = seat?.(a.handle);
                return <SeatCell handle={a.handle} name={who?.name} kind={who?.kind} />;
              },
            },
            // EVERY ONE OF THESE IS A COUNT, and a count's zero is a real
            // answer: an assignee holding nothing is exactly who a lead is
            // looking for on an overloaded sprint, so it must not be the
            // value a `||` turns into a dash.
            {
              key: "total",
              header: "Holding",
              align: "right",
              sortValue: (a) => a.total,
              cell: (a) => <NumberCell value={a.total} />,
            },
            {
              key: "done",
              header: "Done",
              align: "right",
              sortValue: (a) => a.done,
              cell: (a) => <NumberCell value={a.done} />,
            },
            {
              key: "remaining",
              header: "Remaining",
              align: "right",
              sortValue: (a) => a.remaining,
              cell: (a) => <NumberCell value={a.remaining} />,
            },
            {
              key: "capacity",
              header: "Capacity",
              // AN UNDECLARED CAPACITY SORTS LAST IN BOTH DIRECTIONS, which is
              // the grid's own rule for an absent value: "nobody declared one"
              // is not the smallest capacity, it is not a capacity.
              sortValue: (a) => a.capacity ?? null,
              // AN UNDECLARED CAPACITY IS A DASH, never a zero or a bar:
              // rendering zero would put every assignee permanently over, and
              // a bar with no maximum is a claim about a limit nobody set.
              //
              // NOT A `MeterCell`, which draws the bar and nothing else: the
              // number beside it is what a lead acts on — "8 / 21" is a
              // decision and a bar three-eighths full is an impression.
              cell: (a) =>
                a.capacity === undefined ? (
                  <Dash title="this project's sprint policy declares no capacity for this assignee" />
                ) : (
                  <span style={{ minWidth: 120, display: "block" }}>
                    <Meter
                      used={a.total}
                      max={a.capacity}
                      ariaLabel={`${a.handle} — committed against capacity`}
                      right={`${a.total} / ${a.capacity}`}
                      fullMeans="spent"
                      {...(a.over_capacity ? { tone: "critical" as const } : {})}
                    />
                  </span>
                ),
            },
          ]}
        />
      )}
    </Panel>
  );
}
