/**
 * One event, in full.
 *
 * Reached from a row, never from the nav — and from the search box, because an
 * event id pasted out of a log is a destination.
 */

import { useNavigator } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Button, Code, CopyButton, Panel, Skeleton } from "~/ui/primitives.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { PhaseCard } from "~/components/PhaseCard.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, humanize, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { fromPhaseEvent } from "~/lib/phases.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

export function EventScreen({ eventId }: { eventId: string }) {
  const nav = useNavigator();
  const now = useNow();
  const { data, loading, error } = useQuery("event", { id: eventId });

  // A phase event has a first-class rendering; everything else gets its
  // payload shown honestly rather than being squeezed into a shape it is not.
  const phase = data?.type === "agent_phase_completed" ? fromPhaseEvent(data) : null;

  return (
    <>
      <PageActions>
        {data ? (
          <>
            <Badge outline>{humanize(data.category) || "system"}</Badge>
            {data.source && <Badge outline>{data.source}</Badge>}
          </>
        ) : undefined}
        {
          <>
            {data?.trace_id && (
              <Button
                size="sm"
                icon="gitBranch"
                onClick={() => nav.to(["activity", "traces", data.trace_id])}
              >
                Trace
              </Button>
            )}
            {data?.payload?.turn_id != null && (
              <Button
                size="sm"
                icon="layers"
                onClick={() => nav.to(["activity", "turns", String(data.payload!.turn_id)])}
              >
                Turn
              </Button>
            )}
            {data?.actor && (
              <Button
                size="sm"
                icon="user"
                onClick={() => nav.to(["company", "people", data.actor])}
              >
                {data.actor}
              </Button>
            )}
          </>
        }
      </PageActions>
      <PageNote>{data ? <code className="inline">{data.type}</code> : eventId}</PageNote>

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
              <PropertiesRail
                groups={[
                  {
                    properties: [
                      { label: "Id", value: <code className="inline">{data.id}</code> },
                      { label: "Type", value: <code className="inline">{data.type}</code> },
                      {
                        label: "When",
                        value: `${fmtDateTime(data.timestamp)} · ${relTime(data.timestamp, now)}`,
                      },
                      // A WORDED EMPTY, not a dash: an event with no actor was
                      // published by the engine rather than by anybody, which
                      // is a fact rather than a blank.
                      {
                        label: "Actor",
                        value: data.actor || <span className="faint">the engine itself</span>,
                      },
                      { label: "Source", value: data.source },
                      { label: "Category", value: humanize(data.category) || "system" },
                      {
                        label: "Topic",
                        value: data.topic ? (
                          <code className="inline">{data.topic}</code>
                        ) : undefined,
                      },
                      {
                        label: "Trace",
                        value: data.trace_id ? (
                          <code className="inline">{data.trace_id}</code>
                        ) : (
                          <span className="faint">not traced</span>
                        ),
                      },
                      {
                        label: "Span",
                        value: data.span_id ? (
                          <span className="mono t-caption">
                            {data.span_id}
                            {data.parent_span_id && ` (parent ${data.parent_span_id})`}
                          </span>
                        ) : undefined,
                      },
                    ],
                  },
                ]}
              />
            </Panel>

            {phase && (
              <Panel title="The phase this event records" icon="brain" padding="tight">
                <PhaseCard record={phase} defaultOpen showRole />
              </Panel>
            )}

            <Panel
              title="Payload"
              icon="database"
              subtitle="verbatim, as the engine stored it"
              actions={
                data.payload ? (
                  <CopyButton
                    text={() => JSON.stringify(data.payload, null, 2)}
                    title="this event's payload, as JSON"
                  />
                ) : undefined
              }
            >
              {data.payload ? (
                // Same treatment as the Turn record: this screen's whole
                // point is one JSON record, so the record owns select-all
                // rather than the page taking it.
                <div className="col gap-1">
                  <Code plain selectable label="The event payload, as JSON">
                    {JSON.stringify(data.payload, null, 2)}
                  </Code>
                  <span className="t-caption">
                    Click into the payload, and ⌘A / Ctrl+A selects it alone rather than the page.
                  </span>
                </div>
              ) : (
                <span className="t-caption faint">
                  This event carries no payload — its type and summary are the whole record.
                </span>
              )}
            </Panel>

            {data.tags && Object.keys(data.tags).length > 0 && (
              <Panel title="Tags" icon="hash">
                <PropertiesRail
                  groups={[
                    {
                      properties: Object.entries(data.tags).map(([k, v]) => ({
                        label: k,
                        value: <code className="inline">{v}</code>,
                      })),
                    },
                  ]}
                />
              </Panel>
            )}
          </>
        )}
      </QueryState>
    </>
  );
}
