/**
 * What a turn published BESIDE its phases, sorted into the questions a reader
 * actually brings to it.
 *
 * The Turn screen used to render all of it as one flat list under "Everything
 * else this turn published". Three things were wrong with that, and only the
 * first is obvious:
 *
 *  1. **Most of it was not "else".** On a three-iteration turn, six of the
 *     twelve rows were `agent_phase_started` — one per phase card directly
 *     above, saying "started execute (iter 2)" beside the card that already
 *     says EXECUTE, iter 2, its model, its rounds, its tokens and its outcome.
 *     A seventh was `agent_turn_completed`, which the same screen also renders
 *     as the stat strip AND as a JSON dump.
 *
 *  2. **Everything left had the same weight.** `reflection_completed` is a
 *     sentinel whose own payload doc says it carries no per-worker outcome
 *     because its job is to flip a seat back to idle. A guard breach is a turn
 *     the engine stopped. Rendered as two identical rows, an operator scanning
 *     for the second reads past it.
 *
 *  3. **A healthy turn could not say so.** The panel was never empty, so
 *     "nothing went wrong here" was not a state the screen could reach — and
 *     a section that is always full is a section nobody reads.
 *
 * # The engine files the rows; this file only collects them
 *
 * Which band a row is in is the ENGINE'S answer: the `turn` answer tags every
 * row with it (`types.BandOf`, beside the event catalogue in
 * `internal/events/types/turnband.go`), and answers the absorbed inventory and
 * the problem count beside them. It used to be decided here, over sets this
 * dashboard declared and a Go gate read back out of TypeScript — until a
 * member's account of the same turn had to be built from the same rows on the
 * server, which is the only place that can withhold what a member may not
 * read. One rule in two languages is two accounts of one turn the day they
 * disagree, so the rule moved to the side that owns the rows.
 *
 * What is left here is presentation over the answer: collecting the bands the
 * screen draws, collapsing a run of identical rows, and reading the payloads
 * of the two kinds of row whose numbers the screen tabulates.
 */

import type { EventRecord, TurnEventRow } from "~/protocol/index.ts";

/**
 * Whether a row is a failure, read the way the server writes it.
 *
 * `failed` reaches the client two ways — the payload's own field while the
 * event is live, and the `failed` TAG the event-store writer stamps, which is
 * all that survives into history. Reading only one of them makes the same turn
 * red on one surface and not on another, which is the exact reason
 * `events.FailureEventTypes` is declared once in Go. The BAND a row is in
 * already counted its failure on the server; this is what draws the red edge
 * on the row itself.
 */
export function isFailed(event: EventRecord): boolean {
  if (event.failed) return true;
  const payload = event.payload as Record<string, unknown> | undefined;
  return payload?.failed === true;
}

/**
 * A turn's listed rows, collected by the band the engine filed each in.
 *
 * NO `absorbed` HERE: an absorbed row is not listed, and the screen prints
 * where each kind went from the answer's own `absorbed` inventory, which the
 * engine counted over the same rows.
 */
export interface Story {
  wentWrong: TurnEventRow[];
  given: TurnEventRow[];
  did: TurnEventRow[];
  leftBehind: TurnEventRow[];
  /**
   * Everything the engine has no opinion about — a type a newer build
   * publishes among them — AND any band this build does not know. The
   * registry is additive-only, so a row must still render however new it is.
   */
  rest: TurnEventRow[];
}

/**
 * Collect one turn's rows by the band the engine gave each.
 *
 * A PURE FUNCTION OVER THE ANSWER: it decides nothing a second time, so it
 * cannot disagree with the inventory or the problem count the same answer
 * carries. An unknown band — a newer engine's — falls through to `rest`
 * rather than vanishing.
 */
export function tellStory(events: readonly TurnEventRow[]): Story {
  const story: Story = { wentWrong: [], given: [], did: [], leftBehind: [], rest: [] };
  for (const event of events) {
    switch (event.band) {
      case "absorbed":
        break;
      case "went_wrong":
        story.wentWrong.push(event);
        break;
      case "given":
        story.given.push(event);
        break;
      case "did":
        story.did.push(event);
        break;
      case "left_behind":
        story.leftBehind.push(event);
        break;
      default:
        story.rest.push(event);
    }
  }
  return story;
}

/** One phase's final prompt, as the engine measured it. */
export interface PromptWeight {
  /**
   * The measuring event's own id, because the phase key does NOT identify a
   * row here — see [promptWeights]. A resumed executor publishes a second
   * measurement under its own `phase|iteration`, so a list keyed on that pair
   * has duplicate React keys and reconciles two different prompts onto one
   * row.
   */
  id: string;
  phase: string;
  iteration: number;
  /** The engine's own approximation, over every byte term below. */
  approximateTokens: number;
  /**
   * BYTES, which is what the engine measures — `len()` of a Go string — read
   * off the wire keys `system_bytes` / `user_bytes` and the three beside them.
   * A byte and a character only agree on ASCII, so the labels say bytes too;
   * `PromptSize` in internal/events/types/turn.go carries the reason.
   */
  systemBytes: number;
  userBytes: number;
  /**
   * The conversation a RESUMED phase re-entered — zero for a phase that
   * opened one of its own, which is nearly all of them.
   */
  messageBytes: number;
  /** The tool-definition array, as compact JSON: usually the largest term. */
  toolBytes: number;
  toolCount: number;
  /**
   * This measurement is a phase RE-ENTERED rather than opened — the second
   * half of an executor that parked on a detached coding run.
   *
   * Read off [messageBytes] rather than off a flag of its own, because the
   * engine ships no flag and needs none: `PromptSize.MessageBytes` in
   * internal/events/types/turn.go is "the conversation a RESUMED phase
   * re-enters … and zero for a phase that opens one of its own", and the
   * producer is the one call that passes a seed
   * (internal/agent/runner/resume.go's `seed: state.Answer(answer)`).
   *
   * A row with no message term reads false — the safe direction, since
   * nothing then says the phase was re-entered.
   */
  resumed: boolean;
}

/**
 * What each phase's prompt actually weighed.
 *
 * `prompt.size` exists so prompt-slimming is measurable rather than argued
 * about, and it is addressed like every other phase event precisely so the
 * size a TURN paid is readable on that turn. It was banded into `given` here
 * and then read by nobody: the Turn screen took one event out of that band
 * (`prefetch_summary`) and dropped the rest, so a whole row of integers per
 * phase reached the browser and went nowhere. The only way to the number was
 * the raw payload of a row in the residual list.
 *
 * `?? 0` ON EVERY TERM, because the payload is untyped: a key that never
 * arrives — a misspelled tag, a term the engine stopped measuring — renders
 * as a permanent zero instead of NaN, which is why the tests behind these
 * fields assert a value only the engine could have produced.
 *
 * ONE ROW PER MEASUREMENT, and the phase key is NOT the row's identity —
 * which is the opposite of what this function used to do, on a premise the Go
 * source contradicts twice over.
 *
 * The premise was that `turn_id|phase|iteration` is unique per measurement, so
 * a second row under one key had to be that phase RUNNING AGAIN after the
 * turn's dispatch was re-delivered — "the turn id IS the work key". It is not,
 * and has not been since `adr/0017`: `runnerTurn` in
 * internal/engine/telemetry.go opens with "TWO IDENTITIES, NEVER ONE", the run
 * id is minted per dispatch (`newRunID()` in internal/engine/turn.go) and the
 * work key is the trigger's digest. So a re-delivered trigger runs under a
 * DIFFERENT turn id, `EventLog.Turn` is `WHERE turn_id = ?`, and the two
 * attempts land on two Turn screens — which is exactly what `TurnView.attempt`
 * is for. A re-run cannot reach this function at all.
 *
 * What CAN put two measurements under one key is a SUSPEND, which is not a
 * re-run and is explicitly excluded from that ADR ("a detached coding run
 * re-enters the run that parked it, keeping the id from its own row"). The
 * executor parks mid-loop on `run_sandbox`, having already published its
 * opening measurement from `emit.started`; minutes or days later
 * `Runner.Resume` re-enters the SAME phase at the SAME iteration
 * (internal/agent/runner/resume.go: `phase: phase.Execute, iteration:
 * state.Round`) and publishes a second one. Measured on a real suspend and
 * resume, the pair is:
 *
 *     execute|1  system=3166 user=31 message=0     tool=3815  ~1753 tokens
 *     execute|1  system=0    user=0  message=3329  tool=3929  ~1814 tokens
 *
 * Collapsed, that drew ONE row reading `×2`, System 0 B, User 0 B and a
 * tooltip saying the phase "ran 2 times" and "ranged ~1,753–1,814 tokens".
 * Every clause of it is false. The phase ran once. Its opening frame was
 * 3,166 bytes of system prompt, not zero. And the two figures are not a range
 * of one quantity — they are two different prompts, both actually sent and
 * both actually billed, which is precisely what this panel exists to show.
 *
 * So both are drawn, in publish order, and the re-entry is marked rather than
 * merged — see [PromptWeight.resumed]. The old shape's own justification (a
 * turn that "ran five times" drawing ten byte-identical rows) was a
 * screenshot from before the identity split, and the split is what fixed it,
 * at the source. Collapsing here was the band-aid left behind.
 */
export function promptWeights(events: readonly EventRecord[]): PromptWeight[] {
  const out: PromptWeight[] = [];
  for (const event of events) {
    if (event.type !== "prompt.size") continue;
    const p = event.payload as Record<string, unknown> | undefined;
    if (!p) continue;
    const messageBytes = Number(p.message_bytes ?? 0);
    out.push({
      id: event.id,
      phase: String(p.phase ?? ""),
      iteration: Number(p.iteration ?? 0),
      approximateTokens: Number(p.approximate_tokens ?? 0),
      systemBytes: Number(p.system_bytes ?? 0),
      userBytes: Number(p.user_bytes ?? 0),
      messageBytes,
      toolBytes: Number(p.tool_bytes ?? 0),
      toolCount: Number(p.tool_count ?? 0),
      resumed: messageBytes > 0,
    });
  }
  return out;
}

/** One prefetch block: what it is called, whether it hit, and how big it was. */
export interface PrefetchBlock {
  label: string;
  hit: boolean;
  bytes: number;
  /** Set when the block was not searched at all, with why. */
  gated: string;
  /** Extra detail the block alone can say. */
  note: string;
}

/**
 * The seven context blocks an executor's prompt is built from.
 *
 * The event's own one-line summary collapses this to "2/7 hits", which is the
 * right shape for a feed and the wrong one for the screen about this turn:
 * every block degrades to empty on failure by design, so an unreachable store,
 * an unconfigured auxiliary model and a filter that selected nothing all
 * render as the same nothing. `trigger_requires_recon` is the field that tells
 * "empty because gated" from "the filter ran and found nothing", and it is the
 * difference between a configuration problem and a quiet turn.
 */
export function prefetchBlocks(event: EventRecord | undefined): PrefetchBlock[] {
  const p = event?.payload as Record<string, unknown> | undefined;
  if (!p) return [];
  const gated = p.trigger_requires_recon === true;
  const thin = gated ? "the trigger was a bare pointer — this filter never ran" : "";
  const picks = Number(p.relevant_knowledge_selection_count ?? 0);
  return [
    // NOT GATED, and that is the point of it: this block is what makes a thin
    // trigger thick. `trigger_requires_recon` says the trigger BODY is a
    // pointer, which is still true of a "+1" whose thread the engine handed
    // over — so the flag stays set and this block still ran.
    block("The thread so far", p, "thread_context", "", threadNote(p)),
    block("Personal memory", p, "personal_memory", thin),
    block("Similar prior work", p, "episode_recall", thin),
    block(
      "Relevant knowledge",
      p,
      "relevant_knowledge",
      thin,
      p.relevant_knowledge_hit === true && picks === 0
        ? "the search ran and selected nothing"
        : picks > 0
          ? `${picks} page${picks === 1 ? "" : "s"} selected`
          : "",
    ),
    block("Who it was with", p, "counterparty", ""),
    block("Synthesized skills", p, "synthesized_skills", ""),
    block("First-turn onboarding", p, "onboarding_hint", ""),
  ];
}

/**
 * What the thread block actually did, in one phrase.
 *
 * THREE STATES BEHIND ONE HIT. Both of the block's zero-message paths render
 * non-empty prose into the prompt — "there is nothing earlier" and "it could
 * not be read from this node" — so hit and bytes look identical on a thread
 * that was read and empty and on one no backend answered for. This read the
 * first as the second until the engine started reporting `thread_context_read`
 * beside the count, and told an operator that a healthy node could not reach
 * its own chat surface.
 *
 * `thread_context_stopped_short` is the fourth: a thread too long to read to
 * its end is handed over missing the NEWEST messages, the one that woke the
 * turn included, and its message count reads exactly like a whole thread's.
 */
function threadNote(p: Record<string, unknown>): string {
  if (p.thread_context_hit !== true) return "";
  if (p.thread_context_read !== true) return "the thread could not be read from that node";
  const posts = Number(p.thread_context_posts ?? 0);
  if (posts === 0) return "read, and there was nothing earlier";
  const handed = `${posts} message${posts === 1 ? "" : "s"} handed over`;
  return p.thread_context_stopped_short === true
    ? `${handed}, but not the newest — the thread was too long to read`
    : handed;
}

function block(
  label: string,
  p: Record<string, unknown>,
  key: string,
  gated: string,
  note = "",
): PrefetchBlock {
  const hit = p[`${key}_hit`] === true;
  return {
    label,
    hit,
    bytes: Number(p[`${key}_bytes`] ?? 0),
    // A block that HIT was not gated, whatever the flag says: the gate stops
    // the aux-LLM call, and only the three filters behind it are affected.
    gated: hit ? "" : gated,
    note,
  };
}

/**
 * A band's rows, with a repeat rendered ONCE and counted.
 *
 * A provider chain that falls through on every phase of a self-iterating turn
 * publishes one `provider_fallback` per attempt, and `SummaryFor` renders the
 * same sentence for each: the fields that tell the attempts apart — the phase
 * and the iteration — are on the payload and not in the line. So a turn that
 * lost its chain three times over seven phases drew eight byte-identical rows
 * of "default failed (auth) — no provider left in the chain" under a heading
 * that said 13, and the five rows that said something ELSE were what a reader
 * had to find among them.
 *
 * COUNTED, NOT DROPPED. Eight attempts is the fact: it is the difference
 * between one provider that is misconfigured and one that is flapping, and the
 * answer's `problems` goes on counting every one of them, because the heading
 * is a count of what went wrong rather than of what this list draws.
 *
 * CONSECUTIVE ONLY. The axis of every band is time — each row renders its own
 * instant — so merging across an intervening different row would either lie
 * about when the run happened or reorder the band to make the lie true. A run
 * carries its first and last instants and the row prints the span.
 *
 * THE KEY IS WHAT THE READER CAN SEE plus the one thing they cannot: the type,
 * the summary, and the failed flag. Two rows a reader cannot tell apart are
 * what this exists for; two that merely share a sentence while one of them
 * failed are not.
 *
 * A PURE FUNCTION OVER VALUES, for the reason `textindex`'s arithmetic and
 * `tracker`'s coercion table are: a rule that can only be exercised by
 * rendering a screen is a rule nobody re-measures.
 */
export interface Run {
  /** The first event of the run — its id keys the row and its link. */
  event: EventRecord;
  /** How many rows it stands for, 1 for an ordinary row. */
  count: number;
  /** The last event of the run; the same object as `event` when count is 1. */
  last: EventRecord;
}

export function collapseRuns(events: readonly EventRecord[]): Run[] {
  const out: Run[] = [];
  for (const event of events) {
    const open = out[out.length - 1];
    if (open && sameRow(open.last, event)) {
      open.count += 1;
      open.last = event;
      continue;
    }
    out.push({ event, count: 1, last: event });
  }
  return out;
}

/** Whether two rows would draw identically — see [collapseRuns]. */
function sameRow(a: EventRecord, b: EventRecord): boolean {
  return a.type === b.type && a.summary === b.summary && isFailed(a) === isFailed(b);
}
