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
 * learnt one log in this product has learnt this one. The event log's PAGING
 * belongs to that frame too, and was the one part of it this screen did not
 * borrow: it asked for one page, printed that older changes existed, and told
 * the reader to narrow the window — which is the gesture that reaches FEWER
 * and NEWER rows, the opposite of what they were reaching for.
 *
 * # It is the same component inside a project
 *
 * `#/work/history` is this over the company and a project's History lens is
 * this over one container. Written twice they would drift, and the drift would
 * be invisible: both draw rows that look right either way.
 *
 * ONE OF THEM IS A SCREEN AND THE OTHER IS A LENS, and that is the whole of
 * what `embedded` decides. The frame holds ONE coverage slot with one setter
 * (`app/Shell.tsx`), so a body that published it from inside another screen
 * fought that screen's own answer: on `#/work/{KEY}?lens=history` both this and
 * `Project` wrote the slot, whichever polled last won, and switching back to
 * the Items lens unmounted this one and cleared the slot to nothing over a page
 * still showing a project it had read. So: ONLY A SCREEN PUBLISHES TO THE
 * FRAME. The screen publishes and draws no coverage of its own; the lens
 * publishes nothing and states its own rows' coverage inline, which is exactly
 * what the Items lens beside it already does.
 *
 * # The bars are OURS and the rows are the engine's
 *
 * The event log asks the engine for its histogram; this question has no such
 * read — `work_activity` answers with rows — so the axis is bucketed from the
 * pages the screen is holding. That is a different claim from the event log's
 * and it is stated rather than implied: the caption says the bars cover the
 * changes LOADED, the facets say the same about their counts, and `Histogram`
 * takes that scope as a required prop so the sentence a screen reader hears
 * cannot drift from the one on the card. A client-side count dressed as the
 * engine's is the one thing this product never does.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { href, useParam } from "~/app/router.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Coverage, type CoverageFacts } from "~/components/work.tsx";
import { Button, Card, EmptyValue, Skeleton, Tag } from "@crewlethq/ui";
import { TimelineGlyph } from "@crewlethq/icons/glyphs";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { Histogram, type Bar } from "~/ui/Histogram.tsx";
import { FacetRail } from "~/ui/FacetRail.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useClient, useOrg } from "~/lib/store-hooks.ts";
import { useViewer } from "~/lib/viewer.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, plural, relTime } from "~/lib/format.ts";
import { barsOver, useTimeRange, windowParam, type Offer } from "~/lib/range.ts";
import { describeChange, type LabelContext } from "~/lib/work.ts";
import { FEED_PAGE } from "./feed.tsx";
import type { WorkActivityAnswer, WorkActivityRecord } from "~/protocol/index.ts";

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
        onProject={setProject}
      />
    </>
  );
}

export function HistoryView({
  container,
  projectKey = "",
  onProject,
  embedded = false,
}: {
  /** `workspace`, or `project:KEY`. */
  container: string;
  projectKey?: string;
  /**
   * Narrow to one project, or clear it with `""`.
   *
   * ABSENT WHERE THE CONTAINER ALREADY IS ONE PROJECT. On the lens under a
   * project's header every loaded row carries the same key, so a rail there
   * would be one chip that narrows nothing and a chip labelled "Show every
   * project" would leave the page it sits on.
   */
  onProject?: (next: string) => void;
  /**
   * This is a LENS inside another screen rather than the screen itself.
   *
   * It decides two things and they are the same decision twice: who owns the
   * frame's coverage slot (see the module doc) and how much of the log one read
   * asks for — a lens under a header beside two other lenses wants a screenful,
   * and the page, where the log IS the page, wants `FEED_PAGE.page`.
   */
  embedded?: boolean;
}) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const { socket } = useClient();
  const viewer = useViewer();
  // ALIGNED TO THE BUCKET, so the hour in progress is on the chart while it is
  // still being spent and the query changes once per bucket rather than once
  // per second — the rule `lib/range.ts` states for every chart's edges.
  const range = useTimeRange(now, HISTORY_OFFER);
  const [kind, setKind] = useParam("kind", "");
  const [actor, setActor] = useParam("actor", "");
  const limit = embedded ? FEED_PAGE.board : FEED_PAGE.page;

  const params = useMemo(
    () => ({
      container,
      limit,
      // THE AUTHORED INSTANTS, which is what a reader choosing a wall-clock
      // window means — the engine's own `from`/`to` pair rather than a log
      // position, which is a different order and not a time at all.
      from: range.since,
      to: range.until,
      ...(kind ? { kinds: kind } : {}),
      ...(actor ? { actor } : {}),
    }),
    [container, limit, range.since, range.until, kind, actor],
  );
  const state = useQuery("work_activity", params, { pollMs: 60_000 });

  // WHAT THE READER HAS FETCHED PAST THE FIRST PAGE, and the position the next
  // one resumes at. `head` is the first page AS IT WAS when they first asked
  // for an older one — see [loadOlder] for why it is frozen rather than read
  // live from the poll.
  const [head, setHead] = useState<WorkActivityRecord[] | null>(null);
  const [older, setOlder] = useState<WorkActivityRecord[]>([]);
  const [cursor, setCursor] = useState("");
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<string | null>(null);

  // A FILTER CHANGE IS A NEW QUERY, so the pages fetched under the old one go
  // with it — the rule the event log states at length (`routes/activity`). Kept
  // they would break three ways at once: rows fetched under the previous kind
  // would stay in a list the server would never have answered, the cursor would
  // go on walking the old query's history, and the frozen head would hold a
  // page of the narrowing the reader has just left.
  //
  // THE WINDOW'S IDENTITY, not its two instants: those are the clock rounded to
  // the bucket, so keyed on them this would fire once an hour and throw away
  // whatever the reader had loaded.
  const windowKey = windowParam(range.window);
  useEffect(() => {
    setHead(null);
    setOlder([]);
    setCursor("");
    setPageError(null);
  }, [container, kind, actor, windowKey]);

  const live = useMemo(() => state.data?.records ?? [], [state.data]);
  // DEDUPED, because the two sources can overlap: the first page is re-asked on
  // a poll while the older ones are not, so a change landing between two polls
  // shifts the boundary and a row can arrive down both paths.
  const records = useMemo(() => {
    if (!head) return live;
    const seen = new Set<string>();
    const out: WorkActivityRecord[] = [];
    for (const record of [...head, ...older]) {
      if (seen.has(record.id)) continue;
      seen.add(record.id);
      out.push(record);
    }
    return out;
  }, [head, older, live]);

  // A NON-EMPTY CURSOR IS THE ENGINE'S OWN "there is at least one more":
  // `readActivity` asks for limit+1 rows and mints one only when the extra came
  // back. Never derived from `records.length === limit`, which is wrong on the
  // boundary in both directions.
  const next = head ? cursor : (state.data?.next_cursor ?? "");
  const more = !!next;

  const loadOlder = useCallback(async () => {
    if (!next) return;
    setPaging(true);
    setPageError(null);
    // THE FIRST PAGE IS FROZEN THE MOMENT A SECOND ONE IS ASKED FOR, and that
    // is not caching: the cursor this page resumes at was minted from the last
    // row of the first page at the instant it was read, so a poll that then
    // brings newer rows pushes that row out of the first page and the changes
    // between the two pages belong to NEITHER — a silent hole in the middle of
    // a log, which is the one defect a log may not have. Holding the head still
    // makes the pages one snapshot; the footer says so, and a window or a
    // filter change starts a live one again.
    const base = head ?? live;
    try {
      const page = (await socket.query("work_activity", {
        ...params,
        cursor: next,
      })) as WorkActivityAnswer;
      setHead(base);
      setOlder((prev) => [...prev, ...(page.records ?? [])]);
      setCursor(page.next_cursor ?? "");
    } catch (err) {
      setPageError(err instanceof Error ? err.message : "query_failed");
    } finally {
      setPaging(false);
    }
  }, [socket, params, next, head, live]);

  const chrome: LabelContext = {
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
    // WHO IS READING, so a sentence the engine addressed to the seat it woke
    // is re-addressed to whoever is looking at the log. The SEAT HANDLE and not
    // the operator id, because the records that carry such a sentence are a
    // person's and a person subject is keyed on the handle — `lib/work.ts` says
    // so where it compares them.
    viewer: viewer.handle,
  };

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
  // THE THIRD DIMENSION THE RECORD CARRIES. `work_activity` filters on the
  // project the engine already stamps on every row, and the page already owns
  // the `project=` key its chip removes — so until this rail existed the only
  // way into a project narrowing was a link from the project's own page.
  const projects = useMemo(
    () =>
      tally(
        records,
        (r) => r.project || "",
        !!state.data,
        (value) => value,
      ),
    [records, state.data],
  );

  return (
    <>
      {/* ONLY A SCREEN PUBLISHES TO THE FRAME — the module doc says why, and a
          COMPONENT rather than a bare call because the rule is conditional and
          a hook cannot be: called with nothing, `usePageCoverage` writes null
          over whatever the enclosing screen published, which is the same
          clobber under a quieter name. */}
      {!embedded && <PublishCoverage answer={state.data} />}

      <div className="toolbar">
        <TimeRangePicker range={range} ariaLabel="Window" />
        {projectKey && onProject && (
          <Tag
            appearance="outline"
            size="sm"
            onRemove={() => onProject("")}
            removeAriaLabel="Show every project"
          >
            <span className="work-chip-field">Project</span>
            <span className="work-chip-verb">is</span>
            <span className="work-chip-value mono">{projectKey}</span>
          </Tag>
        )}
        <span className="spacer" />
        {/* THE LENS STATES ITS OWN ROWS' COVERAGE and the screen does not:
            the screen's went to the state bar one element up. */}
        {embedded && <Coverage answer={state.data} />}
        <span className="work-summary">
          {plural(records.length, "change")} loaded
          {more ? ", and older ones beyond them" : ""}
        </span>
      </div>

      <Card>
        <Card.Header
          icon={<TimelineGlyph size="sm" />}
          // NOTHING TO DESCRIBE IS NOT A DESCRIPTION OF NOTHING. Drawn
          // unconditionally this promised bars over an empty card, which is the
          // rule `Activity`'s own card follows by leaving its subtitle
          // `undefined` until a series lands.
          subtitle={
            records.length > 0 ? `One bar per ${range.bucket}, over the changes loaded.` : undefined
          }
        >
          <Card.Title>When</Card.Title>
        </Card.Header>
        {/* NO `onPick`. The event log's bars narrow the window because its own
            histogram is the engine's, over the whole of it; these are bucketed
            from the pages loaded, so clicking a bar would narrow the window to
            a slice of what happened to be fetched — and the page that came
            back would be a different set from the bar that was clicked. */}
        {records.length > 0 ? (
          <Histogram
            bars={bars}
            bucket={range.bucket}
            total={records.length}
            noun="change"
            over="loaded"
            axis
            now={now}
            label="Changes over this window"
          />
        ) : (
          // NOT AN EMPTY CHART, and not an empty CARD either — the frame this
          // screen wears decides this case in both the other logs that wear it
          // (`Activity` puts its empty state inside the card, `Turns` renders a
          // caption), and dropping the card instead would make the whole frame
          // flicker in and out as a reader widens the window. Bars of zero
          // across a window would say the company was quiet; what is true here
          // is that nothing came back at all, which is a different fact and the
          // rows below say what would change it.
          <span className="t-caption">No change came back for this window to chart.</span>
        )}
      </Card>

      <FacetRail name="Kind" value={kind} onChange={setKind} over="loaded" facets={kinds} />
      <FacetRail name="By" value={actor} onChange={setActor} over="loaded" facets={actors} />
      {onProject && (
        <FacetRail
          name="Project"
          value={projectKey}
          onChange={onProject}
          over="loaded"
          facets={projects}
        />
      )}

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

      {/* OLDER CHANGES ARE FETCHED, never reached by narrowing. The caption
          this replaces told a reader to narrow the window to see what came
          before this page — which moves the window's newest edge and reaches
          fewer, newer rows. */}
      {(more || paging || pageError || head) && (
        <footer className="panel-foot">
          {pageError ? (
            <QueryState error={pageError} loading={false} />
          ) : (
            <>
              {more ? (
                <Button
                  size="small"
                  variant="secondary"
                  onClick={() => void loadOlder()}
                  disabled={paging}
                >
                  {paging ? "Loading…" : "Load older changes"}
                </Button>
              ) : (
                <span>That is the oldest change in this window.</span>
              )}
              <span className="spacer" />
              {/* SAID ONLY ONCE THE HEAD IS HELD, because until then it is not
                  true: the first page is re-asked on every poll. */}
              {head && (
                <span>
                  Held still while you page back — change the window or a filter to pick up new
                  changes.
                </span>
              )}
            </>
          )}
        </footer>
      )}
      {paging && <Skeleton variant="text" rows={3} label="Loading older changes" />}
    </>
  );
}

/**
 * Hands this screen's answer to the frame, and draws nothing.
 *
 * A COMPONENT RATHER THAN A CALL, because the rule it carries is conditional
 * and a hook may not be: `HistoryView` is a screen on `#/work/history` and a
 * lens inside `#/work/{KEY}`, and only the first of those owns the frame's one
 * coverage slot. Conditional RENDERING is how React spells a conditional
 * effect.
 */
function PublishCoverage({ answer }: { answer?: CoverageFacts | null }) {
  usePageCoverage(answer);
  return null;
}

/** One change, as the delta it was. */
function HistoryRow({
  record,
  chrome,
  now,
}: {
  record: WorkActivityRecord;
  chrome: LabelContext;
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
          it — sharing its delta sentence with the item's own history, so the
          two cannot drift.

          NO `truncate`. It is the one cell here that is prose, `.work-log-what`
          is written to wrap for exactly that reason (`styles/screens.css`), and
          the two rules disagree about `white-space` — so carrying both gave one
          clipped line with no ellipsis and lost everything after the first
          clause. A create moves eleven fields; the row's `min-height` lets the
          wrapped delta grow rather than overflow. */}
      <span className="work-log-what">{describeChange(record, chrome)}</span>
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
 * How many of each value are on the pages loaded, biggest first.
 *
 * `answered` IS WHAT SEPARATES "none" FROM "not yet". A facet count of null is
 * the rail's own "still in flight"; a zero is a claim about the company. The
 * set of values is what the pages HOLD rather than a closed set, because a
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
