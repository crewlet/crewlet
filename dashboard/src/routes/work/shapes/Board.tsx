/**
 * The board shape: one lane per value of the axis, cards inside them.
 *
 * A LANE IS A RECESS AND A CARD IS AN OBJECT ON IT — `styles/screens.css`
 * carries that reasoning, and it is the one place this product spends
 * elevation. Nothing here writes, so there is no drag: a rank is a value on
 * the task, and dragging one would be the dashboard deciding a team's order.
 *
 * A COLUMN'S COUNT IS OVER THE WHOLE SET and its rows are a slice, so a column
 * of four hundred says four hundred and hands back fifty. The rest are
 * reachable rather than merely counted.
 */

import { BoardCard, type RowChrome } from "~/components/work.tsx";
import { GroupMark, headingOf } from "./group.tsx";
import type { WorkGroup, WorkProjectDetail, WorkSummary } from "~/protocol/index.ts";

export function Board({
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
  groups: WorkGroup[];
  axis: string;
  chrome: RowChrome;
  detail?: WorkProjectDetail | null;
  now: number;
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
  const ctx = {
    statuses: detail?.statuses,
    types: chrome.types,
    tags: detail?.tags,
    seatName: chrome.seatName,
  };
  return (
    <div className="work-board">
      {groups.map((group) => (
        <section className="work-col" key={group.key || "—"}>
          <header className="work-col-head">
            <GroupMark axis={axis} groupKey={group.key} chrome={chrome} />
            <span className="truncate">{headingOf(axis, group, ctx)}</span>
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
              />
            ))}
            {/* A DECLARED LANE WITH NOTHING IN IT. The engine's grouping is a
                plain GROUP BY, so this was unreachable from any real answer
                until the screen started padding a closed-set axis to its whole
                declared set (`lib/work.ts`'s `padGroups`) — which is what makes
                a board the WORKFLOW rather than the occupied part of it. A
                heading over nothing at all reads as rows that failed to
                arrive. */}
            {group.rows.length === 0 && <div className="work-col-empty">Nothing here</div>}
          </div>
          {/* The link is THIS screen — this project, these filters — as a list
              narrowed to this column. See `patchedHref` for why the href cannot
              be built here. */}
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
      ))}
    </div>
  );
}
