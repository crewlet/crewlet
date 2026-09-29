/**
 * One trace: every event that shares a trace id, as a tree.
 *
 * The screen this replaces rendered "Trace not found" for EVERY trace: it
 * asked for `trace` and got `{trace_id, events}`, then tested `!rows.length` —
 * `.length` on an object is `undefined`, which is falsy, which is the empty
 * state, always. Its own suite only ever exercised the span arranger on
 * hand-built arrays, so nothing caught it.
 */

import { useMemo } from "react";
import { href, useNavigator } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { CoverageNote } from "~/components/CoverageNote.tsx";
import { Button, Card, Skeleton, Tag } from "@crewlethq/ui";
import {
  ChevronRightGlyph,
  SplitGlyph,
  ChartNoAxesGanttGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import {
  fmtDateTime,
  fmtDuration,
  fmtTime,
  humanize,
  oldestFirst,
  relTime,
  tsKey,
} from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { EventRecord } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";

/** One drawn row: an event, at the depth of the span that carried it. */
export interface TraceRow {
  event: EventRecord;
  depth: number;
}

/** A span: every event that carried its id, and the spans opened under it. */
interface Span {
  events: EventRecord[];
  children: Span[];
  /** When the span's first event landed — where it sits among its siblings. */
  at: number;
}

/**
 * Arrange a trace's events into rows: ONE ROW PER EVENT, indented by the span
 * that carried it.
 *
 * A SPAN IS NOT AN EVENT. Every event of one phase carries that phase's span
 * id, so a span holds many events — and the arranger this replaces keyed its
 * tree on `span_id` as though each were one. It kept the LAST event per span,
 * resolved every other event of that span to it, and pushed that node into
 * its parent once per event: a 25-event trace drew 344 rows, the same seven
 * repeated, and the two failures its header counted never appeared.
 *
 * So the tree is of SPANS, each holding its own events, and a span appears
 * once under its parent. Its parent is named by its events' `parent_span_id`
 * (the first one that names a span present here). Within a span its own
 * events and its child spans are interleaved by time — a child placed where
 * its first event landed — so the rows still read in the order things
 * happened, with the nesting saying which span each belongs to.
 *
 * A span whose parent is not in this trace is a ROOT rather than dropped: a
 * trace can begin mid-flight (the parent was published before the store's
 * retention window, or by a node whose events went elsewhere), and hiding
 * those rows loses the half of the trace that is actually present. An event
 * with no span id is a span of its own. A parent chain that loops (a corrupt
 * or hand-written id) cannot hide its spans either: a span no root reaches is
 * drawn as a root.
 */
export function arrange(events: readonly EventRecord[]): TraceRow[] {
  const ordered = [...events].sort(oldestFirst);
  const spans = new Map<string, Span>();
  const order: Span[] = [];
  for (const event of ordered) {
    const id = event.span_id || "";
    let span = id ? spans.get(id) : undefined;
    if (!span) {
      span = { events: [], children: [], at: tsKey(event.timestamp) };
      if (id) spans.set(id, span);
      order.push(span);
    }
    span.events.push(event);
  }
  const parentOf = new Map<Span, Span>();
  for (const [id, span] of spans) {
    const named = span.events.find(
      (e) => e.parent_span_id && e.parent_span_id !== id && spans.has(e.parent_span_id),
    );
    if (named) parentOf.set(span, spans.get(named.parent_span_id)!);
  }
  // A LOOP IS BROKEN WHERE IT IS FOUND: walking up from a span, meeting it
  // again means its parent link closes a cycle, so that link is dropped and
  // the span becomes a root.
  for (const span of order) {
    const seen = new Set<Span>([span]);
    for (let up = parentOf.get(span); up; up = parentOf.get(up)) {
      if (seen.has(up)) {
        parentOf.delete(span);
        break;
      }
      seen.add(up);
    }
  }
  const roots: Span[] = [];
  for (const span of order) {
    const parent = parentOf.get(span);
    if (parent) parent.children.push(span);
    else roots.push(span);
  }
  const rows: TraceRow[] = [];
  const visit = (span: Span, depth: number) => {
    // OWN EVENTS AND CHILD SPANS, BY TIME. A child sorts at its first event;
    // on a tie the span's own event comes first, since it is what opened it.
    const items: { at: number; rank: number; event?: EventRecord; span?: Span }[] = [
      ...span.events.map((event) => ({ at: tsKey(event.timestamp), rank: 0, event })),
      ...span.children.map((child) => ({ at: child.at, rank: 1, span: child })),
    ];
    items.sort((a, b) => a.at - b.at || a.rank - b.rank);
    for (const item of items) {
      if (item.event) rows.push({ event: item.event, depth });
      else if (item.span) visit(item.span, depth + 1);
    }
  };
  roots.sort((a, b) => a.at - b.at);
  for (const root of roots) visit(root, 0);
  return rows;
}

export function TraceScreen({ traceId }: { traceId: string }) {
  const nav = useNavigator();
  const now = useNow();
  const { data, loading, error } = useQuery("trace", { trace_id: traceId });

  // `.events`, not the answer itself.
  const events = data?.events ?? [];
  // The store caps one trace's rows and the answer says when it did. A trace
  // shown short with no note reads as a complete causal chain that simply
  // ends, which is the one thing a reader must not conclude from it.
  const truncated = Boolean(data?.truncated);
  const rows = useMemo(() => arrange(events), [events]);

  const from = events.length ? Math.min(...events.map((e) => tsKey(e.timestamp))) : 0;
  const to = events.length ? Math.max(...events.map((e) => tsKey(e.timestamp))) : 0;
  const spread = to > from;
  const spanCount = useMemo(() => new Set(events.map((e) => e.span_id || e.id)).size, [events]);
  // THE ROW'S OWN FIELD, not its payload. `EventLog.Trace` scans through
  // `scanRows`, whose SELECT does not include `payload` — so `e.payload` is
  // undefined on every span and this tile read 0 on every trace ever drawn,
  // under a caption asserting that nothing had failed. `failed` is derived
  // server-side from the type and the stored tag and has always been on the
  // wire; it is what the feed's own red rows are drawn from.
  const failed = events.filter((e) => e.failed === true).length;

  // WHAT THE TRACE IS ABOUT, off the span it begins at. A trace id is a
  // hexadecimal string and nothing else; the earliest ROOT is the work that
  // opened it, and its own summary is the only name this object has. Roots
  // come first out of [arrange], so `rows[0]` is that span — and where the
  // whole trace begins mid-flight, it is still the earliest thing present.
  const opening = rows[0]?.event;
  // AND IT IS THE TRACE'S NAME EVERYWHERE, not just in the header below.
  // The breadcrumb, the browser tab and the palette's recents all read the one
  // label a screen publishes, and fall back to the raw path segment otherwise
  // — which for a trace is 32 hexadecimal characters that distinguish it from
  // nothing a reader can remember. Published only once a span is in hand:
  // "Trace" in the trail names no particular trace, and two of them in the
  // recents are two identical rows going to different places.
  const name = opening?.summary || opening?.type || "";
  usePageLabels(name ? { [traceId]: name } : {});
  // ABSENT RATHER THAN ZERO until the answer lands. `FactLine` drops an empty
  // value, and "0 spans · 0 failures" over a trace still loading is a claim
  // that the trace is empty — which is the one thing a reader must not
  // conclude from a read that has not finished.
  const known = events.length > 0;
  //
  // ONE STATEMENT OF EACH NUMBER. A tile row under the header used to state
  // the same three again, larger; the one thing a tile added — what its
  // number is over — is here as the window the elapsed time spans.
  const facts: Fact[] = [
    // EVENTS AND SPANS ARE TWO NUMBERS: a span is every event one phase (or
    // one piece of work) published under its id, so a trace of 25 events may
    // be seven spans. The list below draws the events, nested by span.
    { label: "Events", value: known ? events.length : "" },
    { label: "Spans", value: known ? spanCount : "" },
    { label: "Elapsed", value: to > from ? fmtDuration(to - from) : "" },
    {
      // WHEN IT BEGAN, relative and exact on hover, as an event's own "When"
      // reads — the one thing the elapsed time is measured from.
      label: "Began",
      value: from ? (
        <span title={fmtDateTime(new Date(from).toISOString())}>
          {relTime(new Date(from).toISOString(), now)}
        </span>
      ) : (
        ""
      ),
    },
    { label: "Failures", value: known ? failed : "" },
  ];

  return (
    <>
      <PageActions>
        {/* THE COUNT IS THE HEADER'S "Spans", stated once. A chip here said
            "25 events" beside it; only the truncation note says something
            the header does not. */}
        {truncated && <Tag variant="warning">oldest {events.length} shown</Tag>}
        {
          <Button
            size="small"
            variant="secondary"
            leadingIcon={<ChartNoAxesGanttGlyph size="xs" />}
            // THE LOG NARROWED TO THIS TRACE, which the engine answers by the
            // trace id every event carries — paged and windowed like any log.
            // It carried the id as the log's TEXT search, which matches no
            // summary the engine writes, so it opened on an empty log.
            onClick={() => nav.to(["live", "events"], { trace: traceId })}
          >
            In the log
          </Button>
        }
      </PageActions>
      {/* THE OBJECT'S OWN HEADER, and the trace id with it. The id used to be
          a lone `PageNote` under the page bar — the hand-rolled half of what
          `ObjectHeader` draws as an eyebrow — and the three facts beside it
          are the strip's own, in the strip's own order, so a reader who
          arrives from a turn's "Trace" button lands on the same three
          numbers wherever they meet this trace again. */}
      <ObjectHeader
        kind="Trace"
        icon="split"
        identifier={traceId}
        title={name || "Trace"}
        facts={facts}
      />
      {/* THE SKELETON STANDS WHERE THE BODY WILL BE, under a header the id
          alone is enough to draw — see the same note on the turn screen. */}
      {loading && <Skeleton variant="text" rows={6} label="Loading the trace" />}
      {/* A TRACE IS ASSEMBLED FROM EVERY NODE, and a node that did not answer
          is a gap in the causal chain that would otherwise read as its end. */}
      <CoverageNote coverage={[data?.coverage]} what="this trace" />
      <QueryState
        error={error}
        loading={loading}
        empty={
          events.length
            ? undefined
            : {
                title: "No events carry this trace id",
                hint: "A trace is assembled from the events that share an id. If the work happened outside the store's 30-day window, or on a node whose events went elsewhere, there is nothing to assemble.",
              }
        }
      >
        <Card padding="none">
          <Card.Header icon={<SplitGlyph size="sm" />}>
            <Card.Title>Events</Card.Title>
          </Card.Header>
          <div className="list">
            {rows.map(({ event, depth }) => {
              // WHERE IN THE TRACE'S WINDOW the event landed, on a lane of its
              // own, so a long gap between two events reads as a gap. A trace
              // whose events all share one instant has no window to place
              // anything in, and draws no lane rather than a row of marks all
              // at the left edge.
              const start = tsKey(event.timestamp);
              const at = spread ? ((start - from) / (to - from)) * 100 : 0;
              const what = event.summary || event.type;
              return (
                <a
                  key={event.id}
                  className={`trace-row${event.failed ? " failed" : ""}`}
                  href={href(["live", "events", event.id])}
                >
                  <time
                    className="feed-time"
                    dateTime={event.timestamp}
                    title={fmtDateTime(event.timestamp)}
                  >
                    {fmtTime(event.timestamp)}
                  </time>
                  <span className="feed-actor truncate" style={{ paddingLeft: depth * 12 }}>
                    {depth > 0 && <span className="faint">└ </span>}
                    {event.actor || "engine"}
                  </span>
                  <span className="trace-what" title={what}>
                    {/* THE FAILURE, marked on the event that carried it, as
                        the event log marks it: the header counts failures,
                        and a count a reader cannot find in the list beneath
                        it is a number to distrust. */}
                    {event.failed && (
                      <TriangleAlertGlyph
                        size="xs"
                        aria-label="failed"
                        className="trace-failed-mark"
                      />
                    )}
                    {what}
                  </span>
                  {spread && (
                    <span
                      className="trace-lane"
                      role="img"
                      aria-label={`${fmtDuration(start - from)} into the trace`}
                    >
                      {/* THE POSITION IS THE ONE VALUE THAT VARIES, so it is
                          the one thing set inline — the rail, the mark's
                          colour, size and shape are the stylesheet's. */}
                      <span
                        className="trace-mark"
                        style={{ "--at": `${at}%` } as React.CSSProperties}
                      />
                    </span>
                  )}
                  <span className="feed-tail">
                    <span className="muted">{humanize(event.category)}</span>
                    <ChevronRightGlyph size="xs" />
                  </span>
                </a>
              );
            })}
          </div>
          <footer className="panel-foot">
            Spans whose parent is not in this trace are shown as roots rather than dropped — a trace
            can legitimately begin mid-flight.
          </footer>
        </Card>
      </QueryState>
    </>
  );
}
