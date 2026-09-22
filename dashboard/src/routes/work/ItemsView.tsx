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

import { useCallback, useMemo, useState, type ReactNode } from "react";
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
  bandsOf,
  buildItemsParams,
  calendarWeeks,
  countedLabel,
  dayKey,
  defaultView,
  effectiveArrangement,
  emptyReason,
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
  type Shape,
  type TrackerFilters,
} from "~/lib/work.ts";
import type { WorkProjectDetail, WorkSummary, WorkView } from "~/protocol/index.ts";

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
  const [groupBy, setGroupBy] = useParam("group_by", "");
  const [groupBy2, setGroupBy2] = useParam("group_by2", "");
  const [group, setGroup] = useParam("group", "");
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
  const [scope, setScope] = useParam("scope", viewScope);

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

  // THE EFFECTIVE AXIS, for the same reason the pickers take it: a column
  // narrowing is named by the axis it was cut on, and a view supplying that
  // axis left the chip labelled "Column" over a board grouped by assignee.
  const chips = filterChips({ ...filters, groupBy: axis }, labels);

  return (
    <>
      {saved.length > 0 && (
        <Tabs
          ariaLabel="Saved views"
          value={saved.some((v) => v.key === chosenView) ? chosenView : ""}
          onValueChange={(key) => applyView(key, saved, setViewKey)}
          items={[
            // THE CONTAINER'S OWN DEFAULT IS A TAB, because a strip of saved
            // views with no way back to the unsaved list is a strip a reader
            // gets stuck in.
            { value: "", label: project ? "All in this project" : "All work" },
            ...saved.map((v) => ({
              value: v.key,
              label: `${v.pinned ? "★ " : ""}${v.name}`,
            })),
          ]}
        />
      )}

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

          <QueryState
            error={error}
            loading={loading}
            empty={
              // A TRASH WITH NOTHING IN IT IS NOT A FILTER THAT MATCHED
              // NOTHING, and it is not empty either: a purge leaves no row and
              // its history entry is the only trace the work ever existed.
              //
              // A BOARD WITH LANES IS NOT EMPTY EITHER, however few cards are
              // on them: the engine mints every lane the scope admits, and
              // three empty lanes are the shape of the process. The other
              // shapes draw only the bands that hold something, so for them
              // the rows decide — and the sentence says WHY there are none,
              // which "Nothing matches" over an unfiltered list did not.
              inTrash ||
              (shape === "board" ? groups.length > 0 : shown.length > 0 || bands.length > 0)
                ? undefined
                : emptyReason(filters, project)
            }
          >
            {shape === "board" && (
              <Board
                now={now}
                groups={groups}
                axis={String(params.group_by ?? "status")}
                chrome={chrome}
                detail={detail}
                selected={peek?.kind === "item" ? peek.id : ""}
                hrefOf={itemHref}
                onOpen={(row) => openPeek({ kind: "item", id: row.key })}
                onOverflow={(axis, key) => {
                  const patch = filterPatchForGroup(axis, key);
                  setShapeKey(patch.shape ?? "list");
                  setGroupBy(patch.group_by ?? "");
                  setGroup(patch.group ?? "");
                }}
                overflowHref={boardOverflowHref}
              />
            )}
            {shape === "list" && (
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
            {shape === "timeline" && (
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

/** The panel a company with no projects at all sees, in place of a list. */
export function NoWorkYet({ children }: { children?: ReactNode }) {
  return (
    <EmptyState
      icon={<DashboardGlyph size={32} />}
      title="No work has been filed yet"
      description="Nothing can be filed until a unit in the company config declares a `project` key, and this company has none. Once one does, seats file work with create_work_item — an inbound webhook or a schedule is usually what starts them."
    >
      {children}
    </EmptyState>
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
