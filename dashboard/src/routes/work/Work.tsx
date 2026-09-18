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

import { useCallback, useMemo, type ReactNode } from "react";
import { buildHash, href, useParam, useRoute } from "~/app/router.tsx";
import { peekHref, rowPeekHandler, usePeek, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
// THE HEADER'S FACT IS NOT THE TRACKER'S. `components/work.tsx` exports a
// `Fact` that DRAWS one in the overview strip; this one is the VALUE an
// [ObjectHeader] renders. Both are right and neither may be renamed for the
// other's benefit, so the type is aliased where it is imported.
import { ObjectHeader, type Fact as HeaderFact } from "~/app/frame/ObjectHeader.tsx";
import { NumberCell } from "~/app/frame/cells.tsx";
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
import { TableView, removalsOf } from "~/components/worktable.tsx";
import {
  BarList,
  Button,
  Callout,
  Card,
  cx,
  DATA_COLOR_OTHER,
  dataColor,
  EmptyState,
  EmptyValue,
  FilterChip,
  IconButton,
  Input,
  Legend,
  Select,
  Skeleton,
  StackedBar,
  Tabs,
  Tag,
} from "@crewlethq/ui";
import {
  CalendarTodayGlyph,
  ChevronLeftGlyph,
  ChevronRightGlyph,
  CloseGlyph,
  DashboardGlyph,
  DeleteGlyph,
  ListGlyph,
  SearchGlyph,
  TimelineGlyph,
  ViewColumnGlyph,
} from "@crewlethq/icons/glyphs";
// OURS, DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics`: `radio` selects as the arrows move, `tabs` is manual but
// demands a `panelId` naming a TabPanel this row does not control. The scope
// row drives a `useParam` that re-runs the board's query, which is exactly
// what our own `activate="manual"` exists for — arrowing across three options
// would ask the engine three times.
import { Segmented } from "~/ui/primitives.tsx";
// AND OURS FOR THE METER, because the sprint bar below carries a VALUE and no
// visible label: their legend is all-or-nothing (`hideLabel` hides the value
// text with the label), and their `meterTone` welds in the polarity "full is
// bad", so a delivered sprint would draw red. See the report.
import { Meter } from "~/ui/primitives.tsx";
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
  seededScope,
  shapeOf,
  shiftMonth,
  shownRows,
  SORTS,
  STATUSES,
  STATUS_TONE,
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
  WorkActiveSprint,
  WorkActivityRecord,
  WorkGroup,
  WorkProjectDetail,
  WorkProjectRow,
  WorkSummary,
  WorkTaskCounts,
  WorkView,
} from "~/protocol/index.ts";

/** The glyph a view's tab wears, by what it draws.
 *
 *  A COMPONENT RATHER THAN A NAME, which is the whole of the icon change: the
 *  tab strip takes a rendered node, so the table holds the glyphs themselves
 *  and an unknown shape falls through to the list mark. */
const VIEW_GLYPH = {
  list: ListGlyph,
  board: DashboardGlyph,
  calendar: CalendarTodayGlyph,
  timeline: TimelineGlyph,
  table: ViewColumnGlyph,
} as const;

/** The builtin tabs whose SHAPE is not what they are about. */
const BUILTIN_GLYPH: Record<string, (typeof VIEW_GLYPH)[keyof typeof VIEW_GLYPH]> = {
  trash: DeleteGlyph,
};

/**
 * THE KEY BEFORE THE SHAPE, and only for a builtin row.
 *
 * The trash IS a table — `removed=true` is what makes it the trash — so a
 * strip keyed on the type alone drew the two tabs with one glyph, which is a
 * strip where the one destructive-looking tab is indistinguishable from the
 * one beside it. `builtin` gates it because a saved view somebody happens to
 * name `trash` is theirs, not the engine's.
 */
function viewGlyph(view: WorkView): ReactNode {
  const Glyph =
    (view.builtin ? BUILTIN_GLYPH[view.key] : undefined) ??
    VIEW_GLYPH[view.type as keyof typeof VIEW_GLYPH] ??
    ListGlyph;
  return <Glyph size="sm" />;
}

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
  // THE WHOLE ADDRESS, for the controls that PATCH it rather than replace it —
  // see [patchedHref]. `useParam` answers one key at a time and none of them
  // is the project, which is a path segment.
  const route = useRoute();

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
  // `active` alone — reads as OPEN, which is [seededScope]'s fallback and
  // therefore the one the suite can reach. The segment and the query still
  // agree, which is the property that matters; they agree on
  // `not_started,active`, the wider set the segment stands for, rather than on
  // the narrower one the view named.
  const viewScope = seededScope(viewParams(chosenView, views));
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

  // A TRASH LISTING IS THE ONE VIEW WHOSE ROWS DO NOT CARRY THEIR OWN STORY.
  // The row says a task is removed; WHO removed it, WHEN, and whether the
  // removal rode along with a parent's are facts about the COMMIT, which live
  // in the history — a row carrying them would be a document read per card,
  // which is exactly what the list read is built not to do.
  //
  // And a PURGE has no row at all, by construction: the rows are destroyed and
  // the history entry is the only trace that the work ever existed. So the
  // three kinds are asked for together, on the same page, rather than the
  // removals being read from the rows and the purges from somewhere else.
  //
  // KEYED ON THE PARAMETER, not on the view key. `removed=true` is what makes
  // a listing the trash — the builtin tab is a table carrying it — so a view
  // somebody saves with that parameter is read exactly the same way.
  const inTrash = params.removed === "true" || params.removed === true;
  const tombstones = useQuery(
    "work_activity",
    inTrash ? { container, kinds: "removed,restored,purged", limit: 100 } : undefined,
    { enabled: inTrash, pollMs: 60_000 },
  );
  const tombRecords = useMemo(() => tombstones.data?.records ?? [], [tombstones.data]);
  const removals = useMemo(() => removalsOf(tombRecords), [tombRecords]);
  const purges = useMemo(() => tombRecords.filter((r) => r.kind === "purged"), [tombRecords]);

  const groups = useMemo(() => data?.groups ?? [], [data]);
  const rows = useMemo(() => data?.items ?? [], [data]);
  // THE WORKSPACE HEAD'S OWN ORDER, read back here so the stepper walks the
  // bars as they are drawn — see [projectsByOpen].
  const projectRows = useMemo(() => projectsByOpen(projects.data?.projects ?? []), [projects.data]);
  // WHAT `[` AND `]` WALK: the rows this board actually loaded, sorted and
  // filtered as the reader left them. Published rather than handed to the
  // rail, because only the list knows that order — see `PeekHost`.
  //
  // TWO LISTS ARE ON THIS SCREEN and only one of them was stepped into: the
  // board's items, and the workspace head's projects. The RAIL'S OWN KIND is
  // what tells them apart — a project peek can only have been opened from the
  // project list, so `]` there means the next project. The alternatives are
  // both worse: one array holding both would step off the last item into the
  // first project, which is not a sequence anybody is looking at, and two
  // `usePeekNeighbours` calls would be two hooks racing to own one stepper.
  const neighbours = useMemo(
    () =>
      !project && peek?.kind === "project"
        ? projectRows.map((p) => ({ kind: "project" as const, id: p.key }))
        : rows.map((r) => ({ kind: "item" as const, id: r.key })),
    [project, peek?.kind, projectRows, rows],
  );
  usePeekNeighbours(neighbours);
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

  // WHERE A COLUMN FOOTER GOES, as the link the browser follows on a middle
  // click and shows in the status bar. Built here because this is the only
  // frame that knows the whole address: `route.path` carries the project
  // segment the board is narrowed to, and `route.query` carries the filters
  // the reader has set. See [patchedHref].
  //
  // TWO PATCHES, NOT ONE, because the two controls do different things. A
  // board column switches the screen to the list AND groups it; a list column
  // is already a list and must keep whatever saved view is running, so
  // rewriting `view=` there would throw that view away.
  const boardOverflowHref = useCallback(
    (axis: string, key: string) =>
      patchedHref(route.path, route.query, filterPatchForGroup(axis, key)),
    [route.path, route.query],
  );
  const listOverflowHref = useCallback(
    (axis: string, key: string) =>
      patchedHref(route.path, route.query, { group_by: axis, group: key }),
    [route.path, route.query],
  );

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
          <WorkspaceHead
            projects={projects.data?.projects ?? []}
            // A PROJECT IS AN OBJECT WITH A PEEK, so a plain click on a bar
            // opens it beside the board rather than leaving for its page —
            // the same gesture every item row on this screen already makes.
            // Passed in rather than taken from the frame inside the head,
            // because these two components take an `href` and an `onOpen` and
            // need no router: see `components/work.tsx`.
            onOpen={(key) => openPeek({ kind: "project", id: key })}
          />
        )}

        {views.length > 0 && (
          <Tabs
            ariaLabel="Views"
            value={chosenView}
            onValueChange={(key) => applyView(views.find((v) => v.key === key) ?? views[0]!)}
            items={views.map((v) => ({
              value: v.key,
              label: `${v.pinned ? "★ " : ""}${v.name}`,
              icon: viewGlyph(v),
            }))}
          />
        )}

        <div className="work-filters">
          <div className="work-filters-search">
            {/* THEIR FIELD, WITH THE GLYPH IN ITS LEADING SLOT — which is
                what our own `SearchInput` was: an input, a mark and a name. */}
            <Input
              type="search"
              width="full"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              aria-label="Find an item by key or title"
              leading={<SearchGlyph size="sm" />}
              placeholder="Key or title"
            />
          </div>
          {/* EVERY "ANY" ROW IS AN OPTION RATHER THAN A PLACEHOLDER. Their
              `placeholder` only labels an empty trigger, and a reader who has
              chosen needs a row to choose their way back out of. */}
          <Select
            // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
            // `width: 100%` unless told otherwise, and its own doc says why that
            // is wrong here: "a filter row of full-width selects is one question
            // per line, which is not what a filter bar is".
            width="auto"
            value={status}
            onChange={(value) => setStatus(String(value))}
            ariaLabel="Status"
            active={status !== ""}
            options={[
              { value: "", label: "Any status" },
              ...STATUSES.map((s) => ({
                value: s.value,
                label: statusLabel(s.value, detail?.statuses),
              })),
            ]}
          />
          {(catalogue.data?.types ?? []).length > 0 && (
            <Select
              // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
              // `width: 100%` unless told otherwise, and its own doc says why that
              // is wrong here: "a filter row of full-width selects is one question
              // per line, which is not what a filter bar is".
              width="auto"
              value={type}
              onChange={(value) => setType(String(value))}
              ariaLabel="Type"
              active={type !== ""}
              options={[
                { value: "", label: "Any type" },
                ...(catalogue.data?.types ?? [])
                  .filter((t) => !t.archived)
                  .map((t) => ({ value: t.slug, label: t.name })),
              ]}
            />
          )}
          <Select
            // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
            // `width: 100%` unless told otherwise, and its own doc says why that
            // is wrong here: "a filter row of full-width selects is one question
            // per line, which is not what a filter bar is".
            width="auto"
            value={priority}
            onChange={(value) => setPriority(String(value))}
            ariaLabel="Priority"
            active={priority !== ""}
            options={[
              { value: "", label: "Any priority" },
              ...PRIORITIES.map((p) => ({ value: p, label: p })),
            ]}
          />
          <Select
            // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
            // `width: 100%` unless told otherwise, and its own doc says why that
            // is wrong here: "a filter row of full-width selects is one question
            // per line, which is not what a filter bar is".
            width="auto"
            value={assignee}
            onChange={(value) => setAssignee(String(value))}
            ariaLabel="Assignee"
            active={assignee !== ""}
            options={[
              { value: "", label: "Anybody" },
              // UNASSIGNED IS A VALUE, not a missing filter — it is the one
              // question a lead actually opens a board to ask.
              { value: "none", label: "Unassigned" },
              ...index.seats.map((s) => ({ value: s.handle, label: s.name })),
            ]}
          />
          {project && detail?.sprints && (
            <Select
              // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
              // `width: 100%` unless told otherwise, and its own doc says why that
              // is wrong here: "a filter row of full-width selects is one question
              // per line, which is not what a filter bar is".
              width="auto"
              value={sprint}
              onChange={(value) => setSprint(String(value))}
              ariaLabel="Sprint"
              active={sprint !== ""}
              options={[
                { value: "", label: "Any sprint" },
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
              // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
              // `width: 100%` unless told otherwise, and its own doc says why that
              // is wrong here: "a filter row of full-width selects is one question
              // per line, which is not what a filter bar is".
              width="auto"
              value={groupBy}
              onChange={(value) => {
                setGroupBy(String(value));
                // A COLUMN FILTER BELONGS TO ITS AXIS. Left behind when the
                // axis changes it narrows the board to a key the new axis
                // has never heard of, which answers nothing.
                setGroup("");
              }}
              ariaLabel="Group by"
              active={groupBy !== ""}
              options={[
                { value: "", label: shape === "board" ? "By status" : "No grouping" },
                ...GROUP_AXES.filter((a) => !a.projectOnly || project).map((a) => ({
                  value: a.value,
                  label: a.label,
                })),
              ]}
            />
          )}
          {shape === "list" && (
            <Select
              // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
              // `width: 100%` unless told otherwise, and its own doc says why that
              // is wrong here: "a filter row of full-width selects is one question
              // per line, which is not what a filter bar is".
              width="auto"
              value={sort}
              onChange={(value) => setSort(String(value))}
              ariaLabel="Sort"
              active={sort !== ""}
              options={[{ value: "", label: "Default order" }, ...SORTS]}
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
          <FilterChip
            pressed={filters.blocked}
            onClick={() => setBlocked(filters.blocked ? "" : "true")}
            title="Only work that cannot move"
          >
            Blocked
          </FilterChip>
          <FilterChip
            pressed={filters.overdue}
            onClick={() => setOverdue(filters.overdue ? "" : "true")}
            title="Only open work past its due date"
          >
            Overdue
          </FilterChip>
          {anyFilter(filters) && (
            <Button
              size="small"
              variant="tertiary"
              leadingIcon={<CloseGlyph size="sm" />}
              onClick={clearFilters}
            >
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
          <Callout
            variant="info"
            action={
              <Button size="small" variant="tertiary" onClick={() => setGroup("")}>
                Show every column
              </Button>
            }
          >
            Showing one column of this board.
          </Callout>
        )}
        {data?.groups_overlap && (
          <Callout variant="info">
            One item can be on several of these columns, so the counts add up to more than the
            total.
          </Callout>
        )}
        {data?.groups_dropped ? (
          <Callout variant="warning">
            {data.groups_dropped} more column{data.groups_dropped === 1 ? "" : "s"} did not fit and
            are not shown. Narrow the board to bring them into range.
          </Callout>
        ) : null}

        <div className="work-body">
          <div className="col gap-4" style={{ minWidth: 0 }}>
            {loading && !data && <Skeleton variant="text" rows={6} label="Loading the board" />}

            <QueryState
              error={error}
              loading={loading}
              empty={
                // A TRASH WITH NOTHING IN IT IS NOT A FILTER THAT MATCHED
                // NOTHING, and it is not empty either: a purge leaves no row
                // and its history entry is the only trace the work ever
                // existed, so the band below is the whole answer on a company
                // whose removals have all been purged. Suppressing the screen
                // for it would replace that answer with "widen your filters".
                shown.length || groups.length || inTrash
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
                  overflowHref={boardOverflowHref}
                />
              )}
              {shape === "list" && (
                <List
                  rows={rows}
                  groups={groups}
                  // THE AXIS THE QUERY WAS SENT ON, not the one in the URL —
                  // exactly as [Board] above. A saved view may carry
                  // `group_by`, which [buildItemsParams] resolves as
                  // `filters.groupBy || view.group_by`, so a view that groups
                  // with no key in the URL answered grouped while this read
                  // `""` — and `groupLabel` with no axis falls through to the
                  // raw key, heading the columns `ada-okonkwo` and
                  // `in_progress` instead of the person's name and the team's
                  // word for the status.
                  axis={String(params.group_by ?? "")}
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
                  overflowHref={listOverflowHref}
                />
              )}
              {shape === "table" && (
                <TableView
                  rows={rows}
                  groups={groups}
                  axis={String(params.group_by ?? "")}
                  chrome={chrome}
                  detail={detail}
                  now={now}
                  workspace={!project}
                  selected={peek?.kind === "item" ? peek.id : ""}
                  hrefOf={itemHref}
                  onOpen={(row) => openPeek({ kind: "item", id: row.key })}
                  removals={inTrash ? removals : undefined}
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

            {inTrash && <PurgeBand records={purges} now={now} />}
            <ActivityFeed records={feed.data?.records ?? []} now={now} />
          </div>
        </div>

        {/* "THIS COMPANY HAS FILED NOTHING" IS THE PROJECT LIST'S ANSWER, so
            it is gated on the project list's own state. It was gated on the
            ITEMS query instead — a different question, usually answering
            fine — so a `work_projects` that was refused, timed out, or had
            simply not landed yet drew a positive statement that the company
            had nothing, over a read that failed. A reader acts on that, by
            filing the duplicate. [QueryState] is what puts the refusal ahead
            of the empty state; it renders nothing at all once projects are
            listed. */}
        <QueryState
          error={projects.error}
          loading={projects.loading}
          empty={
            (projects.data?.projects ?? []).length === 0
              ? {
                  title: "No work has been filed yet",
                  hint: "Seats file work with create_work_item, and an inbound webhook or a schedule is usually what starts them. A project appears here the moment a unit in the company config declares its `project` key.",
                }
              : undefined
          }
        />
      </div>
    </>
  );
}

// ---------------------------------------------------------------------------
// The rail
// ---------------------------------------------------------------------------

/**
 * One project, beside the list it was opened from.
 *
 * # What it answers that the row could not
 *
 * The workspace head ranks projects by open work and says how much there is —
 * a bar, three numbers and a name. That is enough to CHOOSE a project and
 * nothing like enough to RECOGNISE one: who leads it, which unit owns it,
 * whether a sprint is running and whether anything has happened this week are
 * facts about the CONTAINER, and a bar carries none of them. So the header is
 * the same facts the project's own page wears — from [projectFacts], so the
 * two cannot drift — and under it the three questions a reader opens a
 * project for: where the work stands, what the sprint is, and what changed.
 *
 * # Why the breakdown asks only about OPEN work
 *
 * `task_counts` is MAINTAINED by the applier on every status-group change, so
 * the census costs the same to read at any project size. A breakdown per
 * STATUS is maintained nowhere and has to be grouped at read time — and asked
 * over the finished work too it would count every task the team has ever
 * closed, on a poll, to draw two rows whose totals are already on the census.
 * The open statuses are the ones that move.
 */
export function ProjectPeek({ projectKey }: { projectKey: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();

  // NAMED FOR WHAT IT IS, not for the slot it arrives in. The rail hands every
  // peek an opaque `id` because the frame knows nothing about any kind
  // (`app/frame/peeks.tsx`), and a project's is its KEY — the string a unit
  // declares and a person types. Every other peek does the same, which is why
  // `ItemPeek` takes an `itemKey` and `SeatPeek` a `handle`: inside the
  // component the word has to be the one the engine's own parameter uses, or
  // the reader of `{ key: id }` has to go and check which id that is.
  const state = useQuery(
    "work_project",
    { key: projectKey },
    { enabled: projectKey !== "", pollMs: 60_000 },
  );
  const detail = state.data;
  // `group_limit: 1` because only the COUNTS are drawn here and the engine
  // runs one paged statement per column — a column's rows are exactly what
  // this panel does not show, and zero is not a bound the grammar accepts.
  const census = useQuery(
    "work_items",
    { container: `project:${projectKey}`, group_by: "status", group_limit: 1 },
    { enabled: projectKey !== "", pollMs: 60_000 },
  );
  // AND WHAT HAPPENED, which is a different question from what is there — the
  // feed is ordered by the LOG, so a change that moved nothing still shows.
  const feed = useQuery(
    "work_activity",
    { container: `project:${projectKey}`, limit: 8 },
    { enabled: projectKey !== "", pollMs: 60_000 },
  );

  const chrome: RowChrome = {
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
    types: detail?.types,
    statuses: detail?.statuses,
  };

  // NOT THE GENERIC "there is no such record" that `QueryState` draws for a
  // `not_found`. A project key reaches this from a pasted URL and from a
  // bookmark as often as from a bar, and WHICH key resolved to nothing is
  // precisely the half the generic banner drops.
  if (state.error === "not_found") return <NoSuchProject projectKey={projectKey} />;

  const records = feed.data?.records ?? [];

  return (
    <>
      {state.loading && !state.data && (
        <Skeleton variant="text" rows={6} label="Loading the project" />
      )}
      <QueryState error={state.error} loading={state.loading}>
        {detail && (
          <>
            <ObjectHeader
              size="peek"
              kind="Project"
              icon="view_column"
              identifier={detail.key}
              title={detail.name}
              status={detail.archived ? <Tag appearance="outline">archived</Tag> : undefined}
              facts={projectFacts(detail, chrome)}
            />
            <div className="col gap-3">
              <Coverage answer={detail} />
              <ProjectBanners detail={detail} />
              {/* WHY THIS PROJECT EXISTS, drawn whenever somebody wrote it:
                  "is this the one I meant" is the question the rail answers,
                  and a purpose is the sentence that answers it. */}
              {detail.purpose && <p className="t-body measure">{detail.purpose}</p>}

              <Card>
                <Card.Header icon={<DashboardGlyph size="sm" />}>
                  <Card.Title>Where the work stands</Card.Title>
                </Card.Header>
                <div className="col gap-3">
                  {/* A BAR OF NOTHING IS NOT A CENSUS — three zero segments
                      draw an empty track that reads as a chart which failed
                      to load rather than as a project nobody has filed
                      anything in. */}
                  {detail.task_counts.open + detail.task_counts.done + detail.task_counts.closed >
                  0 ? (
                    <ProjectCensus counts={detail.task_counts} />
                  ) : (
                    <span className="muted">No work has been filed in this project yet.</span>
                  )}
                  {/* A FAILED BREAKDOWN IS NOT AN EMPTY ONE. This is the only
                      read that can say how the open work is distributed, so a
                      refusal rendered as no rows would read as a project whose
                      every task is in one status. */}
                  <QueryState error={census.error} loading={census.loading}>
                    {census.data && (
                      <div className="col">
                        {statusCounts(census.data.groups ?? []).map((line) => (
                          <div className="work-col-head" key={line.status}>
                            <i className={statusDot(line.status)} aria-hidden="true" />
                            <span className="truncate">
                              {statusLabel(line.status, detail.statuses)}
                            </span>
                            <span className="count-chip">{line.count}</span>
                          </div>
                        ))}
                        <span className="t-caption">
                          Open work only — the {detail.task_counts.done} done and{" "}
                          {detail.task_counts.closed} closed are in the census above.
                        </span>
                      </div>
                    )}
                  </QueryState>
                </div>
              </Card>

              <Card>
                <Card.Header
                  icon={<CalendarTodayGlyph size="sm" />}
                  subtitle="what the team committed to, and how much of it has landed"
                >
                  <Card.Title>{sprintLabel(detail)}</Card.Title>
                </Card.Header>
                <SprintFigure detail={detail} />
              </Card>

              <Card>
                <Card.Header
                  icon={<TimelineGlyph size="sm" />}
                  // A NULL COUNT IS NO COUNT, which is the read still being in
                  // flight rather than a feed with nothing in it. Theirs takes
                  // `undefined` for that, so the null is converted here.
                  count={feed.data ? records.length : undefined}
                >
                  <Card.Title>Recent activity</Card.Title>
                </Card.Header>
                <QueryState error={feed.error} loading={feed.loading}>
                  {feed.data &&
                    (records.length > 0 ? (
                      <div className="col gap-2">
                        {records.map((record) => (
                          <div className="col gap-1" key={record.id}>
                            <div className="row gap-2">
                              {record.subject_key ? (
                                <a
                                  className="mono t-link"
                                  href={href(["work", record.subject_key])}
                                >
                                  {record.subject_key}
                                </a>
                              ) : (
                                <EmptyValue label="No work item" />
                              )}
                              <span className="spacer" />
                              <span className="t-caption" title={fmtDateTime(record.at)}>
                                {relTime(record.at, now)}
                              </span>
                            </div>
                            <span className="t-caption truncate">
                              {describeChange(record)} · {record.actor || "the engine"}
                            </span>
                          </div>
                        ))}
                      </div>
                    ) : (
                      <span className="muted">Nothing has changed in this project yet.</span>
                    ))}
                </QueryState>
              </Card>
            </div>
          </>
        )}
      </QueryState>
    </>
  );
}

/**
 * NOT AN EMPTY RAIL, and not a spinner that never resolves.
 *
 * A project key that resolves to nothing is almost always a key that MOVED or
 * was never real — the engine's own refusal names the nearest ones — so the
 * honest answer prints the key that failed and says what its absence means.
 */
function NoSuchProject({ projectKey }: { projectKey: string }) {
  return (
    <EmptyState
      size="compact"
      icon={<DashboardGlyph size="xl" />}
      title={`No project called “${projectKey}”`}
      description="A project key is declared by a unit in the company configuration. Either it never existed, or the unit that declared it has since been renamed — the workspace list is what this company actually has."
    />
  );
}

// ---------------------------------------------------------------------------
// The two heads
// ---------------------------------------------------------------------------

/**
 * The projects in the order the workspace head draws them.
 *
 * ONE FUNCTION, because two things read this order: the bars, and the sequence
 * `[` and `]` step through. A screen that published one order and rendered
 * another would step off the row the reader clicked into somewhere else.
 */
function projectsByOpen(projects: WorkProjectRow[]): WorkProjectRow[] {
  return [...projects].sort((a, b) => b.task_counts.open - a.task_counts.open);
}

/**
 * The company's work at a glance, ranked by where the open work is.
 *
 * ONE HUE FOR EVERY BAR. A hue per project would be identity colouring by row
 * — rename the project and its colour changes, which is the proof it never
 * meant anything — and the label already carries which project it is. The
 * chart is absent below two projects, because a one-bar chart is a number.
 */
export function WorkspaceHead({
  projects,
  onOpen,
}: {
  projects: WorkProjectRow[];
  /** A plain click on a project. Absent leaves each bar an ordinary link. */
  onOpen?: (key: string) => void;
}) {
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
        <Card>
          <Card.Header icon={<DashboardGlyph size="sm" />}>
            <Card.Title>Open work by project</Card.Title>
          </Card.Header>
          <BarList
            data={projectsByOpen(projects).map((p) => ({
              // THEIR BAR IS KEYED ON AN `id`, and a project key is the one
              // stable thing a row here has.
              id: p.key,
              // THE LINK IS THE LABEL, not the bar.
              //
              // A row that carried both an `href` and an `onSelect` would do
              // BOTH on a plain click: `BarDatum` hands its `onSelect` no
              // event, so nothing there can call `preventDefault`, and the
              // anchor's own navigation would land on the project's page a
              // moment after the rail opened. Moving the anchor inside the
              // label puts the whole rule back under `rowPeekHandler` — the
              // frame's one copy of which clicks mean "open elsewhere" — so a
              // plain click peeks and ⌘-click, the middle button and the
              // status bar all keep meaning the page.
              label: (
                <a
                  className="row gap-2"
                  href={peekHref({ kind: "project", id: p.key })}
                  onClick={rowPeekHandler(onOpen ? () => onOpen(p.key) : undefined)}
                >
                  <span className="key-mark">{p.key}</span>
                  <span className="truncate">{p.name}</span>
                </a>
              ),
              value: p.task_counts.open,
              display: p.task_counts.open,
              sub: `${p.task_counts.done} done · ${p.task_counts.closed} closed`,
              color: dataColor(0),
            }))}
            emptyLabel="No project holds any open work."
          />
        </Card>
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
  const counts = detail.task_counts;
  const total = counts.open + counts.done + counts.closed;

  return (
    <>
      <ProjectBanners detail={detail} />

      {/* A PROJECT IS AN OBJECT, so it wears the header every other object
          wears rather than a title block of its own — the same facts in the
          same order its peek shows, from [projectFacts]. What stays below is
          what a fact line cannot hold: a bar inside `.fact-value` is a bar in
          a `truncate`d inline box, which is no bar at all. */}
      <ObjectHeader
        kind="Project"
        icon="view_column"
        identifier={detail.key}
        title={detail.name}
        status={detail.archived ? <Tag appearance="outline">archived</Tag> : undefined}
        facts={projectFacts(detail, chrome)}
      />
      {detail.purpose && <p className="t-body measure">{detail.purpose}</p>}

      <div className="work-facts">
        {/* THE CENSUS AS A SHAPE. The three numbers say how much; the bar says
            the proportion, which is the fact a reader actually wants — "mostly
            finished" against "mostly ahead". */}
        {total > 0 && (
          <Fact label="Census">
            <span className="col gap-1" style={{ width: "100%" }}>
              <ProjectCensus counts={counts} />
            </span>
          </Fact>
        )}
        <Fact label={sprintLabel(detail)}>
          <SprintFigure detail={detail} />
        </Fact>
      </div>
    </>
  );
}

/**
 * The six facts a project is read by, in one order, on its page and in the rail.
 *
 * ONE FUNCTION rather than two lists that happen to agree today: a reader
 * scans the same facts in the same order wherever the object appears, and a
 * header written twice is two orders as soon as somebody adds a seventh.
 */
function projectFacts(detail: WorkProjectDetail, chrome?: RowChrome): HeaderFact[] {
  const sprint = detail.sprints?.active;
  return [
    {
      label: "Lead",
      // A HANDLE IS THE DATABASE'S WORD FOR A PERSON. Every other surface in
      // the tree resolves it through the chart and shows the name somebody is
      // actually called; this one printed the slug.
      value: detail.lead.handle ? (
        <SeatChip
          name={chrome?.seatName?.(detail.lead.handle) ?? detail.lead.handle}
          handle={detail.lead.handle}
        />
      ) : (
        <span className="muted">nobody</span>
      ),
    },
    {
      label: "Unit",
      value: detail.unit.name || detail.unit.key || <span className="muted">none</span>,
    },
    // THE THREE COUNTS ARE CELLS, not bare numbers: a project with nothing
    // open is the one an operator is most often looking for here, and a zero
    // rendered as a blank or as a dash is the one reading that hides it.
    { label: "Open", value: <NumberCell value={detail.task_counts.open} /> },
    { label: "Done", value: <NumberCell value={detail.task_counts.done} /> },
    { label: "Closed", value: <NumberCell value={detail.task_counts.closed} /> },
    // AND NO SPRINT IS NO VALUE. Which KIND of none it is — between sprints,
    // or a team that does not use them — is a sentence rather than a fact,
    // and [SprintFigure] below is where it is said; `FactLine` drops a fact
    // with an empty value rather than printing a word for it.
    { label: "Sprint", value: sprint ? sprintName(sprint) : "" },
  ];
}

/** The two findings a project's own record can carry, on the page and in the rail. */
function ProjectBanners({ detail }: { detail: WorkProjectDetail }) {
  const pending = detail.sprints?.pending_spillovers ?? [];
  return (
    <>
      {/* A UNIT THE CHART NO LONGER HAS is a finding, not a blank: it is what
          leaves a project's work routed to nobody. */}
      {!detail.unit.resolved && (
        <Callout variant="warning">
          This project names the unit <span className="mono">{detail.unit.key}</span>, which the
          current org chart does not have — work filed here routes to nobody.
        </Callout>
      )}
      {pending.length > 0 && (
        <Callout variant="warning">
          Sprint{pending.length === 1 ? "" : "s"} {pending.join(", ")} closed with the spillover
          still undecided — the unfinished work is waiting on a lead.
        </Callout>
      )}
    </>
  );
}

/**
 * A project's census, drawn once.
 *
 * IN THE STATUS TONES the badges use rather than the chart hues, so the same
 * fact is not two colours on one screen — and NEVER WITHOUT ITS LEGEND, since
 * an unlabelled stack of three colours is three colours.
 */
function ProjectCensus({ counts }: { counts: WorkTaskCounts }) {
  // AN `id` PER SEGMENT, which is what both of their components key on — and
  // it is the status word rather than the position, so a census that gains a
  // fourth group later does not renumber the three that were there.
  const segments = [
    { id: "open", label: "Open", value: counts.open, color: "var(--info)" },
    { id: "done", label: "Done", value: counts.done, color: "var(--positive)" },
    { id: "closed", label: "Closed", value: counts.closed, color: DATA_COLOR_OTHER },
  ];
  return (
    <>
      <StackedBar segments={segments} />
      <Legend items={segments.map(({ id, label, color }) => ({ id, label, color }))} />
    </>
  );
}

/** A running sprint names itself; the number and the name are one string. */
function sprintName(sprint: WorkActiveSprint): string {
  return `${sprint.number} · ${sprint.name}`;
}

/** What the sprint block is CALLED, on the page's fact and on the rail's panel. */
function sprintLabel(detail: WorkProjectDetail): string {
  const sprint = detail.sprints?.active;
  return sprint ? `Sprint ${sprintName(sprint)}` : "Sprint";
}

/**
 * How much of the sprint has landed, or which kind of no-sprint this is.
 *
 * THE MEASURE IS ALWAYS NAMED, because a bare "8 / 25" is points to one team
 * and minutes to another.
 *
 * TWO DIFFERENT FACTS, and one sentence used to cover both: "this team does
 * not work in sprints" and "this team is between sprints" send a reader to
 * different places, and the answer tells them apart — a project that runs
 * sprints carries a POLICY whether or not one is open right now.
 */
function SprintFigure({ detail }: { detail: WorkProjectDetail }) {
  const sprint = detail.sprints?.active;
  if (!sprint) {
    return <span className="muted">{detail.sprint_policy ? "none running" : "not used"}</span>;
  }
  const measure = sprint.figures.measure === "estimate_min" ? "minutes" : "points";
  const committed = sprint.figures.committed + sprint.figures.added;
  return (
    <span className="col gap-1" style={{ width: "100%" }}>
      <Meter
        used={sprint.figures.done}
        max={Math.max(1, committed)}
        ariaLabel={`Sprint ${sprint.number} — delivered`}
        right={`${sprint.figures.done} of ${committed} ${measure} · ${sprint.days_remaining}d left`}
        fullMeans="achieved"
      />
    </span>
  );
}

/**
 * How the open work is distributed, as lines in the board's own order.
 *
 * THE CANONICAL ORDER FIRST, so the panel does not reshuffle between polls as
 * a status empties, and A STATUS WITH NO WORK IS A ZERO rather than an
 * absence: the query asked over the whole project, so a status nothing matched
 * is a count of none — not a fact nobody recorded.
 *
 * AND ANYTHING THIS BUILD HAS NOT HEARD OF is appended rather than dropped.
 * The wire evolves additively and a newer node may name a status this build
 * does not know; a column silently missing from a census is the one shape a
 * reader cannot notice.
 */
function statusCounts(groups: WorkGroup[]): { status: string; count: number }[] {
  const counted = new Map(groups.map((group) => [group.key, group.count]));
  const lines = OPEN_STATUSES.map((status) => ({ status, count: counted.get(status) ?? 0 }));
  for (const group of groups) {
    if (!OPEN_STATUSES.includes(group.key)) {
      lines.push({ status: group.key, count: group.count });
    }
  }
  return lines;
}

/** The statuses a task can be in while it is still somebody's problem. */
const OPEN_STATUSES: string[] = STATUSES.filter(
  (status) => status.group === "not_started" || status.group === "active",
).map((status) => status.value);

// ---------------------------------------------------------------------------
// The board
// ---------------------------------------------------------------------------

/**
 * Where a control that PATCHES THIS SCREEN points, written as an href.
 *
 * THE CLICK AND THE LINK HAVE TO NAME THE SAME PLACE. A column footer's
 * `onClick` moves two or three query keys and leaves everything else alone —
 * the project is a path segment and every live filter is a key beside them —
 * but a middle click never reaches it (the browser dispatches `auxclick`,
 * which React's `onClick` does not see), and the status bar and "copy link
 * address" read the href verbatim. Spelled as a bare `href(["work"], patch)`
 * the two disagreed: from `#/work/ENG?assignee=ada` the anchor named the
 * company-wide board with the assignee dropped — every project's todo column
 * merged, which is not the set the "52 more" was counting.
 *
 * PURE, over the route's two halves rather than over the hook, so the rule is
 * testable beside the board it serves.
 */
export function patchedHref(
  path: string[],
  query: URLSearchParams,
  patch: Record<string, string>,
): string {
  const next = new URLSearchParams(query);
  for (const [key, value] of Object.entries(patch)) {
    // An empty value is the key's ABSENCE on this screen — `useParam` drops it
    // rather than writing `group_by=` — so clearing one here must drop it too.
    if (value === "") next.delete(key);
    else next.set(key, value);
  }
  return buildHash(path, next);
}

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
  overflowHref,
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
  /** Where the column footer GOES — the same pairing as `hrefOf`/`onOpen`,
   *  and for the same reason: the screen owns the address, because only it
   *  knows which project and which filters the reader is looking at. */
  overflowHref: (axis: string, key: string) => string;
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
              {axis === "status" && <i className={statusDot(group.key)} aria-hidden="true" />}
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
                the link is THIS screen — this project, these filters — as a
                list narrowed to this column. See [patchedHref] for why the
                href cannot be built here. */}
            {group.count > group.rows.length && (
              <div className="work-col-foot">
                <a
                  href={overflowHref(axis, group.key)}
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

/**
 * The dot a status is drawn with.
 *
 * THE TONE TABLE IS `lib/work.ts`'S. This file carried a second copy — six
 * statuses mapped to the same four tones — and a second spelling of one rule
 * is two rules as soon as a company's vocabulary moves. NEUTRAL ADDS NO CLASS
 * because `.dot` is already the neutral dot: a `.dot.neutral` rule does not
 * exist, so emitting the word would be a class that styles nothing and reads
 * to the next person like one that does.
 */
function statusDot(status: string): string {
  const tone = STATUS_TONE[status];
  return cx("dot", tone !== undefined && tone !== "neutral" && tone);
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
  overflowHref,
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
  /** As [Board]'s — the screen owns the address. */
  overflowHref: (axis: string, key: string) => string;
}) {
  if (groups.length > 0) {
    return (
      <div className="col gap-3">
        {groups.map((group) => (
          <Card key={group.key || "—"} padding="none">
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
                  href={overflowHref(axis, group.key)}
                  onClick={(e) => {
                    e.preventDefault();
                    onOverflow(axis, group.key);
                  }}
                >
                  {group.count - group.rows.length} more →
                </a>
              </div>
            )}
          </Card>
        ))}
      </div>
    );
  }
  if (rows.length === 0) return null;
  return (
    <Card padding="none">
      <RowList
        rows={rows}
        now={now}
        chrome={chrome}
        hrefOf={hrefOf}
        onOpen={onOpen}
        selected={selected}
      />
    </Card>
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
    <Card padding="none">
      <div className="work-cal">
        <div className="work-cal-nav">
          {/* AN ICON WITH NO LABEL IS AN `IconButton` OVER THERE, and the name
              is a required prop rather than a `title` a caller remembers. */}
          <IconButton
            size="sm"
            variant="ghost"
            icon={<ChevronLeftGlyph size="sm" />}
            label="The month before"
            title="The month before"
            onClick={() => onMonth(shiftMonth(month, -1))}
          />
          <span className="work-cal-month">{monthLabel(month)}</span>
          <IconButton
            size="sm"
            variant="ghost"
            icon={<ChevronRightGlyph size="sm" />}
            label="The month after"
            title="The month after"
            onClick={() => onMonth(shiftMonth(month, 1))}
          />
          <Button size="small" variant="tertiary" onClick={onToday}>
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
                    // THE FRAME'S ONE COPY of which clicks mean "open
                    // elsewhere". This chip carried its own — four modifiers
                    // and no `button` test — which is exactly the drift
                    // `rowPeekHandler` was written to end: the second spelling
                    // is the one that forgets the middle button.
                    onClick={rowPeekHandler(() => onOpen(row))}
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
    </Card>
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
/**
 * What was PURGED, which is the half of the trash with no rows.
 *
 * A removal hides a task and a restore brings it back at any age. A purge
 * destroys the rows — so there is nothing for the grid above to list, and this
 * history entry is the only evidence the work ever existed. Drawing it as a
 * grid row would be drawing a task that is gone; drawing it nowhere would make
 * an emptied trash indistinguishable from a company that has removed nothing.
 *
 * ITS OWN BAND, SAYING IT IS IRREVERSIBLE, because every other row on this
 * screen carries a way back and these do not. And the key is rendered from
 * whatever the entry itself holds: a purged task has no row for the feed to
 * resolve a key against, which is exactly the point.
 */
function PurgeBand({ records, now }: { records: WorkActivityRecord[]; now: number }) {
  if (records.length === 0) return null;
  return (
    <Card padding="none">
      <Card.Header
        icon={<DeleteGlyph size="sm" />}
        count={records.length}
        subtitle="A purge destroys the rows. These cannot be restored — what survives is that it happened, to which key, by whom, and the reason the operator gave."
      >
        <Card.Title>Purged</Card.Title>
      </Card.Header>
      {records.map((record) => (
        <div key={record.id} className="work-feed-row">
          <span className="work-feed-when" title={fmtDateTime(record.at)}>
            {relTime(record.at, now)}
          </span>
          <span className="work-feed-kind">
            <Tag variant="danger" appearance="outline">
              irreversible
            </Tag>
          </span>
          <span className="mono">
            {/* NO LINK. The task is gone, so an anchor here would lead to a
                NotFound on every row — and the key is the entry's own,
                because there is no task row left to resolve one from. */}
            {record.subject_key || <EmptyValue label="No key recorded" />}
          </span>
          <span className="work-feed-what truncate">{record.excerpt || "purged"}</span>
          <span className="work-feed-who">{record.actor || "the engine"}</span>
        </div>
      ))}
    </Card>
  );
}

export function ActivityFeed({ records, now }: { records: WorkActivityRecord[]; now: number }) {
  if (records.length === 0) return null;
  return (
    <Card padding="none">
      <Card.Header icon={<TimelineGlyph size="sm" />} count={records.length}>
        <Card.Title>Recent activity</Card.Title>
      </Card.Header>
      {records.map((record) => (
        <div key={record.id} className="work-feed-row">
          <span className="work-feed-when" title={fmtDateTime(record.at)}>
            {relTime(record.at, now)}
          </span>
          <span className="work-feed-kind">
            <Tag appearance="outline">{record.kind.replaceAll("_", " ")}</Tag>
          </span>
          <span>
            {record.subject_key ? (
              <a className="mono t-link" href={href(["work", record.subject_key])}>
                {record.subject_key}
              </a>
            ) : (
              <EmptyValue label="No work item" />
            )}
          </span>
          <span className="work-feed-what truncate">{describeChange(record)}</span>
          <span className="work-feed-who">{record.actor || "the engine"}</span>
        </div>
      ))}
    </Card>
  );
}

export { describeChange, projectKeys, shownRows, typeName, StatusBadge };
