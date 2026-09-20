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
import { Button, Card, EmptyValue, Skeleton, StatCard, StatGroup, Tag } from "@crewlethq/ui";
import {
  ChevronRightGlyph,
  ErrorGlyph,
  ForkRightGlyph,
  LayersGlyph,
  ScheduleGlyph,
  TimelineGlyph,
} from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, fmtDuration, fmtTime, humanize, oldestFirst, tsKey } from "~/lib/format.ts";
import type { EventRecord } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";

interface Node {
  event: EventRecord;
  children: Node[];
  depth: number;
}

/**
 * Arrange spans into a tree by `parent_span_id`.
 *
 * An event whose parent is not in this trace is a ROOT here rather than being
 * dropped: a trace can begin mid-flight (the parent was published before the
 * store's retention window, or by a node whose events went elsewhere), and
 * hiding those rows loses the half of the trace that is actually present.
 */
function arrange(events: EventRecord[]): Node[] {
  const ordered = [...events].sort(oldestFirst);
  const bySpan = new Map<string, Node>();
  for (const event of ordered) {
    if (event.span_id) bySpan.set(event.span_id, { event, children: [], depth: 0 });
  }
  const roots: Node[] = [];
  for (const event of ordered) {
    const node = (event.span_id && bySpan.get(event.span_id)) || { event, children: [], depth: 0 };
    const parent = event.parent_span_id ? bySpan.get(event.parent_span_id) : undefined;
    if (parent && parent !== node) {
      parent.children.push(node);
      node.depth = parent.depth + 1;
    } else {
      roots.push(node);
    }
  }
  return roots;
}

function flatten(nodes: Node[], out: Node[] = []): Node[] {
  for (const n of nodes) {
    out.push(n);
    flatten(n.children, out);
  }
  return out;
}

export function TraceScreen({ traceId }: { traceId: string }) {
  const nav = useNavigator();
  const { data, loading, error } = useQuery("trace", { trace_id: traceId });

  // `.events`, not the answer itself.
  const events = data?.events ?? [];
  // The store caps one trace's rows and the answer says when it did. A trace
  // shown short with no note reads as a complete causal chain that simply
  // ends, which is the one thing a reader must not conclude from it.
  const truncated = Boolean(data?.truncated);
  const rows = useMemo(() => flatten(arrange(events)), [events]);

  const from = events.length ? Math.min(...events.map((e) => tsKey(e.timestamp))) : 0;
  const to = events.length ? Math.max(...events.map((e) => tsKey(e.timestamp))) : 0;
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
  // come first out of [flatten], so `rows[0]` is that span — and where the
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
  const facts: Fact[] = [
    { label: "Spans", value: known ? events.length : "" },
    { label: "Elapsed", value: to > from ? fmtDuration(to - from) : "" },
    { label: "Failures", value: known ? failed : "" },
  ];

  return (
    <>
      <PageActions>
        {
          <>
            <Tag appearance="outline">{events.length} events</Tag>
            {truncated && <Tag variant="warning">oldest {events.length} shown</Tag>}
          </>
        }
        {
          <Button
            size="small"
            variant="secondary"
            leadingIcon={<TimelineGlyph size="xs" />}
            onClick={() => nav.to(["activity"], { q: traceId })}
          >
            In the log
          </Button>
        }
      </PageActions>
      {loading && <Skeleton variant="text" rows={6} label="Loading the trace" />}

      {/* THE OBJECT'S OWN HEADER, and the trace id with it. The id used to be
          a lone `PageNote` under the page bar — the hand-rolled half of what
          `ObjectHeader` draws as an eyebrow — and the three facts beside it
          are the strip's own, in the strip's own order, so a reader who
          arrives from a turn's "Trace" button lands on the same three
          numbers wherever they meet this trace again. */}
      <ObjectHeader
        kind="Trace"
        icon="fork_right"
        identifier={traceId}
        title={name || "Trace"}
        facts={facts}
      />
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
        {/* THE SAME THREE NUMBERS THE FACT LINE ABOVE STATES, and kept: the
            header states them in the order every frame states them, and each
            tile here says what its number is OVER — which events share the
            trace, the wall clock the elapsed time spans, and whether anything
            in it failed. That is the half a fact line has no room for, and it
            is what a reader chasing a number they distrust actually reads. */}
        <StatGroup columns={3}>
          <StatCard
            icon={<LayersGlyph size="xs" />}
            label="Spans"
            value={events.length}
            sub="events sharing this trace"
          />
          <StatCard
            icon={<ScheduleGlyph size="xs" />}
            label="Elapsed"
            value={to > from ? fmtDuration(to - from) : <EmptyValue label="Not measured" />}
            sub={
              from
                ? `${fmtTime(new Date(from).toISOString())} → ${fmtTime(new Date(to).toISOString())}`
                : ""
            }
          />
          <StatCard
            icon={<ErrorGlyph size="xs" />}
            label="Failures"
            value={failed}
            sub={failed ? "at least one span recorded a failure" : "nothing failed in this trace"}
          />
        </StatGroup>

        <Card padding="none">
          <Card.Header icon={<ForkRightGlyph size="sm" />}>
            <Card.Title>Spans</Card.Title>
          </Card.Header>
          <div className="list">
            {rows.map(({ event, depth }) => {
              // The bar's offset and width place the span inside the trace's
              // own window, so a long gap between two spans reads as a gap.
              const start = tsKey(event.timestamp);
              const left = to > from ? ((start - from) / (to - from)) * 100 : 0;
              return (
                <a
                  key={event.id}
                  className="feed-row"
                  href={href(["activity", "events", event.id])}
                >
                  <time className="feed-time" dateTime={event.timestamp}>
                    {fmtTime(event.timestamp)}
                  </time>
                  <span className="feed-actor truncate" style={{ paddingLeft: depth * 12 }}>
                    {depth > 0 && <span className="faint">└ </span>}
                    {event.actor || "engine"}
                  </span>
                  <span className="feed-what truncate">
                    {event.summary || event.type}
                    <span
                      aria-hidden="true"
                      style={{
                        display: "block",
                        height: 2,
                        marginTop: 4,
                        marginLeft: `${left}%`,
                        width: "6px",
                        minWidth: 6,
                        background: "var(--accent)",
                        borderRadius: 2,
                        opacity: 0.7,
                      }}
                    />
                  </span>
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

        {events[0] && (
          <div className="row">
            <span className="t-caption">First span {fmtDateTime(events[0].timestamp)}</span>
          </div>
        )}
      </QueryState>
    </>
  );
}
