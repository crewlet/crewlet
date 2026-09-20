/**
 * The list shape: one line per task, under the bands the answer grouped it in.
 *
 * # A band, not a card per group
 *
 * Every group was a `Card` with its own border, radius, shadow and padding, so
 * a list of six statuses was six panels stacked down the page and the rows
 * inside them started 24px in from the rows of the group above. A band is a
 * heading ON the list — one continuous run of rows, one left edge, and the
 * group's name and count sticky above its own rows — which is what makes a
 * grouped list scannable as one list rather than as six.
 *
 * # The second axis is a band inside a band
 *
 * `group_by2` comes back as `subgroups` on each group, and this is the one
 * pair of shapes that draws them: a list is lines, and nesting lines under a
 * deeper heading is what a second axis means where there is no second
 * dimension to spend. A board's would be a swimlane grid, which is a different
 * drawing — `buildItemsParams` drops the key there rather than asking for
 * columns this shape cannot place.
 *
 * # A count is the engine's and the rows are a page
 *
 * Every band says both, because a heading reading `12` over twelve visible
 * rows out of three hundred is the number a person plans against — the rule
 * `DataGrid`'s own bands already keep.
 */

import { RowList, type RowChrome } from "~/components/work.tsx";
import { GroupMark, headingOf } from "./group.tsx";
import type { WorkGroup, WorkProjectDetail, WorkSummary } from "~/protocol/index.ts";

export function List({
  rows,
  groups,
  axis,
  subAxis,
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
  /** The second axis, where the answer was asked for one. */
  subAxis?: string;
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
  const ctx = {
    statuses: detail?.statuses,
    types: chrome.types,
    tags: detail?.tags,
    seatName: chrome.seatName,
  };

  if (groups.length > 0) {
    return (
      <div className="work-list">
        {groups.map((group) => (
          <section className="work-band-group" key={group.key || "—"}>
            <header className="work-band">
              <GroupMark axis={axis} groupKey={group.key} chrome={chrome} />
              <span className="truncate">{headingOf(axis, group, ctx)}</span>
              <span className="count-chip">{group.count}</span>
            </header>
            {/* THE SUBGROUPS REPLACE THE ROWS WHERE THERE ARE ANY, exactly as
                groups replace `items` one level up: an answer that carried both
                would be the same rows twice. */}
            {group.subgroups?.length ? (
              <>
                {group.subgroups.map((sub) => (
                  <section className="work-band-group" key={sub.key || "—"}>
                    <header className="work-band work-band-sub">
                      <GroupMark axis={subAxis ?? ""} groupKey={sub.key} chrome={chrome} />
                      <span className="truncate">{headingOf(subAxis ?? "", sub, ctx)}</span>
                      <span className="count-chip">{sub.count}</span>
                    </header>
                    <RowList
                      rows={sub.rows}
                      now={now}
                      chrome={chrome}
                      hrefOf={hrefOf}
                      onOpen={onOpen}
                      selected={selected}
                    />
                  </section>
                ))}
                {/* LANES BEYOND THE CAP ARE SAID, never silently cut — the same
                    rule `groups_dropped` follows one level up. A band whose
                    second axis ran out of room would otherwise read as a group
                    with fewer members than its own count. */}
                {group.subgroups_dropped ? (
                  <div className="work-band-foot">
                    {group.subgroups_dropped} more band
                    {group.subgroups_dropped === 1 ? "" : "s"} did not fit. Narrow the list to bring
                    them into range.
                  </div>
                ) : null}
              </>
            ) : (
              <RowList
                rows={group.rows}
                now={now}
                chrome={chrome}
                hrefOf={hrefOf}
                onOpen={onOpen}
                selected={selected}
              />
            )}
            {group.count > group.rows.length && !group.subgroups?.length && (
              <div className="work-band-foot">
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
  if (rows.length === 0) return null;
  return (
    <div className="work-list">
      <RowList
        rows={rows}
        now={now}
        chrome={chrome}
        hrefOf={hrefOf}
        onOpen={onOpen}
        selected={selected}
      />
    </div>
  );
}
