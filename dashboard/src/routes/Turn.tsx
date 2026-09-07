/**
 * One turn, end to end — read as the story it is rather than as a log of it.
 *
 * A long self-iterating turn pushes its own earlier phases out of any per-seat
 * window, so "show me everything that happened in THIS turn" needs to be a
 * question of its own, which is what the `turn` query answers. It carries more
 * than the phases: the fallbacks, the guard breaches, the prefetch and the
 * learning the turn left behind all name the same turn id and render as
 * anonymous rows in the log otherwise.
 *
 * That last sentence used to describe a single panel called "Everything else
 * this turn published", and it was wrong three ways at once.
 *
 *  1. **Most of it was not "else".** On the three-iteration turn this was
 *     rebuilt against, six of its twelve rows read "started execute (iter 2)"
 *     — one per phase card sitting directly above, which already says EXECUTE,
 *     iter 2, its model, its rounds, its tokens and what it decided. A seventh
 *     was `agent_turn_completed`, which the same screen ALSO rendered as the
 *     stat strip and as a raw JSON dump. The reader who called it "kinda a
 *     duplicate of the phases" was reading it correctly.
 *
 *     Those rows are not dropped. A phase start is folded onto its own phase
 *     card (`withStarts`), where it stops being a duplicate and becomes the
 *     missing half of a fact: `agent_phase_completed` carries only the instant
 *     the phase LANDED, so until now no completed phase had a duration
 *     anywhere on this dashboard — on a turn that self-iterated three times
 *     and cost 290k tokens, "which round took ninety seconds" was derivable
 *     from two events in the same query answer and shown by neither.
 *
 *  2. **Everything left had the same weight.** `reflection_completed` is a
 *     sentinel whose own payload doc says it deliberately carries no outcome.
 *     A guard breach is a turn the engine stopped. As two identical feed rows,
 *     an operator scanning for the second reads past it. So what went wrong is
 *     a banner ABOVE the phases now, and the bookkeeping is grouped under the
 *     question it answers.
 *
 *  3. **A healthy turn could not say so.** The panel was never empty, so
 *     "nothing went wrong here" was not a state this screen could reach.
 *
 * And the header lied. `duration_ms` and `review_outcome` were read off
 * `agent_turn_completed`, which has neither field — they are on `turn_completed`,
 * the learning subsystem's record of the same turn, published in the same
 * breath and sitting in the same query answer. So "Took" silently fell back to
 * the event span on every turn that ever ran, and "Review outcome" showed an
 * em dash while asserting underneath it that the value came from the turn's
 * own record. Both records are read now, and named for what each one is.
 */

import { useMemo } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { href, useNavigator } from "~/app/router.tsx";
import { EventRow, QueryState, SeatChip } from "~/components/common.tsx";
import { PhaseCard } from "~/components/PhaseCard.tsx";
import { Badge, Banner, Button, Panel, Skeleton, Stat, StatRow, cx } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtBytes, fmtCount, fmtDateTime, fmtDuration, oldestFirst, tsKey } from "~/lib/format.ts";
import {
  fromLiveCall,
  fromPhaseEvent,
  groupTurns,
  mergePhases,
  phaseStarts,
  streamedPhases,
  withStarts,
  type PhaseRecord,
} from "~/lib/phases.ts";
import { prefetchBlocks, tellStory, type PrefetchBlock } from "~/lib/turnstory.ts";
import { useAgents, usePhaseEvents } from "~/lib/store-hooks.ts";
import type { EventRecord, FeedRow } from "~/protocol/index.ts";

/** The two records the engine closes every turn with, read as one answer. */
interface TurnRecord {
  /** `agent_turn_completed` — the dashboard's summary: tokens, models, trigger. */
  summary: EventRecord | undefined;
  /** `turn_completed` — the learning record: the clock, the outcome, the words. */
  learning: EventRecord | undefined;
}

function field(event: EventRecord | undefined, key: string): unknown {
  return (event?.payload as Record<string, unknown> | undefined)?.[key];
}

function str(event: EventRecord | undefined, key: string): string {
  const v = field(event, key);
  return typeof v === "string" ? v : "";
}

/**
 * How a turn ended, in the two words that mean different things.
 *
 * `outcome` is the EXECUTOR's own last word — delivered / no_action / blocked,
 * or the engine-written `incomplete` when it never submitted at all.
 * `review_outcome` is the REVIEWER's decision — done / self_iterate / failed —
 * except where a guard ended the turn first, which is exactly when the two
 * disagree. Showing only the second made a turn that delivered nothing and a
 * turn that delivered look identical whenever the reviewer accepted both.
 */
function outcomeOf(rec: TurnRecord): {
  word: string;
  tone: "positive" | "caution" | "critical" | undefined;
  sub: string;
} {
  const failed = field(rec.summary, "failed") === true;
  const review = str(rec.learning, "review_outcome") || str(rec.summary, "decision");
  const executor = str(rec.learning, "outcome");
  if (!review && !executor) return { word: "—", tone: undefined, sub: "" };
  const kind = str(rec.summary, "error_kind");
  if (failed || review === "failed") {
    return {
      word: "failed",
      tone: "critical",
      sub: kind ? `the engine stopped it: ${kind}` : "the turn will not retry",
    };
  }
  const label: Record<string, string> = {
    delivered: "delivered the work",
    no_action: "nothing to do — ended silently",
    blocked: "blocked, and said why",
    incomplete: "never said what it did",
  };
  return {
    word: review === "done" ? "done" : review || executor,
    tone: review === "done" ? "positive" : "caution",
    sub:
      label[executor] ?? (executor ? `the executor said ${executor}` : "the reviewer's decision"),
  };
}

/**
 * What woke this turn, and what it set out to do.
 *
 * Neither was on this screen at all. The trigger rides on EVERY phase event
 * and on the summary, and the feed's own turn card leads with it — here the
 * only way to learn it was to read the raw `trigger` object out of the JSON
 * dump at the bottom. `plan_summary` is the agent's own account of what the
 * turn was for, written by the executor or by the reviewer describing what
 * landed, and nothing read it either.
 */
function TurnBrief({ rec, trigger }: { rec: TurnRecord; trigger: PhaseRecord["trigger"] }) {
  const woke = trigger?.summary || str(rec.summary, "prompt") || trigger?.type || "";
  const said = str(rec.learning, "plan_summary");
  const triggerId = typeof trigger?.id === "string" ? trigger.id : "";
  if (!woke && !said) return null;
  return (
    <Panel padding="tight">
      <div className="col gap-2">
        {woke && (
          <div className="row gap-2" style={{ alignItems: "baseline" }}>
            <span className="t-label" style={{ minWidth: 92 }}>
              Woken by
            </span>
            <span className="prose" style={{ flex: 1, minWidth: 0 }}>
              {woke}
            </span>
            {trigger?.integration && (
              <Badge outline mono>
                {trigger.integration}
              </Badge>
            )}
            {/* The trigger's own event id has always been on the descriptor
                and nothing linked it. "What asked for this" is the first
                question a reader brings to a turn they did not expect. */}
            {triggerId && (
              <a className="t-caption" href={href(["events", triggerId])}>
                the trigger →
              </a>
            )}
          </div>
        )}
        {said && (
          <div className="row gap-2" style={{ alignItems: "baseline" }}>
            <span className="t-label" style={{ minWidth: 92 }}>
              It set out to
            </span>
            <span className="prose" style={{ flex: 1, minWidth: 0 }}>
              {said}
            </span>
          </div>
        )}
      </div>
    </Panel>
  );
}

/**
 * The six context blocks the executor's prompt was built from.
 *
 * The event's own summary collapses this to "2/6 hits", which is right for a
 * feed and useless here: every block degrades to empty on failure by design,
 * so an unreachable store, an unconfigured auxiliary model and a filter that
 * genuinely found nothing all render as the same nothing. A gated block is
 * marked as gated rather than as a miss — that distinction is a configuration
 * problem versus a quiet turn, and it is the whole reason the engine puts
 * `trigger_requires_recon` on the wire.
 */
function Prefetch({ blocks }: { blocks: PrefetchBlock[] }) {
  const hits = blocks.filter((b) => b.hit).length;
  return (
    <Panel
      title="What the turn was given"
      icon="book"
      subtitle={`${hits} of ${blocks.length} context blocks reached the prompt`}
      padding="tight"
    >
      <div className="col gap-1">
        {blocks.map((b) => (
          <div key={b.label} className={cx("row gap-2", !b.hit && "faint")}>
            <Icon
              name={b.hit ? "check" : b.gated ? "minus" : "x"}
              size="xs"
              style={{
                color: b.hit ? "var(--positive-ink)" : "var(--text-muted)",
                flex: "none",
              }}
            />
            <span className="t-cell truncate" style={{ minWidth: 160 }}>
              {b.label}
            </span>
            <span className="t-caption truncate" style={{ flex: 1, minWidth: 0 }}>
              {b.gated || b.note}
            </span>
            <span className="t-caption mono" style={{ flex: "none" }}>
              {b.hit ? fmtBytes(b.bytes) : ""}
            </span>
          </div>
        ))}
      </div>
    </Panel>
  );
}

/**
 * A row about the turn, without the two columns that say nothing on a screen
 * about ONE turn.
 *
 * `EventRow` is the activity feed's row and has four columns — time, actor,
 * summary, source+category. Here the actor is the same seat on every row (it
 * was rendered twelve times on the turn this was rebuilt against) and the
 * category is an internal taxonomy. Two of four columns were noise.
 */
function TurnEventRow({ event }: { event: EventRecord }) {
  return (
    <a className={cx("feed-row", event.failed && "failed")} href={href(["events", event.id])}>
      <time className="feed-time" dateTime={event.timestamp}>
        {fmtDateTime(event.timestamp)}
      </time>
      <span className="feed-what truncate">
        {event.failed && (
          <Icon
            name="alert"
            size="xs"
            style={{ display: "inline", color: "var(--critical-ink)", marginRight: 4 }}
          />
        )}
        {event.summary || event.type}
      </span>
      <span className="feed-tail">
        <span className="faint mono truncate">{event.type}</span>
      </span>
    </a>
  );
}

function EventList({ events }: { events: EventRecord[] }) {
  return (
    <div className="list">
      {events.map((e) => (
        <TurnEventRow key={e.id} event={e} />
      ))}
    </div>
  );
}

export function TurnScreen({ turnId }: { turnId: string }) {
  const nav = useNavigator();
  const { data, loading, error } = useQuery("turn", { turn_id: turnId });

  const agents = useAgents();
  const phaseEvents = usePhaseEvents();

  const events = useMemo(() => [...(data?.events ?? [])].sort(oldestFirst), [data]);

  // The phases the `turn` query knew about, plus the ones that have landed on
  // the stream since it was answered, plus whichever phase is running now.
  //
  // The query is answered ONCE, at mount, so on a turn opened WHILE IT RUNS —
  // the deep link out of a running turn card — it used to be the whole page:
  // the phases that finished afterwards never arrived, the phase in flight was
  // never on the page at all, and a turn deep-linked the moment it started
  // answered with nothing and stayed that way.
  const phases = useMemo<PhaseRecord[]>(() => {
    const answered = events
      .filter((e) => e.type === "agent_phase_completed")
      .map((e) => fromPhaseEvent(e))
      .filter((r): r is PhaseRecord => r !== null);
    const streamed = streamedPhases(phaseEvents, (r) => r.turnId === turnId);
    const live = agents
      .filter((a) => a.live_call && a.live_call.turn_id === turnId)
      .map((a) => fromLiveCall(a.live_call!, a.role));
    // Within a turn, oldest first: a turn is read forwards. `mergePhases`
    // orders newest first, which is right for a feed and wrong here.
    const merged = mergePhases([...streamed, ...answered], live).sort(
      (a, b) => tsKey(a.at) - tsKey(b.at),
    );
    // The starts come from BOTH halves for the same reason the phases do: a
    // turn opened while it runs has no query answer to read them off.
    return withStarts(merged, phaseStarts([...events, ...phaseEvents]));
  }, [events, phaseEvents, agents, turnId]);

  // A NESTED call belongs UNDER the phase that made it. `host_phase` and
  // `host_iteration` have always been on the wire, `groupTurns` has always
  // done the split and `PhaseCard` has always had the prop — and this screen
  // used none of it, so a delegate fan-out of eight rendered as eight
  // siblings of the turn's own two phases and the reader had to work out
  // which round each belonged to. It also made the "N phases" badge and the
  // token total disagree with the feed's card for the same turn.
  const group = useMemo(
    () => groupTurns(phases).find((g) => g.turnId === turnId),
    [phases, turnId],
  );
  const own = group?.phases ?? phases;
  const nested = group?.nested ?? new Map<string, PhaseRecord[]>();
  const workerTokens = phases.reduce((n, p) => n + (p.hostPhase ? p.totalTokens : 0), 0);
  const workerCount = phases.filter((p) => p.hostPhase).length;

  const running = phases.some((p) => p.live);
  const rec: TurnRecord = useMemo(
    () => ({
      summary: events.find((e) => e.type === "agent_turn_completed"),
      learning: events.find((e) => e.type === "turn_completed"),
    }),
    [events],
  );
  const story = useMemo(() => tellStory(events), [events]);
  const prefetch = useMemo(
    () => prefetchBlocks(story.given.find((e) => e.type === "prefetch_summary")),
    [story],
  );

  const role = phases[0]?.role ?? (rec.summary?.actor || "");
  const trigger = phases.find((p) => p.trigger)?.trigger ?? null;
  const outcome = outcomeOf(rec);
  const trouble = story.wentWrong.length + (field(rec.summary, "failed") === true ? 1 : 0);

  // THE SPAN OVER EVERYTHING THE PAGE HOLDS, not over the query's answer alone.
  // Read off `events` only, a turn whose phases all arrived on the stream
  // reported a duration of "—" beside a phase list several minutes long.
  const first = phases[0];
  const last = phases[phases.length - 1];
  const from = Math.min(
    ...[
      events.length ? tsKey(events[0]!.timestamp) : Infinity,
      first ? tsKey(first.startedAt) : Infinity,
    ],
  );
  const to = Math.max(
    ...[events.length ? tsKey(events[events.length - 1]!.timestamp) : 0, last ? tsKey(last.at) : 0],
  );
  // THE ENGINE'S OWN WALL CLOCK, off the record that actually carries it.
  // `agent_turn_completed` has no `duration_ms` — that field is on
  // `turn_completed`, published in the same breath — so reading it off the
  // summary made this an unreachable branch and the span below the only answer
  // this tile ever gave, under a caption claiming otherwise.
  const measured = field(rec.learning, "duration_ms");
  const durationMs = typeof measured === "number" ? measured : null;
  // The turn's trace, from whichever half of the page has it. A turn opened
  // while it runs has no query answer to read it off.
  const traceId =
    events[0]?.trace_id || phaseEvents.find((e) => e.payload?.turn_id === turnId)?.trace_id || "";

  const conversation = str(rec.summary, "conversation_key") || phases[0]?.conversationKey || "";

  return (
    <>
      <ScreenHead
        title="Turn"
        sub={<code className="inline">{turnId}</code>}
        badges={
          <>
            {role && <Badge outline>{role}</Badge>}
            <Badge outline>{own.length} phases</Badge>
            {/* FROM WHAT ACTUALLY WENT WRONG, not from the phase records
                alone. `phases.some(p => p.failed)` misses every turn the
                engine killed BETWEEN phases — a refused charge, an exhausted
                chain, a guard that fired — which are precisely the turns with
                no failed phase record to find. */}
            {trouble > 0 && (
              <Badge tone="critical" icon="alert">
                {trouble === 1 ? "1 problem" : `${trouble} problems`}
              </Badge>
            )}
          </>
        }
        actions={
          <>
            {role && (
              <Button size="sm" icon="user" onClick={() => nav.to(["seats", role])}>
                The seat
              </Button>
            )}
            {traceId && (
              <Button size="sm" icon="gitBranch" onClick={() => nav.to(["traces", traceId])}>
                Trace
              </Button>
            )}
          </>
        }
      />

      {loading && <Skeleton rows={6} />}
      <QueryState
        error={error}
        loading={loading}
        // GATED ON WHAT THE PAGE HOLDS, not on the query's answer alone.
        // `QueryState` renders this INSTEAD of its children, so a turn whose
        // phases are all arriving on the stream — a turn deep-linked the
        // moment it started, which answers the query with nothing — rendered
        // "No events for this turn" while phase after phase streamed in
        // behind it.
        empty={
          events.length || phases.length
            ? undefined
            : {
                title: "No events for this turn",
                hint: "A turn is assembled from the events that name its id. If it ran outside the store's 30-day window there is nothing to assemble.",
              }
        }
      >
        <Panel padding="none">
          <StatRow cols={4}>
            <Stat
              icon="user"
              label="Seat"
              value={role ? <SeatChip name={role} handle={role} size="md" /> : "—"}
              // The conversation key used to sit here, raw and truncated —
              // "mattermost:9zd7xj4mqj8hf8fy4gt7aitiny:cnjzasu…" under a seat's
              // name, with nothing saying what it was. It is a property of the
              // TURN, not of the seat, and it is explained below rather than
              // dumped here.
              sub={running ? "running now" : "ran this turn"}
            />
            <Stat
              icon="clock"
              label="Took"
              value={
                durationMs != null
                  ? fmtDuration(durationMs)
                  : to > from
                    ? fmtDuration(to - from)
                    : "—"
              }
              sub={
                durationMs != null
                  ? "the engine's own measurement"
                  : running
                    ? "still running"
                    : "spanning the turn's first and last event"
              }
            />
            <Stat
              icon="coin"
              label="Tokens"
              // THE TURN'S OWN PHASES. A worker's tokens are already charged
              // through the shared meter, which is why the engine keeps them
              // out of `total_tokens` and reports them as `subagent_tokens` —
              // so summing every record made this tile disagree with the very
              // record shown further down the same page. The split is also
              // the only thing that answers "how much of this turn was
              // fan-out" when a seat's spend jumps and its own rounds did not.
              value={fmtCount(own.reduce((n, p) => n + p.totalTokens, 0))}
              sub={
                workerTokens > 0
                  ? `${own.reduce((n, p) => n + p.tools.length, 0)} tool calls · +${fmtCount(
                      workerTokens,
                    )} in ${workerCount} worker${workerCount === 1 ? "" : "s"}`
                  : `${own.reduce((n, p) => n + p.tools.length, 0)} tool calls`
              }
            />
            <Stat
              icon="check"
              label="Outcome"
              value={outcome.word}
              tone={outcome.tone}
              sub={outcome.sub || (running ? "still running" : "no turn record")}
            />
          </StatRow>
        </Panel>

        <TurnBrief rec={rec} trigger={trigger} />

        {/* ABOVE the phases, because a turn that fell over is not something a
            reader should have to scroll past three panels to discover. Absent
            entirely on a healthy turn, which is the state the flat list could
            never reach. */}
        {story.wentWrong.length > 0 && (
          <Panel
            title="What went wrong"
            icon="alert"
            count={story.wentWrong.length}
            subtitle="guard breaches, exhausted chains, refused calls — the reason to open this page"
            padding="none"
          >
            <EventList events={story.wentWrong} />
          </Panel>
        )}

        {prefetch.length > 0 && <Prefetch blocks={prefetch} />}

        <Panel title="Phases" icon="brain" count={own.length} padding="tight">
          <div className="col gap-2">
            {own.map((p, i) => (
              <PhaseCard key={p.key} record={p} nested={nested.get(p.key)} defaultOpen={i === 0} />
            ))}
            {!own.length && (
              <span className="t-caption">
                No phase completed in this turn — it may have died before its first phase published.
              </span>
            )}
          </div>
        </Panel>

        {story.did.length > 0 && (
          <Panel
            title="What else it did"
            icon="zap"
            count={story.did.length}
            subtitle="work outside the tool loop: coding runs, delegations, colleagues"
            padding="none"
          >
            <EventList events={story.did} />
          </Panel>
        )}

        {story.leftBehind.length > 0 && (
          <Panel
            title="What it left behind"
            icon="database"
            count={story.leftBehind.length}
            subtitle="what the company learned from this turn, after its last phase"
            padding="none"
          >
            <EventList events={story.leftBehind} />
          </Panel>
        )}

        {story.rest.length > 0 && (
          <Panel
            title="Also published"
            icon="activity"
            count={story.rest.length}
            subtitle="rows this build has no particular place for"
            padding="none"
          >
            <div className="list">
              {story.rest.map((e) => (
                <EventRow key={e.id} event={e as unknown as FeedRow} showDate />
              ))}
            </div>
          </Panel>
        )}

        {!story.wentWrong.length && !running && (rec.summary || rec.learning) && (
          <Banner tone="neutral" icon="check">
            Nothing went wrong in this turn: no guard fired, no provider fell through, no call was
            refused.
          </Banner>
        )}
        {running && !story.wentWrong.length && (
          <Banner tone="info">
            This turn is still running. The rows it publishes beside its phases carry no turn id on
            the wire until they are stored, so reload once it has finished to see them.
          </Banner>
        )}

        {(rec.summary || rec.learning) && (
          <Panel
            title="The turn's own record"
            icon="file"
            subtitle="the two events the engine closes every turn with"
            padding="tight"
          >
            <div className="col gap-2">
              {conversation && (
                <div className="row gap-2" style={{ alignItems: "baseline" }}>
                  <span className="t-label" style={{ minWidth: 118 }}>
                    Conversation
                  </span>
                  {/* Labelled, at last. It is "{source}:{channel}:{thread}" —
                      which external thread this turn was answering — and it
                      was previously rendered as an unexplained truncated
                      string under the seat's name. */}
                  <code className="inline truncate" title="the external thread this turn served">
                    {conversation}
                  </code>
                </div>
              )}
              <details>
                <summary className="t-caption">
                  agent_turn_completed — the dashboard's summary
                </summary>
                <pre className="code">{JSON.stringify(rec.summary?.payload ?? {}, null, 2)}</pre>
              </details>
              <details>
                <summary className="t-caption">
                  turn_completed — the learning subsystem's record
                </summary>
                <pre className="code">{JSON.stringify(rec.learning?.payload ?? {}, null, 2)}</pre>
              </details>
            </div>
          </Panel>
        )}
      </QueryState>
    </>
  );
}
