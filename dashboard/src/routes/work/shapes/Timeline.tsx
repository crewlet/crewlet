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
import { Button, EmptyState, Tag, cx } from "@crewlethq/ui";
import { ChartNoAxesGanttGlyph } from "@crewlethq/icons/glyphs";
import { plural } from "~/lib/format.ts";
import { itemAddress } from "~/lib/work.ts";

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

/** The gap between a narrow bar and the label drawn beside it. */
const ASIDE_GAP_PX = 4;

/**
 * The room a label needs after its bar before it is drawn there — about
 * twenty characters at the compact size — and below which it goes on the
 * side with more.
 */
const ASIDE_MIN_PX = 160;

/**
 * Where a narrow bar's label goes, as the style that places it: after the bar
 * where the strip has room, before it where the bar is near the strip's end,
 * and never wider than the room it has.
 *
 * A label written after a bar four days from the end ran off the strip and
 * was cut mid-word at its edge ("GPU schedu"), with nothing to say there was
 * more. Bounded, it ellipsises — and its full title is the bar's own `title`
 * and the names column beside it.
 */
export function asideAt(
  from: number,
  to: number,
  days: number,
): { left?: number; right?: number; maxWidth: number } {
  const after = (days - to) * DAY_PX - 2 * ASIDE_GAP_PX;
  const before = from * DAY_PX - 2 * ASIDE_GAP_PX;
  if (after >= ASIDE_MIN_PX || after >= before) {
    return { left: to * DAY_PX + ASIDE_GAP_PX, maxWidth: Math.max(0, after) };
  }
  return { right: (days - from) * DAY_PX + ASIDE_GAP_PX, maxWidth: before };
}

/**
 * How wide a MILESTONE is: a task with a due date and no start.
 *
 * A POINT, drawn as one — a filled diamond in its status tone, centred on its
 * day. It was a one-day box, dashed on every side to say its start was never
 * stated, and over a planned task's pale ground that read as an EMPTY
 * placeholder: the one thing a deadline is not, since its date is the single
 * fact the timeline knows for certain. Twelve pixels squared is seventeen on
 * the diagonal, inside a 26px day and a 22px bar's height.
 */
const MILESTONE_PX = 12;

/**
 * Where a bar sits on the strip, as the style that places it: its day columns,
 * or for a milestone the centre of its one day.
 */
export function barBox(
  bar: Pick<TimelineBar, "from" | "to" | "kind">,
  row: number,
): { left: number; width: number; top: number } {
  const top = row * ROW_PX + 4;
  if (bar.kind === "deadline") {
    return { left: bar.from * DAY_PX + (DAY_PX - MILESTONE_PX) / 2, width: MILESTONE_PX, top };
  }
  return { left: bar.from * DAY_PX, width: (bar.to - bar.from) * DAY_PX, top };
}

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
                  className={cx("tl-bar", selected === itemAddress(bar.row) && "selected")}
                  data-tone={barTone(bar)}
                  data-kind={bar.kind}
                  href={hrefOf(bar.row)}
                  title={`${bar.row.key} · ${bar.row.title} — ${barTitle(bar)}`}
                  style={barBox(bar, i)}
                  onClick={(e) => {
                    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
                    e.preventDefault();
                    onOpen(bar.row);
                  }}
                >
                  {bar.kind !== "deadline" && bar.to - bar.from >= LABEL_INSIDE_DAYS && (
                    <span className="tl-bar-label">{bar.row.title}</span>
                  )}
                </a>
              ))}
              {/* THE LABELS FOR THE NARROW BARS, beside them rather than in
                  them. Separate elements so they can overflow the bar's own
                  box, which `overflow: hidden` is what keeps the wide ones
                  tidy. */}
              {line.bars.map((bar, i) =>
                bar.kind !== "deadline" && bar.to - bar.from >= LABEL_INSIDE_DAYS ? null : (
                  <span
                    key={`${bar.row.id}-label`}
                    className="tl-bar-aside"
                    style={{ ...asideAt(bar.from, bar.to, line.days), top: i * ROW_PX + 4 }}
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
                className={cx("tl-name", selected === itemAddress(bar.row) && "selected")}
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
                <Assignee
                  handle={bar.row.assignee}
                  seatName={chrome.seatName}
                  seatKind={chrome.seatKind}
                />
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
  more,
}: {
  rows: WorkSummary[];
  groups: { key: string; label?: string; count: number; rows: WorkSummary[] }[];
  chrome: RowChrome;
  hrefOf: (row: WorkSummary) => string;
  onOpen: (row: WorkSummary) => void;
  selected: string;
  now: number;
  /**
   * The rest of an ungrouped answer, where it carried a cursor. A grouped one
   * has none: each band carries its own count and holds a page of its own.
   */
  more?: { load: () => void; note: string };
}) {
  if (groups.length === 0 && rows.length === 0) {
    return (
      // THE MARK SAYS WHICH SCREEN IS EMPTY. uilet's EmptyState draws an inbox
      // by default, which is right for a feed and wrong here — this is the
      // date axis, and it is the axis that has nothing on it.
      <EmptyState
        icon={<ChartNoAxesGanttGlyph size={32} />}
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
      {/* THE AXIS IS DERIVED FROM THE ROWS PRESENT, so a later page may widen
          it — which is the honest drawing of more rows, and the only way to the
          five-hundred-and-first bar at all. */}
      {more && (
        <div className="tl-more">
          <span className="t-caption">{more.note}</span>
          <Button size="small" variant="secondary" onClick={more.load}>
            Load more
          </Button>
        </div>
      )}
    </div>
  );
}
