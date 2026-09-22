/**
 * The company's work, as a list you can arrange — the body of two screens.
 *
 * # One machine, two frames
 *
 * `#/work` is this over the whole company and `#/work/{KEY}`'s Items lens is
 * this over one project. They ask the same question with one parameter
 * different, so they are one component: written twice, the project's copy is
 * the one that quietly falls behind, and a reader who narrowed a board and
 * then opened a project would find a different set of controls.
 *
 * # Three controls, not eleven
 *
 * FILTER adds a narrowing and each one is a chip under the bar. DISPLAY
 * decides the drawing — shape, grouping, order, columns. The SCOPE switch
 * stays in the bar because it is the one control a reader presses constantly
 * and it is always set to something. Everything else about the query is in the
 * chips, which exist only when a filter does.
 *
 * # A view is the query; a shape is the drawing
 *
 * `view=` names what somebody saved and supplies the defaults; `shape=`
 * overrides how it is drawn. They were one key, so switching a saved board to
 * a list threw the saved filters away — and the five shapes sat in the view
 * strip as though a way of drawing were a thing somebody had saved.
 *
 * # Read-only, deliberately
 *
 * Nothing here writes. An item is filed and moved by a seat's own tools, or by
 * an operator through the MCP surface, and both are attributed to somebody —
 * where a board button would write as "the dashboard", which is not a person
 * and not a seat and cannot be asked why. That is also why there is no drag: a
 * rank is a value on the task, and dragging one would be the dashboard
 * deciding a team's order.
 */

import { useCallback, useMemo, useState } from "react";
import { buildHash, href, useNavigator, useParam, useRoute } from "~/app/router.tsx";
import { usePeek, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { QueryState } from "~/components/common.tsx";
import { Coverage, type RowChrome } from "~/components/work.tsx";
import { Board } from "./shapes/Board.tsx";
import { CalendarView } from "./shapes/Calendar.tsx";
import { List } from "./shapes/List.tsx";
import { TimelineView } from "./shapes/Timeline.tsx";
import { TableView, removalsOf } from "./shapes/Table.tsx";
import { FilterMenu } from "./toolbar/FilterMenu.tsx";
import { DisplayMenu } from "./toolbar/DisplayMenu.tsx";
import { FilterChips } from "./toolbar/FilterChips.tsx";
import { FEED_PAGE, PurgeBand } from "./feed.tsx";
import { Button, Callout, EmptyState, IconButton, Input, Skeleton, Tabs } from "@crewlethq/ui";
import { CloseGlyph, DashboardGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
// OURS, DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics`: `radio` selects as the arrows move, `tabs` is manual but
// demands a `panelId` naming a TabPanel this row does not control. The scope
// row drives a `useParam` that re-runs the board's query, which is exactly
// what our own `activate="manual"` exists for — arrowing across three options
// would ask the engine three times.
import { Segmented } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import {
  anyFilter,
  asScope,
  bandsOf,
  buildItemsParams,
  calendarWeeks,
  countedLabel,
  dayKey,
  defaultView,
  effectiveArrangement,
  endNote,
  EXPLICIT_NONE,
  filterChips,
  filterPatchForGroup,
  gridRange,
  monthOrNow,
  seededScope,
  shapeOf,
  shownRows,
  totalHint,
  viewParams,
  type Scope,
  type Shape,
  type TrackerFilters,
} from "~/lib/work.ts";
import { plural } from "~/lib/format.ts";
import type { WorkProjectDetail, WorkSummary, WorkTaskCounts, WorkView } from "~/protocol/index.ts";

/**
 * Every custom-field narrowing on the address, as the grammar spells them.
 *
 * READ OFF THE WHOLE QUERY rather than through `useParam`, which answers one
 * key at a time: the set of a company's own fields is the company's, so there
 * is no list of keys for a build to ask for. A stable string identity keeps
 * the memo below from re-asking the engine on every render.
 */
function useFieldFilters(): Record<string, string> {
  const route = useRoute();
  const raw = useMemo(() => {
    const out: Record<string, string> = {};
    for (const [key, value] of route.query.entries()) {
      if (key.startsWith("f.") && key.length > 2 && value) out[key] = value;
    }
    return out;
  }, [route.query]);
  const identity = JSON.stringify(raw);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  return useMemo(() => raw, [identity]);
}

export function ItemsView({ project = "" }: { project?: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  // ONE CLOCK for the screen, ticking on its own: a relative time computed
  // from Date.now() at render is frozen until something else re-renders, so
  // "2 minutes ago" stays that for an hour on a screen nobody touches.
  const now = useNow();

  // THE SECTIONS — a place the reader called, so each pushes history. The
  // shape is one of them: switching a list to a board is a screen somebody
  // went to, and Back after three should walk out through them.
  const [viewKey, setViewKey] = useParam("view", "", "section");
  const [shapeKey, setShapeKey] = useParam("shape", "", "section");
  const [month, setMonth] = useParam("month", "", "section");

  // AND THE FILTERS, which replace: four ticked chips are ONE screen.
  const [q, setQ] = useParam("q", "");
  const [status, setStatus] = useParam("status", "");
  const [type, setType] = useParam("type", "");
  const [priority, setPriority] = useParam("priority", "");
  const [assignee, setAssignee] = useParam("assignee", "");
  const [tag, setTag] = useParam("tag", "");
  // THE TEAM, which arrives from an item's own "Filed into" line rather than
  // from a control — see [TrackerFilters.unit]. An ordinary filter key from
  // here on: it narrows the query, it carries a chip, and the chip takes it
  // off.
  const [unit] = useParam("unit", "");
  const [groupBy, setGroupBy] = useParam("group_by", "");
  const [groupBy2, setGroupBy2] = useParam("group_by2", "");
  // THE COLUMN NARROWING IS THREE-VALUED and `useParam` cannot say so: its
  // value is "the key, or the fallback", which reads an ABSENT key and a
  // PRESENT EMPTY one as the same string — and the empty one is a real column,
  // the one holding the rows with no value on this axis. So the value is read
  // off the whole query, the way [useFieldFilters] reads the custom fields, and
  // the setter is kept for the one gesture that CLEARS it. See
  // [TrackerFilters.group].
  const [, setGroup] = useParam("group", "");
  const [sort, setSort] = useParam("sort", "");
  const [blocked, setBlocked] = useParam("blocked", "");
  const [due, setDue] = useParam("due", "");
  const [removed, setRemoved] = useParam("removed", "");
  const [cols, setCols] = useParam("cols", "");
  const fields = useFieldFilters();

  const container = project ? `project:${project}` : "workspace";

  // THE PEEK IS THE FRAME'S, and so is the rail: the shell mounts one for
  // every screen (`app/frame/PeekHost.tsx`), so this screen opens peeks and
  // renders none.
  const peek = usePeek();
  const { open: openPeek } = usePeekControls();
  // THE WHOLE ADDRESS, for the controls that PATCH it rather than replace it —
  // see [patchedHref]. `useParam` answers one key at a time and none of them
  // is the project, which is a path segment.
  const route = useRoute();
  const group = route.query.has("group") ? (route.query.get("group") ?? "") : undefined;

  // THE CHOSEN PROJECT'S OWN VOCABULARY: its statuses, its types, its tags.
  // ENABLED ON THE SELECTION, not just parameterised by it — the engine
  // refuses this question without a key, so passing no params is a query that
  // fails every poll rather than "ask for everything".
  const overview = useQuery("work_project", project ? { key: project } : undefined, {
    enabled: project !== "",
    pollMs: 60_000,
  });
  // THE TAB STRIP is drawn once per container where the rows are redrawn on
  // every filter change, so it is a separate question with a slower poll: a
  // saved view is arranged by a person, not by the work.
  const strip = useQuery("work_views", { container }, { pollMs: 120_000 });
  // THE TYPES AND THE COMPANY'S OWN FIELDS COME FROM THE CATALOGUE, never from
  // the rows: a filter built from the page can only offer what happens to be
  // on it, so a board showing no bugs would offer no way to ask for one.
  const catalogue = useQuery("work_catalogue", undefined, { pollMs: 300_000 });

  const views = strip.data?.views ?? [];
  // THE STRIP IS WHAT SOMEBODY SAVED. The five builtins are ways of DRAWING an
  // answer and live in the Display menu; mixed into the strip they made a
  // saved view and a shape read as the same kind of thing.
  const saved = useMemo(() => views.filter((v) => !v.builtin), [views]);
  const chosenView = viewKey || defaultView(views);
  // THE VIEW'S OWN SHAPE IS THE DEFAULT and `shape=` overrides it, which is
  // what lets a reader look at a saved board as a list without leaving it.
  const viewShape: Shape = shapeOf(chosenView, views);
  const shape: Shape = isShape(shapeKey) ? shapeKey : viewShape;
  const detail = overview.data;

  // THE VIEW SETS THE SCOPE, and it is the view's own `status_group` read back
  // through the segment that expresses it. [buildItemsParams] spreads a view's
  // params and then OVERWRITES that key from the scope, so seeding the segment
  // from the view is what makes the two agree.
  const viewOwn = useMemo(() => viewParams(chosenView, views), [chosenView, views]);
  const viewScope = seededScope(viewOwn);
  // ONE READING OF THE SEGMENT for the control, the query, the lanes and the
  // empty state — see [asScope] for what a hand-edited value did to the four of
  // them separately.
  const [scopeKey, setScope] = useParam("scope", viewScope);
  const scope = asScope(scopeKey);

  // WHAT THE ARRANGEMENT ACTUALLY IS, which is not what the address holds: a
  // saved view carries its own `group_by`, `group_by2` and `sort`, and the URL
  // key OVERRIDES them rather than being them. The pickers are built from
  // these, because a control that reads the raw key sits on "No grouping" over
  // a list the engine grouped — and its "No grouping" option only deleted the
  // key, after which the view's grouping was handed straight back.
  const axis = effectiveArrangement(groupBy, viewOwn.group_by);
  const axis2 = effectiveArrangement(groupBy2, viewOwn.group_by2);
  const order = effectiveArrangement(sort, viewOwn.sort);
  // TURNING ONE OFF IS A VALUE, not the absence of one — but only where there
  // is something to override. Written unconditionally, an ordinary ungrouped
  // list would carry `group_by=none` on its address for no reason.
  const off = (inherited: unknown) => (inherited ? EXPLICIT_NONE : "");

  const filters: TrackerFilters = {
    q,
    status,
    type,
    priority,
    assignee,
    tag,
    unit,
    scope,
    groupBy,
    groupBy2,
    group,
    sort,
    blocked: blocked === "true",
    due,
    removed: removed === "true",
    fields,
  };

  const thisMonth = monthOrNow(month, now);
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
        view: viewOwn,
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

  // A TRASH LISTING IS THE ONE VIEW WHOSE ROWS DO NOT CARRY THEIR OWN STORY.
  // The row says a task is removed; WHO removed it, WHEN, and whether the
  // removal rode along with a parent's are facts about the COMMIT, which live
  // in the history. And a PURGE has no row at all, by construction.
  //
  // KEYED ON THE PARAMETER, not on a tab: `removed=true` is what makes a
  // listing the trash (`internal/tracker/viewsread.go` says so), so a saved
  // view carrying it is read exactly the same way as the filter chip.
  const inTrash = params.removed === "true" || params.removed === true;
  const tombstones = useQuery(
    "work_activity",
    inTrash ? { container, kinds: "removed,restored,purged", limit: FEED_PAGE.trash } : undefined,
    { enabled: inTrash, pollMs: 60_000 },
  );
  const tombRecords = useMemo(() => tombstones.data?.records ?? [], [tombstones.data]);
  const removals = useMemo(() => removalsOf(tombRecords), [tombRecords]);
  const purges = useMemo(() => tombRecords.filter((r) => r.kind === "purged"), [tombRecords]);

  // THE ANSWER'S OWN COLUMNS, before anything is added to them. Everything
  // that asks "did this question return work?" reads these: a padded lane is a
  // drawing, and an emptiness derived from one would report a company with no
  // work at all as a populated board.
  const groups = useMemo(() => data?.groups ?? [], [data]);
  const rows = useMemo(() => data?.items ?? [], [data]);
  // WHAT `[` AND `]` WALK: the rows this list actually loaded, sorted and
  // filtered as the reader left them. Published rather than handed to the
  // rail, because only the list knows that order — see `PeekHost`.
  const neighbours = useMemo(() => rows.map((r) => ({ kind: "item" as const, id: r.key })), [rows]);
  usePeekNeighbours(neighbours);
  const shown = useMemo(() => shownRows(rows, groups), [rows, groups]);
  // THE BANDS A LIST DRAWS, which are not the lanes a board draws — see
  // [bandsOf]: the engine mints every lane a closed axis admits, and a band
  // over nothing is a rule separating nothing from nothing.
  const bands = useMemo(() => bandsOf(groups), [groups]);

  const chrome: RowChrome = {
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
    types: catalogue.data?.types,
    statuses: detail?.statuses,
  };
  const labels = {
    statuses: detail?.statuses,
    types: catalogue.data?.types,
    tags: detail?.tags,
    fields: catalogue.data?.fields,
    seatName: chrome.seatName,
    // WHAT THE ANSWER HEADED THIS TEAM'S COLUMN, which is the only name a unit
    // key has on this side of the wire — see [LabelContext.unitName]. Where
    // the board is not grouped by unit there is no column to ask, and the chip
    // then says the key the address holds.
    unitName: (key: string) => groups.find((group) => group.key === key)?.label ?? "",
  };

  // ONE WRITER FOR EVERY FILTER KEY, and it is the router's own: `nav.filter`
  // takes a PATCH, so a gesture that moves three keys is one history entry and
  // one re-render rather than three of each. `useParam`'s setters above are
  // still what the single-key controls use, and both end in the same place.
  const nav = useNavigator();
  const setFilters = useCallback(
    (patch: Record<string, string | null>) => nav.filter(patch),
    [nav],
  );

  /** Writes one filter key by name, which is what a chip and the menu both do. */
  const setFilter = (param: string, value: string) => {
    // THE SCOPE WIDENS WITH THE TRASH, because a removed task is very often a
    // finished one: under the Open segment the trash shows only the removals
    // of work nobody had finished, which reads as a company that deletes
    // almost nothing. One patch, so the two land together; the segment is
    // right beside the chip, so narrowing it back is one press and visible.
    if (param === "removed" && value) {
      setFilters({ removed: value, scope: "all" });
      return;
    }
    setFilters({ [param]: value || null });
  };

  const clearFilters = () => {
    const patch: Record<string, string | null> = {
      q: null,
      status: null,
      type: null,
      priority: null,
      tag: null,
      assignee: null,
      due: null,
      blocked: null,
      group: null,
      removed: null,
      // TO THE VIEW'S, not to "open": clearing a narrowing returns the screen
      // to what the view asked for, and writing the fallback drops the key so
      // the view keeps supplying it.
      scope: null,
    };
    for (const key of Object.keys(fields)) patch[key] = null;
    setFilters(patch);
  };

  const itemHref = (row: WorkSummary) => href(["work", row.key]);

  // WHERE A COLUMN FOOTER GOES, as the link the browser follows on a middle
  // click and shows in the status bar. Built here because this is the only
  // frame that knows the whole address: `route.path` carries the project
  // segment and `route.query` carries the filters the reader has set.
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
  /**
   * AND THE CLICK GOES TO THE ADDRESS THE ANCHOR NAMES, through the same patch.
   *
   * It was three `useParam` setters in a row — a section and two filters — so
   * one gesture wrote three history entries and three renders, and neither
   * setter could write the one value that matters here: `nav.filter` deletes a
   * key set to `""`, which is the UNSET column's own key, so following
   * "3 more →" out of Unassigned narrowed to nothing and loaded the whole
   * board. `patchedQuery` is what the href is built from, so the two cannot
   * name different places.
   *
   * A BOARD'S OVERFLOW PUSHES and a list's REPLACES, which is the router's own
   * rule rather than a choice: the board's patch carries `shape=list`, a screen
   * the reader called, where the list's narrows the screen they are on.
   */
  const openOverflow = useCallback(
    (patch: Record<string, string | null>, how: "push" | "replace") => {
      const query = patchedQuery(route.query, patch);
      if (how === "push") nav.to(route.path, query);
      else nav.replace(route.path, query);
    },
    [nav, route.path, route.query],
  );

  // THE EFFECTIVE AXIS, for the same reason the pickers take it: a column
  // narrowing is named by the axis it was cut on, and a view supplying that
  // axis left the chip labelled "Column" over a board grouped by assignee.
  const chips = filterChips({ ...filters, groupBy: axis }, labels);

  // WHETHER THE READER NARROWED THIS, which decides both what an empty list
  // says and what the foot of a full one calls its rows. `anyFilter` counts a
  // scope off `open`, so the segment is compared against the VIEW's own rather
  // than against the default: a saved view whose author asked for finished work
  // is not a reader who narrowed anything.
  const narrowed = anyFilter(filters) || scope !== viewScope;
  // AND THE SCOPE IS NOT A NARROWING TO THE EMPTY PANEL, though it is one to
  // the foot above. "Nothing matches" blames a filter and carries the control
  // that removes it, and a reader who only pressed Closed has no chip to take
  // off — so the segment decides WHICH of the other three sentences is due,
  // never whether the filter one is.
  const filtered = anyFilter({ ...filters, scope: "open" });
  // A TRASH WITH NOTHING IN IT IS NOT A FILTER THAT MATCHED NOTHING, and it is
  // not empty either: a purge leaves no row, and its history entry below is the
  // only trace the work ever existed.
  //
  // AND A PADDED BOARD IS NEVER SHORT OF LANES, so "did the answer carry any
  // group" stopped meaning "is there any work": the engine mints every column
  // its predicate admits, so an empty board is one whose every lane COUNTS
  // nothing. The other shapes draw only the bands that hold rows, so for them
  // it is the rows that decide.
  const nothingShown =
    !inTrash &&
    (shape === "board"
      ? groups.every((group) => group.count === 0)
      : shown.length === 0 && bands.length === 0);
  const foot = endNote({
    shown: shown.length,
    hint: data?.total_hint ?? 0,
    capped: data?.total_capped,
    cursor: data?.next_cursor,
    narrowed,
  });

  return (
    <>
      {/* THE STRIP IS ALWAYS DRAWN, and the gate it lost was `saved.length > 0`.
          The first tab is the container's OWN list — a real destination, not
          decoration — so gating the strip on somebody else having saved a query
          threw away the one thing that names this page inside its own content
          column, and left the toolbar as the top edge of a screen whose title
          is in the shell. A company that has saved nothing is the only state
          every company starts in.

          IT IS ALSO THE ONLY WAY INTO THE INVENTORY from here, which is why
          the trailing link is part of the strip rather than the page actions:
          saved views are undiscoverable until somebody has used them, and a
          strip with nothing after the container tab says nothing about what
          else the strip is for. */}
      <div className="work-strip">
        <Tabs
          ariaLabel="Saved views"
          value={saved.some((v) => v.key === chosenView) ? chosenView : ""}
          onValueChange={(key) => applyView(key, saved, setViewKey)}
          items={[
            // THE CONTAINER'S OWN DEFAULT IS A TAB, because a strip of saved
            // views with no way back to the unsaved list is a strip a reader
            // gets stuck in.
            //
            // AND IT CARRIES NO COUNT. The engine's total is over the FILTER,
            // not over the container, so a number here would say "All work 2"
            // on a company with two hundred items and one chip on — a number
            // the answer does not give is not drawn. The count belongs where
            // the filters are, which is the bar.
            { value: "", label: project ? "All in this project" : "All work" },
            ...saved.map((v) => ({
              value: v.key,
              label: `${v.pinned ? "★ " : ""}${v.name}`,
            })),
          ]}
        />
        {/* A REAL ANCHOR rather than a button that navigates, for the reason
            `Work.tsx` gives at its own two: it leaves the list, so it is
            middle-clickable and goes through the router's history rules
            instead of around them. Beside the tabs rather than among them — a
            link is not a tab, and one inside the set would be selectable and
            would break the roving focus. */}
        <a className="t-link work-strip-more" href={href(["work", "views"])}>
          All views →
        </a>
      </div>

      {/* `toolbar` FIRST, and it is not decoration: `.screen:has(.toolbar)` is
          what publishes `--sticky-top`, and every other thing that sticks in
          this scroller — the grid's column heads, a band head, the peek —
          offsets itself by it. */}
      <div className="toolbar work-bar">
        <FilterMenu
          filters={filters}
          shape={shape}
          onSet={setFilter}
          types={catalogue.data?.types}
          statuses={detail?.statuses}
          tags={detail?.tags}
          fields={catalogue.data?.fields}
          seats={index.seats.map((s) => ({ handle: s.handle, name: s.name }))}
        />
        {/* THE SUBSTRING BOX IS NOT THE SEARCH SCREEN, and the two looked like
            one control in a bar. This one narrows the rows on screen by key or
            title; `#/work/search` ranks the whole company against a phrase the
            way an agent does. Kept as a chip so it reads as one narrowing
            among the others. */}
        <SubstringSearch value={q} onChange={setQ} />
        <span className="spacer" />
        {/* THREE SEGMENTS, not a checkbox: "open", "closed" and "everything"
            are three real questions, and a two-state control would make the
            third unreachable. */}
        <Segmented
          value={scope}
          onChange={setScope}
          ariaLabel="Open or closed"
          options={[
            { value: "open", label: "Open" },
            { value: "closed", label: "Closed" },
            { value: "all", label: "All" },
          ]}
        />
        <DisplayMenu
          shape={shape}
          axis={String(params.group_by ?? "")}
          workspace={!project}
          viewShape={viewShape}
          groupBy={axis}
          groupBy2={axis2}
          sort={order}
          cols={cols}
          onShape={(next) => setShapeKey(next === viewShape ? "" : next)}
          onGroupBy={(next) => {
            // A BOARD'S OWN DEFAULT IS STATUS, so choosing it is choosing
            // nothing: it is written as the absence of the key, unless a view
            // supplies another axis, which the choice then has to override.
            const chosen = shape === "board" && next === "status" && !viewOwn.group_by ? "" : next;
            setGroupBy(chosen || off(viewOwn.group_by));
            // A COLUMN FILTER BELONGS TO ITS AXIS. Left behind when the axis
            // changes it narrows the list to a key the new axis has never
            // heard of, which answers nothing.
            setGroup("");
            // AND A SECOND AXIS WITHOUT A FIRST IS NOTHING THE ENGINE TAKES,
            // so it goes with it — as a deletion rather than an override,
            // since the key is inert either way and `none` on the address
            // would outlive the grouping it was written against.
            if (!next) setGroupBy2("");
          }}
          onGroupBy2={(next) => setGroupBy2(next || off(viewOwn.group_by2))}
          onSort={(next) => setSort(next || off(viewOwn.sort))}
          onCols={setCols}
        />
        <span className="work-summary">
          <Coverage answer={data} />
          {!loading && !error && (
            <span>
              {countedLabel(shown.length, params)}{" "}
              {totalHint(data?.total_hint ?? 0, shown.length, data?.total_capped)}
            </span>
          )}
        </span>
      </div>

      {anyFilter(filters) && (
        <FilterChips
          chips={chips}
          onRemove={(param) => setFilter(param, "")}
          onClear={clearFilters}
        />
      )}

      {data?.groups_overlap && (
        <Callout variant="info">
          One item can be on several of these columns, so the counts add up to more than the total.
        </Callout>
      )}
      {data?.groups_dropped ? (
        <Callout variant="warning">
          {data.groups_dropped} more column{data.groups_dropped === 1 ? "" : "s"} did not fit and
          are not shown. Narrow the list to bring them into range.
        </Callout>
      ) : null}

      <div className="work-body">
        <div className="col gap-4" style={{ minWidth: 0 }}>
          {loading && !data && <Skeleton variant="text" rows={6} label="Loading the work" />}

          {/* THE EMPTY STATE IS RENDERED HERE rather than through
              `QueryState`'s own `empty`, because one of its three cases carries
              an ACTION — a control that clears the filters, where the sentence
              used to describe one — and that prop takes a title and a hint. A
              refusal and a pending read still come first: they are the two
              states an empty list must never be confused with. */}
          <QueryState error={error} loading={loading}>
            {nothingShown ? (
              <EmptyList
                narrowed={filtered}
                scope={scope}
                project={project}
                counts={detail?.task_counts}
                onClear={clearFilters}
              />
            ) : null}
            {/* A SHAPE IS NOT DRAWN OVER NOTHING. The sentence above is the
                answer to an empty question; a board's declared lanes drawn
                under it would be a workflow and an empty state stacked, each
                saying the other is wrong. They come back the moment one row
                does. */}
            {!nothingShown && shape === "board" && (
              <Board
                now={now}
                groups={groups}
                axis={String(params.group_by ?? "status")}
                chrome={chrome}
                detail={detail}
                selected={peek?.kind === "item" ? peek.id : ""}
                hrefOf={itemHref}
                onOpen={(row) => openPeek({ kind: "item", id: row.key })}
                onOverflow={(axis, key) => openOverflow(filterPatchForGroup(axis, key), "push")}
                overflowHref={boardOverflowHref}
              />
            )}
            {!nothingShown && shape === "list" && (
              <List
                rows={rows}
                groups={bands}
                // THE AXIS THE QUERY WAS SENT ON, not the one in the URL: a
                // saved view may carry `group_by`, which [buildItemsParams]
                // resolves through [effectiveArrangement].
                axis={String(params.group_by ?? "")}
                subAxis={String(params.group_by2 ?? "")}
                chrome={chrome}
                detail={detail}
                now={now}
                selected={peek?.kind === "item" ? peek.id : ""}
                hrefOf={itemHref}
                onOpen={(row) => openPeek({ kind: "item", id: row.key })}
                onOverflow={(axis, key) => openOverflow({ group_by: axis, group: key }, "replace")}
                overflowHref={listOverflowHref}
                // THE SHAPE DRAWS IT, so the sentence sits inside the list's own
                // panel as the last thing under the rows — where a foot rendered
                // by this screen would be a detached line under a bordered box.
                // The words are `endNote`'s, once, for every shape that takes
                // one.
                foot={foot}
              />
            )}
            {!nothingShown && shape === "table" && (
              <TableView
                rows={rows}
                groups={bands}
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
            {!nothingShown && shape === "timeline" && (
              <TimelineView
                rows={rows}
                groups={bands}
                chrome={chrome}
                selected={peek?.kind === "item" ? peek.id : ""}
                now={now}
                hrefOf={itemHref}
                onOpen={(row) => openPeek({ kind: "item", id: row.key })}
              />
            )}
            {!nothingShown && shape === "calendar" && (
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

          {inTrash && (
            <PurgeBand records={purges} answer={tombstones.data ?? undefined} now={now} />
          )}
        </div>
      </div>
    </>
  );
}

/** Whether a `shape=` off the address is one this product draws. */
function isShape(value: string): value is Shape {
  return (
    value === "list" ||
    value === "board" ||
    value === "table" ||
    value === "calendar" ||
    value === "timeline"
  );
}

/**
 * Switching to a saved view, or back off one.
 *
 * THE SHAPE IS NOT CLEARED. A reader who chose to look at their work as a list
 * means it across the views they step through — that is what makes the shape a
 * property of the reader rather than of the view — and a view's own shape is
 * one press away in the Display menu, which says so when the two differ.
 */
function applyView(key: string, saved: WorkView[], setViewKey: (key: string) => void) {
  setViewKey(saved.some((v) => v.key === key) ? key : "");
}

/**
 * Where a control that PATCHES THIS SCREEN points, written as an href.
 *
 * THE CLICK AND THE LINK HAVE TO NAME THE SAME PLACE. A column footer's
 * `onClick` moves two or three query keys and leaves everything else alone —
 * the project is a path segment and every live filter is a key beside them —
 * but a middle click never reaches it (the browser dispatches `auxclick`,
 * which React's `onClick` does not see), and the status bar and "copy link
 * address" read the href verbatim.
 *
 * PURE, over the route's two halves rather than over the hook, so the rule is
 * testable beside the list it serves.
 */
export function patchedHref(
  path: string[],
  query: URLSearchParams,
  patch: Record<string, string | null>,
): string {
  return buildHash(path, patchedQuery(query, patch));
}

/**
 * The same patch as the QUERY it produces, so the click and the link are one
 * address rather than two spellings of one.
 *
 * `null` CLEARS AND `""` WRITES AN EMPTY VALUE, which is the whole of the rule.
 * It used to be that an empty value cleared — "`useParam` drops it rather than
 * writing `group_by=`" — and that made the one narrowing whose value IS the
 * empty string unreachable: the "Unassigned" column's own "N more →" dropped
 * `group` and loaded the whole board. The engine tells the two apart with
 * `Params.Has`, and `URLSearchParams` round-trips `group=`, so presence is
 * expressible on the address; `null` is what a control that clears a key now
 * says, which is what `nav.filter`'s own patch type has always meant.
 *
 * PURE, over the route's two halves rather than over the hook, so the rule is
 * testable beside the list it serves.
 */
export function patchedQuery(
  query: URLSearchParams,
  patch: Record<string, string | null>,
): URLSearchParams {
  const next = new URLSearchParams(query);
  for (const [key, value] of Object.entries(patch)) {
    if (value === null) next.delete(key);
    else next.set(key, value);
  }
  return next;
}

/**
 * WHAT AN EMPTY LIST SAYS, AND IT IS A FUNCTION OF WHAT WAS ASKED.
 *
 * There was one sentence for every empty answer — "Nothing matches … Widen
 * them" — and it was drawn over a list with no filter on it, on the company
 * that has just been created and has not filed anything. `docs/reference/
 * dashboard-design.md` §"Honest empty states" says every empty state names what
 * would fill it, and "widen them" names a control that is not on the screen:
 * the chip row does not exist when nothing is narrowing, so there is nothing to
 * widen and nothing to clear.
 *
 * Three questions were being answered with one sentence, and they send a reader
 * to three different places:
 *
 *  (a) something IS narrowing and nothing matched — the filters are the cause,
 *      so the panel carries the control that removes them rather than a
 *      description of one;
 *  (b) nothing is narrowing and this container has nothing in THIS scope —
 *      the scope switch is the cause, so the panel names it;
 *  (c) nothing is narrowing and there is nothing at all — the cause is that no
 *      work has been filed, so the panel says how work arrives.
 *
 * WHY (c) IS MOSTLY SOMEBODY ELSE'S SENTENCE. At workspace scope a company
 * that has no PROJECTS gets [NoWorkYet] from `Work.tsx` INSTEAD of this list —
 * that panel is gated on the project list rather than on this answer, because
 * a read that failed must never be rendered as a company that has filed
 * nothing, and it replaces the list rather than stacking under it, so there is
 * no second sentence to suppress. At project scope the project's own screen
 * carries the container's empty state above this list, so (c) draws nothing
 * here at all.
 *
 * AND (b) CARRIES A NUMBER ONLY WHERE ONE EXISTS. A project detail carries the
 * three maintained `task_counts`, so the sentence can say how much finished
 * work the other segment holds. The workspace has no such counts — `work_items`
 * counts what MATCHED and nothing else — so it names the switch and claims
 * nothing about what is behind it.
 */
function EmptyList({
  narrowed,
  scope,
  project,
  counts,
  onClear,
}: {
  narrowed: boolean;
  scope: Scope;
  /** Empty at workspace scope. */
  project: string;
  /** The container's own maintained counts, where the container has them. */
  counts?: WorkTaskCounts;
  onClear: () => void;
}) {
  // WHERE, NOT "HERE". A project's own list says which project has nothing in
  // it, which is the fact a reader scanning three screens is actually after;
  // at workspace scope there is nothing to name and the sentence closes early.
  const where = project ? ` in ${project}` : "";
  if (narrowed) {
    return (
      <EmptyState
        size="compact"
        title="Nothing matches"
        description="No item on this node's copy of the tracker matches the filters you have set. Take one off above, or clear the whole narrowing."
        action={
          <Button size="small" variant="secondary" onClick={onClear}>
            Clear filters
          </Button>
        }
      />
    );
  }

  // NOTHING AT ALL. The All segment has every scope on screen already, so an
  // empty answer under it is the container's whole tracker; a container with
  // its own counts can say the same thing sooner and exactly.
  const total = counts ? counts.open + counts.done + counts.closed : undefined;
  if (scope === "all" || total === 0) {
    // THE PROJECT'S OWN EMPTY STATE IS ABOVE THIS LIST, and two panels saying
    // one thing is one too many — the same rule the page keeps at workspace
    // scope by drawing [NoWorkYet] in place of this list.
    if (project) return null;
    return (
      <EmptyState
        size="compact"
        title="Nothing has been filed yet"
        description="Seats file work with create_work_item, and an inbound webhook or a schedule is usually what starts them."
      />
    );
  }

  // NOTHING IN THIS SCOPE. A number is drawn only where the container's own
  // counts give one AND it is not zero: "0 items under Closed" beside "nothing
  // open" is a contradiction a reader has to work out, where the numberless
  // sentence is true in both states.
  const elsewhere = scope === "open" ? (counts && counts.done + counts.closed) || 0 : counts?.open;
  if (scope === "open") {
    return (
      <EmptyState
        size="compact"
        title={`Nothing is open${where}`}
        description={
          elsewhere
            ? `Every item here is finished — ${plural(elsewhere, "item")} under Closed. The scope switch in the bar is what shows them.`
            : "No open item is on this node's copy of the tracker. Finished work is under Closed and All shows both — and if none has been filed at all, seats file it with create_work_item."
        }
      />
    );
  }
  return (
    <EmptyState
      size="compact"
      title={`Nothing has been finished${where} yet`}
      description={
        elsewhere
          ? `Nothing here has been finished — ${plural(elsewhere, "item")} still open. The scope switch in the bar is what shows them.`
          : "No finished item is on this node's copy of the tracker. Open shows what is still in flight, and All shows both."
      }
    />
  );
}

/**
 * The panel a company with no projects at all sees, in place of a list.
 *
 * NO `children`, AND THAT IS NOT A SIMPLIFICATION. It took some and dropped
 * them on the floor: `EmptyState` has no children slot — what it offers is
 * `action`, for the one control that would fill the screen — and its own
 * explicit `children:` key overrode whatever was spread in. A prop that
 * compiles, is passed, and renders nothing is worse than one that does not
 * exist, because the caller has no symptom to chase. Nothing passed any; the
 * three-case panel above uses `action` for exactly what this slot looked like
 * it was for.
 */
export function NoWorkYet() {
  return (
    <EmptyState
      icon={<DashboardGlyph size={32} />}
      title="No work has been filed yet"
      description="Nothing can be filed until a unit in the company config declares a `project` key, and this company has none. Once one does, seats file work with create_work_item — an inbound webhook or a schedule is usually what starts them."
    />
  );
}

/**
 * Narrowing the rows on screen by key or title, as one control that opens.
 *
 * TWO SEARCHES LOOKED LIKE ONE. This is an escaped substring over the rows'
 * own key and title — `q=` in the query grammar — and `#/work/search` ranks
 * the whole company against a phrase the way an agent does, over the engine's
 * own BM25 index. Drawn as the widest box in the bar, the narrow one read as
 * the product's search; drawn as a mark that opens, it reads as what it is.
 *
 * IT OPENS WHEN IT HOLDS SOMETHING, so a reader who lands on a filtered
 * address sees the words that are narrowing their rows rather than a mark
 * whose pressed state is the only evidence.
 */
function SubstringSearch({
  value,
  onChange,
}: {
  value: string;
  onChange: (value: string) => void;
}) {
  const [open, setOpen] = useState(false);
  if (!open && !value) {
    return (
      <IconButton
        size="sm"
        variant="ghost"
        icon={<SearchGlyph size="sm" />}
        label="Narrow these rows by key or title"
        onClick={() => setOpen(true)}
      />
    );
  }
  return (
    <span className="work-bar-find">
      <Input
        type="search"
        width="full"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        aria-label="Narrow these rows by key or title"
        leading={<SearchGlyph size="sm" />}
        placeholder="Key or title"
        autoFocus
      />
      <IconButton
        size="sm"
        variant="ghost"
        icon={<CloseGlyph size="sm" />}
        label="Stop narrowing by key or title"
        onClick={() => {
          onChange("");
          setOpen(false);
        }}
      />
    </span>
  );
}
