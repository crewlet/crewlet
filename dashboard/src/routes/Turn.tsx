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
 *     Those rows are not dropped — they are REDUNDANT. A phase start says
 *     which phase opened, and its own completed record says that and
 *     everything else, its duration included: `agent_phase_completed` carries
 *     `duration_ms`, measured where the clock is. The pairing this screen
 *     used to do instead — fold the start onto the finish, subtract — needed
 *     both events in one reader's hands, which is exactly what a turn
 *     deep-linked WHILE IT RUNS does not have, and it is the screen this
 *     panel exists for. See `phaseDuration` in ./lib/phases.ts.
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

import { useCallback, useMemo, type ReactNode } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { href, useNavigator } from "~/app/router.tsx";
import { EventRow, QueryState, SeatChip } from "~/components/common.tsx";
import { PhaseCard } from "~/components/PhaseCard.tsx";
import {
  Badge,
  Button,
  Code,
  CopyButton,
  Disclosure,
  DownloadButton,
  KeyValue,
  Panel,
  PhaseTag,
  Skeleton,
  Stat,
  StatRow,
  cx,
} from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import {
  fmtBytes,
  fmtCount,
  fmtDateTime,
  fmtDuration,
  fmtTime,
  oldestFirst,
  tsKey,
} from "~/lib/format.ts";
import {
  fromLiveCall,
  fromPhaseEvent,
  groupTurns,
  mergePhases,
  phaseStart,
  streamedPhases,
  type PhaseRecord,
  type Timed,
} from "~/lib/phases.ts";
import {
  prefetchBlocks,
  promptWeights,
  tellStory,
  TURN_STOP,
  type PrefetchBlock,
  type PromptWeight,
} from "~/lib/turnstory.ts";
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
export function outcomeOf(rec: TurnRecord): {
  word: string;
  tone: "positive" | "caution" | "critical" | undefined;
  sub: string;
} {
  const failed = field(rec.summary, "failed") === true;
  const review = str(rec.learning, "review_outcome") || str(rec.summary, "decision");
  const executor = str(rec.learning, "outcome");
  // THE FLAG IS AN ANSWER, so it is read BEFORE an absence of words is taken
  // for an absence of record. A turn the engine stopped before any phase
  // decided anything carries `failed: true` and no word at all — `decision`
  // and `review_outcome` are both the zero `phase.Decision` — and this used
  // to fall through the em-dash return three lines below, which the caller
  // then captioned "no turn record", printed directly above the panel
  // rendering that very record.
  if (failed || review === "failed") {
    const kind = str(rec.summary, "error_kind");
    return {
      word: "failed",
      tone: "critical",
      sub: kind ? `the engine stopped it: ${kind}` : "the turn will not retry",
    };
  }
  if (!review && !executor) return { word: "—", tone: undefined, sub: "" };
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
 * How many problems this turn had, counted as the page can show them.
 *
 * ONE STOP IS ONE PROBLEM, however many records the engine wrote about it.
 * `agent_turn_completed.failed` and the dedicated record behind it — a guard
 * breach, an exhausted chain, an unavailable provider — are published in the
 * same breath about the SAME stop (`internal/engine/telemetry.go` closes a
 * failed turn with `publishEvent` and then `publishFailure`), so adding the
 * flag to the rows counted that one event twice: the header badge said
 * "2 problems" over a panel holding one row, and the reader went looking for
 * a second that is nowhere on the page.
 *
 * The flag is deduped against THAT RECORD, never against the row count. A
 * turn the reviewer decided against carries the flag with no dedicated record
 * behind it — `publishFailure` writes nothing when no guard fired and no
 * error was returned — and it can carry an unrelated `provider_fallback` in
 * the same breath. Against the count, the flag would have been swallowed by
 * that fallback and a genuinely failed turn reported as one problem rather
 * than two; against the count and with no rows at all, `clean` would have
 * gone on to claim "nothing went wrong" about it.
 */
export function problemCount(wentWrong: readonly EventRecord[], failed: boolean): number {
  const stopped = wentWrong.some((e) => TURN_STOP.has(e.type));
  return wentWrong.length + (failed && !stopped ? 1 : 0);
}

/**
 * The window this page can see the turn through.
 *
 * THE SPAN OVER EVERYTHING THE PAGE HOLDS, not over the query's answer alone.
 * Read off `events` only, a turn whose phases all arrived on the stream
 * reported a duration of "—" beside a phase list several minutes long.
 *
 * The start is a minimum over EVERY phase, never over `phases[0]`. That list
 * is ordered by when each phase LANDED, so its first element is the earliest
 * FINISHER — and a worker a delegate spawned lands inside the window of the
 * execute round that spawned it. On the one case the span exists for, a turn
 * deep-linked while it runs (no query answer to supply the other term), that
 * made the window open at the first worker's start and "Took" under-report
 * the whole stretch before the fan-out.
 *
 * Each phase's start comes from [phaseStart], which is the live record's own
 * instant or the finished record's landing less what the engine measured.
 * Reading `startedAt` off a finished record put its END into the minimum.
 *
 * A zero is dropped rather than taken as a minimum: `tsKey` answers 0 for a
 * timestamp it cannot parse, and 0 is the epoch — one unreadable instant
 * would report a turn that has been running since 1970.
 */
export function turnSpan(
  events: readonly { timestamp: string }[],
  phases: readonly Timed[],
): { from: number; to: number } {
  const live = (instants: number[]) => instants.filter((t) => t > 0);
  // EVERY instant on both sides, never the first and last of either. Indexing
  // would make the caller's sort order a precondition this function cannot
  // state or check, and it is the precondition the phase list already broke.
  const stamps = events.map((e) => tsKey(e.timestamp));
  const starts = live([...stamps, ...phases.map(phaseStart)]);
  const ends = live([...stamps, ...phases.map((p) => tsKey(p.at))]);
  // Both or neither: a start with no end would render a duration measured
  // against nothing, which is worse than the em dash the caller falls back to.
  if (!starts.length || !ends.length) return { from: 0, to: 0 };
  return { from: Math.min(...starts), to: Math.max(...ends) };
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
    <Panel padding="normal">
      <div className="col gap-3">
        {woke && (
          <div className="col gap-1">
            <div className="row gap-2">
              <span className="t-label">Woken by</span>
              {/* The SOURCE, and not the sender. The trigger's summary is
                  built by the vendor's own summariser and already opens with
                  who wrote it — "Message from founder: …" — so a `founder`
                  chip beside that sentence was the same fact twice, and two
                  chips plus a link after one line of prose is a hedge, not a
                  header. The integration is the one thing the sentence does
                  not reliably carry. */}
              {trigger?.integration && (
                <Badge outline mono title="where this turn's trigger came from">
                  {trigger.integration}
                </Badge>
              )}
              <span className="spacer" />
              {triggerId && (
                <a className="t-link" href={href(["events", triggerId])}>
                  the trigger →
                </a>
              )}
            </div>
            {/* LABEL ABOVE, PROSE BELOW, full width. As a KeyValue this was a
                two-track grid sized to the longest label, so one short line of
                prose sat in a narrow band with the rest of the panel empty
                beside it — the shape for a metadata list, and these are
                sentences. */}
            <p className="t-body measure">{woke}</p>
          </div>
        )}
        {said && (
          <div className="col gap-1">
            <div className="t-label">It set out to</div>
            <p className="t-body measure">{said}</p>
          </div>
        )}
      </div>
    </Panel>
  );
}

/**
 * What went INTO the prompt, from both directions: the six context blocks the
 * executor's prompt was assembled from, and what each phase's prompt then came
 * to. Two halves of one question — whether a heavy prompt is heavy because of
 * what was prefetched or in spite of it — and either alone leaves it open.
 *
 * Either half can be absent. A turn whose prefetch record fell out of the
 * store still has its `prompt.size` rows, and the reverse holds too, so the
 * panel renders whichever it has rather than gating both on the first.
 *
 * A BLOCK THAT FOUND NOTHING DID NOT FAIL, and the first version of this panel
 * said it did: a ✓/✗ column, four crosses down the left, reading as four
 * things that went wrong on a turn where nothing had. ✗ is a pass/fail
 * vocabulary and this is not a pass/fail question — a seat with no prior
 * episodes on this topic, no synthesized skills yet and a turn that is not its
 * first has four empty blocks and a perfectly healthy prompt.
 *
 * So it leads with what the prompt actually GOT, sized, and the rest is one
 * quiet line naming them. Absence is rendered as absence.
 *
 * The one genuinely diagnostic state stays called out: a GATED block is not
 * empty, it was never searched — the trigger was a bare pointer, so the
 * aux-LLM call was skipped and the executor searches later with
 * `search_knowledge` instead. That distinction is a configuration problem
 * versus a quiet turn, and it is the whole reason the engine puts
 * `trigger_requires_recon` on the wire.
 */
function Given({ blocks, weights }: { blocks: PrefetchBlock[]; weights: PromptWeight[] }) {
  const got = blocks.filter((b) => b.hit);
  const gated = blocks.filter((b) => !b.hit && b.gated);
  const empty = blocks.filter((b) => !b.hit && !b.gated);
  return (
    <Panel
      title="What the turn was given"
      icon="book"
      subtitle="the context blocks its prompt was assembled from, and what each phase's prompt weighed"
      padding="tight"
    >
      <div className="col gap-2">
        {blocks.length === 0 && (
          <span className="t-caption">
            No prefetch record for this turn, so there is no breakdown of where the prompt&rsquo;s
            context came from — only what each phase&rsquo;s prompt came to.
          </span>
        )}
        {blocks.length > 0 && got.length > 0 ? (
          <div className="col gap-1">
            {/* FULL-WIDTH ROWS with the figure at the far end, not a KeyValue.
                The grid's second track starts at 120px, so a byte count sat
                stranded mid-panel with the whole right half empty — and a
                bare "134 B" beside a label says nothing about what was
                measured. The heading says it once, and the rows carry the
                numbers where numbers go. */}
            <div className="row gap-2">
              <span className="t-label spacer">Reached the prompt</span>
              <span className="t-label">Rendered size</span>
            </div>
            {got.map((b) => (
              <div key={b.label} className="row gap-2">
                <span className="t-cell truncate">{b.label}</span>
                {b.note && <span className="t-caption truncate">{b.note}</span>}
                <span className="spacer" />
                <span className="mono t-num t-caption">{fmtBytes(b.bytes)}</span>
              </div>
            ))}
          </div>
        ) : (
          blocks.length > 0 && (
            <span className="t-caption">
              The prompt was built from the seat&rsquo;s own identity and this turn&rsquo;s trigger
              alone — no stored context reached it.
            </span>
          )
        )}
        {gated.length > 0 && (
          <div className="banner neutral">
            <Icon name="info" size="sm" />
            <span>
              Not searched: {list(gated.map((b) => b.label))}. The trigger was a bare pointer, so
              these filters were skipped — the executor searches later with{" "}
              <code className="inline">search_knowledge</code>, once it knows what the task needs.
            </span>
          </div>
        )}
        {empty.length > 0 && (
          <span className="t-caption">Nothing to add from {list(empty.map((b) => b.label))}.</span>
        )}
        {weights.length > 0 && <PromptWeights rows={weights} />}
      </div>
    </Panel>
  );
}

/**
 * What each phase's prompt came to, off the engine's own measurement.
 *
 * `prompt.size` exists so prompt-slimming progress is measurable rather than
 * argued about, and six small integers per phase have been reaching this
 * browser and rendering nowhere: the screen read one event out of the `given`
 * band and dropped the rest, so the only route to the number was the raw
 * payload of a row in the residual list. It belongs here, beside the blocks
 * the prompt was assembled FROM — the two halves of one question, and the
 * pair is what says whether a heavy prompt is heavy because of what was
 * prefetched or in spite of it.
 *
 * PER PHASE AND PER ROUND, never summed. A prompt is re-sent on every round of
 * the tool loop, so a total here would be neither the turn's input bill (which
 * is what the token tiles above already report) nor any single thing that was
 * ever sent. What the number answers is "how big is the frame this phase
 * reasons in", and that is a per-phase question.
 */
function PromptWeights({ rows }: { rows: PromptWeight[] }) {
  return (
    <div className="col gap-1">
      <div className="row gap-2">
        <span className="t-label spacer">Prompt sent</span>
        <span className="t-label num-col">System</span>
        <span className="t-label num-col">User</span>
        <span className="t-label num-col">Approx. tokens</span>
      </div>
      {rows.map((w, i) => (
        <div key={`${w.phase}|${w.iteration}|${i}`} className="row gap-2">
          <PhaseTag phase={w.phase} />
          {w.iteration > 1 && (
            <span className="t-caption" title="self-iterate round">
              iter {w.iteration}
            </span>
          )}
          <span className="spacer" />
          <span className="mono t-num t-caption num-col" title="characters in the system prompt">
            {fmtBytes(w.systemChars)}
          </span>
          <span className="mono t-num t-caption num-col" title="characters in the user message">
            {fmtBytes(w.userChars)}
          </span>
          <span className="mono t-num t-caption num-col" title="the engine's own approximation">
            {fmtCount(w.approximateTokens)}
          </span>
        </div>
      ))}
    </div>
  );
}

/** "a", "a and b", "a, b and c" — a list a sentence can contain. */
function list(items: string[]): string {
  const lower = items.map((s) => s.toLowerCase());
  if (lower.length <= 1) return lower[0] ?? "";
  return `${lower.slice(0, -1).join(", ")} and ${lower[lower.length - 1]}`;
}

/**
 * A row about the turn, without the columns that say nothing on a page about
 * ONE turn.
 *
 * `EventRow` is the activity feed's row: time, actor, summary, source and
 * category, on a four-track grid. Here the actor is the same seat on every
 * row — it was rendered twelve times on the turn this screen was rebuilt
 * against — and the category is an internal taxonomy nobody is filtering by.
 *
 * The actor also has to come off the SUMMARY, not just out of a column: the
 * engine builds these lines as `lead(actor, …)`, so every one of them opens
 * "Agent CEO …". Four rows under one seat's own heading do not each need to
 * name it, and the repetition costs exactly the room the sentence needed.
 *
 * The date goes too. These instants are seconds apart inside a turn the
 * header already dates, so a clock is the whole useful part of the timestamp
 * and the rest was pushing rows to three lines tall.
 */
function TurnEventRow({ event, actor }: { event: EventRecord; actor: string }) {
  return (
    <a
      className={cx("turn-row", event.failed && "failed")}
      href={href(["events", event.id])}
      title={fmtDateTime(event.timestamp)}
    >
      <time className="feed-time" dateTime={event.timestamp}>
        {fmtTime(event.timestamp)}
      </time>
      <span className="what truncate">
        {event.failed && (
          <Icon
            name="alert"
            size="xs"
            style={{ display: "inline", color: "var(--critical-ink)", marginRight: 4 }}
          />
        )}
        {withoutActor(event.summary, actor) || event.type}
      </span>
      <span className="feed-tail">
        <span className="faint mono truncate">{event.type}</span>
      </span>
    </a>
  );
}

/**
 * Drop the seat's own name from the front of a line it wrote about itself.
 *
 * Only from the FRONT, and only when it is followed by more: a summary that is
 * nothing but the actor is left alone rather than emptied, and an actor
 * appearing mid-sentence (the counterparty profiler names a subject) is not
 * this seat talking about itself and stays.
 */
export function withoutActor(summary: string, actor: string): string {
  if (!actor || !summary.startsWith(actor)) return summary;
  const rest = summary.slice(actor.length).trimStart();
  return rest || summary;
}

function EventList({ events, actor }: { events: EventRecord[]; actor: string }) {
  return (
    <div className="list">
      {events.map((e) => (
        <TurnEventRow key={e.id} event={e} actor={actor} />
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
  // THE STORE STOPPED AT ITS CAP, not at the end of the turn. The answer
  // recovers the turn's ENDING beside its opening — that is where the two
  // records this header reads its outcome, its clock and its plan summary off
  // live, and without them a cut turn was indistinguishable from one that
  // died — so what is missing is the MIDDLE. Every claim below that is made
  // over the whole turn rather than over a record has to be weakened anyway:
  // a guard breach in the gap is one this page cannot see.
  const cut = Boolean(data?.truncated);

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
    return mergePhases([...streamed, ...answered], live).sort((a, b) => tsKey(a.at) - tsKey(b.at));
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
  // The other half of the `given` band, and until now the half nothing read.
  const weights = useMemo(() => promptWeights(story.given), [story]);

  const role = phases[0]?.role ?? (rec.summary?.actor || "");
  const trigger = phases.find((p) => p.trigger)?.trigger ?? null;
  const outcome = outcomeOf(rec);
  const trouble = problemCount(story.wentWrong, field(rec.summary, "failed") === true);
  // Only claimable on a FINISHED turn with a record to claim it from, and
  // over the WHOLE turn. A running turn has not been asked about since it
  // started; a turn whose events fell out of the store's window has nothing
  // to say either way; and a turn read to the store's cap has rows this page
  // never saw, any of which could be the failure — "nothing went wrong",
  // "nothing was read" and "not everything was read" must not render alike.
  const clean = trouble === 0 && !running && !cut && Boolean(rec.summary || rec.learning);

  const { from, to } = turnSpan(events, phases);
  // THE ENGINE'S OWN WALL CLOCK, off the record that actually carries it.
  // `agent_turn_completed` has no `duration_ms` — that field is on
  // `turn_completed`, published in the same breath — so reading it off the
  // summary made this an unreachable branch and the span below the only answer
  // this tile ever gave, under a caption claiming otherwise.
  const measured = field(rec.learning, "duration_ms");
  const durationMs = typeof measured === "number" ? measured : null;
  // EVERY TRACE THIS TURN TOUCHED, not the first one to arrive.
  //
  // `events[0].trace_id` is the trace of whichever event happened to sort
  // first, and the store's own doc says a turn resumed on another node after a
  // restart spans more than one — which is exactly the turn somebody opens
  // this page to understand. One button labelled "trace" then led to half the
  // story with nothing saying a second half existed.
  const traceIds = useMemo(() => {
    const seen: string[] = [];
    for (const ev of [...events, ...phaseEvents]) {
      const id = ev.trace_id;
      if (id && !seen.includes(id)) seen.push(id);
    }
    return seen;
  }, [events, phaseEvents]);
  const traceId = traceIds[0] ?? "";

  const conversation = str(rec.summary, "conversation_key") || phases[0]?.conversationKey || "";

  // THE WHOLE SCREEN AS DATA, which is what somebody pasting a turn into a
  // bug report actually needs — and the only copyable thing a RUNNING turn
  // has, since the records below do not exist until the turn ends. Both
  // records are nested rather than flattened, under the event type each one
  // arrived as, so a reader can tell what the engine published from what this
  // page assembled — and which of the two halves a field came from.
  //
  // A THUNK, not a memo. `phases` takes a new identity on every streamed
  // frame — `agents` is pushed twice per tool round — so any memo over it
  // would re-serialize every prompt, narration, tool argument and result of a
  // running turn, twice a round, for a button nobody has clicked.
  const turnJSON = useCallback(
    () =>
      JSON.stringify(
        {
          turn_id: turnId,
          role,
          trace_id: traceId || null,
          running,
          duration_ms: durationMs,
          // WHAT THE SCREEN SAYS, THE FILE SAYS TOO. The page marks a capped
          // turn with a badge and a banner because its opening and ending
          // without its middle is indistinguishable from a turn that died
          // early — which is the ambiguity this whole read exists to remove.
          // Exported without the flag, the file reproduced it exactly: a
          // reader attaches `turn-<id>.json` to a bug report and whoever
          // opens it has no way to tell an incomplete turn from a complete
          // one. Top level, beside `running`, because this is what the page
          // assembled; `record` below is what the engine published.
          truncated: cut,
          record: {
            agent_turn_completed: rec.summary?.payload ?? null,
            turn_completed: rec.learning?.payload ?? null,
          },
          phases,
          events,
        },
        null,
        2,
      ),
    [turnId, role, traceId, running, durationMs, cut, rec, phases, events],
  );
  // Memos here rather than thunks: both ARE rendered, so they are computed
  // either way, and `rec` only changes when the turn ends.
  const summaryJSON = useMemo(
    () => JSON.stringify(rec.summary?.payload ?? {}, null, 2),
    [rec.summary],
  );
  const learningJSON = useMemo(
    () => JSON.stringify(rec.learning?.payload ?? {}, null, 2),
    [rec.learning],
  );

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
            {/* A HEADER BADGE, not a banner at the foot of the page. "This
                turn was clean" is a property of the turn, so it belongs where
                the reader already looks for the turn's state — beside the
                phase count and in the slot the problem badge would occupy.
                A full-width banner said the same thing at ten times the
                weight, after everything, reading as an announcement about
                nothing. */}
            {clean && (
              <Badge
                tone="positive"
                icon="check"
                title="no guard fired, no provider fell through, no call was refused"
              >
                nothing went wrong
              </Badge>
            )}
            {/* WHAT THE VIEW IS MISSING, in the header, because every other
                badge beside it is a claim made from these rows. The `trace`
                answer has carried this flag all along and its screen renders
                it; `turn` did not carry one at all, so a cut turn looked
                exactly like a short one. */}
            {cut && (
              <Badge
                tone="caution"
                icon="alert"
                title="the store stopped at its per-turn cap; this view holds the turn's opening and its ending, and not the middle"
              >
                middle not shown
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
            {traceIds.length > 1 ? (
              // NAMED, not collapsed. Two traces mean the turn was resumed
              // somewhere else, and which one a reader wants depends on which
              // half they are chasing.
              <span className="row gap-1">
                {traceIds.map((id, i) => (
                  <Button
                    key={id}
                    size="sm"
                    icon="gitBranch"
                    onClick={() => nav.to(["traces", id])}
                    title={`trace ${id}`}
                  >
                    Trace {i + 1} of {traceIds.length}
                  </Button>
                ))}
              </span>
            ) : (
              traceId && (
                <Button size="sm" icon="gitBranch" onClick={() => nav.to(["traces", traceId])}>
                  Trace
                </Button>
              )
            )}
            <CopyButton
              text={turnJSON}
              label="Copy turn"
              title="the whole turn as JSON — its record, its phases and everything else it published"
            />
            {/* THE SAME BYTES, out of the same thunk. A turn is pasted into a
                thread and ATTACHED to a bug report, and the second one is not
                a clipboard gesture: an incident is read weeks later, a
                clipboard holds exactly one thing, and a self-iterating turn's
                JSON is past what anyone wants inline. */}
            <DownloadButton
              text={turnJSON}
              filename={`turn-${turnId}.json`}
              label="Download turn"
              title="the same JSON, saved as a file"
            />
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
        {/* ABOVE EVERYTHING, because it is a statement about the rows every
            panel below is built from rather than about the turn. Named
            precisely: this is not "some events are missing", it is "the ones
            that are missing are the ending", which is the difference between
            a reader distrusting the page and a reader distrusting the turn. */}
        {cut && (
          <div className="banner caution">
            <Icon name="alert" size="sm" />
            <span>
              This turn published more than the store returns for one turn. What is here is its{" "}
              <strong>opening and its ending</strong> — {events.length} events, so the records below
              are the turn&rsquo;s own — and what is missing is the middle. Phases from the middle
              of a long self-iterating turn are not on this page, and neither is anything that went
              wrong in them.{" "}
              {traceId ? "The trace carries the same work from the trigger down." : ""}
            </span>
          </div>
        )}

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
                    : cut
                      ? // A CUT VIEW HOLDS BOTH ENDS, so the span is the
                        // turn's real window — but it is the window rather
                        // than the engine's own measurement, and on this
                        // branch the record that carries that measurement is
                        // missing from a turn that has both its ends. Say
                        // which of the two the number is.
                        "spanning the turn's ends — its own record is not among them"
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
              sub={
                outcome.sub ||
                (running
                  ? "still running"
                  : cut
                    ? // "no turn record" is a claim about the TURN, and on a
                      // cut view it would be a claim about the READ. The
                      // answer recovers the turn's ending precisely so this
                      // branch is rare: reaching it means the records are
                      // genuinely absent from a view that holds both ends.
                      "no turn record, and this view holds both ends"
                    : "no turn record")
              }
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
            <EventList events={story.wentWrong} actor={role} />
          </Panel>
        )}

        {(prefetch.length > 0 || weights.length > 0) && (
          <Given blocks={prefetch} weights={weights} />
        )}

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
            <EventList events={story.did} actor={role} />
          </Panel>
        )}

        {story.leftBehind.length > 0 && (
          <Panel
            title="What the seat learned"
            icon="database"
            count={story.leftBehind.length}
            // "What it left behind" read as work abandoned rather than as
            // memory written. This is the reflection pass — it runs AFTER the
            // last phase, on auxiliary workers of its own, and everything in
            // it is something the seat now knows that it did not before.
            subtitle="the reflection pass, once the phases were done"
            padding="none"
          >
            <EventList events={story.leftBehind} actor={role} />
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

        {(rec.summary || rec.learning) && (
          <Panel
            title="The turn's own record"
            icon="file"
            subtitle="the two events the engine closes every turn with"
            padding="tight"
          >
            <div className="col gap-2">
              {conversation && (
                <KeyValue
                  items={[
                    [
                      "Conversation",
                      // LABELLED, and explained. It is "{source}:{channel}:
                      // {thread}" — which external thread this turn was
                      // answering — and it used to be an unexplained
                      // truncated string under the seat's name.
                      <span className="row gap-2 baseline">
                        <code className="inline">{conversation}</code>
                        <span className="t-caption">the external thread this turn served</span>
                      </span>,
                    ],
                  ]}
                />
              )}
              {/* One expander per record, EACH with its own copy button. A
                  single control in the panel head copied one of the two
                  without saying which. */}
              {rec.summary && (
                <Disclosure
                  label="agent_turn_completed — the dashboard's summary"
                  actions={
                    <CopyButton text={summaryJSON} variant="ghost" title="copy this record" />
                  }
                >
                  <Code selectable label="agent_turn_completed, as JSON">
                    {summaryJSON}
                  </Code>
                </Disclosure>
              )}
              {rec.learning && (
                <Disclosure
                  label="turn_completed — the learning subsystem's record"
                  actions={
                    <CopyButton text={learningJSON} variant="ghost" title="copy this record" />
                  }
                >
                  <Code selectable label="turn_completed, as JSON">
                    {learningJSON}
                  </Code>
                </Disclosure>
              )}
            </div>
          </Panel>
        )}
      </QueryState>
    </>
  );
}
