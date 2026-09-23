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
import { href, useNavigator } from "~/app/router.tsx";
import {
  PhasesDroppedNote,
  QueryState,
  RECORD_MAX_HEIGHT,
  SeatChip,
} from "~/components/common.tsx";
import { PhaseCard } from "~/components/PhaseCard.tsx";
import {
  Button,
  Callout,
  Card,
  CodeBlock,
  Disclosure,
  EmptyState,
  Skeleton,
  Tag,
  cx,
} from "@crewlethq/ui";
import {
  Book2Glyph,
  BoltGlyph,
  CheckGlyph,
  DatabaseGlyph,
  DescriptionGlyph,
  ErrorGlyph,
  ForkRightGlyph,
  LayersGlyph,
  NeurologyGlyph,
  PersonGlyph,
  TimelineGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
// STILL OURS, EACH FOR ITS OWN REASON. `CopyButton` and `DownloadButton` are
// bare ACTIONS over text derived at press time — `Copyable` renders the value
// it copies and is named by it, and nothing over there saves a file at all.
// `PhaseTag` has a real peer (`Tag` carries the three phase variants), but it
// is a primitive in `~/ui`, so porting it is that file's half rather than a
// copy inlined into a route. See the report.
import { CopyButton, DownloadButton, PhaseTag, type Tone } from "~/ui/primitives.tsx";
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
  decisionLabel,
  decisionMeaning,
  decisionTone,
  fromLiveCall,
  fromPhaseEvent,
  groupTurns,
  mergePhases,
  phaseDuration,
  streamedPhases,
  turnSpan,
  type PhaseRecord,
} from "~/lib/phases.ts";
import {
  prefetchBlocks,
  promptWeights,
  collapseRuns,
  isFailed,
  tellStory,
  TURN_STOP,
  type PrefetchBlock,
  type PromptWeight,
  type Run,
  type Story,
} from "~/lib/turnstory.ts";
import { useAgents, usePhaseEvents, usePhasesDroppedSince } from "~/lib/store-hooks.ts";
import type { EventRecord, TurnRow } from "~/protocol/index.ts";
import { usePageLabels } from "~/app/Shell.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";

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
  tone: Tone | undefined;
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
  // NO OUTCOME IS THE EMPTY STRING, not the mark an absence is DRAWN as. This
  // returned "—" and two call sites below tested `word === "—"` — so the
  // sentinel was a glyph, and changing how this product draws an absence
  // (which it just did: one `EmptyValue` en dash everywhere, never an em
  // dash) would have turned the sentinel into a value that renders, with
  // both comparisons silently going false and a running turn sprouting an
  // Outcome fact reading "—". A caller that has a tile to fill draws the mark
  // itself; this says only that there is nothing to draw.
  if (!review && !executor) return { word: "", tone: undefined, sub: "" };
  // ONE TABLE, in lib/phases.ts. This held a private copy of the executor's
  // label map and the copies had already drifted: `incomplete` read "never said
  // what it did" here and "never said what it did — the engine marked it
  // incomplete" on every phase card, so one fact printed as two sentences
  // depending on which screen a reader was on. That is the failure `textcut`,
  // `whsec` and `jsprovision` each exist to end on the engine side of this
  // repository.
  //
  // The TONE comes off the same row, which is what makes this tile and the
  // phase chip agree about which outcomes need a reader — `no_action` is no
  // longer a caution here and a neutral there.
  const spoken = review ? "review" : "execute";
  const executorSaid = decisionMeaning("execute", executor);
  return {
    word: review === "done" ? "done" : review || executor,
    tone: decisionTone(spoken, review || executor),
    // A WORD THIS BUILD CANNOT GLOSS IS STILL ATTRIBUTED. `decisionLabel` falls
    // through verbatim, which would print the bare word twice — once as the
    // value and once as its own caption — so the row is read directly and the
    // attribution is written where there is no row.
    sub:
      executorSaid?.label ??
      (executor ? `the executor said ${executor}` : "the reviewer's decision"),
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

// ---------------------------------------------------------------------------
// One turn, as both frames read it
// ---------------------------------------------------------------------------

/**
 * Everything the page and the rail know about one turn.
 *
 * THE PAGE AND THE PEEK ARE ONE DEFINITION of what a turn shows — the rule
 * `routes/work/WorkItem.tsx` states for a task, and the same drift is
 * available here. Written twice, the rail is the one somebody reads forty
 * times a day, so it would be the one that kept its fields while the page
 * quietly fell behind, and a reader who clicked through from the rail to
 * "see the whole thing" would find less than they started with.
 *
 * EVERY FIELD COMES FROM THE SAME THREE SOURCES: the `turn` query, the phases
 * that have landed on the stream since it answered, and whichever phase is
 * running now. The query is answered ONCE, at mount, so a turn deep-linked
 * while it runs answers with nothing — and that is precisely the turn both
 * frames exist for.
 */
export interface TurnView {
  turnId: string;
  loading: boolean;
  error: string | null;
  /** Oldest first: a turn is read forwards. */
  events: EventRecord[];
  /** The store stopped at its per-turn cap, so what is missing is the MIDDLE. */
  cut: boolean;
  /**
   * The tab dropped a streamed phase that completed after the answer did, so
   * this turn MAY be missing one — see `usePhasesDroppedSince`. Not the same
   * gap as [TurnView.cut]: that one is the store's, this one is the tab's, and
   * [TurnView.reread] closes it.
   */
  phasesDropped: boolean;
  /** Ask the `turn` question again, which is what closes [TurnView.phasesDropped]. */
  reread: () => void;
  /** Every phase record in hand, the workers a delegate spawned included. */
  phases: PhaseRecord[];
  /** The turn's OWN phases, without those workers. */
  own: PhaseRecord[];
  /** The workers, filed under the phase that spawned each. */
  nested: Map<string, PhaseRecord[]>;
  rec: TurnRecord;
  role: string;
  trigger: PhaseRecord["trigger"] | null;
  outcome: ReturnType<typeof outcomeOf>;
  running: boolean;
  /** The engine's own wall clock, or null where no record carries one. */
  durationMs: number | null;
  /** The window everything this frame holds falls inside — see [turnSpan]. */
  span: { from: number; to: number };
  /** The turn's own token bill, without the workers — see [turnFacts]. */
  tokens: number;
  workerTokens: number;
  workerCount: number;
  /** The highest self-iterate round its own phases reached. */
  iterations: number;
  /**
   * The turn's own events, sorted into the bands both frames read them in.
   *
   * ON THE VIEW rather than on the screen, because the header's state marks
   * are made from it and the header is one component in two frames. Computed
   * twice — once here and once beside the page's bands — it would be two
   * `tellStory` passes over one array on every streamed frame of a running
   * turn, and the page's own copy is the one that would quietly stop matching
   * the badge above it.
   */
  story: Story;
  /** How many things went wrong, counted the way [problemCount] counts. */
  trouble: number;
  /**
   * Nothing went wrong, AND this turn is in a position to claim it.
   *
   * Only on a FINISHED turn with a record to claim it from, and over the
   * WHOLE turn: a running turn has not been asked since it started, a turn
   * whose events fell out of the store's window has nothing to say either
   * way, and a turn read to the store's cap — or one whose streamed phases
   * the tab has dropped — has rows neither frame saw, any of which could be
   * the failure. "Nothing went wrong", "nothing was read"
   * and "not everything was read" must not render alike.
   */
  clean: boolean;
  /** Every trace this turn touched, store's list first — see [useTurnView]. */
  traceIds: string[];
  /**
   * Which attempt at its trigger this run was, when there was more than one,
   * and the runs it shares that trigger with — oldest first.
   *
   * Null for the ordinary turn that ran once. The SERVER counts it: a count
   * taken here would count only what this page happened to load.
   */
  attempt: { index: number; total: number; rows: TurnRow[] } | null;
}

export function useTurnView(turnId: string): TurnView {
  // GUARDED ON THE ID. A rail is opened from a pasted `peek=turn:` as often as
  // from a row, and an empty one would ask the engine for a turn with no id
  // and be refused on the way in.
  const {
    data,
    loading,
    error,
    refetch: reread,
  } = useQuery("turn", { turn_id: turnId }, { enabled: turnId !== "" });

  const agents = useAgents();
  const phaseEvents = usePhaseEvents();
  const phasesDropped = usePhasesDroppedSince(data);

  const events = useMemo(() => [...(data?.events ?? [])].sort(oldestFirst), [data]);

  // EVERY TRACE THIS TURN TOUCHED, not the first one to arrive.
  //
  // `events[0].trace_id` is the trace of whichever event happened to sort
  // first, and the store's own doc says a turn resumed on another node after a
  // restart spans more than one — which is exactly the turn somebody opens
  // this page to understand. One button labelled "trace" then led to half the
  // story with nothing saying a second half existed.
  //
  // THE STORE'S OWN LIST COMES FIRST, because a derived one can only ever name
  // the traces of rows this frame HOLDS: a cut view is missing its middle and
  // a turn past the retention window is missing most of itself, so a trace
  // that lived only in the gap is one no derivation can reach.
  // `internal/api/queries/insight.go` seeks them separately and DEGRADES to an
  // empty list rather than failing the read — which is why an empty answer
  // falls through to the derivation instead of being taken for "this turn
  // touched none".
  //
  // AND THE STREAMED HALF IS THIS TURN'S ONLY. `usePhaseEvents` is the tab's
  // GLOBAL phase slice — every seat's, every turn's, 200 deep — so folded
  // whole it offered "Trace 3 of 4" buttons leading to other turns' traces.
  // It is derived HERE rather than on the screen so the rail gets the same
  // list: a peek holds no phase slice of its own and could not compute one.
  const traceIds = useMemo(() => {
    const seen: string[] = [];
    const add = (id: string) => {
      if (id && !seen.includes(id)) seen.push(id);
    };
    for (const id of data?.trace_ids ?? []) add(id);
    for (const ev of events) add(ev.trace_id);
    // `fromPhaseEvent` is the one reader of a phase payload's shape, so the
    // turn is matched through it rather than by reaching into `payload` here.
    for (const ev of phaseEvents) {
      if (fromPhaseEvent(ev)?.turnId === turnId) add(ev.trace_id);
    }
    return seen;
  }, [data, events, phaseEvents, turnId]);
  // THE STORE STOPPED AT ITS CAP, not at the end of the turn. The answer
  // recovers the turn's ENDING beside its opening — that is where the two
  // records the header reads its outcome, its clock and its plan summary off
  // live, and without them a cut turn was indistinguishable from one that
  // died — so what is missing is the MIDDLE. Every claim made over the whole
  // turn rather than over a record has to be weakened anyway: a guard breach
  // in the gap is one neither frame can see.
  const cut = Boolean(data?.truncated);

  // WHICH ATTEMPT AT ITS TRIGGER THIS IS, and where the others are.
  //
  // A turn id names one RUN (`adr/0017`), so a trigger that failed without
  // reaching outside the engine and was redelivered is several turns — and
  // this screen is where every deep link in the product lands. Arriving on
  // the failed one with nothing saying a later one succeeded is how a reader
  // concludes the work never happened; arriving on the second with nothing
  // saying it is a retry is how they conclude the company did it twice.
  //
  // The server answers this: it holds the work key and the index, and
  // counting from the page would count only what the page loaded.
  const attempts = useMemo(() => data?.attempts ?? [], [data]);
  const attempt = useMemo(() => {
    if (attempts.length < 2) return null;
    const index = attempts.findIndex((a) => a.turn_id === turnId);
    return index < 0 ? null : { index: index + 1, total: attempts.length, rows: attempts };
  }, [attempts, turnId]);

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

  // THE ENGINE'S OWN WALL CLOCK, off the record that actually carries it.
  // `agent_turn_completed` has no `duration_ms` — that field is on
  // `turn_completed`, published in the same breath — so reading it off the
  // summary made this an unreachable branch and the span below the only answer
  // the tile ever gave, under a caption claiming otherwise.
  const measured = field(rec.learning, "duration_ms");

  // WHAT WENT WRONG, and whether this turn may say nothing did. Both are read
  // by the header, which is one component in two frames, so they are derived
  // once here rather than on each screen — see the fields' own notes above.
  const story = useMemo(() => tellStory(events), [events]);
  const trouble = problemCount(story.wentWrong, field(rec.summary, "failed") === true);

  return {
    turnId,
    loading,
    error,
    events,
    cut,
    phasesDropped,
    reread,
    attempt,
    phases,
    own,
    nested,
    rec,
    role: phases[0]?.role ?? (rec.summary?.actor || ""),
    trigger: phases.find((p) => p.trigger)?.trigger ?? null,
    outcome: outcomeOf(rec),
    running,
    durationMs: typeof measured === "number" ? measured : null,
    span: turnSpan(events, phases),
    tokens: own.reduce((n, p) => n + p.totalTokens, 0),
    workerTokens,
    workerCount,
    traceIds,
    // THE HIGHEST ITERATION ITS OWN PHASES REACHED, which is what a reader
    // means by "how many rounds did this take" and what the turns list counts
    // (`MAX(iteration)`, in `store.Turns`) — a self-iterate round, not a tool
    // round. Over the turn's own phases only: a worker's iteration belongs to
    // the delegate call that spawned it rather than to this turn.
    //
    // THE WORD IS "ITERATIONS" EVERYWHERE NOW. "Rounds" is this product's word
    // for a phase's TOOL rounds — `Model.tsx`'s column, every phase card's `3r`
    // — and one word over two quantities put "Rounds 1" directly above "3r" for
    // the same turn on the same screen.
    iterations: own.reduce((n, p) => Math.max(n, p.iteration), 0),
    story,
    trouble,
    clean:
      trouble === 0 && !running && !cut && !phasesDropped && Boolean(rec.summary || rec.learning),
  };
}

/**
 * The sentence that says what woke this turn, in the order the engine can
 * supply one: the trigger's own summary, the prompt the turn record kept, and
 * the bare trigger type for a wake nothing wrote a sentence about.
 */
function wokeBy(view: TurnView): string {
  return view.trigger?.summary || str(view.rec.summary, "prompt") || view.trigger?.type || "";
}

/**
 * The LEAD SENTENCE of a piece of prose, which is the most a title may take.
 *
 * A heading is a name, and a name is one clause long. What arrives here is not
 * a name: `plan_summary` is the reviewer's own account of the turn, written by
 * a model against the engine's call ledger, and a model asked to account for
 * four tool calls routinely writes four sentences. Measured on a real turn,
 * the header read "Founder posted a test message in Mattermost. Since the task
 * was delivered directly, I need to reply in the thread rather than staying
 * silent. Activating mattermost_post_message to reply to founder's test
 * message Replying to founder's test message to confirm Mattermost
 * integration works" — 280 characters set at `--fs-xl` semibold, three lines
 * deep, pushing the facts under it off a laptop's first screen. The turns
 * list, the peek rail and the feed card head with the same string.
 *
 * THE REST IS NOT DROPPED. [TurnBrief] prints the whole summary whenever this
 * took less than all of it, so the lead is an entry point rather than a cut.
 *
 * A BOUNDARY IS A STOP FOLLOWED BY A SPACE, which is what keeps `1m 52s`,
 * `v1.2` and `mattermost_post_message` whole — a bare `.` split every one of
 * them. Prose with no boundary at all is returned as it stands: a summary that
 * is one long sentence is still one sentence, and the two-line clamp on
 * `.object-title` is what bounds that case, because a cut mid-clause reads as
 * a broken string rather than as a short title.
 */
export function lead(prose: string): string {
  const at = /[.?!]\s/.exec(prose);
  return at ? prose.slice(0, at.index + 1) : prose;
}

/**
 * What this turn is called — or the empty string, where nothing has named it.
 *
 * A TURN HAS NO NAME, so this is the nearest thing it has to one: the LEAD of
 * the plan summary the executor or the reviewer wrote about what this turn was
 * for. That is also what the turns list puts in its "What it did" column —
 * `Turn.Summary` at the store IS `plan_summary` — so a reader who opened a
 * rail from a row is headed by the sentence they clicked on.
 *
 * It falls back to what WOKE the turn, because a turn that has not finished
 * has no plan summary yet. That fallback is led too: a vendor summariser's
 * sentence is no more bounded than a model's. The panel below drops whichever
 * sentence the title took, and prints what the lead left behind — see
 * [TurnBrief].
 *
 * EMPTY RATHER THAN A PLACEHOLDER, because the two readers of this want
 * different things from the unnamed case. A header needs a word in the slot,
 * and [turnTitle] supplies it. The BREADCRUMB needs to know there is no name,
 * so that it can go on showing the id: "Turn" in the trail names no particular
 * turn, and two of them in the palette's recents are two identical rows
 * pointing at different places — strictly worse than the two uuids they
 * replaced, which at least told them apart.
 */
export function turnName(view: TurnView): string {
  return lead(str(view.rec.learning, "plan_summary") || wokeBy(view));
}

/**
 * What to call one turn in a header, which always needs a word in the slot.
 *
 * "Turn" over a turn is the eyebrow twice, and that is what makes it a last
 * resort rather than a fallback anything should reach for: it is what a turn
 * nothing has said a word about yet is headed with, and nothing else.
 */
export function turnTitle(view: TurnView): string {
  return turnName(view) || "Turn";
}

/**
 * The six facts a turn wears, in the one order.
 *
 * ONE BUILDER FOR THE PAGE AND THE RAIL, which is `ObjectHeader`'s own rule
 * and the reason it exists: a reader who scans "seat, outcome, took" in the
 * rail must find them in that order on the page behind it.
 *
 * THE THREE COUNTS ARE ABSENT RATHER THAN ZERO until a phase record is in
 * hand. `FactLine` drops an empty value, and "0 phases · 0 rounds · 0 tokens"
 * is a claim that this turn did nothing — which is exactly what a turn still
 * loading, or one whose events fell out of the store's window, has NOT been
 * shown to have done.
 *
 * THREE OF THEM CARRY A NOTE, and they are the three a reader can reasonably
 * doubt: whether a duration was measured or derived, what a token figure
 * covers, and whose word an outcome is. The page used to answer that in a
 * four-tile strip directly under this line, which restated `Seat`, `Took`,
 * `Tokens` and `Outcome` at three times the size and — because a tile is
 * narrower than the line above it — ellipsed the seat name the line rendered
 * whole. Its fourth caption was filler ("ran this turn", under a seat's own
 * name); the other three are here, which is also how they reach the RAIL,
 * where no tile ever rendered and a reader had nothing at all.
 */
export function turnFacts(view: TurnView): Fact[] {
  const counted = view.own.length > 0;
  const { from, to } = view.span;
  const spanned = view.durationMs == null && to > from;
  return [
    {
      label: "Seat",
      // The chip is its own link, so the fact carries no `path`: an anchor
      // inside the fact's own anchor is markup no browser agrees about.
      value: view.role ? <SeatChip name={view.role} handle={view.role} /> : "the engine",
    },
    {
      // A RUNNING TURN HAS NO OUTCOME, and `outcomeOf` says so with an empty
      // word. A fact line has no tile to fill: the fact is dropped and the
      // status beside the title says "running" — and the note goes with it,
      // since a caption under nothing is a caption about nothing.
      label: "Outcome",
      value: view.outcome.word,
      // WHOSE WORD THIS IS. `done` is the reviewer's verdict and `delivered`
      // is the executor's own, and the badge renders one word for both — so
      // without this a reader cannot tell a turn the reviewer passed from one
      // that merely reported itself finished. On a failure it is the engine's
      // `error_kind`, which is the difference between a turn that was stopped
      // and one that decided against itself.
      note: view.outcome.word ? view.outcome.sub || undefined : undefined,
    },
    {
      label: "Took",
      // THE ENGINE'S OWN MEASUREMENT where a record carries one, and the
      // window over everything this frame holds otherwise.
      value:
        view.durationMs != null
          ? fmtDuration(view.durationMs)
          : spanned
            ? fmtDuration(to - from)
            : "",
      // WHICH OF THE TWO THIS NUMBER IS. Only on the derived branch: a fact
      // that says "measured" under every duration teaches a reader to stop
      // reading the line, and then the one time it says something else they
      // miss it. A CUT VIEW HOLDS BOTH ENDS, so the span is the turn's real
      // window — but it is still the window rather than the engine's own
      // milliseconds, and on that branch the record carrying them is missing
      // from a turn this page has both ends of.
      note: spanned
        ? view.cut
          ? "spanning the turn's ends — its own record is not among them"
          : "spanning the turn's first and last event"
        : undefined,
    },
    { label: "Phases", value: counted ? view.own.length : "" },
    { label: "Iterations", value: counted ? view.iterations : "" },
    {
      label: "Tokens",
      value: counted ? fmtCount(view.tokens) : "",
      // THE TURN'S OWN PHASES, and the note is what says so. A worker's
      // tokens are already charged through the shared meter, which is why the
      // engine keeps them out of `total_tokens` and reports them as
      // `subagent_tokens` — so a figure summing every record disagreed with
      // the very record shown further down this page. The split is also the
      // only thing that answers "how much of this turn was fan-out" when a
      // seat's spend jumps and its own rounds did not.
      note:
        counted && view.workerTokens > 0
          ? `+${fmtCount(view.workerTokens)} in ${view.workerCount} worker${
              view.workerCount === 1 ? "" : "s"
            }`
          : undefined,
    },
  ];
}

/**
 * What state this turn is in, beside its own title.
 *
 * Everything in the fact line is settled when the turn ends — an outcome, a
 * duration, a bill — so a turn still in flight reads as a turn that recorded
 * none of them. These are the marks that answer the rest, and they carry the
 * only tones in this header, because each of them is a STATE where a seat, an
 * id and a token count are identity.
 *
 * THEY WERE IN THE PAGE BAR, portalled in beside Copy turn and Download turn,
 * and that is the mistake this replaces. The bar's own subject is "where you
 * are, and what you can do about it": five state chips in its action slot are
 * neither, they pushed a turn page's bar to ten items so it broke onto a
 * second line at 1587px, and the reader's eye had to travel to the far right
 * corner and back for a fact about the object named 40px below. `ObjectHeader`
 * has carried a `status` slot for exactly this all along — "a status glyph or
 * pill — state, never identity" — and one of these five was already in it.
 *
 * TWO OF THEM DID NOT MOVE, THEY WENT: the seat and the phase count were
 * `Agent CEO` and `7 phases` in the bar directly above `SEAT Agent CEO` and
 * `PHASES 7` in the fact line. A chip repeating the fact under it is not a
 * second reading of the turn, it is the same reading twice.
 *
 * ON THE VIEW, so the peek gets them too. A rail opened from a turns row
 * showed no problem badge at all — the one mark a reader opening a rail over
 * a failed turn is looking for — because the count lived on the page.
 */
function turnStatus(view: TurnView): ReactNode {
  const { attempt } = view;
  return (
    <>
      {view.running && (
        <Tag variant="info" dot>
          running
        </Tag>
      )}
      {/* A RE-RUN SAYS SO, and says where the others are. Neutral, because
          being a second attempt is a fact about the trigger rather than a
          fault — the attempt that FAILED carries the problem badge beside
          this one, which is the pairing a reader needs to see at once. */}
      {attempt && (
        <Tag
          appearance="outline"
          title={
            `attempt ${attempt.index} of ${attempt.total} at this trigger — a turn ` +
            `that fails without reaching outside the engine is redelivered and runs again`
          }
        >
          attempt {attempt.index}/{attempt.total}
        </Tag>
      )}
      {/* FROM WHAT ACTUALLY WENT WRONG, not from the phase records alone.
          `phases.some(p => p.failed)` misses every turn the engine killed
          BETWEEN phases — a refused charge, an exhausted chain, a guard that
          fired — which are precisely the turns with no failed phase record to
          find. */}
      {view.trouble > 0 && (
        <Tag variant="danger" leadingIcon={<ErrorGlyph size="xs" />}>
          {view.trouble === 1 ? "1 problem" : `${view.trouble} problems`}
        </Tag>
      )}
      {view.clean && (
        <Tag
          variant="success"
          leadingIcon={<CheckGlyph size="xs" />}
          title="no guard fired, no provider fell through, no call was refused"
        >
          nothing went wrong
        </Tag>
      )}
      {/* WHAT THE VIEW IS MISSING, in the header, because every other badge
          beside it is a claim made from these rows. The `trace` answer has
          carried this flag all along and its screen renders it; `turn` did not
          carry one at all, so a cut turn looked exactly like a short one. */}
      {view.cut && (
        <Tag
          variant="warning"
          leadingIcon={<WarningGlyph size="xs" />}
          title="the store stopped at its per-turn cap; this view holds the turn's opening and its ending, and not the middle"
        >
          middle not shown
        </Tag>
      )}
    </>
  );
}

/**
 * What woke this turn.
 *
 * It was not on this screen at all. The trigger rides on EVERY phase event and
 * on the summary, and the feed's own turn card leads with it — here the only
 * way to learn it was to read the raw `trigger` object out of the JSON dump at
 * the bottom of the page.
 *
 * WHAT IT SET OUT TO DO IS THE TITLE'S LEAD, AND THE REST IS HERE.
 * `plan_summary` used to be the second half of this panel, under "It set out
 * to"; it is the nearest thing a turn has to a name, so it heads the page, the
 * rail and the row in the turns list, and repeating it whole directly under
 * that header was one sentence twice in the space of two lines. That is still
 * true of the sentence the title TOOK — and not of the ones it did not, which
 * had nowhere else on this screen to be. So the summary comes back the moment
 * `lead` left something behind, and stays away when it did not.
 *
 * THE TRIGGER FOLLOWS THE SAME RULE, and that is a change from what this
 * comment used to claim. It said the prose here is "dropped rather than
 * printed again" where the title took this sentence, which was true while the
 * title was the WHOLE string: `woke === omit` matched exactly, and the
 * paragraph went. Now that the title is a LEAD, that comparison only matches a
 * trigger which is one sentence long — so a multi-sentence trigger prints its
 * prose here, with the title's own sentence at the front of it.
 *
 * That is the answer, not an oversight, and it is the same one the summary
 * gets four lines up: the panel prints the whole prose under its own label,
 * and the title is an entry point into it rather than a replacement for it.
 * Dropping the paragraph instead would lose everything the lead did not take,
 * and on a turn with no plan summary the trigger's prose is the only account
 * of it this screen has. What is still dropped is the case the original rule
 * was written for — a trigger the title says ALL of — where printing it would
 * be one sentence twice in the space of two lines and nothing more.
 */
function TurnBrief({ view, omit }: { view: TurnView; omit?: string }) {
  const trigger = view.trigger;
  const woke = wokeBy(view);
  const triggerId = typeof trigger?.id === "string" ? trigger.id : "";
  // Only when the title says ALL of it. See the note above for why a title
  // that says only its lead does not count as having said it.
  const repeated = woke !== "" && woke === omit;
  // What the title could not take. Compared against the WHOLE summary rather
  // than recomputing the lead, so the two can never disagree about where the
  // sentence ended.
  const summary = str(view.rec.learning, "plan_summary");
  const rest = summary !== "" && summary !== omit ? summary : "";
  if (!woke && !rest) return null;
  if (!rest && repeated && !trigger?.integration && !triggerId) return null;
  return (
    <Card>
      <div className="col gap-1">
        <div className="row gap-2">
          <span className="t-label">Woken by</span>
          {/* The SOURCE, and not the sender. The trigger's summary is built
              by the vendor's own summariser and already opens with who wrote
              it — "Message from founder: …" — so a `founder` chip beside that
              sentence was the same fact twice, and two chips plus a link
              after one line of prose is a hedge, not a header. The
              integration is the one thing the sentence does not reliably
              carry. */}
          {trigger?.integration && (
            <Tag appearance="outline" monospace title="where this turn's trigger came from">
              {trigger.integration}
            </Tag>
          )}
          <span className="spacer" />
          {triggerId && (
            <a className="t-link" href={href(["activity", "events", triggerId])}>
              the trigger →
            </a>
          )}
        </div>
        {/* LABEL ABOVE, PROSE BELOW, full width. As a KeyValue this was a
            two-track grid sized to the longest label, so one short line of
            prose sat in a narrow band with the rest of the panel empty
            beside it — the shape for a metadata list, and these are
            sentences. */}
        {!repeated && woke !== "" && <p className="t-body measure">{woke}</p>}
        {rest !== "" && (
          <>
            <span className="t-label">It set out to</span>
            <p className="t-body measure">{rest}</p>
          </>
        )}
      </div>
    </Card>
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
    <Card padding="sm">
      <Card.Header
        icon={<Book2Glyph size="sm" />}
        subtitle="the context blocks its prompt was assembled from, and what each phase's prompt weighed"
      >
        <Card.Title>What the turn was given</Card.Title>
      </Card.Header>
      <div className="col gap-2">
        {blocks.length === 0 && (
          <span className="t-caption">
            No prefetch record for this turn, so there is no breakdown of where the prompt&rsquo;s
            context came from — only what each phase&rsquo;s prompt came to.
          </span>
        )}
        {blocks.length > 0 && got.length > 0 ? (
          <div className="num-block">
            {/* ROWS WITH THE FIGURE AT THE FAR END, not a KeyValue. The
                grid's second track starts at 120px, so a byte count sat
                stranded mid-panel with the whole right half empty — and a
                bare "134 B" beside a label says nothing about what was
                measured. The heading says it once, and the rows carry the
                numbers where numbers go.

                The far end of the LEDGER, which is not the card's: see
                `.num-block` for why a figure column that tracks a 1500px
                panel is a figure nobody reads. The size takes `.num-col`
                rather than trailing the spacer bare, so the heading and the
                value under it are one column of the same width — which is
                what the block is then sized to. */}
            <div className="col gap-1 num-rows">
              <div className="row gap-2">
                <span className="t-label spacer">Reached the prompt</span>
                <span className="t-label num-col">Rendered size</span>
              </div>
              {got.map((b) => (
                <div key={b.label} className="row gap-2">
                  <span className="t-cell truncate">{b.label}</span>
                  {b.note && <span className="t-caption truncate">{b.note}</span>}
                  <span className="spacer" />
                  <span className="mono t-num t-caption num-col">{fmtBytes(b.bytes)}</span>
                </div>
              ))}
            </div>
          </div>
        ) : (
          blocks.length > 0 && (
            <span className="t-caption measure">
              The prompt was built from the seat&rsquo;s own identity and this turn&rsquo;s trigger
              alone — no stored context reached it.
            </span>
          )
        )}
        {gated.length > 0 && (
          <Callout variant="neutral">
            Not searched: {list(gated.map((b) => b.label))}. The trigger was a bare pointer, so
            these filters were skipped — the executor searches later with{" "}
            <code className="inline">search_knowledge</code>, once it knows what the task needs.
          </Callout>
        )}
        {empty.length > 0 && (
          <span className="t-caption measure">
            Nothing to add from {list(empty.map((b) => b.label))}.
          </span>
        )}
        {weights.length > 0 && <PromptWeights rows={weights} />}
      </div>
    </Card>
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
 *
 * AND ONE ROW PER PHASE KEY, which is the same rule one step further:
 * [promptWeights] collapses a phase key measured more than once — a turn
 * re-delivered and re-run under its work key — into the run that stands, and
 * the row says how many there were. A turn that ran five times drew ten
 * byte-identical rows before that, which is the flat-list failure this whole
 * screen was rebuilt to stop repeating.
 */
function PromptWeights({ rows }: { rows: PromptWeight[] }) {
  return (
    <div className="num-block ledger-follows">
      <div className="col gap-1 num-rows">
        <div className="row gap-2">
          <span className="t-label spacer">Prompt sent</span>
          <span className="t-label num-col">System</span>
          <span className="t-label num-col">User</span>
          <span className="t-label num-col">Approx. tokens</span>
        </div>
        {rows.map((w) => (
          <div key={`${w.phase}|${w.iteration}`} className="row gap-2">
            <PhaseTag phase={w.phase} />
            {w.iteration > 1 && (
              <span className="t-caption" title="self-iterate round">
                iter {w.iteration}
              </span>
            )}
            {/* THE COUNT IS THE NEWS. Everything else on this row is the last
                run's, so without it five identical rows said one thing five
                times and a reader had no way to tell a re-run turn from a
                repeating panel. */}
            {w.runs > 1 && (
              <span className="count-chip" title={runsTitle(w)}>
                &times;{w.runs}
              </span>
            )}
            <span className="spacer" />
            <span className="mono t-num t-caption num-col" title="bytes in the system prompt">
              {fmtBytes(w.systemBytes)}
            </span>
            <span className="mono t-num t-caption num-col" title="bytes in the user message">
              {fmtBytes(w.userBytes)}
            </span>
            <span className="mono t-num t-caption num-col" title="the engine's own approximation">
              {fmtCount(w.approximateTokens)}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}

/**
 * What a run count means, said in full where the chip cannot.
 *
 * It names the RANGE rather than only the count when the runs disagreed,
 * because that is the one thing the rows this collapses still had to say: the
 * figures on the row are the last run's, and "they were all the same" and
 * "the first four were bigger" are different facts about the same turn.
 */
function runsTitle(w: PromptWeight): string {
  const ran = `this phase ran ${w.runs} times in this turn`;
  if (w.minTokens === w.maxTokens) return `${ran} — every run measured the same`;
  return (
    `${ran} — the last is shown; they ranged ` +
    `~${fmtCount(w.minTokens)}–${fmtCount(w.maxTokens)} tokens`
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
function TurnEventRow({ run, actor }: { run: Run; actor: string }) {
  const { event, count, last } = run;
  // THE SPAN, WHERE THERE IS ONE. A run's clock is two instants and a single
  // row's is one, and the two must not render alike: "15:42:15" over a row
  // standing for eight attempts would date the first and silently claim the
  // rest happened then too. Identical clocks collapse back to one, because
  // eight attempts inside a second are not a span a reader can act on.
  const from = fmtTime(event.timestamp);
  const to = fmtTime(last.timestamp);
  const span = count > 1 && to !== from ? `${from}–${to}` : from;
  // `isFailed`, NOT `event.failed`. The band this row is in was chosen by
  // `bandOf`, which asks `isFailed` — and that reads the payload's own flag as
  // well as the promoted column, because a LIVE event carries the first and
  // only history carries the second (see `lib/turnstory.ts`). Read one of the
  // two here and a failure the panel filed under "What went wrong" rendered
  // with no red edge and no glyph, which is the same turn drawn two ways on
  // one screen. `collapseRuns` keys on `isFailed` too, so this is also what
  // stops two rows it deliberately kept apart from drawing identically.
  const failed = isFailed(event);
  return (
    <a
      className={cx("turn-row", failed && "failed")}
      href={href(["activity", "events", event.id])}
      title={
        count > 1
          ? `${count} identical events, ${fmtDateTime(event.timestamp)} to ${fmtDateTime(
              last.timestamp,
            )} — this opens the first`
          : fmtDateTime(event.timestamp)
      }
    >
      <time className="feed-time" dateTime={event.timestamp}>
        {span}
      </time>
      <span className="what truncate">
        {failed && (
          <ErrorGlyph
            size="xs"
            style={{ display: "inline", color: "var(--critical-ink)", marginRight: 4 }}
          />
        )}
        {withoutActor(event.summary, actor) || event.type}
      </span>
      <span className="feed-tail">
        {/* THE COUNT BEFORE THE TYPE, because it qualifies the SENTENCE to
            its left rather than the wire name to its right. Absent at one:
            "×1" on every ordinary row is a column that says nothing on all
            but a handful of them. */}
        {count > 1 && (
          <span className="t-num run-count" title={`${count} of these, one after another`}>
            ×{count}
          </span>
        )}
        <span className="muted mono truncate">{event.type}</span>
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

/**
 * One band of a turn's rows.
 *
 * COLLAPSED HERE rather than in the panel that noticed the problem, so all
 * four bands answer the same way: a repeat is a repeat wherever it lands, and
 * a rule applied to one list is a rule the next list silently does not have.
 */
function EventList({ events, actor }: { events: EventRecord[]; actor: string }) {
  const runs = useMemo(() => collapseRuns(events), [events]);
  return (
    <div className="list">
      {runs.map((run) => (
        <TurnEventRow key={run.event.id} run={run} actor={actor} />
      ))}
    </div>
  );
}

export function TurnScreen({ turnId }: { turnId: string }) {
  const nav = useNavigator();
  // ONE DERIVATION FOR THE PAGE AND THE RAIL — see [useTurnView]. What stays
  // here is what only a page has room for: the story bands, the prompt
  // weights, every trace the turn touched, and the JSON somebody attaches to
  // a bug report.
  const view = useTurnView(turnId);
  const { loading, error, events, cut, attempt, phases, own, nested, rec, role } = view;
  const { running, durationMs, story } = view;

  const prefetch = useMemo(
    () => prefetchBlocks(story.given.find((e) => e.type === "prefetch_summary")),
    [story],
  );
  // The other half of the `given` band, and until now the half nothing read.
  const weights = useMemo(() => promptWeights(story.given), [story]);

  // THE BREADCRUMB, THE BROWSER TAB AND THE PALETTE'S RECENTS, which all read
  // the one label a screen publishes and otherwise fall back to the raw path
  // segment — for a turn, its uuid. The page has had a name for this object
  // all along and simply never said so, which is why a reader's recents read
  // as three identical-looking hex strings.
  //
  // THE SCREEN'S OWN TITLE, never re-derived: `Recent.label` is documented as
  // the label the screen showed, so what the header renders and what the
  // palette offers are the same string by construction. The CSS bounds it —
  // `.crumb-here` ellipsises and the palette row clamps — so a long lead
  // sentence is cut where it is drawn rather than cut here, once, in a length
  // nobody could defend.
  const name = turnName(view);
  usePageLabels(name ? { [turnId]: name } : {});

  const title = turnTitle(view);

  const traceIds = view.traceIds;
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
      {/* WHAT THIS PAGE CAN DO — and nothing about what the turn IS. Five
          state chips used to open this slot (the seat, the phase count, the
          attempt, the problem count, "nothing went wrong"), which took the
          bar to ten items and broke it onto a second line on a laptop. They
          are the object header's `status` now; see [turnStatus]. */}
      <PageActions>
        {
          <>
            {role && (
              <Button
                size="small"
                variant="secondary"
                leadingIcon={<PersonGlyph size="xs" />}
                onClick={() => nav.to(["company", "people", role])}
              >
                The seat
              </Button>
            )}
            {/* THE OTHER ATTEMPTS, reachable rather than merely announced.
                The header's badge says this is attempt 2 of 2; a reader who
                has landed on the failed one needs to get to the one that worked,
                and a deep link out of a tracker comment or an event payload
                is exactly how they landed here. Each button says whether that
                attempt failed, so the pair reads as the story it is. */}
            {attempt?.rows.map((row, i) =>
              row.turn_id === turnId ? null : (
                <Button
                  key={row.turn_id}
                  size="small"
                  variant="secondary"
                  leadingIcon={<LayersGlyph size="xs" />}
                  onClick={() => nav.to(["activity", "turns", row.turn_id])}
                  title={
                    `attempt ${i + 1} of ${attempt.total} at the same trigger` +
                    (row.failed ? ", which carried a failure" : "")
                  }
                >
                  Attempt {i + 1}
                  {row.failed ? " (failed)" : ""}
                </Button>
              ),
            )}
            {traceIds.length > 1 ? (
              // NAMED, not collapsed. Two traces mean the turn was resumed
              // somewhere else, and which one a reader wants depends on which
              // half they are chasing.
              <span className="row gap-1">
                {traceIds.map((id, i) => (
                  <Button
                    key={id}
                    size="small"
                    variant="secondary"
                    leadingIcon={<ForkRightGlyph size="xs" />}
                    onClick={() => nav.to(["activity", "traces", id])}
                    title={`trace ${id}`}
                  >
                    Trace {i + 1} of {traceIds.length}
                  </Button>
                ))}
              </span>
            ) : (
              traceId && (
                <Button
                  size="small"
                  variant="secondary"
                  leadingIcon={<ForkRightGlyph size="xs" />}
                  onClick={() => nav.to(["activity", "traces", traceId])}
                >
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
      </PageActions>
      {/* THE OBJECT'S OWN HEADER, and the turn id with it. The id used to be
          a lone `PageNote` under the page bar — a hand-rolled identity line,
          which is exactly the eyebrow `ObjectHeader` draws — and the facts
          beside it come out of the same builder the rail uses, so the six
          things a reader scans are in one order wherever a turn appears. */}
      <ObjectHeader
        kind="Turn"
        icon="layers"
        identifier={turnId}
        title={title}
        status={turnStatus(view)}
        facts={turnFacts(view)}
      />
      {/* THE SKELETON STANDS WHERE THE BODY WILL BE, under a header that is
          drawn from the id and needs no answer to exist. Above it, it was six
          rows of grey pushing the header down the page and then letting it
          spring back the moment the query landed — so opening a turn moved
          the one part of the screen that had been readable all along. The
          screen is keyed on the turn id (`app/App.tsx`), so this runs on
          every navigation between turns, not only on a cold open. */}
      {loading && <Skeleton variant="text" rows={6} label="Loading the turn" />}
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
          <Callout variant="warning" icon={<WarningGlyph size="md" />}>
            This turn published more than the store returns for one turn. What is here is its{" "}
            <strong>opening and its ending</strong> — {events.length} events, so the records below
            are the turn&rsquo;s own — and what is missing is the middle. Phases from the middle of
            a long self-iterating turn are not on this page, and neither is anything that went wrong
            in them. {traceId ? "The trace carries the same work from the trigger down." : ""}
          </Callout>
        )}
        {view.phasesDropped && <PhasesDroppedNote reread={view.reread} />}

        <TurnBrief view={view} omit={title} />

        {/* ABOVE the phases, because a turn that fell over is not something a
            reader should have to scroll past three panels to discover. Absent
            entirely on a healthy turn, which is the state the flat list could
            never reach. */}
        {story.wentWrong.length > 0 && (
          <Card padding="none">
            <Card.Header
              icon={<ErrorGlyph size="sm" />}
              count={story.wentWrong.length}
              subtitle="guard breaches, exhausted chains, refused calls — the reason to open this page"
            >
              <Card.Title>What went wrong</Card.Title>
            </Card.Header>
            <EventList events={story.wentWrong} actor={role} />
          </Card>
        )}

        {(prefetch.length > 0 || weights.length > 0) && (
          <Given blocks={prefetch} weights={weights} />
        )}

        <Card padding="sm">
          <Card.Header icon={<NeurologyGlyph size="sm" />} count={own.length}>
            <Card.Title>Phases</Card.Title>
          </Card.Header>
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
        </Card>

        {story.did.length > 0 && (
          <Card padding="none">
            <Card.Header
              icon={<BoltGlyph size="sm" />}
              count={story.did.length}
              subtitle="work outside the tool loop: coding runs, delegations, colleagues"
            >
              <Card.Title>What else it did</Card.Title>
            </Card.Header>
            <EventList events={story.did} actor={role} />
          </Card>
        )}

        {story.leftBehind.length > 0 && (
          <Card padding="none">
            <Card.Header
              icon={<DatabaseGlyph size="sm" />}
              count={story.leftBehind.length}
              // "What it left behind" read as work abandoned rather than as
              // memory written. This is the reflection pass — it runs AFTER the
              // last phase, on auxiliary workers of its own, and everything in
              // it is something the seat now knows that it did not before.
              subtitle="the reflection pass, once the phases were done"
            >
              <Card.Title>What the seat learned</Card.Title>
            </Card.Header>
            <EventList events={story.leftBehind} actor={role} />
          </Card>
        )}

        {story.rest.length > 0 && (
          <Card padding="none">
            <Card.Header
              icon={<TimelineGlyph size="sm" />}
              count={story.rest.length}
              subtitle="rows this build has no particular place for"
            >
              <Card.Title>Also published</Card.Title>
            </Card.Header>
            {/* THE SCREEN'S OWN ROW, like the three bands above it. This band
                alone rendered the activity feed's row with the date forced
                on, which is the exact shape `.turn-row` exists to replace: a
                FOUR-column grid (time, actor, summary, tail) taking three
                children, so the summary landed in the 132px actor track and a
                full date wrapped to three lines inside the 62px time one.
                Every row was three lines tall, in a different grid and a
                different height from the panels above it, naming the same
                seat on each. The `as unknown as FeedRow` cast was the tell —
                an `EventRecord` is not a `FeedRow`. Nothing is lost: the row
                carries the full instant in its title, and the header dates
                the turn. */}
            <EventList events={story.rest} actor={role} />
          </Card>
        )}

        {(rec.summary || rec.learning) && (
          <Card padding="sm">
            <Card.Header
              icon={<DescriptionGlyph size="sm" />}
              subtitle="the two events the engine closes every turn with"
            >
              <Card.Title>The turn&rsquo;s own record</Card.Title>
            </Card.Header>
            <div className="col gap-2">
              {conversation && (
                <PropertiesRail
                  groups={[
                    {
                      properties: [
                        {
                          // LABELLED, and explained. It is "{source}:{channel}:
                          // {thread}" — which external thread this turn was
                          // answering — and it used to be an unexplained
                          // truncated string under the seat's name.
                          label: "Conversation",
                          value: (
                            <span className="row gap-2 baseline">
                              <code className="inline">{conversation}</code>
                              <span className="t-caption">
                                the external thread this turn served
                              </span>
                            </span>
                          ),
                        },
                      ],
                    },
                  ]}
                />
              )}
              {/* One expander per record, EACH with its own copy button. A
                  single control in the panel head copied one of the two
                  without saying which. */}
              {rec.summary && (
                <Disclosure
                  title="agent_turn_completed — the dashboard's summary"
                  actions={
                    <CopyButton text={summaryJSON} variant="ghost" title="copy this record" />
                  }
                >
                  <CodeBlock
                    plain
                    copyable={false}
                    maxHeight={RECORD_MAX_HEIGHT}
                    selectable
                    label="agent_turn_completed, as JSON"
                    code={summaryJSON}
                  />
                </Disclosure>
              )}
              {rec.learning && (
                <Disclosure
                  title="turn_completed — the learning subsystem's record"
                  actions={
                    <CopyButton text={learningJSON} variant="ghost" title="copy this record" />
                  }
                >
                  <CodeBlock
                    plain
                    copyable={false}
                    maxHeight={RECORD_MAX_HEIGHT}
                    selectable
                    label="turn_completed, as JSON"
                    code={learningJSON}
                  />
                </Disclosure>
              )}
            </div>
          </Card>
        )}
      </QueryState>
    </>
  );
}

// ---------------------------------------------------------------------------
// The peek
// ---------------------------------------------------------------------------

/**
 * Which phases ran, one line each.
 *
 * NOT `PhaseCard`. That is the page's, and it is a transcript — a model's
 * reasoning, its arguments and its results — which in a rail 420 px wide is a
 * page in a narrow column, and the reason the rail exists is that the list
 * behind it stays on screen. What a reader wants HERE is the shape of the
 * turn: which phases it ran, what each decided, and what each cost. The
 * reading happens through `Open ↗`.
 *
 * THE WORKERS ARE NOT IN IT, for the reason the Tokens tile gives: a delegate
 * fan-out of eight would be eight lines of somebody else's phases under a
 * heading that counts two.
 */
function PhaseStrip({ phases }: { phases: PhaseRecord[] }) {
  return (
    <Card padding="sm">
      <Card.Header icon={<NeurologyGlyph size="sm" />} count={phases.length}>
        <Card.Title>Phases</Card.Title>
      </Card.Header>
      <div className="col gap-2">
        {phases.map((p) => {
          const ms = phaseDuration(p);
          const decided = decisionLabel(p.phase, p.decision);
          return (
            <div key={p.key} className="col gap-1">
              <div className="row gap-2">
                <PhaseTag phase={p.phase} />
                {p.iteration > 1 && (
                  <span className="t-caption" title="self-iterate round">
                    iter {p.iteration}
                  </span>
                )}
                {p.live && (
                  <Tag variant="info" dot>
                    running
                  </Tag>
                )}
                {p.failed && <Tag variant="danger">{p.errorKind || "failed"}</Tag>}
                <span className="spacer" />
                <span className="phase-meta" title="tokens this phase spent">
                  {fmtCount(p.totalTokens)}
                </span>
                {/* WHAT THE ENGINE MEASURED, and nothing where it measured
                    nothing: a live phase has no duration yet, and rendering
                    its zero would make the phase still running look like the
                    cheapest one in the turn. */}
                {ms != null && (
                  <span className="phase-meta" title="what the engine measured">
                    {fmtDuration(ms)}
                  </span>
                )}
              </div>
              {/* WHAT IT DECIDED, under the row rather than beside it. A
                  decision is a sentence — "nothing to do — ended silently" —
                  and a sentence in the metadata cluster of a 420 px rail is
                  the thing that wraps. */}
              {decided && <span className="t-caption truncate">{decided}</span>}
            </div>
          );
        })}
        {phases.length === 0 && (
          <span className="t-caption">
            No phase completed in this turn — it may have died before its first phase published.
          </span>
        )}
      </div>
    </Card>
  );
}

/**
 * One turn, beside the list it was found in.
 *
 * WHAT A ROW CANNOT SAY. A turns row carries a seat, a sentence and four
 * numbers; the question a reader has in front of a list of them is "which turn
 * is this, and what did it do" — which is the phases it ran and the thing that
 * woke it, and neither fits in a column.
 *
 * IT ASKS THE SAME QUESTION THE PAGE ASKS, through [useTurnView], rather than
 * rendering the row it was opened from: a peek is opened from a pasted URL as
 * often as from a grid, and a peek built out of a row would show a different
 * set of facts depending on which list it came from.
 */
export function TurnPeek({ turnId }: { turnId: string }) {
  const view = useTurnView(turnId);
  // NOTHING HAS BEEN READ, which is three states and only one of them is an
  // empty rail: an answer that has not landed is a skeleton, a refusal is the
  // engine's own words, and a turn no event names is said plainly. A header
  // over six absent facts would be none of the three.
  const nothing = view.events.length === 0 && view.phases.length === 0;
  if (nothing) {
    if (view.loading) return <Skeleton variant="text" rows={6} label="Loading the turn" />;
    if (view.error) return <QueryState error={view.error} loading={false} />;
    return (
      <EmptyState
        size="compact"
        icon={<LayersGlyph size={32} />}
        title="No events carry this turn id"
        description="A turn is assembled from the events that name its id. If it ran outside the store's 30-day window there is nothing to assemble."
      />
    );
  }

  const title = turnTitle(view);
  return (
    <>
      <ObjectHeader
        size="peek"
        kind="Turn"
        icon="layers"
        identifier={turnId}
        title={title}
        status={turnStatus(view)}
        facts={turnFacts(view)}
      />
      <div className="col gap-3">
        {/* LOADING IS SETTLED ABOVE. The rail draws only once it holds
            something — a query answer, or a phase off the stream — so passing
            the in-flight flag here would blank a running turn's rail every
            time the query it does not need re-ran on a reconnect. */}
        <QueryState error={view.error} loading={false}>
          {/* THE SAME WARNING THE PAGE CARRIES, because every count in the
              header above is made from these rows: a cut turn holds its
              opening and its ending and not its middle, and a rail that said
              nothing would report a long turn as a short one. */}
          {view.cut && (
            <Callout variant="warning" icon={<WarningGlyph size="md" />}>
              The store stopped at its per-turn cap. This is the turn&rsquo;s opening and its ending
              — the phases from its middle are not here, and neither is anything that went wrong in
              them.
            </Callout>
          )}
          {view.phasesDropped && <PhasesDroppedNote reread={view.reread} />}
          <PhaseStrip phases={view.own} />
          <TurnBrief view={view} omit={title} />
        </QueryState>
      </div>
    </>
  );
}
