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
import { ScreenHead } from "~/app/Shell.tsx";
import { href, useNavigator } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Button, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, fmtDuration, fmtTime, humanize, oldestFirst, tsKey } from "~/lib/format.ts";
import type { EventRecord } from "~/protocol/index.ts";

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
  const rows = useMemo(() => flatten(arrange(events)), [events]);

  const from = events.length ? Math.min(...events.map((e) => tsKey(e.timestamp))) : 0;
  const to = events.length ? Math.max(...events.map((e) => tsKey(e.timestamp))) : 0;
  const failed = events.filter((e) => (e.payload?.failed as boolean) === true).length;

  return (
    <>
      <ScreenHead
        title="Trace"
        sub={<code className="inline">{traceId}</code>}
        badges={<Badge outline>{events.length} events</Badge>}
        actions={
          <Button size="sm" icon="activity" onClick={() => nav.to(["activity"], { q: traceId })}>
            In the log
          </Button>
        }
      />

      {loading && <Skeleton rows={6} />}
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
        <Panel padding="none">
          <StatRow cols={3}>
            <Stat
              icon="layers"
              label="Spans"
              value={events.length}
              sub="events sharing this trace"
            />
            <Stat
              icon="clock"
              label="Elapsed"
              value={to > from ? fmtDuration(to - from) : "—"}
              sub={
                from
                  ? `${fmtTime(new Date(from).toISOString())} → ${fmtTime(new Date(to).toISOString())}`
                  : ""
              }
            />
            <Stat
              icon="alert"
              label="Failures"
              value={failed}
              sub={failed ? "at least one span recorded a failure" : "nothing failed in this trace"}
            />
          </StatRow>
        </Panel>

        <Panel title="Spans" icon="gitBranch" padding="none">
          <div className="list">
            {rows.map(({ event, depth }) => {
              // The bar's offset and width place the span inside the trace's
              // own window, so a long gap between two spans reads as a gap.
              const start = tsKey(event.timestamp);
              const left = to > from ? ((start - from) / (to - from)) * 100 : 0;
              return (
                <a key={event.id} className="feed-row" href={href(["events", event.id])}>
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
                    <span className="faint">{humanize(event.category)}</span>
                    <Icon name="chevronRight" size="xs" />
                  </span>
                </a>
              );
            })}
          </div>
          <footer className="panel-foot">
            Spans whose parent is not in this trace are shown as roots rather than dropped — a trace
            can legitimately begin mid-flight.
          </footer>
        </Panel>

        {events[0] && (
          <div className="row">
            <span className="t-caption">First span {fmtDateTime(events[0].timestamp)}</span>
          </div>
        )}
      </QueryState>
    </>
  );
}
