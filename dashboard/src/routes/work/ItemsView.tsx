/**
 * The company's work, as a list you can arrange — the body of three screens.
 *
 * # One machine, three frames
 *
 * `#/work` is this over the whole company, `#/work/{KEY}`'s Items lens is this
 * over one project, and `#/me`'s Assigned tab is this over one person. They
 * ask the same question with one parameter different, so they are one
 * component: written twice, the second copy is the one that quietly falls
 * behind, and a reader who narrowed a board and then opened a project — or
 * their own day — would find a different set of controls.
 *
 * # A HOST fixes what its screen IS, and the reader keeps the rest
 *
 * A [ItemsHost] is the third frame's whole difference: one narrowing the
 * screen cannot be without (the assignee on `#/me`), what the list OPENS on
 * (grouped by due band, soonest first), and what an empty one says in that
 * screen's own voice. Everything else — the shape, the grouping, the second
 * axis, the order, the columns, every other filter — stays the reader's and
 * stays in the URL, exactly as it is on the other two.
 *
 * THE LOCK IS NOT A CHIP AND NOT A KEY. It is not a narrowing somebody chose,
 * so there is nothing to take off: it is not offered in the Filter menu, it
 * draws no chip, it is never written to the address, and [buildItemsParams]
 * applies it after everything else so no saved default and no hand-edited
 * address can widen the list past the person it is about.
 *
 * # Two rows: what is drawn, and how the answer is cut
 *
 * The first row is the SHAPE (five tabs), the saved views this reader pinned,
 * the substring box and the Display menu; the second — the sticky toolbar — is
 * the chips ending in "+ Filter", then which work is shown and the two
 * arrangements a reader changes constantly, Group by and Sort, with a board's
 * lanes out of view and the count. `toolbar/WorkBar.tsx` carries why each sits
 * where it does.
 *
 * # A view is the query; a shape is the drawing
 *
 * `view=` names what somebody saved and supplies the defaults; `shape=`
 * overrides how it is drawn. They were one key, so switching a saved board to
 * a list threw the saved filters away.
 *
 * # It writes, as the reader
 *
 * A board card can be dragged, and a writer changes a row's status, priority
 * or holder in place — each through `useAct`, as the person the token is bound
 * to (ADR-0024), conditional on the version the row was drawn at. Nothing moves
 * until the engine answers; see `shapes/Board.tsx` and `shapes/cells.tsx`.
 *
 * # Every row is reachable
 *
 * A list and a table are a page of a hundred and a calendar or a timeline a
 * page of five hundred, and the rest follow the answer's cursor on "Load more"
 * ([usePagedItems]). A board's lanes each end in a link to the list narrowed to
 * that lane. `[` and `]` walk the rows in the order they are DRAWN — lanes left
 * to right, bands top to bottom — and a task opened from here carries the list
 * it came from (`list=`), so its own page can say "3 of 18".
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { buildHash, href, useNavigator, useParam, useRoute } from "~/app/router.tsx";
import { usePeek, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { QueryState } from "~/components/common.tsx";
import {
  Coverage,
  cardHidden,
  cardHideParam,
  type CardWaiting,
  type RowChrome,
} from "~/components/work.tsx";
import { PinButton, SaveViewButton } from "~/components/writes.tsx";
import { Board } from "./shapes/Board.tsx";
import { CalendarView } from "./shapes/Calendar.tsx";
import { TimelineView } from "./shapes/Timeline.tsx";
import { WorkGrid, colsParam, isGridShape, removalsOf } from "./shapes/Grid.tsx";
import { headingOf } from "./shapes/group.tsx";
import { FilterMenu } from "./toolbar/FilterMenu.tsx";
import { DisplayMenu } from "./toolbar/DisplayMenu.tsx";
import { FilterChips } from "./toolbar/FilterChips.tsx";
import {
  AllViewsLink,
  ArrangeControls,
  SCOPE_OPTIONS,
  ScopeControl,
  ShapeTabs,
  SubstringBox,
  ViewTabs,
} from "./toolbar/WorkBar.tsx";
import { FEED_PAGE, PurgeBand } from "./feed.tsx";
import { usePagedItems } from "./usePagedItems.ts";
import { Button, Callout, EmptyState, Skeleton } from "@crewlethq/ui";
import { LayoutDashboardGlyph } from "@crewlethq/icons/glyphs";
import { useOpenNewTask } from "~/app/newTask.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { useViewer } from "~/lib/viewer.ts";
import {
  activityOf,
  indexOrg,
  liveOnItems,
  ringOf,
  seatResolvers,
  type SeatRing,
} from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import {
  anyFilter,
  asScope,
  bandsOf,
  buildItemsParams,
  calendarWeeks,
  countedLabel,
  dayKey,
  dayRange,
  defaultView,
  drawnRows,
  effectiveArrangement,
  endNote,
  EXPLICIT_NONE,
  filterChips,
  filterPatchForGroup,
  finishedLanes,
  gridRange,
  hiddenLanes,
  hideParam,
  listParam,
  loadedOf,
  manualOrder,
  monthOrNow,
  seededScope,
  shapeOf,
  shownRows,
  totalHint,
  unfinished,
  URL_HOMES,
  viewParams,
  viewQuery,
  type Scope,
  type Shape,
  type TrackerFilters,
} from "~/lib/work.ts";
import { plural } from "~/lib/format.ts";
import type { WorkSummary, WorkTaskCounts, WorkView } from "~/protocol/index.ts";

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

/**
 * What a HOST screen fixes about this list, where one is holding it.
 *
 * Absent, this IS the screen — `#/work` and the project's Items lens — and
 * everything below is the reader's. Present, the list is a panel inside
 * somebody else's tabs and three things belong to that screen rather than to
 * the reader. The type has no optional field for that reason: a host that
 * locked nothing, opened on nothing and had nothing to say when empty would be
 * the screen again, spelled a second way.
 */
export interface ItemsHost {
  /**
   * The one assignee every read here is narrowed to.
   *
   * Handed to [buildItemsParams] as its lock rather than as a filter — see
   * that function and this file's head for what the difference buys.
   */
  assignee: string;
  /**
   * What this list OPENS on, as `work_items` parameters.
   *
   * THE SAME SLOT A SAVED VIEW FILLS, which is what makes it a default rather
   * than a setting: every one of these keys is overridden by the reader's own
   * on the address, through the same [effectiveArrangement] the strip's views
   * go through, so a host that opens grouped by due band does not stop anybody
   * grouping by status. Declare it as a CONSTANT — a literal rebuilt every
   * render would re-ask the engine on every render.
   */
  opens: Record<string, string>;
  /**
   * What an empty list says where nothing the reader set is narrowing it.
   *
   * THE HOST'S OWN VOICE, per scope, because only the host knows whose list
   * this is: "Nothing is assigned to you" and "Nothing is assigned to them"
   * are the same fact about two different readers, and the container sentences
   * below can say neither. "Nothing matches" still outranks it — a filter that
   * is actually on is the cause a reader can act on.
   */
  empty: (scope: Scope) => { title: string; description: string };
}

/**
 * How often the rows are asked again. A change to an item publishes onto the
 * seat inbox rather than to the dashboard socket, so there is no push behind
 * this and a poll is correct; twenty seconds because a board is read rather
 * than watched, and a tracker's own pace is a person typing a comment. A turn
 * ending on a task on screen asks at once — see [ItemsView].
 */
const ITEMS_POLL_MS = 20_000;

/** No lanes put away, as a set that keeps its identity across renders. */
const NOTHING_HIDDEN: ReadonlySet<string> = new Set();

export function ItemsView({
  project = "",
  host,
  archivedProjects = 0,
}: {
  project?: string;
  host?: ItemsHost;
  /**
   * How many of the company's projects are archived, where EVERY one of them
   * is — the workspace screen's own fact, and zero everywhere else.
   *
   * It exists for one sentence. This list is narrowed to the active set by
   * the item query's own default, so a company that has archived every
   * project answers nothing here — and "nothing has been filed yet" is then
   * false about a company holding hundreds of items. Only `Work.tsx` can say
   * so, because only it reads `work_projects`' census; a second read from
   * inside this list would be the same question asked twice, free to
   * disagree with the sidebar drawn beside it.
   */
  archivedProjects?: number;
}) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const viewer = useViewer();
  const agents = useAgents();
  const openNewTask = useOpenNewTask();
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
  // THE BOARD LANES PUT AWAY — a drawing of the same answer, so it replaces
  // like a filter rather than pushing like a section.
  const [hide, setHide] = useParam("hide", "");
  const hidden = useMemo(() => hiddenLanes(hide), [hide]);
  // AND THE CARD FACTS PUT AWAY, the same kind of key: how the board's cards
  // are drawn, never what the board holds.
  const [cardHide, setCardHide] = useParam("card_hide", "");
  const cardOmit = useMemo(() => cardHidden(cardHide), [cardHide]);

  // AND THE FILTERS, which replace: four ticked chips are ONE screen.
  const [q, setQ] = useParam("q", "");
  const [status] = useParam("status", "");
  const [type] = useParam("type", "");
  const [priority] = useParam("priority", "");
  // THE LOCKED KEY IS READ AND THEN DROPPED, which is the only shape a hook
  // allows: a conditional `useParam` would change the hook order the first
  // time a host arrived. On a hosted list `assignee=` is not part of the
  // grammar — nothing on the screen writes it, no chip draws it and the lock
  // overwrites it on the way to the wire — so one left on the address by hand
  // is inert rather than a second, silent narrowing under the person's name.
  const [assigneeKey] = useParam("assignee", "");
  const assignee = host ? "" : assigneeKey;
  const [tag] = useParam("tag", "");
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
  const [blocked] = useParam("blocked", "");
  const [due] = useParam("due", "");
  const [removed] = useParam("removed", "");
  // THE COLUMN SET IS THE SHAPE'S, so there is a key per grid shape rather
  // than one `cols=` read against whichever set happens to be on. Both are
  // read unconditionally — a hook's key is an argument and this is one
  // component, so branching here would be a branch in the hook order — and
  // the active one is handed to the Display menu. [colsParam] is what keeps
  // these two spellings the same as the ones the grid reads. See
  // `shapes/Grid.tsx` for why one shared key is wrong.
  const [listCols, setListCols] = useParam(colsParam("list"), "");
  const [tableCols, setTableCols] = useParam(colsParam("table"), "");
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
  //
  // AND A HOSTED LIST HAS NO STRIP, so it does not ask. The strip is
  // unconditional on the two screens that ARE this list, because its first tab
  // is the only thing that names the page inside its own content column — and
  // a host has already named it: `#/me`'s own tabs are the strip over this
  // panel, and a second row of them under it would offer the WORKSPACE's saved
  // queries as though they were claims on one person's attention, with the
  // first tab reading "All work" over a list that is one person's. So `view=`
  // is not part of a hosted list's grammar at all: no control writes it and
  // nothing reads it, rather than a key that steers a screen with no tab
  // showing which way.
  //
  // AND IT NAMES THE READER, because a pin is a person's: without `viewer` the
  // engine answers every view unpinned, and the strip could never draw the
  // views this reader pinned.
  const strip = useQuery(
    "work_views",
    viewer.handle ? { container, viewer: viewer.handle } : { container },
    { pollMs: 120_000, enabled: !host },
  );
  // THE TYPES AND THE COMPANY'S OWN FIELDS COME FROM THE CATALOGUE, never from
  // the rows: a filter built from the page can only offer what happens to be
  // on it, so a board showing no bugs would offer no way to ask for one.
  const catalogue = useQuery("work_catalogue", undefined, { pollMs: 300_000 });

  const views = strip.data?.views ?? [];
  // THE STRIP IS WHAT SOMEBODY SAVED. The five builtins are ways of DRAWING an
  // answer and live in the Display menu; mixed into the strip they made a
  // saved view and a shape read as the same kind of thing.
  const saved = useMemo(() => views.filter((v) => !v.builtin), [views]);
  const chosenView = host ? "" : viewKey || defaultView(views);
  // THE VIEW'S OWN SHAPE IS THE DEFAULT and `shape=` overrides it, which is
  // what lets a reader look at a saved board as a list without leaving it. A
  // hosted list has no view, so it opens on the landing shape like a container
  // that has saved nothing.
  const viewShape: Shape = shapeOf(chosenView, views);
  const shape: Shape = isShape(shapeKey) ? shapeKey : viewShape;
  // A SAVED VIEW'S OWN SHAPE, and nothing where the view running is a builtin
  // or none: the Display menu's way back to "this view's own shape" is a way
  // back to what somebody SAVED, and offered over a plain board it pointed at
  // a view nobody was looking at.
  const savedShape = saved.find((v) => v.key === chosenView)?.type;
  const detail = overview.data;
  // AND THE ACTIVE SET'S VALUE, for the menu that writes it.
  const cols = shape === "table" ? tableCols : listCols;
  const setCols = shape === "table" ? setTableCols : setListCols;

  // THE VIEW SETS THE SCOPE, and it is the view's own `status_group` read back
  // through the scope that expresses it. [buildItemsParams] spreads a view's
  // params and then OVERWRITES that key from the scope, so seeding the scope
  // from the view is what makes the two agree.
  //
  // A HOST FILLS THE SAME SLOT, which is the whole of what "opens on" means
  // here: its defaults are overridden by the reader's own keys through the
  // same [effectiveArrangement] and the same scope seeding, so `#/me` opens
  // grouped by due band and soonest first and still lets anybody group by
  // status, sort by priority or look at their week as a board.
  const viewOwn = useMemo(
    () => host?.opens ?? viewParams(chosenView, views),
    [host, chosenView, views],
  );
  const viewScope = seededScope(viewOwn, shape);
  // ONE READING OF THE SEGMENT for the control, the query, the lanes and the
  // empty state — see [asScope] for what a hand-edited value did to the four of
  // them separately.
  //
  // THE TRASH WIDENS IT, as a default rather than as a write: a removed task
  // is very often a finished one, and under the Open scope the trash showed
  // only the removals of work nobody had finished — which reads as a company
  // that deletes almost nothing. It was written by the Filter menu's gesture
  // alone, so the same trash opened from its own address (`removed=true`,
  // a link somebody sent) said "1 in Open" and hid the rest. A default reads
  // the same whichever way the reader arrived, and a Show picked on the bar
  // still overrides it.
  const [scopeKey, setScope] = useParam("scope", removed === "true" ? "all" : viewScope);
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
        lock: host ? { assignee: host.assignee } : undefined,
      }),
    // The filters object is a fresh literal on every render; its content is
    // what the query depends on. THE LOCK IS A DEPENDENCY IN ITS OWN RIGHT: it
    // reaches the wire without passing through `filters`, so a memo that did
    // not name it kept asking for the previous person's work when `#/me`'s
    // whose-day picker moved.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [container, shape, chosenView, views, viewOwn, host?.assignee, weeks, JSON.stringify(filters)],
  );

  // A change to an item publishes onto the seat inbox rather than to the
  // dashboard socket, so there is no push behind this and a poll is correct.
  // Twenty seconds: a board is read, not watched, and a tracker's own pace is
  // a person typing a comment.
  const paged = usePagedItems(params, { pollMs: ITEMS_POLL_MS });
  const { data, loading, error } = paged;

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
  // EVERY PAGE LOADED, the first and every "Load more" after it.
  const rows = paged.rows;
  // WHAT `[` AND `]` WALK: the rows in the order they are DRAWN — a board's
  // lanes left to right, a grouped list's bands top to bottom — which is what
  // the reader is looking at. It walked the flat `items`, which a grouped
  // answer does not carry, so on every board and every grouped list the two
  // keys stepped through nothing. Published rather than handed to the rail,
  // because only the list knows that order — see `PeekHost`.
  const drawn = useMemo(
    () => drawnRows(rows, groups, shape === "board" ? hidden : NOTHING_HIDDEN),
    [rows, groups, shape, hidden],
  );
  const neighbours = useMemo(
    () => drawn.map((r) => ({ kind: "item" as const, id: r.key })),
    [drawn],
  );
  // AND THE QUESTION, so a task opened through the peek's Open carries it like
  // one opened from its row — see `PeekHost` and `ListPosition`.
  const listQuery = useMemo(() => {
    const list = listParam(params);
    return list ? { list } : undefined;
  }, [params]);
  usePeekNeighbours(neighbours, listQuery);

  // WHAT IS HAPPENING ON THE CARDS: the turn a seat is running on each task,
  // its holder's state ring, and — for a bound reader — the coding runs parked
  // on a question put to THEM. All the engine's words: `activity` from the
  // push, the item the turn is charged to, and `decisions`' own reading of
  // which runs wait on this person.
  const live = useMemo(() => liveOnItems(agents), [agents]);
  const rings = useMemo(() => {
    const out = new Map<string, SeatRing>();
    for (const a of agents) {
      const ring = ringOf(activityOf(a));
      if (a.handle && ring) out.set(a.handle, ring);
    }
    return out;
  }, [agents]);
  const decisions = useQuery("decisions", undefined, {
    enabled: shape === "board" && viewer.handle !== "",
    pollMs: ITEMS_POLL_MS,
  });
  const waiting = useMemo(() => {
    const out = new Map<string, CardWaiting>();
    for (const item of decisions.data?.items ?? []) {
      const key = item.run?.work_item?.key;
      if (item.kind === "run" && key && !out.has(key)) out.set(key, { since: item.run?.paused_at });
    }
    return out;
  }, [decisions.data]);
  const facts = useMemo(
    () => ({ live, waiting, ring: (handle: string) => rings.get(handle) }),
    [live, waiting, rings],
  );

  // A TURN ENDING ON A TASK THIS LIST DRAWS IS A TASK THAT JUST CHANGED — the
  // seat moved it, commented, handed it on — and nothing pushes the row. So
  // when a task on screen loses its running turn, the list asks again then,
  // rather than drawing the task as it was for up to a poll's length.
  const runningHere = drawn
    .filter((r) => live.has(r.key))
    .map((r) => r.key)
    .join(",");
  const wasRunning = useRef("");
  const { refetch } = paged;
  useEffect(() => {
    const before = wasRunning.current ? wasRunning.current.split(",") : [];
    const now = new Set(runningHere ? runningHere.split(",") : []);
    wasRunning.current = runningHere;
    if (before.some((key) => !now.has(key))) refetch();
  }, [runningHere, refetch]);
  const shown = useMemo(() => shownRows(rows, groups), [rows, groups]);
  // THE BANDS A LIST DRAWS, which are not the lanes a board draws — see
  // [bandsOf]: the engine mints every lane a closed axis admits, and a band
  // over nothing is a rule separating nothing from nothing.
  const bands = useMemo(() => bandsOf(groups), [groups]);

  const chrome: RowChrome = {
    ...seatResolvers(index),
    types: catalogue.data?.types,
    statuses: detail?.statuses,
  };
  // WHO THE FILTER MENU CAN OFFER, held still across renders. The menu builds
  // its field list behind a memo keyed on this, and a fresh array literal in
  // the JSX is a new identity every render — so that memo rebuilt every field,
  // every option and every custom-field row on each poll and each keystroke in
  // the substring box, while looking exactly like a memo that holds. The chart
  // is what it derives from, and `index` already moves only when that does.
  const roster = useMemo(
    () => index.seats.map((s) => ({ handle: s.handle, name: s.name })),
    [index],
  );
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
    // THE TRASH'S WIDER SCOPE IS THE SCOPE'S OWN DEFAULT (see `scopeKey`),
    // so adding the Removed filter writes that key alone — and taking the chip
    // off puts the list back on the scope it had, rather than leaving an
    // `scope=all` behind that nobody chose.
    setFilters({ [param]: value || null });
  };

  const clearFilters = () => {
    // EVERY KEY THE GRAMMAR CALLS A CHIP, from [URL_HOMES] rather than from a
    // list written out here. Written out, this one was missing `unit` — a key
    // no control on this screen writes, which is exactly why nobody noticed —
    // so a list reached from an item's own "Filed into" line could not be
    // widened again by the control that offers to do it, and the chip it left
    // standing was the only trace of a narrowing Clear had just claimed to
    // remove.
    const patch: Record<string, string | null> = {};
    for (const [key, home] of Object.entries(URL_HOMES)) {
      // A FAMILY IS NOT A KEY: the company's own fields are one key per
      // declared field, and the set is the company's — so what is cleared is
      // whichever of them the address actually holds, below.
      if (home === "chip" && !key.endsWith(".")) patch[key] = null;
    }
    for (const key of Object.keys(fields)) patch[key] = null;
    // AND THE SEGMENT, which is the one key Clear touches that is not a chip.
    // It is not a narrowing somebody added, but it decides which half of the
    // company is on screen, so a "nothing matched" that could not widen it
    // would be offering to undo less than it says. TO THE VIEW'S, not to
    // "open": clearing a narrowing returns the screen to what the view asked
    // for, and writing the fallback drops the key so the view keeps supplying
    // it.
    patch.scope = null;
    setFilters(patch);
  };

  // A TASK OPENED FROM HERE CARRIES THIS LIST'S QUESTION, so its own page can
  // ask where it sits in it (`around=`) and step to the next one.
  const list = useMemo(() => listParam(params), [params]);
  const itemHref = (row: WorkSummary) => href(["work", row.key], list ? { list } : undefined);

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
  // scope off `open`, so the scope is compared against the VIEW's own rather
  // than against the default: a saved view whose author asked for finished work
  // is not a reader who narrowed anything.
  const narrowed = anyFilter(filters) || scope !== viewScope;
  // AND THE SCOPE IS NOT A NARROWING TO THE EMPTY PANEL, though it is one to
  // the foot above. "Nothing matches" blames a filter and carries the control
  // that removes it, and a reader who only pressed Closed has no chip to take
  // off — so the scope decides WHICH of the other three sentences is due,
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
  // THE REST OF A PAGED ANSWER: "Load more", and what is loaded out of how
  // many match. Absent on a complete answer and on a grouped one, which has no
  // cursor.
  const pageMore = paged.next
    ? {
        load: paged.more,
        note: paged.pageError
          ? "The next page could not be read — try again."
          : loadedOf(rows.length, data?.total_hint ?? 0, data?.total_capped),
      }
    : undefined;
  // A BUSY CALENDAR DAY, as this list narrowed to that day and drawn as a
  // list: the day is then the whole screen, every task due on it a row.
  const dayPatch = (day: string) => ({ shape: "list", due: dayRange(day), month: null });
  const dayListHref = (day: string) => patchedHref(route.path, route.query, dayPatch(day));
  // WHO CAN HOLD A TASK, for a writer's inline assignee picker.
  const seats = useMemo(
    () => index.seats.map((s) => ({ handle: s.handle, name: s.name, human: s.kind === "human" })),
    [index],
  );

  const foot = endNote({
    shown: shown.length,
    hint: data?.total_hint ?? 0,
    capped: data?.total_capped,
    cursor: data?.next_cursor,
    narrowed,
  });

  // WHAT THE GROUP BY PICKER WRITES, which is the same three-way rule the
  // Display menu used to carry: a board's own default is status, so choosing
  // it is choosing nothing.
  const onGroupBy = (next: string) => {
    const chosen = shape === "board" && next === "status" && !viewOwn.group_by ? "" : next;
    // ONE PATCH: a column filter belongs to its axis and goes with it, and a
    // second axis without a first is nothing the engine takes.
    setFilters({
      group_by: chosen || off(viewOwn.group_by) || null,
      group: null,
      ...(next ? {} : { group_by2: null }),
    });
  };
  // THE SAVED VIEW RUNNING, if one is — what the strip's pin acts on.
  const running = saved.find((v) => v.key === chosenView && v.id);
  // WHERE THE BOARD NAMES ITS LANES OUT OF VIEW: a slot at the bar's end the
  // board portals its pager into, so naming them costs the lanes no row.
  const [laneSlot, setLaneSlot] = useState<HTMLElement | null>(null);
  const laneLabel = (key: string) => {
    const group = groups.find((g) => g.key === key);
    return group ? headingOf(axis || "status", group, labels) : key || "No value";
  };

  return (
    <>
      {/* THE FIRST ROW: what is drawn. The shape tabs, then — on the screens
          that ARE this list, never on a host, whose own tabs already name the
          page — the container's own list, the views this reader pinned, a way
          to save the query on screen, and the inventory. See
          `toolbar/WorkBar.tsx`. */}
      <div className="work-tabs">
        <ShapeTabs shape={shape} onShape={(next) => setShapeKey(next === viewShape ? "" : next)} />
        {!host && (
          <>
            <span className="work-tabs-sep" aria-hidden="true" />
            <ViewTabs
              views={saved}
              chosen={chosenView}
              containerLabel={project ? "All in this project" : "All work"}
              onView={(key) => applyView(key, saved, setViewKey)}
            />
            {/* THE VIEW RUNNING, PINNED OR UNPINNED FROM WHERE IT RUNS: a pin
                is offered wherever a view is listed, and a project's strip is
                the only list a view saved on that project was ever in. */}
            {running?.id && (
              <PinButton
                key={running.id}
                view={running.id}
                name={running.name}
                pinned={Boolean(running.pinned)}
              />
            )}
            <SaveViewButton
              container={container}
              type={shape}
              params={viewQuery(params, shape)}
              onSaved={setViewKey}
            />
            <AllViewsLink />
          </>
        )}
        {/* THE TOOLS ARE ONE CLUSTER, so a row too narrow for everything
            wraps them together onto a line of their own, right-aligned —
            never the Display button alone under the rest. */}
        <div className="work-tabs-tools">
          <SubstringBox value={q} onChange={setQ} scopeLabel={project || "work"} />
          <DisplayMenu
            shape={shape}
            workspace={!project}
            viewShape={savedShape}
            groupBy={axis}
            groupBy2={axis2}
            cols={cols}
            hidden={[...hidden].map((key) => ({ key, label: laneLabel(key) }))}
            onShape={(next) => setShapeKey(next === viewShape ? "" : next)}
            onGroupBy2={(next) => setGroupBy2(next || off(viewOwn.group_by2))}
            onCols={setCols}
            onShowLane={(key) =>
              setHide(key === undefined ? "" : hideParam([...hidden].filter((k) => k !== key)))
            }
            cardHidden={cardOmit}
            onCardHidden={(next) => setCardHide(cardHideParam(next))}
          />
        </div>
      </div>

      {/* THE SECOND ROW: how the answer is cut. `toolbar` FIRST, and it is not
          decoration: the scroller's `:has(.toolbar)` is what publishes
          `--sticky-top`, and every other thing that sticks in this scroller —
          the grid's column heads, a band head, the peek — offsets itself by
          it. */}
      <div className="toolbar work-bar">
        {/* THE CHIPS OPEN THE ROW, ending in "+ Filter": what narrows the
            answer is what a reader reads first, and the control that adds a
            narrowing sits after the ones it adds to — the approved board's
            one bar, with the arrangement at its far end. */}
        <FilterChips
          chips={anyFilter({ ...filters, scope: "open" }) ? chips : []}
          onRemove={(param) => setFilter(param, "")}
          onClear={clearFilters}
          add={
            <FilterMenu
              filters={filters}
              shape={shape}
              onSet={setFilter}
              lockedAssignee={host?.assignee}
              types={catalogue.data?.types}
              statuses={detail?.statuses}
              tags={detail?.tags}
              fields={catalogue.data?.fields}
              seats={roster}
            />
          }
        />
        {/* THE ARRANGEMENT AND THE COUNT ARE ONE CLUSTER at the far end, so a
            narrow row — an open peek, a 1280 window — moves them down
            together rather than leaving the count alone on a line. */}
        <span className="work-bar-end">
          <ScopeControl scope={scope} onScope={setScope} />
          <ArrangeControls
            shape={shape}
            workspace={!project}
            groupBy={axis}
            sort={order}
            manual={manualOrder(order, project)}
            onGroupBy={onGroupBy}
            onSort={(next) => setSort(next || off(viewOwn.sort))}
          />
          {/* THE LANES OUT OF A BOARD'S VIEW are named here, at the bar's end,
              rather than on a row of their own above the lanes: see
              `shapes/Board.tsx`, which draws them into this slot. */}
          <span className="work-bar-lanes" ref={setLaneSlot} />
          <span className="work-summary">
            <Coverage answer={data} />
            {!loading && !error && (
              <span>
                {countedLabel(
                  shown.length,
                  params,
                  SCOPE_OPTIONS.find((o) => o.value === scope)?.label,
                )}{" "}
                {totalHint(data?.total_hint ?? 0, shown.length, data?.total_capped)}
              </span>
            )}
          </span>
        </span>
      </div>

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
                archivedProjects={archivedProjects}
                onClear={clearFilters}
                host={host}
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
                facts={facts}
                hidden={hidden}
                onHide={(key) => setHide(hideParam([...hidden, key]))}
                movable={manualOrder(order, project)}
                finishedLanes={finishedLanes(String(params.group_by ?? "status"))}
                weekly={scope === "recent"}
                laneSlot={laneSlot}
                cardOmit={cardOmit}
                // A LANE'S `+` FILES INTO THAT LANE: the one sheet, told what
                // puts a task in the lane (the board decides which lanes take
                // one, from [presetForLane]) and the project this list is
                // scoped to — and, on a person's own list, that it is theirs.
                onAdd={(lane) =>
                  openNewTask({
                    ...(project ? { project } : {}),
                    ...(host?.assignee ? { assignee: host.assignee } : {}),
                    ...lane,
                  })
                }
              />
            )}
            {/* ONE BRANCH FOR BOTH GRID SHAPES. The list and the table are one
                renderer drawn in two column sets — see `shapes/Grid.tsx` — so
                a second arm here would be a second copy of every prop they
                already share, which is what let the two drift apart in the
                first place. */}
            {!nothingShown && isGridShape(shape) && (
              <WorkGrid
                shape={shape}
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
                workspace={!project}
                selected={peek?.kind === "item" ? peek.id : ""}
                hrefOf={itemHref}
                onOpen={(row) => openPeek({ kind: "item", id: row.key })}
                onOverflow={(axis, key) => openOverflow({ group_by: axis, group: key }, "replace")}
                overflowHref={listOverflowHref}
                removals={inTrash ? removals : undefined}
                // THE GRID DRAWS IT, so the sentence sits inside the grid's own
                // panel as the last thing under the rows — where a foot rendered
                // by this screen would be a detached line under a bordered box.
                // The words are `endNote`'s, once, for every shape that takes
                // one.
                foot={foot}
                seats={seats}
                more={pageMore}
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
                more={pageMore}
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
                dayHref={dayListHref}
                onDay={(day) => openOverflow(dayPatch(day), "push")}
                more={pageMore}
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
 * maintained `task_counts`, so the sentence can say how much finished
 * work the other scope holds. The workspace has no such counts — `work_items`
 * counts what MATCHED and nothing else — so it names the switch and claims
 * nothing about what is behind it.
 */
function EmptyList({
  narrowed,
  scope,
  project,
  counts,
  archivedProjects,
  onClear,
  host,
}: {
  narrowed: boolean;
  scope: Scope;
  /** Empty at workspace scope. */
  project: string;
  /** The container's own maintained counts, where the container has them. */
  counts?: WorkTaskCounts;
  /** Every project archived — see [ItemsView]'s own prop. Zero otherwise. */
  archivedProjects?: number;
  onClear: () => void;
  /** The screen holding this list, where one is — see [ItemsHost.empty]. */
  host?: ItemsHost;
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

  // AND A HOSTED LIST SPEAKS FOR ITSELF, once the filter case above is out of
  // the way. Three of the sentences below are about a CONTAINER — what has
  // been filed in this project, what is open in this company — and a host's
  // list is neither: on `#/me` the answer is about a person, and "Nothing has
  // been filed yet" over a company with four hundred tasks and one idle seat
  // is false about the only subject the reader came for. `narrowed` still
  // wins, because a filter that is actually on is the cause somebody can act
  // on, which is the whole of [ItemsHost.empty]'s rule.
  if (host) {
    const said = host.empty(scope);
    return <EmptyState size="compact" title={said.title} description={said.description} />;
  }

  // EVERY PROJECT ARCHIVED, WHICH IS WHY EVERY SCOPE IS EMPTY.
  //
  // BEFORE THE SCOPE BRANCHES, because the cause is the container rather than
  // the switch: Open, Closed and All all answer nothing here, and each of the
  // three sentences below would blame the scope or claim the company has
  // filed nothing — over a company holding every item it ever filed, in
  // projects it has retired. The reader's next move is the projects, not the
  // scope switch, so this says how many there are and goes to them, which is
  // the shape the directory's own Active empty state already takes.
  //
  // `Work.tsx` IS THE ONLY CALLER THAT CAN SAY SO, since the count is
  // `work_projects`' census and this list never reads it — so at project
  // scope and on a hosted list the number is zero and this arm never fires.
  if (!project && archivedProjects) {
    return (
      <EmptyState
        size="compact"
        title="Every project is archived"
        description={
          <>
            {archivedProjects === 1
              ? "The company’s one project has been archived"
              : `All ${archivedProjects} of the company’s projects have been archived`}
            , so nothing is listed here — an archived project keeps its work and stops taking new
            items.{" "}
            <a className="prose-link" href={href(["work", "projects"], { shown: "archived" })}>
              See them under Archived
            </a>
            .
          </>
        }
      />
    );
  }

  // NOTHING AT ALL. The All scope has every scope on screen already, so an
  // empty answer under it is the container's whole tracker; a container with
  // its own counts can say the same thing sooner and exactly.
  const total = counts ? unfinished(counts) + counts.done + counts.closed : undefined;
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
  const elsewhere =
    scope === "open" ? (counts && counts.done + counts.closed) || 0 : counts && unfinished(counts);
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
      icon={<LayoutDashboardGlyph size={32} />}
      title="No work has been filed yet"
      description="Nothing can be filed until a unit in the company config declares a `project` key, and this company has none. Once one does, seats file work with create_work_item — an inbound webhook or a schedule is usually what starts them."
    />
  );
}
