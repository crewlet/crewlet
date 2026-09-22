/**
 * The tracker as one grid, drawn in two column sets: the LIST and the TABLE.
 *
 * # One renderer, because they were never two drawings
 *
 * They were two components. `List.tsx` hand-rolled bands over `RowList` and
 * `Table.tsx` went through [DataGrid], so one answer had two ideas of what a
 * band is, what a row's inset is, what the head does, what happens on a phone
 * and which of the two could be sorted at a column head. Every rule the grid
 * had — the track list declared once, the cap on a shrink column, the card the
 * row becomes below 860px, the cursor `j` and `k` walk — the list simply did
 * not have, and every rule the list had the table did not: the table drew a
 * band per group with NO rows in it the moment a second axis was asked for,
 * because sub-groups replace a group's rows and it only ever read `rows`.
 *
 * So there is one renderer and the shape picks a column SET. That is the whole
 * of the difference, which is what `docs/reference/dashboard-design.md` means
 * by "Every list is one grid": a shape decides how a row is DRAWN and never
 * what was asked for.
 *
 * # The two sets share a vocabulary and differ in default, order and rendering
 *
 * Both offer the same twelve columns — the company's list adds Project, and a
 * trash listing adds the three facts a removal has. What differs is which are
 * ON with nothing chosen, the ORDER they are drawn in, and how a value is
 * drawn: the list's priority is a bare mark opening the row, the table's is
 * the word in a column you can sort; the list's assignee is an avatar, the
 * table's is the seat's name.
 *
 * # `cols=` IS KEYED PER SHAPE — `cols.list=` and `cols.table=`
 *
 * One key shared between the two sets is wrong, and not because a key might
 * name a column the other set lacks — that part is survivable, and [DataGrid]
 * drops a name its columns do not hold. It is wrong because `cols=` carries an
 * ORDER as well as a selection: the Display menu writes the keys in the set's
 * own declaration order and the grid draws them in the order the reader asked
 * for. So a value written against the table, read against the list, draws the
 * list's columns in the TABLE's order — Key before Priority, Assignee before
 * Due — which is a row nobody arranged and which no validation can repair,
 * because every name in it is legal. The DEFAULT differs too, so the meaning
 * of "no key at all" is per shape before any value exists.
 *
 * Per shape, each arrangement survives the other: a reader who narrows the
 * table, looks at the list and comes back finds the table as they left it. And
 * an address is self-describing — `shape=table&cols.table=key,title` says
 * which set its columns belong to, where a bare `cols=` on a link whose shape
 * comes from a saved view is read against whichever set that view happens to
 * name.
 *
 * THE SORT IS NOT KEYED THAT WAY, and the asymmetry is the point. `sort=` is a
 * fact about the QUESTION — the grid writes the key, the screen sends it, and
 * the engine orders the whole set the same way whatever draws it — so a list
 * ordered by due and a table ordered by due are one question and share one
 * key. The column set is a fact about the DRAWING. Two facts, two keys; see
 * [DataGrid]'s `colsName`.
 *
 * # The sort is the engine's
 *
 * [DataGrid] writes the sort into `sort=`, and on this screen that key is the
 * one the query already carries — so a header click re-asks the question and
 * the whole set is ordered, not the hundred rows that happen to be loaded.
 * That is why `serverSorted` is set: a grid that also sorted locally would
 * order the page a second time, by a comparator over rendered values, and the
 * two orders disagree on exactly the rows a page boundary falls between.
 *
 * A column the engine cannot order by is therefore NOT sortable here. That is
 * a real constraint rather than an oversight: a clickable head that wrote
 * `sort=blocked` would take the whole board down with a refusal, because the
 * query grammar refuses a sort key it does not know rather than ignoring it.
 *
 * # The trash is this grid with three more columns
 *
 * `removed=true` is what makes a listing the trash — see the engine's own
 * [ViewKeyTrash] — so this component takes the removals rather than a flag
 * naming a tab: any view saved with that parameter gets the same treatment,
 * which is the property a magic view key would not have. They append to
 * whichever set is active, so a reader who prefers the trash as a list gets
 * the compact row and the removal facts rather than being pinned to one shape.
 *
 * WHO REMOVED IT AND WHEN COME FROM THE ACTIVITY FEED, not from the row. The
 * row carries no tombstone — it is deliberately not a document read per card —
 * and the feed is where a removal IS: one history entry per commit, carrying
 * the actor, their kind and the instant. A row with no entry on the loaded page
 * of the feed says so rather than drawing a blank, because "removed by nobody"
 * is not a fact this product can state.
 */

import { useMemo } from "react";
import { Button, EmptyValue, Tag, useClipboard } from "@crewlethq/ui";
import { ContentCopyGlyph } from "@crewlethq/icons/glyphs";

import { DataGrid, type GridBand, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, SeatCell } from "~/app/frame/cells.tsx";
import {
  Assignee,
  DueMark,
  PriorityMark,
  StatusBadge,
  TypeIcon,
  blockedBy,
  type RowChrome,
} from "~/components/work.tsx";
import { GroupMark, headingOf } from "./group.tsx";
import { type Shape } from "~/lib/work.ts";
import { fmtDuration } from "~/lib/format.ts";
import { callText } from "~/lib/toolcall.ts";
import type {
  WorkActivityRecord,
  WorkGroup,
  WorkProjectDetail,
  WorkSummary,
} from "~/protocol/index.ts";

/** How long the copy button says it worked, matching `ToolCall.tsx`'s. */
const COPIED_MS = 1_400;

/**
 * The two shapes this grid draws, as the screen's `shape=` spells them.
 *
 * A TYPE RATHER THAN A STRING, so the column set, the URL key and the branch
 * in `ItemsView` cannot come to disagree about which shapes are a grid.
 */
export type GridShape = "list" | "table";

/** Whether a shape is one this grid draws. */
export function isGridShape(shape: Shape): shape is GridShape {
  return shape === "list" || shape === "table";
}

/**
 * The URL key holding one shape's chosen columns.
 *
 * ONE PLACE, because two readers need it: the grid hands the name to
 * [DataGrid], which reads it, and the screen reads the same key to fill the
 * Display menu's checkboxes. Spelled twice, a menu would write one key and a
 * grid read another, and every tick would look ignored.
 */
export function colsParam(shape: GridShape): string {
  return `cols.${shape}`;
}

/**
 * Every column a header click orders by — and each name IS one of the query
 * grammar's own sort keys.
 *
 * [DataGrid] writes the column's key straight into `sort=`, which this screen
 * sends to the engine, and `ParseQuery` REFUSES a sort key it does not know
 * rather than ignoring it. So one wrong header does not mis-sort a column: it
 * takes the whole board down with a refusal, on the click.
 *
 * Held against the grammar by `internal/tracker/client_gate_test.go`, because
 * this is a copy the dashboard has to keep — a separate build in a separate
 * language cannot import a Go identifier. The union makes a typo a compile
 * error; the gate makes a RENAME in the engine one. It covers BOTH sets, which
 * is what collapsing them bought: the list's heads are sortable now, and they
 * are the same declaration the table's are held against.
 */
const COLUMN_SORT_KEYS = [
  "title",
  "priority",
  "due",
  "start",
  "points",
  "estimate",
  "updated",
  "removed",
] as const;

type SortKey = (typeof COLUMN_SORT_KEYS)[number];

/**
 * A column ordered at its own head.
 *
 * The key is typed to [COLUMN_SORT_KEYS] rather than to `string`, so a column
 * that gains a `sortValue` without a key the engine knows does not compile.
 * A column with no ordering is spelled as a plain object and its head is
 * inert — which is the honest rendering for `status`, `assignee` and the key
 * itself, none of which the engine has an order for.
 */
function sorted(key: SortKey, column: Omit<GridColumn<WorkSummary>, "key">) {
  return { key, ...column };
}

/**
 * The removal facts for one task, as the feed carries them.
 *
 * A MAP RATHER THAN A FIELD ON THE ROW, because the two answers are two
 * queries: the rows are what is in the trash and the feed is what happened.
 * They page independently, so a row can be present with no entry — which is a
 * state the cell renders rather than one the type pretends away.
 */
export type Removals = Map<string, WorkActivityRecord>;

/** Index a feed page by the task each entry is about. */
export function removalsOf(records: WorkActivityRecord[], kind = "removed"): Removals {
  const out: Removals = new Map();
  for (const record of records) {
    if (record.kind !== kind) continue;
    // NEWEST WINS, and the feed arrives newest first — a task removed,
    // restored and removed again has two entries and the current one is the
    // later. `set` on an id already held would overwrite it with the older.
    if (!out.has(record.subject_id)) out.set(record.subject_id, record);
  }
  return out;
}

/** What a column set is built from. */
interface ColumnContext {
  chrome: RowChrome;
  detail?: WorkProjectDetail | null;
  now: number;
  /** The company's own list rather than one project's — so project is a column. */
  workspace: boolean;
  removals?: Removals;
  /** A blocker's KEY from its id, where this page holds its row — see
   *  [blockedBy]. The list's title cell names what holds a task up. */
  keyOf?: (id: string) => string | undefined;
}

/**
 * THE LIST'S SET: the compact row, as columns.
 *
 * Priority, key, status, title, due, type, assignee, then when it last moved —
 * the same eight the list drew as a hand-rolled subgrid, in the same order,
 * for the same reason its own track list gave: the three that OPEN a row are
 * the three a reader scans DOWN, so the title always starts at one x.
 *
 * The four the table draws and this does not are here as choices rather than
 * missing: a reader who wants points on a list should not have to become a
 * reader of tables to get them. They are `optional` — off until `cols=` names
 * one — and they sit where they would be drawn if it did, before the type and
 * the seat, so the timestamp stays the row's last column either way.
 */
function listColumns(ctx: ColumnContext): GridColumn<WorkSummary>[] {
  const { chrome, detail, now, workspace } = ctx;
  const out: GridColumn<WorkSummary>[] = [
    {
      key: "priority",
      // A GLYPH HEAD AND THEREFORE NOT SORTABLE. The mark is the whole column
      // — twenty pixels of it — so there is no word for a sort button to be
      // named by, and a blank clickable head is an affordance nobody can see.
      // The order is one press away in Display's own "Order by", which writes
      // the same `sort=priority` the table's head would.
      header: "",
      label: "Priority",
      shrink: true,
      cell: (row) => <PriorityMark priority={row.priority} />,
    },
    {
      key: "key",
      header: "Key",
      shrink: true,
      // NOT SORTABLE. A key is `PROJ-<n>` and the engine has no ordering
      // over it; the nearest thing is `created`, which is a different
      // claim and is not on the row at all.
      cell: (row) => <KeyCell value={row.key} />,
    },
    {
      key: "status",
      header: "Status",
      shrink: true,
      // NOT SORTABLE, for the reason the table's own status column gives:
      // there is no ordering over a company's own status words, and
      // `group_by=status` is what a reader asking this actually wants.
      cell: (row) => <StatusBadge status={row.status} defs={detail?.statuses} />,
    },
    sorted("title", {
      header: "Title",
      sortValue: (row) => row.title,
      // THE CELL IS ALREADY A FLEX ROW WITH A GAP, so the title and its mark
      // are two children of it rather than a box inside a box.
      cell: (row) => (
        <>
          <span className="truncate">{row.title}</span>
          {/* WHICH TASK HOLDS THIS ONE UP, not merely that something does —
              the row carries the edges, so the badge names the blocker a
              reader can go to, and the FIRST of them rather than all, because
              a row is one line and a task waiting on four is still one fact.
              It rides the title in the compact set because that set has no
              column to spare; the table's question is a field at a time, and
              the engine has no `blocked` sort key for a column to be headed
              by. */}
          {row.blocked && (
            <Tag variant="danger" appearance="outline">
              {blockedBy(row, ctx.keyOf)}
            </Tag>
          )}
        </>
      ),
    }),
  ];
  if (workspace) out.push(projectColumn(true));
  out.push(
    sorted("due", {
      header: "Due",
      shrink: true,
      sortValue: (row) => row.due ?? "",
      cell: (row) =>
        row.due ? (
          <DueMark due={row.due} overdue={row.overdue} now={now} />
        ) : (
          <EmptyValue label="No date" />
        ),
    }),
    startColumn(now),
    pointsColumn(),
    estimateColumn(),
    {
      key: "type",
      header: "",
      label: "Type",
      shrink: true,
      cell: (row) => <TypeIcon type={row.type} types={chrome.types} />,
    },
    {
      key: "assignee",
      header: "",
      label: "Assignee",
      shrink: true,
      // THE AVATAR ALONE, which is what the compact row draws: a name beside
      // it is a second column's worth of width on a row whose point is that
      // the title has the space. The table prints the name, because there the
      // column is what the reader came for. Unassigned is a dashed square
      // rather than a dash — an unassigned item routes to the project's lead,
      // and a project with no lead routes to nobody at all, so it is a state
      // worth seeing down a queue.
      cell: (row) => <Assignee handle={row.assignee} seatName={chrome.seatName} />,
    },
    updatedColumn(now),
  );
  return out;
}

/**
 * THE TABLE'S SET: one field per column, every value of a column at one x.
 *
 * The list draws each task as a block — title, then badges, then dates, then
 * the seat — which is the right shape for "what is this task" and the wrong
 * one for every question about a FIELD. Comparing estimates down a list means
 * finding the same badge at a different x on every row, because the badge's
 * position depends on how long the title above it was.
 */
function tableColumns(ctx: ColumnContext): GridColumn<WorkSummary>[] {
  const { chrome, detail, now, workspace } = ctx;
  const out: GridColumn<WorkSummary>[] = [
    {
      key: "key",
      header: "Key",
      shrink: true,
      cell: (row) => <KeyCell value={row.key} />,
    },
    {
      key: "type",
      header: "",
      label: "Type",
      shrink: true,
      cell: (row) => <TypeIcon type={row.type} types={chrome.types} />,
    },
    sorted("title", {
      header: "Title",
      sortValue: (row) => row.title,
      cell: (row) => <span className="truncate">{row.title}</span>,
    }),
    {
      key: "status",
      header: "Status",
      shrink: true,
      // NOT SORTABLE, deliberately. The engine's nearest key is
      // `status_entered` — how long a task has been in the status it is
      // in — which is a different claim, and there is no ordering over
      // the status words themselves: they are a company's own vocabulary
      // and alphabetical order over them means nothing. `group_by=status`
      // is what a reader asking this actually wants, and the filter bar
      // already offers it.
      cell: (row) => <StatusBadge status={row.status} defs={detail?.statuses} />,
    },
    sorted("priority", {
      header: "Priority",
      shrink: true,
      sortValue: (row) => row.priority ?? "",
      cell: (row) => <PriorityMark priority={row.priority} word />,
    }),
    {
      key: "assignee",
      header: "Assignee",
      shrink: true,
      cell: (row) =>
        row.assignee ? (
          <SeatCell handle={row.assignee} name={chrome.seatName?.(row.assignee)} />
        ) : (
          <EmptyValue label="Nobody holds this" />
        ),
    },
  ];
  if (workspace) out.push(projectColumn(false));
  out.push(
    sorted("due", {
      header: "Due",
      shrink: true,
      sortValue: (row) => row.due ?? "",
      // THE LIST'S OWN MARK, not a second rendering of a due date: the
      // overdue tint is `DueMark`'s and two spellings of it is two rules
      // as soon as one screen's changes.
      cell: (row) =>
        row.due ? (
          <DueMark due={row.due} overdue={row.overdue} now={now} />
        ) : (
          <EmptyValue label="No date" />
        ),
    }),
    startColumn(now),
    pointsColumn(),
    estimateColumn(),
    updatedColumn(now),
  );
  return out;
}

/**
 * The columns both sets share verbatim.
 *
 * WRITTEN ONCE rather than per set: a value and its emptiness rule are facts
 * about the FIELD, and the four here are drawn the same way in both shapes.
 * What a set decides is which are on, where they sit and — for the five that
 * genuinely differ — how they are drawn.
 */
/**
 * `optional` IS THE ONE THING THE TWO SETS DISAGREE ABOUT HERE, so it is a
 * parameter rather than two declarations of one column. The table draws it
 * because a table answers a field at a time and "which project is this in" is
 * one of them; the compact row does not, because the key beside it already
 * opens with the project and a row whose whole point is that the title has the
 * space cannot spend a column saying it twice.
 */
function projectColumn(optional: boolean): GridColumn<WorkSummary> {
  return {
    key: "project",
    header: "Project",
    shrink: true,
    optional,
    cell: (row) => <span className="mono">{row.project}</span>,
  };
}

function startColumn(now: number): GridColumn<WorkSummary> {
  return sorted("start", {
    header: "Start",
    shrink: true,
    optional: true,
    sortValue: (row) => row.start ?? "",
    cell: (row) =>
      row.start ? <DateCell at={row.start} now={now} /> : <EmptyValue label="No date" />,
  });
}

// TWO MEASURES, TWO COLUMNS. A company sizes in points or in minutes per
// PROJECT, and the engine orders by one or the other — so a single "Size"
// column falling back from one to the other would sort by a number half its
// cells were not showing.
function pointsColumn(): GridColumn<WorkSummary> {
  return sorted("points", {
    header: "Points",
    shrink: true,
    align: "right",
    optional: true,
    sortValue: (row) => row.points ?? 0,
    cell: (row) =>
      row.points ? <span className="t-num">{row.points}</span> : <EmptyValue label="Unestimated" />,
  });
}

function estimateColumn(): GridColumn<WorkSummary> {
  return sorted("estimate", {
    header: "Estimate",
    shrink: true,
    align: "right",
    optional: true,
    sortValue: (row) => row.estimate_min ?? 0,
    cell: (row) =>
      row.estimate_min ? (
        <span className="t-num">{fmtDuration(row.estimate_min * 60_000)}</span>
      ) : (
        <EmptyValue label="Unestimated" />
      ),
  });
}

// RELATIVE, WITH THE INSTANT ON THE TITLE — [DateCell]'s own rendering, which
// is what the compact row spelled by hand. One value, one drawing, wherever it
// appears: that is the rule `app/frame/cells.tsx` exists for.
function updatedColumn(now: number): GridColumn<WorkSummary> {
  return sorted("updated", {
    header: "Updated",
    shrink: true,
    sortValue: (row) => row.updated ?? "",
    cell: (row) => <DateCell at={row.updated} now={now} />,
  });
}

/**
 * Every column one shape can draw, in the order it draws them.
 *
 * ONE ENTRY POINT, so the Display menu offers exactly what the grid draws.
 * The menu renders a checkbox per column and writes `cols.<shape>=`, which
 * this grid reads — and a menu holding its own list of column names would be a
 * second declaration of them, wrong the first time a column is added and
 * looking exactly like a correct one.
 */
function buildColumns(shape: GridShape, ctx: ColumnContext): GridColumn<WorkSummary>[] {
  const out = shape === "table" ? tableColumns(ctx) : listColumns(ctx);
  if (!ctx.removals) return out;
  return out.concat(trashColumns(ctx.removals, ctx.chrome, ctx.now));
}

/** One column a reader may turn on or off, as the Display menu offers it. */
export interface ColumnChoice {
  key: string;
  label: string;
  /** Off until `cols.<shape>=` names it. */
  optional?: boolean;
}

/**
 * The columns a reader can choose between, DERIVED from the ones drawn.
 *
 * A menu that held its own list would be a second declaration of the grid's
 * columns — wrong the first time one is added, and indistinguishable from a
 * correct one. This calls the builder and takes the three facts a chooser
 * needs, so a column added above appears in the menu with no second edit.
 *
 * THE CELL RENDERERS ARE NEVER CALLED, which is what makes the stub context
 * honest rather than a fake: a name and a flag are properties of the COLUMN,
 * and nothing here touches a row.
 *
 * `workspace` decides whether the Project column exists at all, because it
 * does not inside a project — offering it there would be a checkbox for a
 * column the grid never draws.
 */
export function columnChoices(shape: GridShape, workspace: boolean): ColumnChoice[] {
  return buildColumns(shape, { chrome: {}, detail: null, now: 0, workspace }).map((column) => ({
    key: column.key,
    // THE HEAD, OR THE NAME IT CARRIES FOR WHERE THE HEAD IS A GLYPH — which
    // is exactly what `GridColumn.label` is for on the card layout a narrow
    // grid takes.
    label:
      typeof column.header === "string" && column.header
        ? column.header
        : (column.label ?? column.key),
    optional: column.optional,
  }));
}

export function WorkGrid({
  shape,
  rows,
  groups,
  axis,
  subAxis,
  chrome,
  detail,
  now,
  selected,
  workspace,
  hrefOf,
  onOpen,
  onOverflow,
  overflowHref,
  removals,
  foot,
}: {
  /** Which column set, and which `cols=` key. */
  shape: GridShape;
  rows: WorkSummary[];
  groups: WorkGroup[];
  axis: string;
  /** The second axis, where the answer was asked for one. */
  subAxis?: string;
  chrome: RowChrome;
  detail?: WorkProjectDetail | null;
  now: number;
  selected?: string;
  /** The company's own list rather than one project's. */
  workspace: boolean;
  hrefOf: (row: WorkSummary) => string;
  onOpen: (row: WorkSummary) => void;
  onOverflow: (axis: string, key: string) => void;
  /** As [Board]'s — the screen owns the address. */
  overflowHref: (axis: string, key: string) => string;
  /** Present only on a trash listing; see the header. */
  removals?: Removals;
  /**
   * THE END OF THE LIST, said — `lib/work.ts`'s `endNote`, and "" where the
   * answer is not complete.
   *
   * THE WORDS ARE THE SCREEN'S because they are a fact about the ANSWER — how
   * many matched, whether a cursor is outstanding, whether the count stopped at
   * its ceiling — and this component holds none of that. What it owns is WHERE
   * it goes: the grid's own foot, under the rows and under every band, so a
   * list that reached its end and one that was cut off stop looking identical.
   * On BOTH shapes, because "is that everything?" is the same question however
   * the rows are drawn — the table used to answer it with a bare "N loaded",
   * which is the count the toolbar above it already carries.
   */
  foot?: string;
}) {
  const ctx = useMemo<ColumnContext>(() => {
    // THE PAGE IS WHAT CAN RESOLVE AN EDGE. A dependency names the blocking
    // task's id and the key a reader recognises is on that task's own row, so
    // only something holding the rows can turn one into the other — and a
    // grouped answer's rows are under its groups rather than in `items`. Built
    // once per answer rather than per row, because a lookup rebuilt inside a
    // cell is quadratic over a page of a hundred.
    const keys = new Map<string, string>();
    const add = (list: WorkSummary[]) => {
      for (const row of list) keys.set(row.id, row.key);
    };
    add(rows);
    for (const group of groups) {
      add(group.rows);
      for (const sub of group.subgroups ?? []) add(sub.rows);
    }
    return {
      chrome,
      detail,
      now,
      workspace,
      removals,
      keyOf: (id: string) => keys.get(id),
    };
  }, [rows, groups, chrome, detail, now, workspace, removals]);

  const columns = useMemo(() => buildColumns(shape, ctx), [shape, ctx]);

  const labels = useMemo(
    () => ({
      statuses: detail?.statuses,
      types: chrome.types,
      tags: detail?.tags,
      seatName: chrome.seatName,
    }),
    [detail, chrome],
  );

  const bands = useMemo<GridBand<WorkSummary>[] | undefined>(() => {
    if (groups.length === 0) return undefined;
    return groups.map((group) => ({
      key: group.key || "—",
      mark: <GroupMark axis={axis} groupKey={group.key} chrome={chrome} />,
      label: headingOf(axis, group, labels),
      // THE ENGINE'S COUNT OVER THE WHOLE SET, never the band's row count:
      // a column of four hundred says four hundred and carries fifty.
      total: group.count,
      rows: group.rows,
      // THE SUB-BANDS REPLACE THE ROWS WHERE THERE ARE ANY, exactly as groups
      // replace `items` one level up: an answer carrying both would be the
      // same rows twice.
      bands: group.subgroups?.length
        ? group.subgroups.map((sub) => ({
            key: sub.key || "—",
            mark: <GroupMark axis={subAxis ?? ""} groupKey={sub.key} chrome={chrome} />,
            label: headingOf(subAxis ?? "", sub, labels),
            total: sub.count,
            rows: sub.rows,
          }))
        : undefined,
      footer: bandFoot({ group, axis, onOverflow, overflowHref }),
    }));
  }, [groups, axis, subAxis, chrome, labels, onOverflow, overflowHref]);

  return (
    <DataGrid
      rows={bands ? undefined : rows}
      bands={bands}
      columns={columns}
      rowKey={(row) => row.key}
      rowHref={hrefOf}
      onRowActivate={(row) => onOpen(row)}
      isSelected={(row) => row.key === selected}
      // THE ORDER IS THE QUERY'S. See the header: the grid writes `sort=`,
      // the screen sends it, and the engine orders the whole set.
      serverSorted
      // AND THE COLUMN SET IS THE SHAPE'S, which is the one key the two shapes
      // may not share — see the header.
      colsName={shape}
      empty={
        removals
          ? {
              title: "The trash is empty",
              hint: "Nothing in this container has been removed. A removal is reversible at any age, so what lands here stays until somebody restores or purges it.",
              icon: "delete",
            }
          : {
              title: "Nothing matches",
              hint: "No item on this node's copy of the tracker matches these filters.",
            }
      }
      footer={foot}
    />
  );
}

/**
 * What a band says under its rows, where it has anything to say.
 *
 * TWO SENTENCES AND NEVER BOTH. A band whose second axis ran out of room says
 * how many bands did not fit; a band carrying a page of its own count offers
 * the way to the rest. A group with sub-groups has no page of its own — its
 * rows are under them — so the overflow link is the ungrouped case's.
 */
function bandFoot({
  group,
  axis,
  onOverflow,
  overflowHref,
}: {
  group: WorkGroup;
  axis: string;
  onOverflow: (axis: string, key: string) => void;
  overflowHref: (axis: string, key: string) => string;
}) {
  // LANES BEYOND THE CAP ARE SAID, never silently cut — the same rule
  // `groups_dropped` follows one level up. A band whose second axis ran out of
  // room would otherwise read as a group with fewer members than its own
  // count.
  if (group.subgroups?.length) {
    if (!group.subgroups_dropped) return undefined;
    return `${group.subgroups_dropped} more band${
      group.subgroups_dropped === 1 ? "" : "s"
    } did not fit. Narrow the list to bring them into range.`;
  }
  if (group.count <= group.rows.length) return undefined;
  return (
    <a
      href={overflowHref(axis, group.key)}
      onClick={(e) => {
        e.preventDefault();
        onOverflow(axis, group.key);
      }}
    >
      {group.count - group.rows.length} more →
    </a>
  );
}

/** The two facts a trash listing adds, plus the way back. */
function trashColumns(
  removals: Removals,
  chrome: RowChrome,
  now: number,
): GridColumn<WorkSummary>[] {
  return [
    sorted("removed", {
      header: "Removed",
      shrink: true,
      // THE ENGINE'S OWN SORT KEY, which is what makes this head honest: the
      // trash already arrives newest-removal-first by default, and clicking
      // this asks for the other end of it over the whole set rather than
      // re-ordering the page by whichever entries the feed happened to carry.
      sortValue: (row) => removals.get(row.id)?.at ?? "",
      cell: (row) => {
        const record = removals.get(row.id);
        // THE FEED IS A PAGE. A row whose removal is older than the entries
        // loaded has no record here, and the honest cell says which fact is
        // missing rather than drawing an empty one that reads as "just now".
        if (!record) return <EmptyValue label="Its removal is older than the loaded history" />;
        return <DateCell at={record.at} now={now} />;
      },
    }),
    {
      key: "removed_by",
      header: "By",
      shrink: true,
      cell: (row) => {
        const record = removals.get(row.id);
        if (!record?.actor)
          return <EmptyValue label="Its removal is older than the loaded history" />;
        return (
          <span className="row gap-1">
            <SeatCell handle={record.actor} name={chrome.seatName?.(record.actor)} />
            {/* WHICH KIND OF WRITER, because that is the question a trash
                screen exists to answer: an assistant removing a subtree and
                a person removing one task look identical without it. */}
            {record.actor_kind && record.actor_kind !== "seat" && (
              <Tag appearance="outline">{record.actor_kind}</Tag>
            )}
          </span>
        );
      },
    },
    {
      key: "restore",
      header: "",
      label: "Restore",
      shrink: true,
      cell: (row) => <RestoreCall id={row.key} />,
    },
  ];
}

/**
 * The call that brings one task back, ready to paste.
 *
 * A COPY BUTTON RATHER THAN A RESTORE BUTTON, for the reason the whole
 * `ToolCall` surface exists: this dashboard reads, and a browser posting a
 * restore would be attributed to "the dashboard", which is not a person and
 * cannot be asked why. The full block is too tall for a grid row, so the row
 * carries the one call a reader on this tab wants and the peek carries the
 * rest.
 */
function RestoreCall({ id }: { id: string }) {
  const clip = useClipboard({ resetMs: COPIED_MS });
  const text = callText({
    tool: "restore_work_item",
    label: "Bring it back from the trash",
    args: { item: id },
  });
  return (
    <Button
      size="small"
      variant="tertiary"
      leadingIcon={<ContentCopyGlyph />}
      title={text}
      onClick={(e) => {
        // THE ROW IS AN ANCHOR. Without this the copy also opens the task,
        // so the reader lands on a page they did not ask for holding a call
        // they did.
        e.preventDefault();
        e.stopPropagation();
        void clip.copy(text);
      }}
    >
      {clip.state === "copied" ? "Copied" : "Restore"}
    </Button>
  );
}
