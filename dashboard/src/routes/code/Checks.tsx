/**
 * Code › Checks: every repository's attempts of checks over a window, newest
 * ended first — when they ran, which failed, and why.
 *
 * # The window is the engine's `since`, the end is this screen's
 *
 * The engine lists the attempts that ENDED at or after `since`, over every
 * repository the reader may read (`code_checks` naming none), and has no end
 * of its own. A window that ends before now — a custom interval — keeps the
 * rows that ended inside it; the pager reads past the newer ones to reach it.
 * A repository's own listing, every attempt it keeps, is its page's Checks
 * tab.
 *
 * # Counted over the rows loaded, and said so
 *
 * The histogram and the facets bucket and count the pages this screen holds:
 * the engine counts nothing server-side for these, so a bar or a chip that
 * claimed the whole window would be inventing a number. A narrowing by state
 * or by a check's name is the ENGINE'S (`state=`, `name=`), so the pages that
 * come back are the narrowed ones.
 */

import { useCallback, useMemo } from "react";
import { Card } from "@crewlethq/ui";
import { ChartNoAxesGanttGlyph } from "@crewlethq/icons/glyphs";
import { useParam } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { checkStateWord } from "~/lib/checks.ts";
import { barsOver, useTimeRange, type Bucket, type Offer } from "~/lib/range.ts";
import { CHECK_STATES } from "~/contract/checks.ts";
import type { CodeCheckRow } from "~/protocol/index.ts";
import { FacetRail, type Facet } from "~/ui/FacetRail.tsx";
import { Histogram } from "~/ui/Histogram.tsx";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { CheckListing } from "./checks/CheckList.tsx";

/**
 * The windows this screen offers: a DAY first, since a check is read while
 * its change waits, and at most the thirty days an ended attempt's rows are
 * kept (`code.CheckHistory`) — a wider window would only ever be empty past
 * them. The hour and the day are the buckets: a minute bucket over a day is
 * 1,440 bars for a page of fifty attempts.
 */
const CHECKS_OFFER: Offer = {
  ranges: ["1d", "7d", "30d"],
  custom: true,
  fallback: "1d",
  buckets: ["hour", "day"],
};

export function Checks() {
  const now = useNow();
  const range = useTimeRange(now, CHECKS_OFFER);
  // THE NARROWINGS, as filters: choosing another replaces the history entry.
  const [state, setState] = useParam("state", "", "filter");
  const [name, setName] = useParam("name", "", "filter");
  const params = useMemo(
    () => ({
      since: range.since,
      ...(state ? { state } : {}),
      ...(name ? { name } : {}),
    }),
    [range.since, state, name],
  );
  const until = range.until;
  const inWindow = useCallback(
    (r: CodeCheckRow) => !r.finished_at || Date.parse(r.finished_at) < Date.parse(until),
    [until],
  );

  const header = (rows: readonly CodeCheckRow[], more: boolean) => (
    <>
      <div className="toolbar">
        <TimeRangePicker range={range} ariaLabel="Window" />
      </div>
      <FacetRail
        name="State"
        value={state}
        onChange={setState}
        facets={stateFacets(rows, state)}
        over="loaded"
      />
      <FacetRail
        name="Check"
        value={name}
        onChange={setName}
        facets={nameFacets(rows, name)}
        over="loaded"
      />
      <Card>
        <Card.Header icon={<ChartNoAxesGanttGlyph size="sm" />}>
          <Card.Title>When</Card.Title>
        </Card.Header>
        {/* NO `onPick`: the bars are bucketed from the pages loaded, so a
            click would narrow the window to a slice of what was fetched. */}
        {rows.length > 0 ? (
          <Histogram
            bars={checkBars(rows, range.since, range.until, range.bucket)}
            bucket={range.bucket}
            total={rows.length}
            noun="attempt"
            over="loaded"
            axis
            now={now}
            label={more ? "Attempts on the pages loaded" : "Attempts over this window"}
          />
        ) : (
          <span className="t-caption">No attempt ended in this window to chart.</span>
        )}
      </Card>
    </>
  );

  return (
    <CheckListing
      params={params}
      label="Attempts of checks"
      showRepo
      narrow={inWindow}
      header={header}
      empty={{
        title: "No check ended in this window",
        hint: "An attempt is listed once it ends: a change's head, a queue's candidate or a branch's tip, checked.",
      }}
    />
  );
}

/**
 * The bars: every attempt that ended, bucketed by when it ended, the failed
 * ones the danger-toned foot of each bar.
 */
export function checkBars(
  rows: readonly CodeCheckRow[],
  since: string,
  until: string,
  bucket: Bucket,
) {
  const ended = rows.filter((r) => r.finished_at);
  const all = barsOver(
    ended.map((r) => r.finished_at!),
    since,
    until,
    bucket,
  );
  const failed = barsOver(
    ended.filter((r) => r.state === "failed").map((r) => r.finished_at!),
    since,
    until,
    bucket,
  );
  return all.map((b, i) => ({ ...b, failed: failed[i]?.count ?? 0 }));
}

/** The state chips: every state a check can be in, counted over the rows loaded. */
function stateFacets(rows: readonly CodeCheckRow[], chosen: string): Facet[] {
  return CHECK_STATES.map((s) => ({
    value: s,
    label: checkStateWord(s),
    // NARROWED BY STATE, the rows loaded are that state's alone: another
    // state's count is not known, not zero.
    count: chosen && chosen !== s ? null : rows.filter((r) => r.state === s).length,
  }));
}

/** The check-name chips: every name on the rows loaded, and the one chosen. */
function nameFacets(rows: readonly CodeCheckRow[], chosen: string): Facet[] {
  const names = [...new Set(rows.map((r) => r.name))];
  if (chosen && !names.includes(chosen)) names.unshift(chosen);
  return names.sort().map((n) => ({
    value: n,
    label: n,
    count: chosen && chosen !== n ? null : rows.filter((r) => r.name === n).length,
  }));
}
