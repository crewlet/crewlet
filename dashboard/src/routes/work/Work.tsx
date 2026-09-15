/**
 * The tracker — the company's own work, as a place rather than a table.
 *
 * # Why this screen exists at all, and what it is not
 *
 * It is NOT a second tracker. A company running Jira has none of these
 * questions registered, and this screen says so rather than drawing an empty
 * board: an operator who wired Jira and then found a blank Crewlet board would
 * reasonably conclude their integration was broken.
 *
 * # An unhydrated projection is not an empty company
 *
 * Every row here comes from this node's own projection of the fleet's record,
 * which is the same copy a seat's tools read — so an operator and an agent
 * looking at one item see one item. A node that has not finished its boot
 * reconcile REFUSES rather than answering empty, and `QueryState` renders that
 * refusal as "still catching up". Drawing it as "no work" would be an answer
 * somebody acts on, by filing the duplicate.
 *
 * # Read-only, deliberately
 *
 * Nothing here writes. An item is filed and moved by a seat's own tools, or by
 * an operator through the MCP surface, and both are attributed to somebody —
 * where a board button would write as "the dashboard", which is not a person
 * and not a seat and cannot be asked why. That is also why there is no drag: a
 * rank is a value on the task, and dragging one would be the dashboard
 * deciding a team's order.
 *
 * # A tracker is a workspace, not a table
 *
 * The rail, the tabs and the peek exist so a reader moves between projects,
 * shapes and items without leaving the place they are in. Each of those is a
 * SECTION and leaves a history entry, because each is a place the reader
 * called; every filter REPLACES, because four ticked chips are one screen and
 * Back means "off this list" rather than "untick one".
 */

import { useEffect, useMemo } from "react";
import { href, useParam } from "~/app/router.tsx";
import { usePeek, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import {
  BoardCard,
  Coverage,
  Fact,
  RowList,
  StatusBadge,
  TypeIcon,
  type RowChrome,
} from "~/components/work.tsx";
import { TimelineView } from "~/components/timeline.tsx";
import {
  Badge,
  Banner,
  Button,
  Chip,
  Empty,
  Meter,
  Panel,
  SearchInput,
  Segmented,
  Select,
  Skeleton,
  Tabs,
} from "~/ui/primitives.tsx";
import { BarList, StackedBar, Legend } from "~/ui/charts.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtDateTime, plural, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import {
  anyFilter,
  bucketByDay,
  buildItemsParams,
  CALENDAR_CELL_CHIPS,
  calendarWeeks,
  dayKey,
  defaultView,
  describeChange,
  filterPatchForGroup,
  GROUP_AXES,
  gridRange,
  groupLabel,
  monthOf,
  monthLabel,
  PRIORITIES,
  projectKeys,
  scopeOf,
  shapeOf,
  shiftMonth,
  shownRows,
  SORTS,
  STATUSES,
  statusLabel,
  totalHint,
  typeName,
  viewParams,
  WEEKDAYS,
  type CalendarCell,
  type Shape,
  type TrackerFilters,
} from "~/lib/work.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import type {
  WorkActivityRecord,
  WorkGroup,
  WorkProjectDetail,
  WorkProjectRow,
  WorkSummary,
  WorkView,
} from "~/protocol/index.ts";

/** The icon a view's tab wears, by what it draws. */
const VIEW_ICON = {
  list: "menu",
  board: "columns",
  calendar: "calendar",
  timeline: "activity",
} as const;

export function Work({ project = "" }: { project?: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  // ONE CLOCK for the screen, ticking on its own: a relative time computed
  // from Date.now() at render is frozen until something else re-renders, so
  // "2 minutes ago" stays that for an hour on a screen nobody touches.
  const now = useNow();

  // THE SECTIONS — a place the reader called, so each pushes history.
  //
  // THE PROJECT IS NOT ONE OF THEM ANY MORE: it is a PATH now (`#/work/ENG`),
  // because a project is an object with a page rather than a filter on the
  // company's list. That is what lets it have sprints, a catalogue and an
  // activity feed of its own without any of them being a query key on a
  // screen called "Work".
  const [viewKey, setViewKey] = useParam("view", "", "section");
  const [month, setMonth] = useParam("month", "", "section");

  // AND THE FILTERS, which replace: four ticked chips are ONE screen.
  const [q, setQ] = useParam("q", "");
  const [status, setStatus] = useParam("status", "");
  const [type, setType] = useParam("type", "");
  const [priority, setPriority] = useParam("priority", "");
  const [assignee, setAssignee] = useParam("assignee", "");
  const [sprint, setSprint] = useParam("sprint", "");
  const [groupBy, setGroupBy] = useParam("group_by", "");
  const [group, setGroup] = useParam("group", "");
  const [sort, setSort] = useParam("sort", "");
  const [blocked, setBlocked] = useParam("blocked", "");
  const [overdue, setOverdue] = useParam("overdue", "");

  const container = project ? `project:${project}` : "workspace";

  // THE PEEK IS THE FRAME'S, and so is the rail: the shell mounts one for
  // every screen (`app/frame/PeekHost.tsx`), so this screen opens peeks and
  // renders none. It was `item=` on this screen alone, which is why a board
  // could peek a task and nothing else in the product could peek anything.
  const peek = usePeek();
  const { open: openPeek } = usePeekControls();

  // THE PROJECT LIST IS THE COMPANY'S, never the page's — a rail built from
  // the rows could only ever offer the projects already on screen, so a board
  // narrowed to one offered no way back. The counts beside each name are the
  // MAINTAINED columns, three reads rather than an aggregate over every task.
  const projects = useQuery("work_projects", undefined, { pollMs: 60_000 });
  // AND THE CHOSEN PROJECT'S OWN OVERVIEW: its sprint, its lead, the unit that
  // owns it and its vocabulary — facts about the CONTAINER that a board can
  // say none of from its rows.
  // ENABLED ON THE SELECTION, not just parameterised by it. The engine
  // refuses this question without a key — correctly, since there is no
  // default project — so passing no params is not "ask for everything", it is
  // a query that fails every poll: a warning a minute in the operator's log,
  // and a screen holding an error for a question nobody asked.
  const overview = useQuery("work_project", project ? { key: project } : undefined, {
    enabled: project !== "",
    pollMs: 60_000,
  });
  // THE TAB STRIP is drawn once per container where the rows are redrawn on
  // every filter change, so it is a separate question with a slower poll: a
  // saved view is arranged by a person, not by the work.
  const strip = useQuery("work_views", { container }, { pollMs: 120_000 });
  // THE TYPES COME FROM THE CATALOGUE, never from the rows: a filter built
  // from the page can only offer the types that happen to be on it, so a board
  // showing no bugs would offer no way to ask for one. The catalogue is the
  // company's own vocabulary and changes about once a quarter.
  const catalogue = useQuery("work_catalogue", undefined, { pollMs: 300_000 });

  const views = strip.data?.views ?? [];
  const chosenView = viewKey || defaultView(views);
  const shape: Shape = shapeOf(chosenView, views);
  const detail = overview.data;

  // THE VIEW SETS THE SCOPE, and it is the view's own `status_group` read back
  // through the segment that expresses it.
  //
  // This control is the single authority on `status_group`: [buildItemsParams]
  // spreads a view's params and then OVERWRITES that key from the scope, so a
  // view saved over closed work was answered as open work on the segment's
  // default — the saved filter could not take effect, and the segment named a
  // scope the rows did not match. Seeding the segment from the view is what
  // makes the two agree, and the reason it is the DEFAULT rather than a write
  // is that a default is not a value: the control reads the view until
  // somebody moves it, and a scope they chose is in the URL and outlives the
  // view switch, exactly as every other filter on this screen does.
  //
  // A group the three segments cannot express — a saved view narrowed to
  // `active` alone — reads as ALL, which is what [scopeOf] already answers for
  // it. The segment and the query still agree, which is the property that
  // matters; they simply agree on the wider set.
  const viewScope = scopeOf(viewParams(chosenView, views).status_group) || "open";
  const [scope, setScope] = useParam("scope", viewScope);

  const filters: TrackerFilters = {
    q,
    status,
    type,
    priority,
    assignee,
    sprint,
    scope,
    groupBy,
    group,
    sort,
    blocked: blocked === "true",
    overdue: overdue === "true",
  };

  const thisMonth = month || monthOf(now);
  const todayKey = dayKey(new Date(now).toISOString());
  const weeks = useMemo(
    () => (shape === "calendar" ? calendarWeeks(thisMonth, todayKey) : []),
    [shape, thisMonth, todayKey],
  );

  const params = useMemo(
    () =>
      buildItemsParams({
        container,
        shape,
        view: viewParams(chosenView, views),
        filters,
        range: weeks.length ? gridRange(weeks) : undefined,
      }),
    // The filters object is a fresh literal on every render; its content is
    // what the query depends on.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [container, shape, chosenView, views, weeks, JSON.stringify(filters)],
  );

  // A change to an item publishes onto the seat inbox rather than to the
  // dashboard socket, so there is no push behind this and a poll is correct.
  // Twenty seconds: a board is read, not watched, and a tracker's own pace is
  // a person typing a comment.
  const { data, loading, error } = useQuery("work_items", params, { pollMs: 20_000 });
  // AND WHAT HAPPENED, which is a different question from what is there: the
  // feed is ordered by the LOG rather than by anything this board sorts on,
  // so a change that moved nothing on screen is still visible.
  const feed = useQuery("work_activity", { container, limit: 20 }, { pollMs: 60_000 });

  const groups = useMemo(() => data?.groups ?? [], [data]);
  const rows = useMemo(() => data?.items ?? [], [data]);
  // WHAT `[` AND `]` WALK: the rows this board actually loaded, sorted and
  // filtered as the reader left them. Published rather than handed to the
  // rail, because only the list knows that order — see `PeekHost`.
  usePeekNeighbours(useMemo(() => rows.map((r) => ({ kind: "item", id: r.key })), [rows]));
  const shown = useMemo(() => shownRows(rows, groups), [rows, groups]);

  const chrome: RowChrome = {
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
    types: catalogue.data?.types,
    statuses: detail?.statuses,
  };

  const applyView = (v: WorkView) => setViewKey(v.key);

  const clearFilters = () => {
    setQ("");
    setStatus("");
    setType("");
    setPriority("");
    setAssignee("");
    setSprint("");
    // TO THE VIEW'S, not to "open": clearing a narrowing returns the screen to
    // what the view asked for, and passing the fallback drops the key from the
    // URL so the view keeps supplying it.
    setScope(viewScope);
    setGroupBy("");
    setGroup("");
    setSort("");
    setBlocked("");
    setOverdue("");
  };

  const itemHref = (row: WorkSummary) => href(["work", row.key]);

  return (
    <>
      <PageActions>
        {
          // REAL ANCHORS rather than buttons that navigate: these leave the
          // tracker, so they are middle-clickable like every other way out of
          // a screen, and they go through the router's own history rules
          // instead of around them.
          <>
            <a className="t-link" href={href(["me"])}>
              My work →
            </a>
            {project && (
              <a className="t-link" href={href(["work", project, "sprints"])}>
                Sprints →
              </a>
            )}
            <a className="t-link" href={href(["goals"])}>
              Goals →
            </a>
          </>
        }
      </PageActions>
      <PageNote>
        The company's own work — every project, board and item. Read-only here: work is filed and
        moved by the seats themselves, so every change is attributed to somebody.
      </PageNote>

      <div className="work-main">
        {project ? (
          <ProjectHead detail={detail} chrome={chrome} />
        ) : (
          <WorkspaceHead projects={projects.data?.projects ?? []} />
        )}

        {views.length > 0 && (
          <Tabs
            ariaLabel="Views"
            value={chosenView}
            onChange={(key) => applyView(views.find((v) => v.key === key) ?? views[0]!)}
            options={views.map((v) => ({
              value: v.key,
              label: `${v.pinned ? "★ " : ""}${v.name}`,
              icon: VIEW_ICON[v.type] ?? "menu",
            }))}
          />
        )}

        <div className="work-filters">
          <div className="work-filters-search">
            <SearchInput
              value={q}
              onChange={setQ}
              ariaLabel="Find an item by key or title"
              placeholder="Key or title"
            />
          </div>
          <Select
            value={status}
            onChange={setStatus}
            ariaLabel="Status"
            anyLabel="Any status"
            options={STATUSES.map((s) => ({
              value: s.value,
              label: statusLabel(s.value, detail?.statuses),
            }))}
          />
          {(catalogue.data?.types ?? []).length > 0 && (
            <Select
              value={type}
              onChange={setType}
              ariaLabel="Type"
              anyLabel="Any type"
              options={(catalogue.data?.types ?? [])
                .filter((t) => !t.archived)
                .map((t) => ({ value: t.slug, label: t.name }))}
            />
          )}
          <Select
            value={priority}
            onChange={setPriority}
            ariaLabel="Priority"
            anyLabel="Any priority"
            options={PRIORITIES.map((p) => ({ value: p, label: p }))}
          />
          <Select
            value={assignee}
            onChange={setAssignee}
            ariaLabel="Assignee"
            anyLabel="Anybody"
            options={[
              // UNASSIGNED IS A VALUE, not a missing filter — it is the one
              // question a lead actually opens a board to ask.
              { value: "none", label: "Unassigned" },
              ...index.seats.map((s) => ({ value: s.handle, label: s.name })),
            ]}
          />
          {project && detail?.sprints && (
            <Select
              value={sprint}
              onChange={setSprint}
              ariaLabel="Sprint"
              anyLabel="Any sprint"
              options={[
                { value: "active", label: "Active sprint" },
                { value: "next", label: "Next sprint" },
                { value: "none", label: "Backlog" },
                ...(detail.recent_sprints ?? []).map((s) => ({
                  value: String(s.number),
                  label: `${s.number} · ${s.name}`,
                })),
              ]}
            />
          )}
          {shape !== "calendar" && (
            <Select
              value={groupBy}
              onChange={(value) => {
                setGroupBy(value);
                // A COLUMN FILTER BELONGS TO ITS AXIS. Left behind when the
                // axis changes it narrows the board to a key the new axis
                // has never heard of, which answers nothing.
                setGroup("");
              }}
              ariaLabel="Group by"
              anyLabel={shape === "board" ? "By status" : "No grouping"}
              options={GROUP_AXES.filter((a) => !a.projectOnly || project).map((a) => ({
                value: a.value,
                label: a.label,
              }))}
            />
          )}
          {shape === "list" && (
            <Select
              value={sort}
              onChange={setSort}
              ariaLabel="Sort"
              anyLabel="Default order"
              options={SORTS}
            />
          )}
          {/* THREE SEGMENTS, not a checkbox: "open", "closed" and
                "everything" are three real questions, and a two-state control
                would make the third unreachable — which is how a board that
                can never show a closed item ships. */}
          <Segmented
            value={scope}
            onChange={setScope}
            ariaLabel="Open or closed"
            options={[
              { value: "open", label: "Open" },
              { value: "closed", label: "Closed" },
              { value: "", label: "All" },
            ]}
          />
          <Chip
            on={filters.blocked}
            onClick={() => setBlocked(filters.blocked ? "" : "true")}
            title="Only work that cannot move"
          >
            Blocked
          </Chip>
          <Chip
            on={filters.overdue}
            onClick={() => setOverdue(filters.overdue ? "" : "true")}
            title="Only open work past its due date"
          >
            Overdue
          </Chip>
          {anyFilter(filters) && (
            <Button size="sm" variant="ghost" icon="x" onClick={clearFilters}>
              Clear
            </Button>
          )}
          <span className="work-summary">
            <Coverage answer={data} />
            {!loading && !error && (
              <span>
                {plural(shown.length, "item")}{" "}
                {totalHint(data?.total_hint ?? 0, shown.length, data?.total_capped)}
              </span>
            )}
          </span>
        </div>

        {group && groups.length > 0 && (
          <Banner tone="info">
            Showing one column of this board.
            <Button size="sm" variant="ghost" onClick={() => setGroup("")}>
              Show every column
            </Button>
          </Banner>
        )}
        {data?.groups_overlap && (
          <Banner tone="info">
            One item can be on several of these columns, so the counts add up to more than the
            total.
          </Banner>
        )}
        {data?.groups_dropped ? (
          <Banner tone="caution">
            {data.groups_dropped} more column{data.groups_dropped === 1 ? "" : "s"} did not fit and
            are not shown. Narrow the board to bring them into range.
          </Banner>
        ) : null}

        <div className="work-body">
          <div className="col gap-4" style={{ minWidth: 0 }}>
            {loading && !data && <Skeleton rows={6} />}

            <QueryState
              error={error}
              loading={loading}
              empty={
                shown.length || groups.length
                  ? undefined
                  : {
                      title: "Nothing matches",
                      hint: "No item on this node's copy of the tracker matches these filters. Widen them, or check that work is being filed at all.",
                    }
              }
            >
              {shape === "board" && (
                <Board
                  now={now}
                  groups={groups}
                  axis={String(params.group_by ?? "status")}
                  chrome={chrome}
                  detail={detail}
                  workspace={!project}
                  selected={peek?.kind === "item" ? peek.id : ""}
                  hrefOf={itemHref}
                  onOpen={(row) => openPeek({ kind: "item", id: row.key })}
                  onOverflow={(axis, key) => {
                    const patch = filterPatchForGroup(axis, key);
                    setViewKey(patch.view ?? "list");
                    setGroupBy(patch.group_by ?? "");
                    setGroup(patch.group ?? "");
                  }}
                />
              )}
              {shape === "list" && (
                <List
                  rows={rows}
                  groups={groups}
                  axis={groupBy}
                  chrome={chrome}
                  detail={detail}
                  now={now}
                  selected={peek?.kind === "item" ? peek.id : ""}
                  hrefOf={itemHref}
                  onOpen={(row) => openPeek({ kind: "item", id: row.key })}
                  onOverflow={(axis, key) => {
                    setGroupBy(axis);
                    setGroup(key);
                  }}
                />
              )}
              {shape === "timeline" && (
                <TimelineView
                  rows={rows}
                  groups={groups}
                  chrome={chrome}
                  selected={peek?.kind === "item" ? peek.id : ""}
                  now={now}
                  hrefOf={itemHref}
                  onOpen={(row) => openPeek({ kind: "item", id: row.key })}
                />
              )}
              {shape === "calendar" && (
                <CalendarView
                  weeks={weeks}
                  rows={rows}
                  month={thisMonth}
                  chrome={chrome}
                  hrefOf={itemHref}
                  onOpen={(row) => openPeek({ kind: "item", id: row.key })}
                  onMonth={setMonth}
                  onToday={() => setMonth("")}
                />
              )}
            </QueryState>

            <ActivityFeed records={feed.data?.records ?? []} now={now} />
          </div>
        </div>

        {!loading && !error && (projects.data?.projects ?? []).length === 0 && (
          <Empty
            icon="inbox"
            title="No work has been filed yet"
            hint="Seats file work with create_work_item, and an inbound webhook or a schedule is usually what starts them. A project appears here the moment a unit in the company config declares its `project` key."
          />
        )}
      </div>
    </>
  );
}

// ---------------------------------------------------------------------------
// The rail
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// The two heads
// ---------------------------------------------------------------------------

/**
 * The company's work at a glance, ranked by where the open work is.
 *
 * ONE HUE FOR EVERY BAR. A hue per project would be identity colouring by row
 * — rename the project and its colour changes, which is the proof it never
 * meant anything — and the label already carries which project it is. The
 * chart is absent below two projects, because a one-bar chart is a number.
 */
export function WorkspaceHead({ projects }: { projects: WorkProjectRow[] }) {
  if (projects.length === 0) return null;
  const counts = projects.reduce(
    (acc, p) => ({
      open: acc.open + p.task_counts.open,
      done: acc.done + p.task_counts.done,
      closed: acc.closed + p.task_counts.closed,
    }),
    { open: 0, done: 0, closed: 0 },
  );
  return (
    <>
      <div className="work-facts">
        <Fact label="Projects">{projects.length}</Fact>
        <Fact label="Open">{counts.open}</Fact>
        <Fact label="Done">{counts.done}</Fact>
        <Fact label="Closed">{counts.closed}</Fact>
      </div>
      {projects.length > 1 && (
        <Panel title="Open work by project" icon="columns" padding="normal">
          <BarList
            data={[...projects]
              .sort((a, b) => b.task_counts.open - a.task_counts.open)
              .map((p) => ({
                label: (
                  <span className="row gap-2">
                    <span className="key-mark">{p.key}</span>
                    <span className="truncate">{p.name}</span>
                  </span>
                ),
                value: p.task_counts.open,
                display: p.task_counts.open,
                sub: `${p.task_counts.done} done · ${p.task_counts.closed} closed`,
                color: "var(--viz-1)",
                // A REAL LINK: a project is a page now, so the bar opens in a
                // tab like anything else on this screen.
                href: href(["work", p.key]),
              }))}
            emptyLabel="No project holds any open work."
          />
        </Panel>
      )}
    </>
  );
}

/**
 * One project's own facts — the container half a board cannot say from its rows.
 *
 * ABSENT RATHER THAN EMPTY while the read is in flight: an overview rendered
 * with zeroes is a project that looks unstaffed, unled and out of sprint,
 * which is a conclusion somebody acts on.
 */
export function ProjectHead({
  detail,
  chrome,
}: {
  detail?: WorkProjectDetail | null;
  chrome?: RowChrome;
}) {
  if (!detail) return null;
  const sprint = detail.sprints?.active;
  const pending = detail.sprints?.pending_spillovers ?? [];
  const counts = detail.task_counts;
  const total = counts.open + counts.done + counts.closed;
  const measure = sprint?.figures.measure === "estimate_min" ? "minutes" : "points";
  const committed = sprint ? sprint.figures.committed + sprint.figures.added : 0;

  return (
    <>
      {/* A UNIT THE CHART NO LONGER HAS is a finding, not a blank: it is what
          leaves a project's work routed to nobody. */}
      {!detail.unit.resolved && (
        <Banner tone="caution">
          This project names the unit <span className="mono">{detail.unit.key}</span>, which the
          current org chart does not have — work filed here routes to nobody.
        </Banner>
      )}
      {pending.length > 0 && (
        <Banner tone="caution">
          Sprint{pending.length === 1 ? "" : "s"} {pending.join(", ")} closed with the spillover
          still undecided — the unfinished work is waiting on a lead.
        </Banner>
      )}

      <div className="work-facts">
        {/* A HANDLE IS THE DATABASE'S WORD FOR A PERSON. Every other
            surface in the tree resolves it through the chart and shows the
            name somebody is actually called; this one printed the slug. */}
        <Fact label="Lead">
          {detail.lead.handle ? (
            <SeatChip
              name={chrome?.seatName?.(detail.lead.handle) ?? detail.lead.handle}
              handle={detail.lead.handle}
            />
          ) : (
            <span className="muted">nobody</span>
          )}
        </Fact>
        <Fact label="Unit">
          <span className="truncate">
            {detail.unit.name || detail.unit.key || <span className="muted">none</span>}
          </span>
        </Fact>
        <Fact label="Open">{counts.open}</Fact>
        <Fact label="Done">{counts.done}</Fact>
        {/* THE CENSUS AS A SHAPE. The three numbers say how much; the bar says
            the proportion, which is the fact a reader actually wants — "mostly
            finished" against "mostly ahead" — and it is drawn in the STATUS
            tones the badges use rather than the chart hues, so the same fact
            is not two colours on one screen. */}
        {total > 0 && (
          <Fact label="Census">
            <span className="col" style={{ gap: 4, width: "100%" }}>
              <StackedBar
                segments={[
                  { label: "Open", value: counts.open, color: "var(--info)" },
                  { label: "Done", value: counts.done, color: "var(--positive)" },
                  { label: "Closed", value: counts.closed, color: "var(--viz-other)" },
                ]}
              />
              <Legend
                items={[
                  { label: "Open", color: "var(--info)" },
                  { label: "Done", color: "var(--positive)" },
                  { label: "Closed", color: "var(--viz-other)" },
                ]}
              />
            </span>
          </Fact>
        )}
        {/* THE MEASURE IS ALWAYS NAMED, because a bare "8 / 25" is points to
            one team and minutes to another. */}
        <Fact label={sprint ? `Sprint ${sprint.number} · ${sprint.name}` : "Sprint"}>
          {sprint ? (
            <span className="col" style={{ gap: 4, width: "100%" }}>
              <Meter
                used={sprint.figures.done}
                max={Math.max(1, committed)}
                ariaLabel={`Sprint ${sprint.number} — delivered`}
                right={`${sprint.figures.done} of ${committed} ${measure} · ${sprint.days_remaining}d left`}
                fullMeans="achieved"
              />
            </span>
          ) : (
            // TWO DIFFERENT FACTS, and one sentence used to cover both:
            // "this team does not work in sprints" and "this team is
            // between sprints" send a reader to different places, and the
            // answer tells them apart — a project that runs sprints has a
            // POLICY whether or not one is open right now. It is also a
            // value rather than a sentence, because the five facts beside
            // it are words and numbers.
            <span className="muted">{detail.sprint_policy ? "none running" : "not used"}</span>
          )}
        </Fact>
      </div>
    </>
  );
}

// ---------------------------------------------------------------------------
// The board
// ---------------------------------------------------------------------------

export function Board({
  groups,
  axis,
  chrome,
  detail,
  now,
  workspace,
  selected,
  hrefOf,
  onOpen,
  onOverflow,
}: {
  groups: WorkGroup[];
  axis: string;
  chrome: RowChrome;
  detail?: WorkProjectDetail | null;
  now: number;
  workspace?: boolean;
  selected?: string;
  hrefOf: (row: WorkSummary) => string;
  onOpen: (row: WorkSummary) => void;
  onOverflow: (axis: string, key: string) => void;
}) {
  if (groups.length === 0) return null;
  return (
    <div className="work-board">
      {groups.map((group) => {
        const label = groupLabel(axis, group, {
          statuses: detail?.statuses,
          types: chrome.types,
          tags: detail?.tags,
          seatName: chrome.seatName,
        });
        return (
          <section className="work-col" key={group.key || "—"}>
            <header className="work-col-head">
              {axis === "status" && (
                <i className={`dot ${statusTone(group.key)}`} aria-hidden="true" />
              )}
              {axis === "type" && <TypeIcon type={group.key} types={chrome.types} />}
              <span className="truncate">{label}</span>
              <span className="count-chip">{group.count}</span>
            </header>
            <div className="work-col-body">
              {group.rows.map((row) => (
                <BoardCard
                  key={row.id}
                  row={row}
                  now={now}
                  chrome={chrome}
                  href={hrefOf(row)}
                  selected={selected === row.key}
                  onOpen={() => onOpen(row)}
                  showSprint={workspace}
                />
              ))}
              {group.rows.length === 0 && <div className="work-col-empty">Nothing here</div>}
            </div>
            {/* A COLUMN'S COUNT IS OVER THE WHOLE SET and its rows are a
                slice, so a column of four hundred says four hundred and hands
                back fifty. The rest are reachable rather than merely counted:
                the link is the same query as a list narrowed to this column. */}
            {group.count > group.rows.length && (
              <div className="work-col-foot">
                <a
                  href={href(["work"], { view: "list", group_by: axis, group: group.key })}
                  onClick={(e) => {
                    e.preventDefault();
                    onOverflow(axis, group.key);
                  }}
                >
                  {group.count - group.rows.length} more →
                </a>
              </div>
            )}
          </section>
        );
      })}
    </div>
  );
}

function statusTone(status: string): string {
  return (
    {
      todo: "",
      in_progress: "info",
      in_review: "caution",
      done: "positive",
      closed: "",
      cancelled: "",
    }[status] ?? ""
  );
}

// ---------------------------------------------------------------------------
// The list
// ---------------------------------------------------------------------------

export function List({
  rows,
  groups,
  axis,
  chrome,
  detail,
  now,
  selected,
  hrefOf,
  onOpen,
  onOverflow,
}: {
  rows: WorkSummary[];
  groups: WorkGroup[];
  axis: string;
  chrome: RowChrome;
  detail?: WorkProjectDetail | null;
  now: number;
  selected?: string;
  hrefOf: (row: WorkSummary) => string;
  onOpen: (row: WorkSummary) => void;
  onOverflow: (axis: string, key: string) => void;
}) {
  if (groups.length > 0) {
    return (
      <div className="col gap-3">
        {groups.map((group) => (
          <Panel key={group.key || "—"} padding="none">
            <header className="work-group-head">
              <span className="truncate">
                {groupLabel(axis, group, {
                  statuses: detail?.statuses,
                  types: chrome.types,
                  tags: detail?.tags,
                  seatName: chrome.seatName,
                })}
              </span>
              <span className="count-chip">{group.count}</span>
            </header>
            <RowList
              rows={group.rows}
              now={now}
              chrome={chrome}
              hrefOf={hrefOf}
              onOpen={onOpen}
              selected={selected}
            />
            {group.count > group.rows.length && (
              <div className="work-group-foot">
                <a
                  href={href(["work"], { view: "list", group_by: axis, group: group.key })}
                  onClick={(e) => {
                    e.preventDefault();
                    onOverflow(axis, group.key);
                  }}
                >
                  {group.count - group.rows.length} more →
                </a>
              </div>
            )}
          </Panel>
        ))}
      </div>
    );
  }
  if (rows.length === 0) return null;
  return (
    <Panel padding="none">
      <RowList
        rows={rows}
        now={now}
        chrome={chrome}
        hrefOf={hrefOf}
        onOpen={onOpen}
        selected={selected}
      />
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// The calendar
// ---------------------------------------------------------------------------

export function CalendarView({
  weeks,
  rows,
  month,
  chrome,
  hrefOf,
  onOpen,
  onMonth,
  onToday,
}: {
  weeks: CalendarCell[][];
  rows: WorkSummary[];
  month: string;
  chrome: RowChrome;
  hrefOf: (row: WorkSummary) => string;
  onOpen: (row: WorkSummary) => void;
  onMonth: (month: string) => void;
  onToday: () => void;
}) {
  const buckets = useMemo(() => bucketByDay(rows), [rows]);
  const undated = rows.length - [...buckets.values()].flat().length;
  return (
    <Panel padding="none">
      <div className="work-cal">
        <div className="work-cal-nav">
          <Button
            size="sm"
            variant="ghost"
            icon="chevronLeft"
            title="The month before"
            onClick={() => onMonth(shiftMonth(month, -1))}
          />
          <span className="work-cal-month">{monthLabel(month)}</span>
          <Button
            size="sm"
            variant="ghost"
            icon="chevronRight"
            title="The month after"
            onClick={() => onMonth(shiftMonth(month, 1))}
          />
          <Button size="sm" variant="ghost" onClick={onToday}>
            Today
          </Button>
        </div>
        <div className="work-cal-grid">
          {WEEKDAYS.map((day) => (
            <div className="work-cal-dow" key={day}>
              {day}
            </div>
          ))}
          {weeks.flat().map((cell) => {
            const due = buckets.get(cell.key) ?? [];
            const shownChips = due.slice(0, CALENDAR_CELL_CHIPS);
            return (
              <div
                className={`work-cal-cell${cell.inMonth ? "" : " out"}${cell.today ? " today" : ""}`}
                key={cell.key}
              >
                <span className="work-cal-day">{cell.day}</span>
                {shownChips.map((row) => (
                  <a
                    key={row.id}
                    className="work-cal-chip"
                    data-overdue={row.overdue ? "true" : undefined}
                    data-done={
                      row.status_group === "done" || row.status_group === "closed"
                        ? "true"
                        : undefined
                    }
                    href={hrefOf(row)}
                    title={`${row.key} · ${row.title}`}
                    onClick={(e) => {
                      if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
                      e.preventDefault();
                      onOpen(row);
                    }}
                  >
                    <TypeIcon type={row.type} types={chrome.types} />
                    <span className="work-cal-key">{row.key}</span>
                    <span className="truncate">{row.title}</span>
                  </a>
                ))}
                {due.length > shownChips.length && (
                  <span className="work-cal-more">+{due.length - shownChips.length} more</span>
                )}
              </div>
            );
          })}
        </div>
        {/* THERE IS NO `due=null` IN THE GRAMMAR, so the count of undated work
            is what this answer happens to carry rather than the company's. It
            is said all the same: a reader who cannot see the omission reads
            the month as the whole backlog. */}
        <div className="work-cal-note">
          Only work with a due date appears here — the list shows the rest.
          {undated > 0 ? ` ${undated} of the items on this page carry no due date.` : ""}
        </div>
      </div>
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// The feed
// ---------------------------------------------------------------------------

/**
 * What happened in this container lately.
 *
 * A DIFFERENT QUESTION from what is on the board: the feed is ordered by the
 * log rather than by anything the rows sort on, so a change that moved nothing
 * on screen — a comment, a watcher, a quiet re-type — is still visible. It
 * renders the AUTHORED instant, which is what the writer's clock said and what
 * "yesterday" has to keep meaning.
 */
export function ActivityFeed({ records, now }: { records: WorkActivityRecord[]; now: number }) {
  if (records.length === 0) return null;
  return (
    <Panel title="Recent activity" icon="activity" count={records.length} padding="none">
      {records.map((record) => (
        <div key={record.id} className="work-feed-row">
          <span className="work-feed-when" title={fmtDateTime(record.at)}>
            {relTime(record.at, now)}
          </span>
          <span className="work-feed-kind">
            <Badge outline>{record.kind.replaceAll("_", " ")}</Badge>
          </span>
          <span>
            {record.subject_key ? (
              <a className="mono t-link" href={href(["work", record.subject_key])}>
                {record.subject_key}
              </a>
            ) : (
              <span className="faint">—</span>
            )}
          </span>
          <span className="work-feed-what truncate">{describeChange(record)}</span>
          <span className="work-feed-who">{record.actor || "the engine"}</span>
        </div>
      ))}
    </Panel>
  );
}

export { describeChange, projectKeys, shownRows, typeName, StatusBadge };
