/**
 * What a turn published BESIDE its phases, sorted into the three questions a
 * reader actually brings to it.
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
 *     as the stat strip AND as a JSON dump. The list read as a duplicate
 *     because it largely was one — a phase start says which phase opened, and
 *     its own completed record says that, its duration included (see
 *     `phaseDuration` in ./phases.ts).
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
 * So the rows are split by the question they answer. WENT_WRONG is loud and
 * usually absent. GIVEN is what the turn's prompt was built from, before it
 * ran. LEFT_BEHIND is what the company learned from it, after. Anything this
 * build has no opinion about falls through to REST rather than being dropped:
 * the event registry is additive-only and a type published by a newer node
 * must still render.
 *
 * # A BAND ENTRY IS A QUERY PREDICATE, NOT A WISH
 *
 * Every row these sets sort came from ONE read: `EventLog.Turn` in
 * `internal/store/eventlog.go`, which is `WHERE turn_id = ?`. That column is
 * filled from the event's own top-level `turn_id` FIELD — `store.ExtractTags`
 * pulls it out of the marshalled envelope and `EventLog.Append` copies it
 * across — so a payload that declares no `turn_id` key writes an empty column,
 * never matches the predicate, and cannot reach this file however it is
 * banded. Naming one here is not a rule about where it goes: it is a promise
 * the wire cannot keep, and it fails EMPTY, which is the one way a panel
 * cannot say it is broken.
 *
 * Eight types were named here that way. The three A2A audit records were the
 * loudest, because the "What else it did" band advertised "colleagues" on
 * their behalf and an A2A ask has never once appeared under it. The rest
 * carried the same defect quietly: `task_assigned` is the SCHEDULER's cron
 * fire, published before the turn it wakes exists; `turn_trigger_skipped` and
 * `notification_skipped` are both records that NO turn ran; `skill_promoted`
 * is the curator duty promoting a unit's skill across many turns; and
 * `skill_telemetry_write_failed` is a reflection worker that holds the turn id
 * and does not stamp it.
 *
 * `internal/events/types/turnbands_client_test.go` holds these sets against the
 * frozen wire contract now, in both directions — so a band entry that can
 * never fill fails the build, and so does a kept-out type that GAINS a
 * `turn_id` and is therefore ready to come back.
 */

import type { EventRecord } from "~/protocol/index.ts";

/**
 * Which question a row answers — the WHOLE taxonomy, `absorbed` inside it
 * rather than beside it.
 *
 * This union was missing that member and the rule was therefore written
 * twice. [bandOf] answered `rest` for every type in [ABSORBED] and [tellStory]
 * short-circuited those same types into `Story.absorbed` before bandOf ever
 * ran, so the two exported readers of one rule disagreed on all five of them:
 * the screen filed a phase start as absorbed while the function called it
 * residual, and nothing compared the two answers. A band a row can land in is
 * a member of this union, or it is a second rule waiting to drift from the
 * first.
 */
export type Band = "went_wrong" | "given" | "did" | "left_behind" | "absorbed" | "rest";

/**
 * Types that are ALREADY on the screen in a better form, and are therefore not
 * rows.
 *
 * Each names where it went instead — the point is that nothing is silently
 * dropped, and the next reader can check the claim.
 */
export const ABSORBED: Record<string, string> = {
  // Folded onto its own phase card as that phase's start instant.
  agent_phase_started: "the phase card it opens",
  // The phase cards themselves.
  agent_phase_completed: "its own phase card",
  // Stream-only; never persisted, so it cannot be in a query answer anyway.
  agent_turn_progress: "the live phase card",
  // The stat strip, the header and the turn record.
  agent_turn_completed: "the turn's header and record",
  turn_completed: "the turn's header and record",
};

/**
 * Where this row already is, in the words [ABSORBED] names it with — and the
 * empty string for a row that is a row.
 *
 * ONE READER OF THE MAP, because there are two callers now and the second is a
 * screen: [bandOf] asks whether a row is absorbed and the Turn screen asks
 * where each absorbed row went. Written out at each site, the `in` check and
 * the lookup are the same rule stated twice over a map whose own values are
 * the answer.
 */
export function absorbedInto(event: EventRecord): string {
  return Object.hasOwn(ABSORBED, event.type) ? (ABSORBED[event.type] ?? "") : "";
}

/**
 * The engine talking about something going wrong — a turn the engine stopped,
 * a chain that fell through, a guard that fired, a refusal.
 *
 * These are the rows the old panel's subtitle promised ("fallbacks, guard
 * breaches") and — until the events behind them were given a producer and a
 * turn id — could never actually contain. Six of the nine that claim carried:
 * every one below publishes `turn_id`, which is what puts it in the answer
 * this file sorts. The three that did not are on the kept-out roster in
 * `internal/events/types/turnbands_client_test.go` — a skipped trigger and a
 * skipped notification are both records that NO turn ran, and the skill
 * telemetry failure is a reflection worker that holds the turn id without
 * stamping it.
 */
const WENT_WRONG = new Set([
  "turn.guard_breach",
  "llm_unavailable",
  "budget_exhausted",
  "provider_fallback",
  "phase.tool_skill_blocked",
  "sandbox_run_failed",
]);

/**
 * The three records `publishFailure` writes for a turn the engine STOPPED.
 *
 * A subset of WENT_WRONG, and the distinction is the whole of it: these are
 * the rows that describe the same stop `agent_turn_completed.failed` already
 * reports, so a reader counting problems must not count both. Everything else
 * in WENT_WRONG — a recovered provider fallback, a refused tool skill, a
 * failed sandbox run — is an INDEPENDENT problem that happens to share a turn
 * with the stop. `sandbox_run_failed` is deliberately not here although it is
 * a failure: `internal/engine/telemetry.go` does not publish it, so it never
 * describes the turn's own stop.
 */
export const TURN_STOP = new Set(["turn.guard_breach", "budget_exhausted", "llm_unavailable"]);

/** What the turn's prompt was assembled from, before the first phase ran. */
const GIVEN = new Set(["prefetch_summary", "prompt.size"]);

/**
 * What the turn changed about the company, after its last phase.
 *
 * `skill_promoted` is NOT here, although it is the reflection estate's own
 * event and reads like one of these: the curator duty promotes a unit's skill
 * off a cluster of many seats' turns, so it names no turn and there is no
 * single right one to name. That is the same reason `skill_synthesized`
 * carries an agent id and an empty turn id on its clustered path.
 */
const LEFT_BEHIND = new Set([
  "episode_written",
  "persist_decider_completed",
  "counterparty_profile_updated",
  "reflection_completed",
  "skill_synthesized",
  "skill_refined",
]);

/**
 * Work the turn did that is not a phase: a coding run, a delegation, a tool.
 *
 * NO COLLEAGUE ROWS, and the panel's subtitle no longer offers any. The three
 * A2A audit records were named here and the band advertised "colleagues" for
 * them, but `a2a_channel_opened`, `a2a_message_sent` and `a2a_channel_closed`
 * carry no `turn_id` on the wire, so an ask has never been drawn under this
 * heading on any turn this engine has ever run — see the note at the top of
 * this file, and the roster in
 * `internal/events/types/turnbands_client_test.go` for what has to change in
 * the engine before they can come back.
 *
 * `task_assigned` was named here too and is a different mistake: it is the
 * SCHEDULER's cron fire — the wake that starts a turn — rather than work a
 * turn did. The trigger is already on this screen, as the brief.
 */
const DID = new Set([
  "sandbox_run_started",
  "sandbox_clarification_requested",
  "sandbox_run_completed",
  "subagent_batched",
  "skill_used",
]);

/**
 * Which band a row belongs in — the ONE rule, and [tellStory] is a filing
 * clerk over it rather than a second copy of it.
 *
 * A FAILED row is `went_wrong` whatever its SET says. The failure taxonomy is
 * the engine's (`events.Failed` — the type, the payload's own flag, or the
 * store's tag), and a sandbox run that came back failed is something that went
 * wrong on this turn even though `sandbox_run_completed` is ordinary work.
 *
 * ABSORPTION OUTRANKS EVEN THAT, which is the one precedence worth stating
 * because it looks like an exception and is not: the destination draws the
 * failure itself. A failed `agent_phase_completed` is a phase card carrying
 * the engine's own error kind and its error text (`PhaseCard`), so banding it
 * `went_wrong` as well would put a red row above the card that already says
 * so, and add one to a count of problems that has not gained a problem.
 *
 * `Object.hasOwn`, NEVER `in`. Every key of `Object.prototype` answers `in` on
 * an object literal — `"toString" in ABSORBED` is true — so an event type
 * colliding with one would be absorbed to a destination that does not exist
 * and vanish off the screen, which is exactly the silent drop this file's
 * residual band exists to prevent. No type in `internal/events/types` collides
 * today, so this is a hazard rather than an incident; it costs one call to
 * make it neither.
 */
export function bandOf(event: EventRecord): Band {
  if (absorbedInto(event)) return "absorbed";
  if (isFailed(event)) return "went_wrong";
  if (WENT_WRONG.has(event.type)) return "went_wrong";
  if (GIVEN.has(event.type)) return "given";
  if (LEFT_BEHIND.has(event.type)) return "left_behind";
  if (DID.has(event.type)) return "did";
  return "rest";
}

/**
 * Whether a row is a failure, read the way the server writes it.
 *
 * `failed` reaches the client two ways — the payload's own field while the
 * event is live, and the `failed` TAG the event-store writer stamps, which is
 * all that survives into history. Reading only one of them makes the same turn
 * red on one surface and not on another, which is the exact reason
 * `events.FailureEventTypes` is declared once in Go.
 */
export function isFailed(event: EventRecord): boolean {
  if (event.failed) return true;
  const payload = event.payload as Record<string, unknown> | undefined;
  return payload?.failed === true;
}

export interface Story {
  wentWrong: EventRecord[];
  given: EventRecord[];
  did: EventRecord[];
  leftBehind: EventRecord[];
  /**
   * Everything this build has no opinion about — and NOTHING absorbed.
   *
   * The two used to be one sentence here ("plus absorbed duplicates"), which
   * no row has ever satisfied: [tellStory] has always filed an absorbed row
   * under `absorbed`, so a reader of this doc looking for a phase start in
   * the residual list was looking in the one band it can never be in.
   */
  rest: EventRecord[];
  /**
   * The absorbed rows alone, so the screen can say where each went rather
   * than leaving a reader to wonder why the numbers do not add up.
   *
   * Rendered by the Turn screen through [absorbedGroups] — which is what this
   * field says and, until that panel existed, was not true: the value was
   * built on every frame, asserted by this file's own test and read by no
   * screen, so a turn's answer quietly lost twelve of its rows between the
   * query and the page. A count nothing prints is indistinguishable from a
   * row the store never returned.
   */
  absorbed: EventRecord[];
}

/**
 * Sort one turn's non-phase events into the story it tells.
 *
 * EVERY ROW GOES THROUGH [bandOf], including an absorbed one. This loop used
 * to test [ABSORBED] itself and `continue` before the call, which made it a
 * second implementation of the first line of that function — and the two
 * answered differently, because bandOf called the same rows `rest`. One
 * switch over one rule is what stops a band existing in one reader and not in
 * the other.
 */
export function tellStory(events: readonly EventRecord[]): Story {
  const story: Story = {
    wentWrong: [],
    given: [],
    did: [],
    leftBehind: [],
    rest: [],
    absorbed: [],
  };
  for (const event of events) {
    switch (bandOf(event)) {
      case "absorbed":
        story.absorbed.push(event);
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

/** One absorbed TYPE, and how many rows of it this turn published. */
export interface AbsorbedGroup {
  type: string;
  /** Where those rows already are, in [ABSORBED]'s own words. */
  destination: string;
  count: number;
}

/**
 * What the absorbed rows were, and where each kind went — the inventory the
 * Turn screen prints so a reader can check the claim this file makes.
 *
 * BY TYPE, NOT BY DESTINATION, because the destination is the claim and the
 * type is what makes it checkable: two types share "the turn's header and
 * record", and merged into one line a reader cannot tell which pair of
 * records that stands for. It is also the only key [ABSORBED] is written in.
 *
 * ACROSS THE WHOLE TURN, which is the opposite of [collapseRuns] and for the
 * reason that rule gives for its own shape: the axis of a BAND is time, so a
 * repeat there may only merge with its neighbour. Nothing here prints an
 * instant. A turn's phase starts and phase records interleave one for one, so
 * a consecutive-only rule would report twelve groups of one and say nothing
 * at all.
 *
 * A PURE FUNCTION OVER VALUES, like the rest of this file: handed a whole
 * turn or handed `Story.absorbed`, it answers the same, because it files on
 * [absorbedInto] rather than on where the caller got the rows.
 */
export function absorbedGroups(events: readonly EventRecord[]): AbsorbedGroup[] {
  const out: AbsorbedGroup[] = [];
  const seen = new Map<string, AbsorbedGroup>();
  for (const event of events) {
    const destination = absorbedInto(event);
    if (!destination) continue;
    const already = seen.get(event.type);
    if (already) {
      already.count += 1;
      continue;
    }
    const group: AbsorbedGroup = { type: event.type, destination, count: 1 };
    seen.set(event.type, group);
    out.push(group);
  }
  return out;
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
  /** The engine's own approximation, over every character term below. */
  approximateTokens: number;
  /**
   * BYTES, which is what the engine measures — `len()` of a Go string — read
   * off the wire keys `system_chars` / `user_chars` and the three beside them.
   *
   * THE NAMES DISAGREE ON PURPOSE. The measurement has always been bytes and
   * the keys have always said chars, and the panel used to print "24 KB"
   * under a tooltip claiming it had counted characters; the two units only
   * agree on ASCII. The label is what was lying, so the label was fixed. The
   * KEY is a peer contract frozen by ADR-0006 — renaming it would read back
   * as a rendered 0 on every row already in the store — so it stays, and
   * `PromptSize` in internal/events/types/turn.go carries the full reason.
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
   * An older engine's row reads false, which is the safe direction: before
   * the message term existed a resumed phase published nothing but zeros, so
   * there is no measurement to label either way.
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
 * `?? 0` ON EVERY TERM, which is load-bearing in one direction only: a node
 * running an older engine publishes a row without the tool and message keys,
 * and the columns for them read 0 rather than NaN. It is also how a key that
 * never arrives — a misspelled tag, a term the engine stopped measuring —
 * renders as a permanent zero instead of raising, which is why the tests
 * behind these fields assert a value only the engine could have produced.
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
    // ONE KEY EACH, never a both-spellings chain. The wire key never moved,
    // so there is no second spelling to accept — and a `??` would not have
    // rescued one anyway: scalars in the catalogue carry no omitempty, so a
    // relayed event asserts `0` rather than omitting the key, and `??` does
    // not fall through on 0.
    const messageBytes = Number(p.message_chars ?? 0);
    out.push({
      id: event.id,
      phase: String(p.phase ?? ""),
      iteration: Number(p.iteration ?? 0),
      approximateTokens: Number(p.approximate_tokens ?? 0),
      systemBytes: Number(p.system_chars ?? 0),
      userBytes: Number(p.user_chars ?? 0),
      messageBytes,
      toolBytes: Number(p.tool_chars ?? 0),
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
 * between one provider that is misconfigured and one that is flapping, and
 * `problemCount` goes on counting every one of them, because the heading is a
 * count of what went wrong rather than of what this list draws.
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
