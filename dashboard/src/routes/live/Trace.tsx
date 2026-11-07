/**
 * One trace: every event that shares a trace id, as a tree.
 *
 * The screen this replaces rendered "Trace not found" for EVERY trace: it
 * asked for `trace` and got `{trace_id, events}`, then tested `!rows.length` —
 * `.length` on an object is `undefined`, which is falsy, which is the empty
 * state, always. Its own suite only ever exercised the span arranger on
 * hand-built arrays, so nothing caught it.
 *
 * AN ADMIN'S SCREEN, under an address that has no section of its own: a
 * trace is every event a piece of work caused — what the seats said and ran —
 * and the engine answers `trace` to an admin alone. Its address sits under
 * Live's overview, which everybody reads, so the screen itself draws the lock
 * (`GrantRequired`) for anybody else and asks nothing, as the frame does for
 * a section.
 */

import { useMemo, type CSSProperties } from "react";
import { href, useNavigator } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { CoverageNote } from "~/components/CoverageNote.tsx";
import { Button, Card, Skeleton, TableTextCell, Tag, type TreeGridColumn } from "@crewlethq/ui";
import { TreeGrid } from "~/app/frame/TreeGrid.tsx";
import { SplitGlyph, ChartNoAxesGanttGlyph, TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { ADMIN } from "~/lib/access.ts";
import { GrantRequired } from "~/app/frame/GrantRequired.tsx";
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

/** One row of the tree: an event, and the events of the spans it opened. */
export interface TraceNode {
  id: string;
  label: string;
  children: TraceNode[];
}

/** A span: every event that carried its id, and the spans opened under it. */
interface Span {
  events: EventRecord[];
  children: Span[];
  /** When the span's first event landed — where it sits among its siblings. */
  at: number;
}

/**
 * Arrange a trace's events into a tree: ONE ROW PER EVENT, nested by the span
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
export function arrange(events: readonly EventRecord[]): TraceNode[] {
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
  // A CHILD SPAN HANGS UNDER THE EVENT OF ITS PARENT THAT CAME BEFORE IT, which
  // keeps every event of a span on one level and its child spans one deeper.
  // A child that opens before any event of its parent stands beside them.
  const forest: TraceNode[] = [];
  const visit = (span: Span, into: TraceNode[]) => {
    // OWN EVENTS AND CHILD SPANS, BY TIME. A child sorts at its first event;
    // on a tie the span's own event comes first, since it is what opened it.
    const items: { at: number; rank: number; event?: EventRecord; span?: Span }[] = [
      ...span.events.map((event) => ({ at: tsKey(event.timestamp), rank: 0, event })),
      ...span.children.map((child) => ({ at: child.at, rank: 1, span: child })),
    ];
    items.sort((a, b) => a.at - b.at || a.rank - b.rank);
    let last: TraceNode | undefined;
    for (const item of items) {
      if (item.event) {
        last = { id: item.event.id, label: item.event.summary || item.event.type, children: [] };
        into.push(last);
      } else if (item.span) visit(item.span, last ? last.children : into);
    }
  };
  roots.sort((a, b) => a.at - b.at);
  for (const root of roots) visit(root, forest);
  return forest;
}

export function TraceScreen({ traceId }: { traceId: string }) {
  const viewer = useViewer();
  // THE FIRST READING IS WAITED OUT, as the frame waits; a viewer read that
  // FAILED mounts the screen, and the engine's own answer says what it finds.
  if (viewer.asking) return <Skeleton variant="text" rows={6} label="Checking your access" />;
  if (!viewer.loading && !viewer.admin) {
    return <GrantRequired grants={[ADMIN]} reader={viewer.login} />;
  }
  return <Trace traceId={traceId} />;
}

function Trace({ traceId }: { traceId: string }) {
  const nav = useNavigator();
  const now = useNow();
  const { data, loading, error, refusal } = useQuery("trace", { trace_id: traceId });

  // `.events`, not the answer itself.
  const events = data?.events ?? [];
  // The store caps one trace's rows and the answer says when it did. A trace
  // shown short with no note reads as a complete causal chain that simply
  // ends, which is the one thing a reader must not conclude from it.
  const truncated = Boolean(data?.truncated);
  const forest = useMemo(() => arrange(events), [events]);
  const byId = useMemo(() => new Map(events.map((e) => [e.id, e])), [events]);

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
  // come first out of [arrange], so `forest[0]` is that span, and where the
  // whole trace begins mid-flight, it is still the earliest thing present.
  const opening = forest[0] ? byId.get(forest[0].id) : undefined;
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

  // THE SENTENCE IS THE TREE'S COLUMN, indented by its span; the facts give
  // way before it does. A head sorts siblings within their parent, never
  // across, and with no sort chosen the rows are in the order things happened.
  const columns: TreeGridColumn[] = [
    {
      key: "what",
      header: "What happened",
      floor: "12rem",
      sortValue: (id) => byId.get(id)?.summary || byId.get(id)?.type,
    },
    {
      key: "actor",
      header: "Actor",
      shrink: true,
      drop: 2,
      sortValue: (id) => byId.get(id)?.actor || "engine",
    },
    // NO LANE where every event shares one instant: there is no window to place
    // anything in.
    ...(spread ? [{ key: "lane", header: "In the trace", width: "120px", drop: 1 }] : []),
    {
      key: "time",
      header: "Time",
      align: "right",
      shrink: true,
      sortValue: (id) => tsKey(byId.get(id)?.timestamp ?? ""),
    },
    {
      key: "category",
      header: "Category",
      shrink: true,
      drop: 3,
      sortValue: (id) => humanize(byId.get(id)?.category),
    },
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
      {/* A TRACE IS ASSEMBLED FROM EVERY DATA NODE, and a node that did not answer
          is a gap in the causal chain that would otherwise read as its end. */}
      <CoverageNote coverage={[data?.coverage]} what="this trace" />
      <QueryState
        error={error}
        refusal={refusal}
        loading={loading}
        empty={
          events.length
            ? undefined
            : {
                title: "No events carry this trace id",
                hint: "Traces older than 30 days aren’t kept.",
              }
        }
      >
        <Card padding="none">
          <Card.Header icon={<SplitGlyph size="sm" />}>
            <Card.Title>Events</Card.Title>
          </Card.Header>
          <TreeGrid
            name="events"
            label="Events in this trace"
            flush
            columns={columns}
            rows={forest}
            defaultExpanded="all"
            readOnly
            phoneRows="compact"
            rowHref={(id) => href(["live", "events", id])}
            isFailed={(id) => byId.get(id)?.failed === true}
            renderCell={(id, column, grid) => {
              const event = byId.get(id);
              if (!event) return null;
              const what = event.summary || event.type;
              switch (grid.columnKey(column)) {
                case "what":
                  return (
                    <span className="row gap-1 trace-what" title={what}>
                      {/* THE FAILURE, marked on the event that carried it, as
                          the event log marks it: the header counts failures. */}
                      {event.failed && (
                        <TriangleAlertGlyph
                          size="xs"
                          aria-label="failed"
                          className="trace-failed-mark"
                        />
                      )}
                      <span className="truncate">{what}</span>
                    </span>
                  );
                case "actor":
                  return <TableTextCell>{event.actor || "engine"}</TableTextCell>;
                case "lane": {
                  // WHERE IN THE TRACE'S WINDOW the event landed, so a long gap
                  // between two events reads as a gap.
                  const start = tsKey(event.timestamp);
                  return (
                    <span
                      className="trace-lane"
                      role="img"
                      aria-label={`${fmtDuration(start - from)} into the trace`}
                    >
                      <span
                        className="trace-mark"
                        style={
                          { "--at": `${((start - from) / (to - from)) * 100}%` } as CSSProperties
                        }
                      />
                    </span>
                  );
                }
                case "time":
                  return (
                    <time
                      className="t-num"
                      dateTime={event.timestamp}
                      title={fmtDateTime(event.timestamp)}
                    >
                      {fmtTime(event.timestamp)}
                    </time>
                  );
                case "category":
                  return <TableTextCell>{humanize(event.category)}</TableTextCell>;
                default:
                  return null;
              }
            }}
          />
        </Card>
      </QueryState>
    </>
  );
}
