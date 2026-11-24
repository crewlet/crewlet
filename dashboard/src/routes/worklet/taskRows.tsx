/**
 * Worklet's rows, narrowed to a card, shared by Worklet's, My work's and
 * Inbox's widgets.
 *
 * A widget is a third of the page as often as the whole of it, so a task is
 * drawn as one line of the design system's list (its state, its key and its
 * name, and the one fact the list is about) rather than as Worklet's grid, whose
 * columns need the page's width.
 *
 * Not a `*.widgets.tsx` module: it offers no widgets of its own.
 */

import type { ReactNode } from "react";
import { List, ListItem, VisuallyHidden } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { DueMark, StatusMark } from "~/components/work.tsx";
import { itemPath, statusLabel } from "~/lib/work.ts";
import type { WorkClaimTotal, WorkSummary } from "~/protocol/index.ts";
import { WidgetList } from "~/routes/home/board/kit.tsx";

/** How many rows a list widget draws at its content's height. */
export const ROWS = 6;

/** A count the engine may have stopped at its ceiling, written as the floor it is. */
export function countLabel(total: number, capped?: boolean): string {
  return `${total.toLocaleString()}${capped ? "+" : ""}`;
}

/** One task as one line: the state, the key and the name, and the fact the list is about. */
function taskRow(row: WorkSummary, trailing?: (row: WorkSummary) => ReactNode) {
  return (
    <ListItem
      key={row.id}
      href={href(itemPath(row))}
      leading={<StatusMark status={row.status} />}
      trailing={trailing ? trailing(row) : <DueMark due={row.due} overdue={row.overdue} />}
    >
      <span className="home-tasks-line">
        <span className="home-tasks-key mono">{row.key}</span>
        <span className="home-tasks-title">{row.title}</span>
        <VisuallyHidden>{`, ${statusLabel(row.status)}`}</VisuallyHidden>
      </span>
    </ListItem>
  );
}

/**
 * Tasks as one line each: the state, the key and the name, and the fact the
 * list is about at its end (a due date by default).
 */
export function TaskList({
  rows,
  trailing,
}: {
  rows: readonly WorkSummary[];
  trailing?: (row: WorkSummary) => ReactNode;
}) {
  return <List className="home-tasks">{rows.map((row) => taskRow(row, trailing))}</List>;
}

/**
 * [TaskList]'s rows in a widget, fitted to its card, with the tasks `total`
 * counts beyond them said in the card's foot.
 */
export function WidgetTasks({
  rows,
  total,
  ordinals = false,
}: {
  rows: readonly WorkSummary[];
  total?: WorkClaimTotal | undefined;
  /** Numbered, keyed and named only: an order the reader works down. */
  ordinals?: boolean;
}) {
  return (
    <WidgetList
      variant={ordinals ? "numbered" : undefined}
      className="home-tasks"
      rows={ROWS}
      total={total?.total}
      capped={total?.capped}
    >
      {ordinals
        ? rows.map((row) => (
            <ListItem key={row.id} href={href(itemPath(row))}>
              <span className="home-tasks-line">
                <span className="home-tasks-key mono">{row.key}</span>
                <span className="home-tasks-title">{row.title}</span>
              </span>
            </ListItem>
          ))
        : rows.map((row) => taskRow(row))}
    </WidgetList>
  );
}
