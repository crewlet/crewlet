/**
 * The date axis: one bar per task, ruled by week, with the dependencies drawn
 * between them.
 *
 * READ-ONLY, deliberately. No dragging, no resizing, no handle to stretch —
 * a bar is a LINK. Every timeline that offers to move dates has to answer what
 * a drag means when a task has a dependency, or is somebody
 * else's, and the answer is a modal dialogue nobody wanted; the tracker's own
 * edit surfaces already say those things properly.
 *
 * THE ARITHMETIC IS `lib/timeline.ts`. Everything here is geometry: a column
 * is a CSS grid track, a bar spans tracks, and an arrow is an SVG path in a
 * layer over the same grid. Nothing in this file decides which day a task
 * falls on.
 */

import { Fragment, useMemo } from "react";
import type { WorkSummary } from "~/protocol/index.ts";
import {
  barTitle,
  monthDay,
  timelineOf,
  weekTicks,
  type Timeline,
  type TimelineBar,
} from "~/lib/timeline.ts";
import { Assignee, PriorityMark, TypeIcon, type RowChrome } from "~/components/work.tsx";
import { EmptyState, Tag, cx } from "@crewlethq/ui";
import { TimelineGlyph } from "@crewlethq/icons/glyphs";
import { plural } from "~/lib/format.ts";

/** How wide one day is, in pixels. */
const DAY_PX = 26;

/** How tall one row is, so an arrow can find the bar it points at. */
const ROW_PX = 30;

/** How wide the name column is. */
const NAME_PX = 260;

/**
 * How many day columns a bar needs before its title fits inside it.
 *
 * Six: at [DAY_PX] that is about 150px, which holds four or five words before
 * the ellipsis. Below it the label goes BESIDE the bar instead — a one-day
 * deadline marker rendered with the title inside showed a single clipped
 * letter, which reads as a rendering fault rather than as a date.
 */
const LABEL_INSIDE_DAYS = 6;

/** Where a bar's tone comes from — the same vocabulary the rest of the
 *  tracker uses, so a blocked bar and a blocked card read alike. */
function barTone(bar: TimelineBar): string {
  if (bar.kind === "inverted") return "critical";
  if (bar.row.blocked) return "blocked";
  if (bar.row.overdue) return "overdue";
  const group = bar.row.status_group;
  if (group === "done" || group === "closed") return "done";
  if (group === "active") return "active";
  return "planned";
}

/**
 * The arrows, as one SVG layer over the grid.
 *
 * ONE LAYER rather than an element per edge, because an arrow crosses rows and
 * anything positioned inside a row would be clipped by it. The path is an
 * elbow — out of the blocker's right edge, along, and into the dependent's
 * left — which is what every planner draws and the only shape that stays
 * readable when two bars are on the same line.
 */
function Arrows({ line, height }: { line: Timeline; height: number }) {
  const at = useMemo(() => {
    const index = new Map<string, { bar: TimelineBar; row: number }>();
    line.bars.forEach((bar, row) => index.set(bar.row.id, { bar, row }));
    return index;
  }, [line.bars]);

  const paths = line.edges.flatMap((edge) => {
    const from = at.get(edge.from);
    const to = at.get(edge.to);
    if (!from || !to) return [];
    const x1 = from.bar.to * DAY_PX;
    const y1 = from.row * ROW_PX + ROW_PX / 2;
    const x2 = to.bar.from * DAY_PX;
    const y2 = to.row * ROW_PX + ROW_PX / 2;
    // OUT, DOWN, IN. The elbow leaves 6px of clearance either side so the
    // corner never sits under a bar's own end cap.
    const out = x1 + 6;
    const into = Math.max(out, x2 - 6);
    const d = `M ${x1} ${y1} H ${out} V ${y2} H ${into} L ${x2} ${y2}`;
    return [
      <path
        key={`${edge.from}-${edge.to}`}
        className={cx("tl-arrow", edge.open ? "is-open" : "is-cleared")}
        // DASHED WHEN ONE-SIDED: the blocker does not list this dependent
        // back, so the edge is one only one end can see.
        strokeDasharray={edge.oneSided ? "3 3" : undefined}
        d={d}
        markerEnd="url(#tl-head)"
      />,
    ];
  });

  if (paths.length === 0) return null;
  return (
    <svg className="tl-arrows" width={line.days * DAY_PX} height={height} aria-hidden="true">
      <defs>
        <marker id="tl-head" markerWidth="6" markerHeight="6" refX="5" refY="3" orient="auto">
          <path d="M 0 0 L 6 3 L 0 6 z" className="tl-head" />
        </marker>
      </defs>
      {paths}
    </svg>
  );
}

/** One band of the timeline: a heading, its bars, and the axis behind them. */
function Band({
  title,
  count,
  rows,
  chrome,
  hrefOf,
  onOpen,
  selected,
  now,
}: {
  title?: string;
  count?: number;
  rows: WorkSummary[];
  chrome: RowChrome;
  hrefOf: (row: WorkSummary) => string;
  onOpen: (row: WorkSummary) => void;
  selected: string;
  now: number;
}) {
  const line = useMemo(() => timelineOf(rows, { now }), [rows, now]);
  const ticks = useMemo(() => weekTicks(line), [line]);
  const height = line.bars.length * ROW_PX;

  return (
    <div className="tl-band">
      {title !== undefined && (
        <div className="tl-band-head">
          <span className="tl-band-name">{title}</span>
          {count !== undefined && <span className="tl-band-count">{count}</span>}
        </div>
      )}
      {line.days === 0 ? (
        <div className="tl-none">
          Nothing here carries a start or a due date, so there is no axis to draw it against.
        </div>
      ) : (
        <div className="tl-scroll">
          <div className="tl-inner" style={{ ["--tl-days" as string]: line.days }}>
            <div className="tl-axis" style={{ width: line.days * DAY_PX }}>
              {ticks.map((tick) => (
                <span key={tick.day} className="tl-tick" style={{ left: tick.at * DAY_PX }}>
                  {tick.label}
                </span>
              ))}
              {line.today >= 0 && (
                <span
                  className="tl-today"
                  style={{ left: line.today * DAY_PX }}
                  title={`Today, ${monthDay(line.from)} + ${line.today} days`}
                />
              )}
            </div>
            <div className="tl-rows" style={{ width: line.days * DAY_PX, height }}>
              {/* The week rules, behind everything. */}
              {ticks.map((tick) => (
                <span key={tick.day} className="tl-rule" style={{ left: tick.at * DAY_PX }} />
              ))}
              {line.today >= 0 && <span className="tl-now" style={{ left: line.today * DAY_PX }} />}
              <Arrows line={line} height={height} />
              {line.bars.map((bar, i) => (
                <a
                  key={bar.row.id}
                  className={cx("tl-bar", selected === bar.row.key && "selected")}
                  data-tone={barTone(bar)}
                  data-kind={bar.kind}
                  href={hrefOf(bar.row)}
                  title={`${bar.row.key} · ${bar.row.title} — ${barTitle(bar)}`}
                  style={{
                    left: bar.from * DAY_PX,
                    width: (bar.to - bar.from) * DAY_PX,
                    top: i * ROW_PX + 4,
                  }}
                  onClick={(e) => {
                    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
                    e.preventDefault();
                    onOpen(bar.row);
                  }}
                >
                  {bar.to - bar.from >= LABEL_INSIDE_DAYS && (
                    <span className="tl-bar-label">{bar.row.title}</span>
                  )}
                </a>
              ))}
              {/* THE LABELS FOR THE NARROW BARS, beside them rather than in
                  them. Separate elements so they can overflow the bar's own
                  box, which `overflow: hidden` is what keeps the wide ones
                  tidy. */}
              {line.bars.map((bar, i) =>
                bar.to - bar.from >= LABEL_INSIDE_DAYS ? null : (
                  <span
                    key={`${bar.row.id}-label`}
                    className="tl-bar-aside"
                    style={{ left: bar.to * DAY_PX + 4, top: i * ROW_PX + 4 }}
                  >
                    {bar.row.title}
                  </span>
                ),
              )}
            </div>
          </div>
          {/* THE NAMES ARE A STICKY COLUMN, in the same row order as the bars,
              so a scroll along the axis keeps the labels. */}
          <div className="tl-names" style={{ width: NAME_PX }}>
            {line.bars.map((bar) => (
              <a
                key={bar.row.id}
                className={cx("tl-name", selected === bar.row.key && "selected")}
                href={hrefOf(bar.row)}
                title={`${bar.row.key} · ${bar.row.title}`}
                onClick={(e) => {
                  if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
                  e.preventDefault();
                  onOpen(bar.row);
                }}
              >
                <TypeIcon type={bar.row.type} types={chrome.types} />
                <span className="work-key mono">{bar.row.key}</span>
                <span className="truncate">{bar.row.title}</span>
                <PriorityMark priority={bar.row.priority} />
                <Assignee handle={bar.row.assignee} seatName={chrome.seatName} />
              </a>
            ))}
          </div>
        </div>
      )}

      {/* WHAT THE AXIS COULD NOT SAY, beneath it rather than omitted: a reader
          who cannot see the omission reads the picture as the whole of it. */}
      {(line.unscheduled.length > 0 || line.edgesOffAxis > 0) && (
        <div className="tl-aside">
          {line.unscheduled.length > 0 && (
            <div className="tl-unscheduled">
              <Tag appearance="outline">Unscheduled</Tag>
              <span className="tl-aside-note">
                {plural(line.unscheduled.length, "item")} with neither a start nor a due date. They
                have no bar because placing them would invent a schedule nobody set.
              </span>
              <div className="tl-chips">
                {line.unscheduled.map((row) => (
                  <a
                    key={row.id}
                    className="tl-chip"
                    href={hrefOf(row)}
                    title={`${row.key} · ${row.title}`}
                    onClick={(e) => {
                      if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
                      e.preventDefault();
                      onOpen(row);
                    }}
                  >
                    <TypeIcon type={row.type} types={chrome.types} />
                    <span className="work-key mono">{row.key}</span>
                    <span className="truncate">{row.title}</span>
                  </a>
                ))}
              </div>
            </div>
          )}
          {line.edgesOffAxis > 0 && (
            <div className="tl-aside-note">
              {plural(line.edgesOffAxis, "dependency", "dependencies")} not drawn: the blocker is
              off this page, or carries no dates of its own.
            </div>
          )}
        </div>
      )}
    </div>
  );
}

/**
 * The timeline view.
 *
 * ONE AXIS PER BAND rather than one across all of them. A band's window is
 * derived from ITS rows, so a group whose work sits in March is drawn against
 * March instead of being squeezed into the corner of a company-wide year —
 * which is the failure that makes a shared axis useless the moment one group
 * plans further out than the rest.
 */
export function TimelineView({
  rows,
  groups,
  chrome,
  hrefOf,
  onOpen,
  selected,
  now,
}: {
  rows: WorkSummary[];
  groups: { key: string; label?: string; count: number; rows: WorkSummary[] }[];
  chrome: RowChrome;
  hrefOf: (row: WorkSummary) => string;
  onOpen: (row: WorkSummary) => void;
  selected: string;
  now: number;
}) {
  if (groups.length === 0 && rows.length === 0) {
    return (
      // THE MARK SAYS WHICH SCREEN IS EMPTY. uilet's EmptyState draws an inbox
      // by default, which is right for a feed and wrong here — this is the
      // date axis, and it is the axis that has nothing on it.
      <EmptyState
        icon={<TimelineGlyph size={32} />}
        title="Nothing to lay out"
        description="No item on this node's copy of the tracker matches these filters."
      />
    );
  }
  if (groups.length > 0) {
    return (
      <div className="tl">
        {groups.map((group) => (
          <Fragment key={group.key}>
            <Band
              title={group.label || group.key}
              count={group.count}
              rows={group.rows}
              chrome={chrome}
              hrefOf={hrefOf}
              onOpen={onOpen}
              selected={selected}
              now={now}
            />
          </Fragment>
        ))}
      </div>
    );
  }
  return (
    <div className="tl">
      <Band
        rows={rows}
        chrome={chrome}
        hrefOf={hrefOf}
        onOpen={onOpen}
        selected={selected}
        now={now}
      />
    </div>
  );
}
