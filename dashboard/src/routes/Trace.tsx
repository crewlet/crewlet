/**
 * One trace: every event that shares a trace id, as a tree.
 *
 * The screen this replaces rendered "Trace not found" for EVERY trace: it
 * asked for `trace` and got `{trace_id, events}`, then tested `!rows.length` —
 * `.length` on an object is `undefined`, which is falsy, which is the empty
 * state, always. Its own suite only ever exercised the span arranger on
 * hand-built arrays, so nothing caught it.
 */

import { useMemo, type CSSProperties } from "react";
import { href, useNavigator } from "~/app/router.tsx";
import { EventRow, QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, fmtDuration, fmtTime, humanize, oldestFirst, tsKey } from "~/lib/format.ts";
import type { EventRecord } from "~/protocol/index.ts";
import {
  ChevronRightGlyph,
  ErrorGlyph,
  ForkRightGlyph,
  LayersGlyph,
  ScheduleGlyph,
  TimelineGlyph,
} from "@crewlethq/icons/glyphs";
import {
  Button,
  Card,
  EmptyValue,
  InlineCode,
  PageHeader,
  Skeleton,
  Stack,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";

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
  const failed = events.filter((e) => (e.payload?.failed as boolean) === true).length;

  return (
    <>
      <PageHeader
        title="Trace"
        description={<InlineCode>{traceId}</InlineCode>}
        badges={
          <>
            <Tag appearance="outline">{events.length} events</Tag>
            {truncated && <Tag variant="warning">oldest {events.length} shown</Tag>}
          </>
        }
        actions={
          <Button
            variant="secondary"
            size="small"
            leadingIcon={<TimelineGlyph />}
            onClick={() => nav.to(["activity"], { q: traceId })}
          >
            In the log
          </Button>
        }
      />

      {loading && <Skeleton label="Loading the trace" variant="text" rows={6} />}
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
        <StatGroup columns={3}>
          <StatCard
            icon={<LayersGlyph />}
            label="Spans"
            value={events.length}
            sub="events sharing this trace"
          />
          <StatCard
            icon={<ScheduleGlyph />}
            label="Elapsed"
            value={to > from ? fmtDuration(to - from) : <EmptyValue label="Not measured" />}
            sub={
              from
                ? `${fmtTime(new Date(from).toISOString())} → ${fmtTime(new Date(to).toISOString())}`
                : ""
            }
          />
          <StatCard
            icon={<ErrorGlyph />}
            label="Failures"
            value={failed}
            sub={failed ? "at least one span recorded a failure" : "nothing failed in this trace"}
          />
        </StatGroup>

        <Card as="section" padding="none">
          <Card.Header
            divided
            style={{ paddingInline: "var(--spacing-4)", paddingTop: "var(--spacing-3)" }}
            icon={<ForkRightGlyph size="sm" />}
          >
            <Card.Title>Spans</Card.Title>
          </Card.Header>
          <Stack gap={0}>
            {rows.map(({ event, depth }) => {
              // The bar's offset and width place the span inside the trace's
              // own window, so a long gap between two spans reads as a gap.
              const start = tsKey(event.timestamp);
              const left = to > from ? ((start - from) / (to - from)) * 100 : 0;
              return (
                <EventRow
                  key={event.id}
                  // A span row's `failed` is optional on the record and
                  // required on a feed row: an event nobody marked did not
                  // fail, which is the only reading that is not a guess.
                  event={{ ...event, failed: event.failed === true }}
                  depth={depth}
                  mark={
                    <span
                      aria-hidden="true"
                      className="feed-span"
                      style={{ "--feed-span-at": `${left}%` } as CSSProperties}
                    />
                  }
                />
              );
            })}
          </Stack>
          <Card.Footer
            variant="meta"
            style={{ paddingInline: "var(--spacing-4)", paddingBottom: "var(--spacing-3)" }}
          >
            Spans whose parent is not in this trace are shown as roots rather than dropped. A trace
            can legitimately begin mid-flight.
          </Card.Footer>
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
