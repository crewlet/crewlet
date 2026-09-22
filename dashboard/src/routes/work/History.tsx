/**
 * Every change to the company's work, over a window you choose.
 *
 * # Where the feed went, and why it needed a page
 *
 * A twenty-row "Recent activity" card sat under every board and every list. It
 * had a time axis nobody could set, no filters at all, and a page size that WAS
 * its count — so on any company busier than twenty rows the number a reader saw
 * was a constant. It answered "what changed" with "here is the last screenful",
 * which is the shape of an answer rather than one.
 *
 * The log is its own question, so it gets the frame every other log in this
 * product wears: a window, a time axis, then the rows, with the dimensions a
 * reader narrows on as facet chips. That frame is `TimeRangePicker`,
 * `Histogram` and `FacetRail` — the event log's own — because a reader who has
 * learnt one log in this product has learnt this one.
 *
 * # It is the same component inside a project
 *
 * `#/work/history` is this over the company and a project's History lens is
 * this over one container. Written twice they would drift, and the drift would
 * be invisible: both draw rows that look right either way.
 *
 * # The bars are OURS and the rows are the engine's
 *
 * The event log asks the engine for its histogram; this question has no such
 * read — `work_activity` answers with rows — so the axis is bucketed from the
 * page the screen is holding. That is a different claim from the event log's
 * and it is stated rather than implied: the caption says the bars cover the
 * changes ON THIS PAGE, and the facets say the same about their counts. A
 * client-side count dressed as the engine's is the one thing this product
 * never does.
 */

import { useMemo } from "react";
import { href, useParam } from "~/app/router.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Coverage, type RowChrome } from "~/components/work.tsx";
import { Card, EmptyValue, Tag } from "@crewlethq/ui";
import { TimelineGlyph } from "@crewlethq/icons/glyphs";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { Histogram, type Bar } from "~/ui/Histogram.tsx";
import { FacetRail } from "~/ui/FacetRail.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, plural, relTime } from "~/lib/format.ts";
import { barsOver, useTimeRange, type Offer } from "~/lib/range.ts";
import { describeChange } from "~/lib/work.ts";
import { FEED_PAGE } from "./feed.tsx";
import type { WorkActivityRecord } from "~/protocol/index.ts";

/**
 * The window this screen offers.
 *
 * NO `15m` AND NO `1h`. A tracker's pace is a person typing a comment, so a
 * quarter of an hour of it is almost always empty and reads as a log that
 * stopped — where the event log, whose question is "what is happening now",
 * starts there. A week is the fallback for the same reason the audit's is: this
 * is read after the fact.
 *
 * THE HOUR AND THE DAY are the only buckets, because the rows are bounded by a
 * page rather than by the window: a minute bucket over a day is 1,440 bars for
 * at most a hundred changes.
 */
const HISTORY_OFFER: Offer = {
  ranges: ["1d", "7d", "30d", "90d"],
  custom: true,
  fallback: "7d",
  buckets: ["hour", "day"],
};

export function History() {
  const [project, setProject] = useParam("project", "");
  return (
    <>
      <PageNote>
        Every change the company made to its own work, newest first. Ordered by the LOG rather than
        by anything a board sorts on, so a change that moved nothing on screen is still here.
      </PageNote>
      <HistoryView
        container={project ? `project:${project}` : "workspace"}
        projectKey={project}
        onClearProject={project ? () => setProject("") : undefined}
      />
    </>
  );
}

export function HistoryView({
  container,
  projectKey = "",
  onClearProject,
}: {
  /** `workspace`, or `project:KEY`. */
  container: string;
  projectKey?: string;
  onClearProject?: () => void;
}) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  // ALIGNED TO THE BUCKET, so the hour in progress is on the chart while it is
  // still being spent and the query changes once per bucket rather than once
  // per second — the rule `lib/range.ts` states for every chart's edges.
  const range = useTimeRange(now, HISTORY_OFFER);
  const [kind, setKind] = useParam("kind", "");
  const [actor, setActor] = useParam("actor", "");

  const params = useMemo(
    () => ({
      container,
      limit: FEED_PAGE.page,
      // THE AUTHORED INSTANTS, which is what a reader choosing a wall-clock
      // window means — the engine's own `from`/`to` pair rather than a log
      // position, which is a different order and not a time at all.
      from: range.since,
      to: range.until,
      ...(kind ? { kinds: kind } : {}),
      ...(actor ? { actor } : {}),
    }),
    [container, range.since, range.until, kind, actor],
  );
  const state = useQuery("work_activity", params, { pollMs: 60_000 });
  usePageCoverage(state.data);

  const records = useMemo(() => state.data?.records ?? [], [state.data]);
  const chrome: RowChrome = {
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
  };
  const more = !!state.data?.next_cursor;

  const bars: Bar[] = useMemo(
    () =>
      barsOver(
        records.map((r) => r.at),
        range.since,
        range.until,
        range.bucket,
      ),
    [records, range.since, range.until, range.bucket],
  );

  // THE FACET COUNTS ARE OVER THE ROWS LOADED, and `over="loaded"` is what says
  // so under the chips: the engine answers this question with rows rather than
  // with counts, so a chip claiming a total would be a number this screen
  // invented about somebody's company.
  //
  // NULL WHILE NOTHING HAS ANSWERED, never 0: "no change was made by ada" and
  // "we have not asked yet" are different facts, and only the first is one a
  // reader acts on.
  const kinds = useMemo(
    () =>
      tally(
        records,
        (r) => r.kind,
        !!state.data,
        (value) => value.replaceAll("_", " "),
      ),
    [records, state.data],
  );
  const actors = useMemo(
    () =>
      tally(
        records,
        (r) => r.actor || "",
        !!state.data,
        (value) => index.byHandle.get(value)?.name ?? value,
      ),
    [records, state.data, index],
  );

  return (
    <>
      <div className="toolbar">
        <TimeRangePicker range={range} ariaLabel="Window" />
        {projectKey && onClearProject && (
          <Tag
            appearance="outline"
            size="sm"
            onRemove={onClearProject}
            removeAriaLabel="Show every project"
          >
            <span className="work-chip-field">Project</span>
            <span className="work-chip-verb">is</span>
            <span className="work-chip-value mono">{projectKey}</span>
          </Tag>
        )}
        <span className="spacer" />
        <Coverage answer={state.data} />
        <span className="work-summary">
          {plural(records.length, "change")}
          {more ? "+" : ""} on this page
        </span>
      </div>

      <Card>
        <Card.Header
          icon={<TimelineGlyph size="sm" />}
          subtitle={`One bar per ${range.bucket}, over the changes on this page.`}
          actions={
            more ? (
              <span className="t-caption">
                Older changes exist beyond this page — narrow the window to reach them.
              </span>
            ) : undefined
          }
        >
          <Card.Title>When</Card.Title>
        </Card.Header>
        {/* NO `onPick`. The event log's bars narrow the window because its own
            histogram is the engine's, over the whole of it; these are bucketed
            from one page, so clicking a bar would narrow the window to a slice
            of what happened to be loaded — and the page that came back would
            be a different set from the bar that was clicked. */}
        {records.length > 0 && (
          <Histogram
            bars={bars}
            bucket={range.bucket}
            total={records.length}
            label="Changes over this window"
          />
        )}
      </Card>

      <FacetRail name="Kind" value={kind} onChange={setKind} over="loaded" facets={kinds} />
      <FacetRail name="By" value={actor} onChange={setActor} over="loaded" facets={actors} />

      <QueryState
        error={state.error}
        loading={state.loading}
        empty={
          records.length
            ? undefined
            : {
                title: "Nothing changed in this window",
                hint: "Widen the window, or take off a filter. A change is written when a seat or an operator writes to the tracker, so a company whose agents are idle has none.",
              }
        }
      >
        <div className="work-log">
          {records.map((record) => (
            <HistoryRow key={record.id} record={record} chrome={chrome} now={now} />
          ))}
        </div>
      </QueryState>
    </>
  );
}

/** One change, as the delta it was. */
function HistoryRow({
  record,
  chrome,
  now,
}: {
  record: WorkActivityRecord;
  chrome: RowChrome;
  now: number;
}) {
  return (
    <div className="work-log-row">
      <span className="work-log-when" title={fmtDateTime(record.at)}>
        {relTime(record.at, now)}
      </span>
      <span className="work-log-key">
        {record.subject_key ? (
          <a className="mono t-link" href={href(["work", record.subject_key])}>
            {record.subject_key}
          </a>
        ) : (
          <EmptyValue label="No work item" />
        )}
      </span>
      {/* THE DELTA, not the kind. `project updated` and a zero is what the card
          under every board drew; which field moved from what to what is the
          fact a reader came for, and `describeChange` is the one renderer for
          it — shared with the item's own history, so the two cannot drift. */}
      <span className="work-log-what truncate">{describeChange(record, chrome)}</span>
      <span className="work-log-who">
        <span className="truncate">
          {record.actor ? (chrome.seatName?.(record.actor) ?? record.actor) : "the engine"}
        </span>
        {/* AN OPERATOR IS NOT AN AGENT, and the tracker records which: a write
            made with an API token carries the token's own name and the author
            kind `operator`, which is the whole point of the audit trail.

            `agent` IS THE ORDINARY CASE and the one left unmarked — it is
            `tracker.AuthorAgent`, one of the four the engine mints
            (`agent`/`human`/`operator`/`system`). There is no `seat` among
            them, so a check against that word marks every write in the
            company, which is a tag that separates nothing. */}
        {record.actor_kind && record.actor_kind !== "agent" && (
          <Tag appearance="outline" size="xs">
            {record.actor_kind}
          </Tag>
        )}
        {record.turn_id && (
          <a className="t-link" href={href(["activity", "turns", record.turn_id])}>
            turn →
          </a>
        )}
      </span>
    </div>
  );
}

/**
 * How many of each value are on this page, biggest first.
 *
 * `answered` IS WHAT SEPARATES "none" FROM "not yet". A facet count of null is
 * the rail's own "still in flight"; a zero is a claim about the company. The
 * set of values is what the page HOLDS rather than a closed set, because a
 * change kind is one of thirty-two and an actor is whoever has written — a
 * rail of thirty-two chips, most of them zero, is not a filter.
 */
function tally(
  records: WorkActivityRecord[],
  of: (record: WorkActivityRecord) => string,
  answered: boolean,
  label: (value: string) => string,
): { value: string; label: string; count: number | null }[] {
  const held = new Map<string, number>();
  for (const record of records) {
    const value = of(record);
    if (!value) continue;
    held.set(value, (held.get(value) ?? 0) + 1);
  }
  return [...held.entries()]
    .map(([value, count]) => ({ value, label: label(value), count: answered ? count : null }))
    .sort((a, b) => (b.count ?? 0) - (a.count ?? 0) || a.label.localeCompare(b.label));
}
