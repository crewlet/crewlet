/**
 * One event, in full.
 *
 * Reached from a row, never from the nav — and from the search box, because an
 * event id pasted out of a log is a destination.
 */

import { useNavigator } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Button, Card, CodeBlock, Disclosure, Skeleton, Tag } from "@crewlethq/ui";
import {
  DatabaseGlyph,
  DescriptionGlyph,
  ErrorGlyph,
  ForkRightGlyph,
  LayersGlyph,
  NeurologyGlyph,
  PersonGlyph,
  TagGlyph,
} from "@crewlethq/icons/glyphs";
// OURS, AND THERE IS NO PEER. `Copyable` renders the value it copies and is
// named by it; this is a bare ACTION over text derived at press time — a
// payload that is not on the screen as a string — with its own copied /
// refused feedback. See the report.
import { CopyButton } from "~/ui/primitives.tsx";
import { PropertiesRail, type Property } from "~/app/frame/PropertiesRail.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PhaseCard } from "~/components/PhaseCard.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, humanize, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { fromPhaseEvent } from "~/lib/phases.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import type { EventRecord } from "~/protocol/index.ts";

/** What an id that resolves to nothing means, on the page and in the rail. */
const NO_EVENT_HINT =
  "The event store keeps 30 days. An id older than that, or from a different node's store, will not resolve.";

/**
 * What an event IS — the five facts, in one order, for the page and the rail.
 *
 * ONE BUILDER, which is `ObjectHeader`'s own rule and the only thing that
 * keeps the two frames honest with each other: an event is read by scanning
 * WHAT happened, in which part of the engine, WHO it happened to, WHEN, and
 * whether the causal chain it belongs to was recorded. A page that led with
 * the category and a rail that led with the actor would make a reader
 * re-learn the same record every time it changed frame.
 */
function eventFacts(event: EventRecord, now: number): Fact[] {
  return [
    { label: "Type", value: <code className="inline">{event.type}</code> },
    { label: "Category", value: humanize(event.category) || "system" },
    {
      // A WORDED EMPTY, not a dash: an event with no actor was published by
      // the engine rather than by anybody, which is a fact rather than a
      // blank — and only a real actor carries a link out to its seat.
      label: "Actor",
      value: event.actor || <span className="faint">the engine itself</span>,
      path: event.actor ? ["company", "people", event.actor] : undefined,
    },
    {
      label: "When",
      // Relative in the line and absolute on hover, the way `DateCell` reads
      // in every grid: a reader who has the record open asks "how long ago"
      // first and "at what instant" only once they are writing it down.
      value: <span title={fmtDateTime(event.timestamp)}>{relTime(event.timestamp, now)}</span>,
    },
    {
      label: "Trace",
      value: event.trace_id ? (
        <code className="inline">{event.trace_id}</code>
      ) : (
        <span className="faint">not traced</span>
      ),
      path: event.trace_id ? ["activity", "traces", event.trace_id] : undefined,
    },
  ];
}

/**
 * The half of the envelope the facts above do NOT carry.
 *
 * Shared, for the same reason the facts are: the page and the rail draw one
 * rail of properties out of one list, so a row that is dropped on one of them
 * cannot survive on the other.
 *
 * The id is absent on purpose — it is the header's own identifier, and a
 * record that repeated it immediately underneath would be spending the
 * widest row in the rail on the one string already above it.
 */
function envelopeProperties(event: EventRecord): Property[] {
  return [
    { label: "Source", value: event.source },
    { label: "Topic", value: event.topic ? <code className="inline">{event.topic}</code> : "" },
    {
      label: "Span",
      value: event.span_id ? (
        <span className="mono t-caption">
          {event.span_id}
          {event.parent_span_id && ` (parent ${event.parent_span_id})`}
        </span>
      ) : (
        ""
      ),
    },
  ];
}

/**
 * The failure mark, and the one thing about an event that is a STATE.
 *
 * `failed` is derived server-side from the type plus the stored tag and rides
 * on the record as well as on the feed row, precisely so that the two agree —
 * and this screen read neither. A turn that failed rendered red in the feed
 * and entirely clean on its own page, which is the single worst place for the
 * two to disagree: the page is where somebody goes to find out what went
 * wrong.
 */
function eventStatus(event: EventRecord) {
  if (!event.failed) return undefined;
  return (
    <Tag variant="danger" leadingIcon={<ErrorGlyph size="xs" />}>
      failed
    </Tag>
  );
}

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
        {
          <>
            {data?.trace_id && (
              <Button
                size="small"
                variant="secondary"
                leadingIcon={<ForkRightGlyph size="xs" />}
                onClick={() => nav.to(["activity", "traces", data.trace_id])}
              >
                Trace
              </Button>
            )}
            {data?.payload?.turn_id != null && (
              <Button
                size="small"
                variant="secondary"
                leadingIcon={<LayersGlyph size="xs" />}
                onClick={() => nav.to(["activity", "turns", String(data.payload!.turn_id)])}
              >
                Turn
              </Button>
            )}
            {data?.actor && (
              <Button
                size="small"
                variant="secondary"
                leadingIcon={<PersonGlyph size="xs" />}
                onClick={() => nav.to(["company", "people", data.actor])}
              >
                {data.actor}
              </Button>
            )}
          </>
        }
      </PageActions>
      {/* WHAT THE SCREEN ANSWERS, which is what a note is for. It used to
          hold the event's own type — identity, in the one line on the screen
          that is not about identity, and now the header's first fact. */}
      <PageNote>One stored event, exactly as the engine wrote it.</PageNote>

      {loading && <Skeleton variant="text" rows={5} label="Loading the event" />}

      {/* THE CATEGORY AND THE SOURCE WERE TWO BADGES IN THE PAGE BAR, which
          is where a screen's CONTROLS live — so the record's identity was
          drawn in the one strip that is not about the object, and the rail
          would have had to spell it a second way. Nothing is lost: the
          summary is the title, the failure is the status, the category is the
          fact it always was and the source is a row of the record below. */}
      {data && (
        <ObjectHeader
          kind="Event"
          icon="timeline"
          identifier={data.id}
          title={data.summary || data.type}
          status={eventStatus(data)}
          facts={eventFacts(data, now)}
        />
      )}

      <QueryState
        error={error === "not_found" ? null : error}
        loading={loading}
        empty={
          !loading && !data ? { title: "No event with that id", hint: NO_EVENT_HINT } : undefined
        }
      >
        {data && (
          <>
            <Card>
              <Card.Header icon={<DescriptionGlyph size="sm" />}>
                <Card.Title>Envelope</Card.Title>
              </Card.Header>
              <PropertiesRail groups={[{ properties: envelopeProperties(data) }]} />
            </Card>

            {phase && (
              // `sm` IS OUR `tight`, and for the reason their own type gives:
              // a panel has one left edge and its header sets it, so the
              // vertical-only step is the one that keeps a card's body on the
              // same line as its title.
              <Card padding="sm">
                <Card.Header icon={<NeurologyGlyph size="sm" />}>
                  <Card.Title>The phase this event records</Card.Title>
                </Card.Header>
                <PhaseCard record={phase} defaultOpen showRole />
              </Card>
            )}

            <Card>
              <Card.Header
                icon={<DatabaseGlyph size="sm" />}
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
                <Card.Title>Payload</Card.Title>
              </Card.Header>
              {data.payload ? (
                // Same treatment as the Turn record: this screen's whole
                // point is one JSON record, so the record owns select-all
                // rather than the page taking it.
                <div className="col gap-1">
                  {/* `plain` is THEIR word for no header; ours was "no wrap",
                      which is `wrap={false}`. The copy control is the panel's
                      own, so the block's is off — two of them over one record
                      is a reader choosing between identical buttons. */}
                  <CodeBlock
                    plain
                    wrap={false}
                    copyable={false}
                    maxHeight={460}
                    selectable
                    label="The event payload, as JSON"
                    code={JSON.stringify(data.payload, null, 2)}
                  />
                  <span className="t-caption">
                    Click into the payload, and ⌘A / Ctrl+A selects it alone rather than the page.
                  </span>
                </div>
              ) : (
                <span className="t-caption faint">
                  This event carries no payload — its type and summary are the whole record.
                </span>
              )}
            </Card>

            {data.tags && Object.keys(data.tags).length > 0 && (
              <Card>
                <Card.Header icon={<TagGlyph size="sm" />}>
                  <Card.Title>Tags</Card.Title>
                </Card.Header>
                <PropertiesRail
                  groups={[
                    {
                      properties: Object.entries(data.tags).map(([k, v]) => ({
                        label: k,
                        code: true,
                        value: <code className="inline">{v}</code>,
                      })),
                    },
                  ]}
                />
              </Card>
            )}
          </>
        )}
      </QueryState>
    </>
  );
}

/**
 * One event, beside the feed it was found in.
 *
 * # Why the payload is folded here and open on the page
 *
 * The page's whole point is the record: a reader who navigated to one event
 * wants the JSON, and it is the last thing on the screen. The rail's question
 * is the other one — is this the event I meant, and what does it say — and a
 * forty-line dump answers it by pushing the answer off the top of a 420 px
 * column. So the disclosure is shut, with its size on the head, and one click
 * opens it without leaving the list.
 *
 * # The phase card is the page's
 *
 * An `agent_phase_completed` event carries a whole turn phase — the prompt,
 * the rounds, the tool calls — and `PhaseCard` draws it at a width the rail
 * does not have. The rail names what the event IS and ends at `Open ↗`, which
 * is one click to the frame that can draw it.
 */
export function EventPeek({ eventId }: { eventId: string }) {
  const now = useNow();
  const { data, loading, error } = useQuery("event", { id: eventId }, { enabled: eventId !== "" });

  // NOT AN EMPTY RAIL. `peek=event:` is reached from a pasted id as often as
  // from a row — the search box takes one — so an id that resolves to nothing
  // says so, and says why an id can stop resolving.
  const missing = !loading && !data;
  const payload = data?.payload;

  return (
    <>
      {data && (
        <ObjectHeader
          size="peek"
          kind="Event"
          icon="timeline"
          identifier={data.id}
          title={data.summary || data.type}
          status={eventStatus(data)}
          facts={eventFacts(data, now)}
        />
      )}
      <div className="col gap-3">
        {loading && !data && <Skeleton variant="text" rows={6} label="Loading the event" />}
        <QueryState
          // `not_found` is an ANSWER here rather than a refusal: the store was
          // reached and has no such row, which the empty state below states
          // precisely. Every other code is the engine failing to answer.
          error={error === "not_found" ? null : error}
          loading={loading}
          empty={missing ? { title: "No event with that id", hint: NO_EVENT_HINT } : undefined}
        >
          {data && (
            <>
              <Card>
                <Card.Header icon={<DescriptionGlyph size="sm" />}>
                  <Card.Title>Record</Card.Title>
                </Card.Header>
                <PropertiesRail
                  groups={[
                    { properties: envelopeProperties(data) },
                    ...(data.tags && Object.keys(data.tags).length > 0
                      ? [
                          {
                            name: "Tags",
                            properties: Object.entries(data.tags).map(([k, v]) => ({
                              label: k,
                              code: true,
                              value: <code className="inline">{v}</code>,
                            })),
                          },
                        ]
                      : []),
                  ]}
                />
              </Card>

              {payload ? (
                <Disclosure
                  title="Payload"
                  // WHAT IS INSIDE, on the closed head. A fold with no count
                  // is a fold nobody opens: the reader cannot tell a record
                  // with two fields from one with forty without clicking.
                  count={Object.keys(payload).length}
                  actions={
                    <CopyButton
                      text={() => JSON.stringify(payload, null, 2)}
                      title="this event's payload, as JSON"
                    />
                  }
                >
                  <CodeBlock
                    plain
                    wrap={false}
                    copyable={false}
                    maxHeight={460}
                    selectable
                    label="The event payload, as JSON"
                    code={JSON.stringify(payload, null, 2)}
                  />
                </Disclosure>
              ) : (
                <p className="t-caption faint">
                  This event carries no payload — its type and summary are the whole record.
                </p>
              )}
            </>
          )}
        </QueryState>
      </div>
    </>
  );
}
