/**
 * The tracker read by COLUMN, and the trash read at all.
 *
 * # Why a fifth shape
 *
 * The list draws each task as a block — title, then badges, then dates, then
 * the seat — which is the right shape for "what is this task" and the wrong one
 * for every question about a FIELD. Comparing estimates down a list means
 * finding the same badge at a different x on every row, because the badge's
 * position depends on how long the title above it was. A table puts one field
 * per column and every value of that column at one x, which is what makes a
 * column scannable at all and its head worth clicking.
 *
 * # The sort is the engine's
 *
 * [DataGrid] writes the sort into `sort=`, and on this screen that key is the
 * one the query already carries — so a header click re-asks the question and
 * the whole set is ordered, not the hundred rows that happen to be loaded. That
 * is why every sortable column here is keyed on one of the engine's own sort
 * keys and why `serverSorted` is set: a grid that also sorted locally would
 * order the page a second time, by a comparator over rendered values, and the
 * two orders disagree on exactly the rows a page boundary falls between.
 *
 * A column the engine cannot order by is therefore NOT sortable here. That is
 * a real constraint rather than an oversight: a clickable head that wrote
 * `sort=blocked` would take the whole board down with a refusal, because the
 * query grammar refuses a sort key it does not know rather than ignoring it.
 *
 * # The trash is this grid with two more columns
 *
 * `removed=true` is what makes a listing the trash — see the engine's own
 * [ViewKeyTrash] — so this component takes the removals rather than a flag
 * naming a tab: any view saved with that parameter gets the same treatment,
 * which is the property a magic view key would not have.
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
import { DueMark, PriorityMark, StatusBadge, TypeIcon, type RowChrome } from "./work.tsx";
import { groupLabel } from "~/lib/work.ts";
import { fmtCount, fmtDuration } from "~/lib/format.ts";
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
 * Every column a header click orders by — and each name IS one of the query
 * grammar's own sort keys.
 *
 * [DataGrid] writes the column's key straight into `sort=`, which this screen
 * sends to the engine, and `ParseQuery` REFUSES a sort key it does not know
 * rather than ignoring it. So one wrong header does not mis-sort a column: it
 * takes the whole board down with a refusal, on the click.
 *
 * Held against `tracker.sortKeys` by `internal/tracker/sortkeys_client_test.go`,
 * because this is a copy the dashboard has to keep — a separate build in a
 * separate language cannot import a Go identifier. The union makes a typo a
 * compile error; the gate makes a RENAME in the engine one.
 */
const TABLE_SORT_KEYS = [
  "title",
  "priority",
  "due",
  "start",
  "points",
  "estimate",
  "updated",
  "removed",
] as const;

type SortKey = (typeof TABLE_SORT_KEYS)[number];

/**
 * A column ordered at its own head.
 *
 * The key is typed to [TABLE_SORT_KEYS] rather than to `string`, so a column
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

export function TableView({
  rows,
  groups,
  axis,
  chrome,
  detail,
  now,
  selected,
  workspace,
  hrefOf,
  onOpen,
  removals,
}: {
  rows: WorkSummary[];
  groups: WorkGroup[];
  axis: string;
  chrome: RowChrome;
  detail?: WorkProjectDetail | null;
  now: number;
  selected?: string;
  /** The company's own list rather than one project's — so project is a column. */
  workspace: boolean;
  hrefOf: (row: WorkSummary) => string;
  onOpen: (row: WorkSummary) => void;
  /** Present only on a trash listing; see the header. */
  removals?: Removals;
}) {
  const columns = useMemo<GridColumn<WorkSummary>[]>(() => {
    const out: GridColumn<WorkSummary>[] = [
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
        key: "type",
        header: "",
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
    if (workspace) {
      out.push({
        key: "project",
        header: "Project",
        shrink: true,
        cell: (row) => <span className="mono">{row.project}</span>,
      });
    }
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
      sorted("start", {
        header: "Start",
        shrink: true,
        optional: true,
        sortValue: (row) => row.start ?? "",
        cell: (row) =>
          row.start ? <DateCell at={row.start} now={now} /> : <EmptyValue label="No date" />,
      }),
      // TWO MEASURES, TWO COLUMNS. A company sizes in points or in minutes
      // per PROJECT, and the engine orders by one or the other — so a single
      // "Size" column falling back from one to the other would sort by a
      // number half its cells were not showing.
      sorted("points", {
        header: "Points",
        shrink: true,
        align: "right",
        optional: true,
        sortValue: (row) => row.points ?? 0,
        cell: (row) =>
          row.points ? (
            <span className="t-num">{row.points}</span>
          ) : (
            <EmptyValue label="Unestimated" />
          ),
      }),
      sorted("estimate", {
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
      }),
      sorted("updated", {
        header: "Updated",
        shrink: true,
        sortValue: (row) => row.updated ?? "",
        cell: (row) => <DateCell at={row.updated} now={now} />,
      }),
    );
    if (!removals) return out;
    return out.concat(trashColumns(removals, chrome, now));
  }, [chrome, detail, now, workspace, removals]);

  const bands = useMemo<GridBand<WorkSummary>[] | undefined>(() => {
    if (groups.length === 0) return undefined;
    return groups.map((group) => ({
      key: group.key || "—",
      label: groupLabel(axis, group, {
        statuses: detail?.statuses,
        types: chrome.types,
        tags: detail?.tags,
        seatName: chrome.seatName,
      }),
      // THE ENGINE'S COUNT OVER THE WHOLE SET, never the band's row count:
      // a column of four hundred says four hundred and carries fifty.
      total: group.count,
      rows: group.rows,
    }));
  }, [groups, axis, chrome, detail]);

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
      loadedNote={`${fmtCount(bands ? bands.reduce((n, b) => n + b.rows.length, 0) : rows.length)} loaded`}
    />
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
