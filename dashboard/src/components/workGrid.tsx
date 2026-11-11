/**
 * Tasks as the kit's grid, wherever a task is listed outside Worklet's own
 * screen: My work's claims and priorities, and a seat's queue.
 *
 * ONE COLUMN SET, THE LIST'S. Worklet's list (`routes/worklet/shapes/Grid.tsx`)
 * and these lists draw the same eight facts, in the same order, from the
 * columns here. What the list adds is its own: a writer's inline pickers, the
 * engine's order at the heads, and the columns a reader can switch on.
 *
 * THE HEADS ARE PLAIN. Each list here is one page of a claim the engine orders
 * itself, so a press could only order the page while reading as an order over
 * the whole claim.
 */

import { useMemo, type ReactNode } from "react";
import {
  Card,
  EmptyValue,
  Tag,
  TableDateCell,
  TableKeyCell,
  type DataGridColumn,
} from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import {
  Assignee,
  DueMark,
  PriorityMark,
  StatusBadge,
  TypeIcon,
  blockedBy,
  type RowChrome,
} from "~/components/work.tsx";
import { checklistTask, itemAddress, readsBlocked } from "~/lib/work.ts";
import { plural } from "~/lib/format.ts";
import type {
  WorkChecklistRow,
  WorkClaimTotal,
  WorkHoldingRow,
  WorkSummary,
} from "~/protocol/index.ts";

type Column = DataGridColumn<WorkSummary>;

/** What a list's columns are drawn from. */
export interface WorkColumnContext {
  chrome: RowChrome;
  /** A blocker's key from its id, where the list holds its row: see [blockedBy]. */
  keyOf?: (id: string) => string | undefined;
}

/** A cell a screen draws its own way, such as a writer's inline picker. */
export interface WorkColumnCells {
  priority?: (row: WorkSummary) => ReactNode;
  status?: (row: WorkSummary) => ReactNode;
  assignee?: (row: WorkSummary) => ReactNode;
}

/**
 * The compact list's columns, by name, so a screen can place them among its own.
 *
 * The three that open a row are the three a reader scans down, so the title
 * always starts at one x; on a phone a task is two lines, key and title and
 * then its marks.
 */
export function workListColumns(ctx: WorkColumnContext, cells: WorkColumnCells = {}) {
  const { chrome, keyOf } = ctx;
  const priority: Column = {
    key: "priority",
    // A GLYPH HEAD: the mark is the whole column, so there is no word for a
    // sort button to be named by.
    header: "",
    label: "Priority",
    icon: true,
    cell: cells.priority ?? ((row) => <PriorityMark priority={row.priority} />),
  };
  const key: Column = {
    key: "key",
    header: "Key",
    shrink: true,
    phoneLead: true,
    cell: (row) => <TableKeyCell value={row.key} />,
  };
  const status: Column = {
    key: "status",
    header: "Status",
    shrink: true,
    cell: cells.status ?? ((row) => <StatusBadge status={row.status} defs={chrome.statuses} />),
  };
  const title: Column = {
    key: "title",
    header: "Title",
    phoneLead: true,
    // THE WHOLE TITLE ON HOVER: the span below cuts it where the kit cannot measure.
    title: (row) => row.title,
    cell: (row) => (
      <>
        <span className="truncate">{row.title}</span>
        {/* WHICH TASK HOLDS THIS ONE UP, not merely that something does: the
            first of them, because a row is one line and a task waiting on four
            is still one fact. */}
        {readsBlocked(row) && (
          <Tag variant="danger" appearance="outline">
            {blockedBy(row, keyOf)}
          </Tag>
        )}
      </>
    ),
  };
  const due: Column = {
    key: "due",
    header: "Due",
    shrink: true,
    align: "right",
    cell: (row) =>
      row.due ? <DueMark due={row.due} overdue={row.overdue} /> : <EmptyValue label="No date" />,
  };
  // TWO MARKS UNDER WORDS: a glyph head left the type and the seat looking like
  // decoration rather than columns, and a sortable head needs a word to press.
  const type: Column = {
    key: "type",
    header: "Type",
    shrink: true,
    align: "center",
    cell: (row) => <TypeIcon type={row.type} types={chrome.types} />,
  };
  const assignee: Column = {
    key: "assignee",
    header: "Assignee",
    shrink: true,
    align: "center",
    cell:
      cells.assignee ??
      ((row) => (
        <Assignee
          handle={row.assignee}
          seatName={chrome.seatName}
          seatKind={chrome.seatKind}
          seatAvatar={chrome.seatAvatar}
        />
      )),
  };
  const updated: Column = {
    key: "updated",
    header: "Updated",
    shrink: true,
    align: "right",
    // THE FIRST TO GIVE WAY ON A PHONE as it is at 1280: what the task's page
    // answers one click away.
    phoneOmit: true,
    cell: (row) => <TableDateCell value={row.updated} />,
  };
  return { priority, key, status, title, due, type, assignee, updated };
}

/**
 * WHAT GIVES WAY FIRST when the list does not fit its box (`drop`, lowest
 * first). Beside a peek at 1280 the list is about 450px wide, and with every
 * column kept the title was 70px of "Which regi…".
 *
 * NEVER THE TITLE, AND NEVER WHAT OPENS OR OWNS A ROW: the key, the status and
 * who holds it. What goes first is what the task's own page answers one click
 * away, and the grid says which it hid.
 */
export const LIST_GIVES_WAY: Record<string, number> = {
  updated: 1,
  start: 2,
  points: 3,
  estimate: 4,
  project: 5,
  due: 6,
  type: 7,
  priority: 8,
};

/**
 * THE TITLE'S FLOOR: the narrowest it is still a title at. Sixteen ems carries
 * about thirty-five characters, a task title's first five or six words, which
 * is what tells two tasks apart down a list. Ten ems, tried first, kept every
 * mark and cut every title to its first three words.
 */
const TITLE_FLOOR = "16rem";

/** A set of task columns with its floor, its whole words and its give-way order. */
export function givingWay(columns: Column[], order: Record<string, number>): Column[] {
  return columns.map((c) => {
    if (c.key === "title") return { ...c, floor: TITLE_FLOOR };
    // THE STATUS AND THE KEY ARE DRAWN WHOLE. A shrink column's cap read
    // "In progr…" and "FA…" beside a peek, and neither is ever long by nature.
    if (c.key === "status" || c.key === "key") return { ...c, width: "max-content" };
    return order[c.key] !== undefined ? { ...c, drop: order[c.key] } : c;
  });
}

/** The list's own columns in the order it draws them, with nothing switched on. */
export function workRowColumns(ctx: WorkColumnContext, cells?: WorkColumnCells): Column[] {
  const c = workListColumns(ctx, cells);
  return givingWay(
    [c.priority, c.key, c.status, c.title, c.due, c.type, c.assignee, c.updated],
    LIST_GIVES_WAY,
  );
}

/**
 * A blocker's key from its id: an edge names the blocking task's id, and the
 * key a reader recognises is on that task's own row, so only what holds the
 * rows can turn one into the other. Built once per list, not per row.
 */
export function keysOf(rows: readonly WorkSummary[]) {
  const keys = new Map(rows.map((row) => [row.id, row.key]));
  return (id: string) => keys.get(id);
}

/**
 * A person's held work that other work waits on, the most waited on first: the
 * list's own opening columns, then how much waits and on whom, then how long
 * it has been held. The order is the engine's, never re-sorted here.
 */
export function HoldingList({
  rows,
  chrome,
  hrefOf,
  now,
  label,
}: {
  rows: WorkHoldingRow[];
  chrome: RowChrome;
  hrefOf: (row: WorkSummary) => string;
  now: number;
  label: string;
}) {
  const columns = useMemo(() => {
    const c = workListColumns({ chrome, keyOf: keysOf(rows) });
    const waiting: DataGridColumn<WorkHoldingRow> = {
      key: "waiting",
      header: "Waiting on it",
      cell: (row) => (
        <span className="truncate">{holdingWaiting(row, (h) => chrome.seatName?.(h) ?? h)}</span>
      ),
    };
    const held: DataGridColumn<WorkHoldingRow> = {
      key: "held",
      header: "Held for",
      shrink: true,
      align: "right",
      cell: (row) => <span className="t-caption">{heldFor(row.held_since, now)}</span>,
    };
    return [
      c.priority,
      c.key,
      c.title,
      waiting,
      held,
      c.status,
    ] as DataGridColumn<WorkHoldingRow>[];
  }, [chrome, rows, now]);
  return (
    <DataGrid
      aria-label={label}
      rows={rows}
      columns={columns}
      rowKey={(row) => row.id}
      rowHref={hrefOf}
      phoneRows="compact"
    />
  );
}

/**
 * What waits on a held row, as its cell says it: the person's answer where the
 * row is held by a question to them, then the items behind it, how many of
 * them agents hold, and who holds them.
 */
export function holdingWaiting(row: WorkHoldingRow, nameOf: (handle: string) => string): string {
  const behind = `${plural(row.blocks, "item")}${row.blocks_capped ? " or more" : ""}`;
  const base = row.asked ? `Your answer${row.blocks > 0 ? `, then ${behind}` : ""}` : behind;
  const agents = row.agent_waiting > 0 ? ` (${plural(row.agent_waiting, "agent item")})` : "";
  const who = row.holders.map(nameOf).join(", ");
  return `${base}${agents}${who ? `, held by ${who}` : ""}`;
}

/** How long a row has been held, as a span a person reads: "3 days". */
export function heldFor(since: string, now: number): string {
  const at = Date.parse(since);
  if (Number.isNaN(at)) return "";
  const hours = Math.max(0, Math.floor((now - at) / 3_600_000));
  if (hours < 1) return "under an hour";
  if (hours < 48) return plural(hours, "hour");
  return plural(Math.floor(hours / 24), "day");
}

/** A page of tasks. Draws nothing of its own when it holds none: see its callers. */
export function RowList({
  rows,
  chrome,
  hrefOf,
  name,
  label,
}: {
  rows: WorkSummary[];
  chrome?: RowChrome;
  hrefOf: (row: WorkSummary) => string;
  /** Which grid on this screen, for the address (`cols.<name>=`). */
  name?: string;
  /** What the table is called, for assistive technology. */
  label?: string;
}) {
  const columns = useMemo(
    () => workRowColumns({ chrome: chrome ?? {}, keyOf: keysOf(rows) }),
    [chrome, rows],
  );
  return (
    <DataGrid
      name={name}
      aria-label={label}
      rows={rows}
      columns={columns}
      rowKey={(row) => row.id}
      rowHref={hrefOf}
      phoneRows="compact"
    />
  );
}

/**
 * A stack of tasks under a heading, as a card.
 *
 * THE SCREEN CHOOSES THE FRAME AND THIS IS ONE OF THEM. A seat's page stacks
 * several of these among other cards, so a heading and a count are what tell
 * one block from the next; My work draws the same rows under a tab.
 *
 * ABSENT WHEN IT HOLDS NOTHING, which is right for a stack: a page of seven
 * "nothing here" panels buries the one that has something.
 */
export function TaskBlock({
  title,
  hint,
  total,
  rows,
  chrome,
  hrefOf,
  name,
}: {
  title: string;
  hint?: string;
  /**
   * The engine's count of the WHOLE claim (`WorkMyWork.totals`), which is what
   * the header draws, never `rows.length`, which is the page. Absent while the
   * answer has not arrived, which draws no figure rather than the page's.
   */
  total: WorkClaimTotal | undefined;
  rows: WorkSummary[];
  chrome?: RowChrome;
  /** Where a row goes. The screen owns the address. */
  hrefOf: (row: WorkSummary) => string;
  /** Which grid on this screen: a seat's page stacks several. */
  name: string;
}) {
  if (rows.length === 0) return null;
  return (
    <Card padding="none">
      <Card.Header
        subtitle={hint}
        count={total ? `${total.total.toLocaleString()}${total.capped ? "+" : ""}` : undefined}
      >
        <Card.Title>{title}</Card.Title>
      </Card.Header>
      <RowList rows={rows} chrome={chrome} hrefOf={hrefOf} name={name} label={title} />
    </Card>
  );
}

/**
 * Sub-items claimed by one person on tasks that are not theirs.
 *
 * ITS OWN LIST because no assignee filter over tasks reaches one: a person
 * holding six checklist items and no assignment reads their queue as empty.
 * Here rather than in either screen that draws it, because a route importing
 * another route's module pulls that workspace's whole chunk into its own.
 *
 * Nothing when there are none: what an empty one draws is the caller's
 * decision.
 */
export function ChecklistClaims({
  rows,
  hrefOf,
  name,
}: {
  rows: WorkChecklistRow[];
  /**
   * Where a task goes, by its ADDRESS (`itemAddress`): the key, unless another
   * task claimed that key first. The screen owns the route.
   */
  hrefOf: (address: string) => string;
  /** Which grid on this screen, for the address (`cols.<name>=`). */
  name?: string;
}) {
  if (rows.length === 0) return null;
  return (
    <DataGrid<WorkChecklistRow>
      name={name}
      aria-label="Checklist items"
      rows={rows}
      rowKey={(item) => `${item.task}:${item.item}`}
      rowHref={(item) => hrefOf(itemAddress(checklistTask(item)))}
      phoneRows="compact"
      columns={[
        {
          key: "task",
          header: "Task",
          shrink: true,
          width: "max-content",
          phoneLead: true,
          cell: (item) => <TableKeyCell value={item.task_key} />,
        },
        {
          key: "name",
          header: "Item",
          floor: "12rem",
          overflow: "truncate",
          phoneLead: true,
          cell: (item) => item.name,
        },
        {
          key: "title",
          header: "On",
          overflow: "truncate",
          drop: 1,
          cell: (item) => item.task_title,
        },
        {
          key: "done",
          header: "Done",
          shrink: true,
          cell: (item) =>
            item.done ? (
              <Tag variant="success" appearance="outline">
                Done
              </Tag>
            ) : (
              <EmptyValue label="Not done yet" />
            ),
        },
      ]}
    />
  );
}
