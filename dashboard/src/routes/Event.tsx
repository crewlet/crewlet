/**
 * One event, in full.
 *
 * Reached from a row, never from the nav — and from the search box, because an
 * event id pasted out of a log is a destination.
 */

import { ScreenHead } from "~/app/Shell.tsx";
import { useNavigator } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Button, Code, KeyValue, Panel, Skeleton } from "~/ui/primitives.tsx";
import { PhaseCard } from "~/components/PhaseCard.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, humanize, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { fromPhaseEvent } from "~/lib/phases.ts";

export function EventScreen({ eventId }: { eventId: string }) {
  const nav = useNavigator();
  const now = useNow();
  const { data, loading, error } = useQuery("event", { id: eventId });

  // A phase event has a first-class rendering; everything else gets its
  // payload shown honestly rather than being squeezed into a shape it is not.
  const phase = data?.type === "agent_phase_completed" ? fromPhaseEvent(data) : null;

  return (
    <>
      <ScreenHead
        title={data ? data.summary || data.type : "Event"}
        sub={data ? <code className="inline">{data.type}</code> : eventId}
        badges={
          data ? (
            <>
              <Badge outline>{humanize(data.category) || "system"}</Badge>
              {data.source && <Badge outline>{data.source}</Badge>}
            </>
          ) : undefined
        }
        actions={
          <>
            {data?.trace_id && (
              <Button size="sm" icon="gitBranch" onClick={() => nav.to(["traces", data.trace_id])}>
                Trace
              </Button>
            )}
            {data?.payload?.turn_id != null && (
              <Button
                size="sm"
                icon="layers"
                onClick={() => nav.to(["turns", String(data.payload!.turn_id)])}
              >
                Turn
              </Button>
            )}
            {data?.actor && (
              <Button size="sm" icon="user" onClick={() => nav.to(["seats", data.actor])}>
                {data.actor}
              </Button>
            )}
          </>
        }
      />

      {loading && <Skeleton rows={5} />}
      <QueryState
        error={error === "not_found" ? null : error}
        loading={loading}
        empty={
          !loading && !data
            ? {
                title: "No event with that id",
                hint: "The event store keeps 30 days. An id older than that, or from a different node's store, will not resolve.",
              }
            : undefined
        }
      >
        {data && (
          <>
            <Panel title="Envelope" icon="file">
              <KeyValue
                items={[
                  [
                    "Id",
                    <code key="i" className="inline">
                      {data.id}
                    </code>,
                  ],
                  [
                    "Type",
                    <code key="t" className="inline">
                      {data.type}
                    </code>,
                  ],
                  ["When", `${fmtDateTime(data.timestamp)} · ${relTime(data.timestamp, now)}`],
                  ["Actor", data.actor || <span className="faint">the engine itself</span>],
                  ["Source", data.source || <span className="faint">—</span>],
                  ["Category", humanize(data.category) || "system"],
                  [
                    "Topic",
                    data.topic ? (
                      <code key="tp" className="inline">
                        {data.topic}
                      </code>
                    ) : (
                      <span className="faint">—</span>
                    ),
                  ],
                  [
                    "Trace",
                    data.trace_id ? (
                      <code key="tr" className="inline">
                        {data.trace_id}
                      </code>
                    ) : (
                      <span className="faint">not traced</span>
                    ),
                  ],
                  [
                    "Span",
                    data.span_id ? (
                      <span key="s" className="mono t-caption">
                        {data.span_id}
                        {data.parent_span_id && ` (parent ${data.parent_span_id})`}
                      </span>
                    ) : (
                      <span className="faint">—</span>
                    ),
                  ],
                ]}
              />
            </Panel>

            {phase && (
              <Panel title="The phase this event records" icon="brain" padding="tight">
                <PhaseCard record={phase} defaultOpen showRole />
              </Panel>
            )}

            <Panel title="Payload" icon="database" subtitle="verbatim, as the engine stored it">
              {data.payload ? (
                <Code plain>{JSON.stringify(data.payload, null, 2)}</Code>
              ) : (
                <span className="t-caption faint">
                  This event carries no payload — its type and summary are the whole record.
                </span>
              )}
            </Panel>

            {data.tags && Object.keys(data.tags).length > 0 && (
              <Panel title="Tags" icon="hash">
                <KeyValue
                  items={Object.entries(data.tags).map(([k, v]) => [
                    k,
                    <code key={k} className="inline">
                      {v}
                    </code>,
                  ])}
                />
              </Panel>
            )}
          </>
        )}
      </QueryState>
    </>
  );
}
