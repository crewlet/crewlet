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
 *     because it largely was one. Those rows are not dropped — a phase start
 *     is folded onto its own phase card (see `withStarts` in ./phases.ts),
 *     where it is the missing half of that phase's duration.
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
 */

import type { EventRecord } from "~/protocol/index.ts";

export type Band = "went_wrong" | "given" | "did" | "left_behind" | "rest";

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
 * The engine talking about something going wrong — a turn the engine stopped,
 * a chain that fell through, a guard that fired, a refusal.
 *
 * These are the rows the old panel's subtitle promised ("fallbacks, guard
 * breaches") and — until the events behind them were given a producer and a
 * turn id — could never actually contain.
 */
const WENT_WRONG = new Set([
  "turn.guard_breach",
  "llm_unavailable",
  "budget_exhausted",
  "provider_fallback",
  "execute.missing_tool",
  "phase.tool_skill_blocked",
  "sandbox_run_failed",
  "turn_trigger_skipped",
  "notification_skipped",
  "skill_telemetry_write_failed",
]);

/** What the turn's prompt was assembled from, before the first phase ran. */
const GIVEN = new Set(["prefetch_summary", "prompt.size"]);

/** What the turn changed about the company, after its last phase. */
const LEFT_BEHIND = new Set([
  "episode_written",
  "persist_decider_completed",
  "counterparty_profile_updated",
  "reflection_completed",
  "skill_synthesized",
  "skill_refined",
  "skill_promoted",
]);

/** Work the turn did that is not a phase: a coding run, a delegation, a tool. */
const DID = new Set([
  "sandbox_run_started",
  "sandbox_clarification_requested",
  "sandbox_run_completed",
  "subagent_batched",
  "phase.tool_activated",
  "skill_used",
  "a2a_channel_opened",
  "a2a_message_sent",
  "a2a_message_delivered",
  "a2a_channel_closed",
  "message_sent",
  "task_created",
  "task_assigned",
  "task_delegated",
  "task_completed",
  "task_failed",
]);

/**
 * Which band a row belongs in.
 *
 * A FAILED row is `went_wrong` whatever its type says. The failure taxonomy is
 * the engine's (`events.Failed` — the type, the payload's own flag, or the
 * store's tag), and a sandbox run that came back failed is something that went
 * wrong on this turn even though `sandbox_run_completed` is ordinary work.
 */
export function bandOf(event: EventRecord): Band {
  if (event.type in ABSORBED) return "rest";
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
  /** Everything this build has no opinion about, plus absorbed duplicates. */
  rest: EventRecord[];
  /** The absorbed rows alone — counted, so the screen can say where they went
      rather than leaving a reader to wonder why the numbers do not add up. */
  absorbed: EventRecord[];
}

/** Sort one turn's non-phase events into the story it tells. */
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
    if (event.type in ABSORBED) {
      story.absorbed.push(event);
      continue;
    }
    switch (bandOf(event)) {
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
 * The six context blocks an executor's prompt is built from.
 *
 * The event's own one-line summary collapses this to "2/6 hits", which is the
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
