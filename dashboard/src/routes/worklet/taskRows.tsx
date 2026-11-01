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
import { useNow } from "~/lib/clock.ts";
import { itemPath, statusLabel } from "~/lib/work.ts";
import type { WorkClaimTotal, WorkSummary } from "~/protocol/index.ts";

/** How many rows a list widget draws before it links to the rest. */
export const ROWS = 6;

/** A count the engine may have stopped at its ceiling, written as the floor it is. */
export function countLabel(total: number, capped?: boolean): string {
  return `${total.toLocaleString()}${capped ? "+" : ""}`;
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
  const now = useNow();
  return (
    <List className="home-tasks">
      {rows.map((row) => (
        <ListItem
          key={row.id}
          href={href(itemPath(row))}
          leading={<StatusMark status={row.status} />}
          trailing={
            trailing ? trailing(row) : <DueMark due={row.due} overdue={row.overdue} now={now} />
          }
        >
          <span className="home-tasks-line">
            <span className="home-tasks-key mono">{row.key}</span>
            <span className="home-tasks-title">{row.title}</span>
            <VisuallyHidden>{`, ${statusLabel(row.status)}`}</VisuallyHidden>
          </span>
        </ListItem>
      ))}
    </List>
  );
}

/** The "and N more" line under a list that holds more than it draws. */
export function MoreLink({
  shown,
  total,
  to,
  query,
  noun,
}: {
  shown: number;
  total: WorkClaimTotal | undefined;
  to: string[];
  query?: Record<string, string>;
  noun: string;
}) {
  if (!total || total.total <= shown) return null;
  return (
    <a className="home-card-more t-link" href={href(to, query)}>
      {`All ${countLabel(total.total, total.capped)} ${total.total === 1 ? noun : `${noun}s`}`}
    </a>
  );
}
