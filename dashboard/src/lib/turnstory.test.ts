/**
 * The rules that decide what a turn's non-phase events MEAN.
 *
 * Each case here is one of the ways the flat "Everything else this turn
 * published" list misled a reader: a duplicate presented as news, a sentinel
 * given the same weight as a failure, a healthy turn unable to say it was
 * healthy, and a type this build has never seen quietly disappearing.
 */

import { describe, expect, test } from "vitest";
import { ABSORBED, bandOf, prefetchBlocks, promptWeights, tellStory } from "./turnstory.ts";
import type { EventRecord } from "~/protocol/index.ts";

function event(type: string, over: Partial<EventRecord> = {}): EventRecord {
  return {
    id: `ev-${type}-${over.timestamp ?? ""}`,
    type,
    timestamp: "2026-01-01T00:00:00Z",
    source: "engine",
    actor: "CEO",
    summary: "",
    category: "system",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    ...over,
  };
}

describe("what is not 'else'", () => {
  test("a phase start is absorbed, not listed", () => {
    // Six of the twelve rows on the turn this was rebuilt against were
    // `agent_phase_started` — one per phase card directly above, saying
    // "started execute (iter 2)" beside a card that already says EXECUTE,
    // iter 2, its model, its rounds, its tokens and what it decided.
    const story = tellStory([event("agent_phase_started")]);
    expect(story.absorbed).toHaveLength(1);
    expect(story.rest).toHaveLength(0);
    expect(story.did).toHaveLength(0);
  });

  test("both halves of the turn's own record are absorbed", () => {
    // `agent_turn_completed` and `turn_completed` describe the same turn for
    // two different consumers and land at the same instant. As two near
    // identical rows they read as noise; as the screen's header and record
    // they are the answer.
    const story = tellStory([event("agent_turn_completed"), event("turn_completed")]);
    expect(story.absorbed).toHaveLength(2);
    expect(story.rest).toHaveLength(0);
  });

  test("every absorbed type names where it went", () => {
    // The claim is checkable, which is the point: nothing here is dropped,
    // and the next reader can go and confirm each destination exists.
    for (const [type, destination] of Object.entries(ABSORBED)) {
      expect(destination, type).toBeTruthy();
    }
  });
});

describe("weight", () => {
  test("a guard breach is what went wrong, not bookkeeping", () => {
    expect(bandOf(event("turn.guard_breach"))).toBe("went_wrong");
    expect(bandOf(event("llm_unavailable"))).toBe("went_wrong");
    expect(bandOf(event("budget_exhausted"))).toBe("went_wrong");
    expect(bandOf(event("provider_fallback"))).toBe("went_wrong");
  });

  test("a failed row is what went wrong whatever its type says", () => {
    // The failure taxonomy is the engine's — the type, the payload's own
    // flag, or the store's tag — and a coding run that came back failed is
    // something that went wrong on this turn even though a completed run is
    // ordinary work.
    expect(bandOf(event("sandbox_run_completed"))).toBe("did");
    expect(bandOf(event("sandbox_run_completed", { failed: true }))).toBe("went_wrong");
  });

  test("the failed flag is read from the payload too", () => {
    // It reaches the client two ways: the payload's own field while the event
    // is live, and the `failed` tag the store writer stamps, which is all
    // that survives into history. Reading only one makes the same turn red on
    // one surface and clean on another.
    expect(bandOf(event("skill_used", { payload: { failed: true } }))).toBe("went_wrong");
  });

  test("the learning aftermath is its own question, not a failure and not work", () => {
    for (const type of [
      "episode_written",
      "persist_decider_completed",
      "counterparty_profile_updated",
      "reflection_completed",
      "skill_synthesized",
    ]) {
      expect(bandOf(event(type)), type).toBe("left_behind");
    }
  });

  test("a healthy turn can say nothing went wrong", () => {
    // The old panel was never empty, so "nothing went wrong here" was not a
    // state the screen could reach — and a section that is always full is a
    // section nobody reads.
    const story = tellStory([
      event("agent_phase_started"),
      event("agent_phase_completed"),
      event("prefetch_summary"),
      event("episode_written"),
      event("reflection_completed"),
      event("agent_turn_completed"),
      event("turn_completed"),
    ]);
    expect(story.wentWrong).toHaveLength(0);
    expect(story.given).toHaveLength(1);
    expect(story.leftBehind).toHaveLength(2);
  });
});

describe("a type this build has never seen", () => {
  test("falls through to the residual list rather than disappearing", () => {
    // The event registry is additive-only precisely so a rolling upgrade can
    // put unknown types on the wire. A banding rule that dropped what it did
    // not recognise would make the newer node's turns look emptier than they
    // were.
    const story = tellStory([event("something.a_later_build_publishes")]);
    expect(story.rest).toHaveLength(1);
    expect(story.rest[0]!.type).toBe("something.a_later_build_publishes");
  });

  test("and is still loud if it is marked failed", () => {
    expect(bandOf(event("something.a_later_build_publishes", { failed: true }))).toBe("went_wrong");
  });
});

describe("the prefetch", () => {
  function prefetch(over: Record<string, unknown> = {}) {
    return event("prefetch_summary", {
      payload: {
        turn_id: "t1",
        personal_memory_hit: true,
        personal_memory_bytes: 512,
        episode_recall_hit: false,
        relevant_knowledge_hit: false,
        counterparty_hit: true,
        counterparty_bytes: 64,
        synthesized_skills_hit: false,
        onboarding_hint_hit: false,
        ...over,
      },
    });
  }

  test("all six blocks are named, hit or not", () => {
    // The event's own summary is "2/6 hits", which is right for a feed and
    // useless on the screen about this turn: WHICH two is the whole question.
    const blocks = prefetchBlocks(prefetch());
    expect(blocks).toHaveLength(6);
    expect(blocks.filter((b) => b.hit).map((b) => b.label)).toEqual([
      "Personal memory",
      "Who it was with",
    ]);
  });

  test("gated is not the same as empty", () => {
    // Every block degrades to empty on failure by design, so an unreachable
    // store, an unconfigured auxiliary model and a filter that genuinely
    // found nothing all render as the same nothing. `trigger_requires_recon`
    // is the field that separates a configuration problem from a quiet turn,
    // and it is the whole reason the engine puts it on the wire.
    const blocks = prefetchBlocks(prefetch({ trigger_requires_recon: true }));
    const recall = blocks.find((b) => b.label === "Similar prior work")!;
    expect(recall.hit).toBe(false);
    expect(recall.gated).toBeTruthy();
    // A block that HIT was not gated whatever the flag says: the gate stops
    // the aux-LLM call, and only the filters behind it are affected.
    expect(blocks.find((b) => b.label === "Personal memory")!.gated).toBe("");
  });

  test("a knowledge search that ran and picked nothing says so", () => {
    // hit=true with a zero selection count is the filter running and finding
    // nothing relevant — which reads nothing like the filter never running.
    const blocks = prefetchBlocks(
      prefetch({ relevant_knowledge_hit: true, relevant_knowledge_selection_count: 0 }),
    );
    expect(blocks.find((b) => b.label === "Relevant knowledge")!.note).toBe(
      "the search ran and selected nothing",
    );
  });

  test("and one that picked pages counts them", () => {
    const blocks = prefetchBlocks(
      prefetch({ relevant_knowledge_hit: true, relevant_knowledge_selection_count: 3 }),
    );
    expect(blocks.find((b) => b.label === "Relevant knowledge")!.note).toBe("3 pages selected");
  });

  test("no prefetch event means no blocks, not six empty ones", () => {
    // A turn whose prefetch event fell outside the store's window has no
    // answer here, which is different from a turn whose every block missed.
    expect(prefetchBlocks(undefined)).toEqual([]);
  });
});

describe("what each phase's prompt weighed", () => {
  function size(
    phase: string,
    iteration: number,
    tokens: number,
    over: Record<string, unknown> = {},
  ): EventRecord {
    return event("prompt.size", {
      payload: {
        turn_id: "t-1",
        phase,
        iteration,
        approximate_tokens: tokens,
        system_bytes: 24_000,
        user_bytes: 2_800,
        ...over,
      },
    });
  }

  test("a turn that ran once gets one row per phase, in the order it published", () => {
    // The control. Without it the collapse below passes on a build that
    // returns nothing at all.
    const rows = promptWeights([
      size("onboarding", 0, 3_046),
      size("execute", 1, 6_807),
      size("review", 1, 2_071),
      size("execute", 2, 7_646),
    ]);
    expect(rows.map((r) => `${r.phase}|${r.iteration}`)).toEqual([
      "onboarding|0",
      "execute|1",
      "review|1",
      "execute|2",
    ]);
    expect(rows.every((r) => r.runs === 1)).toBe(true);
  });

  test("a phase key measured twice is one row that says so", () => {
    // `turn_id|phase|iteration` IS the phase key, so a second measurement
    // under one key is that phase RUNNING AGAIN — the turn's whole dispatch
    // re-delivered and re-run under its work key. Listed flat it drew two
    // byte-identical rows, which reads as a repeating panel rather than as
    // news about the turn.
    const rows = promptWeights([
      size("execute", 1, 6_807),
      size("execute", 1, 6_807),
      size("execute", 1, 6_807),
    ]);
    expect(rows).toHaveLength(1);
    expect(rows[0]!.runs).toBe(3);
    expect(rows[0]!.minTokens).toBe(6_807);
    expect(rows[0]!.maxTokens).toBe(6_807);
  });

  test("the row carries the LAST run's figures and the range of the others", () => {
    // The run that stands is the one whose frame the phase actually reasoned
    // in; the earlier ones measured a prompt the turn then threw away. The
    // range is the only thing those discarded rows still had to say.
    const rows = promptWeights([
      size("execute", 1, 6_807, { system_bytes: 24_000 }),
      size("execute", 1, 6_807, { system_bytes: 24_000 }),
      size("execute", 1, 6_616, { system_bytes: 23_000 }),
    ]);
    expect(rows[0]!.approximateTokens).toBe(6_616);
    expect(rows[0]!.systemBytes).toBe(23_000);
    expect(rows[0]!.minTokens).toBe(6_616);
    expect(rows[0]!.maxTokens).toBe(6_807);
  });

  test("a re-run turn keeps its phases in first-seen order, not re-run order", () => {
    // The screenshot this was rebuilt against interleaved five onboarding and
    // five executor passes. Collapsed naively by deleting and re-inserting,
    // ONBOARDING would have jumped below EXECUTE on the second pass and the
    // page would read as though the executor went first.
    const rows = promptWeights([
      size("onboarding", 0, 3_046),
      size("execute", 1, 6_807),
      size("onboarding", 0, 3_046),
      size("execute", 1, 6_616),
    ]);
    expect(rows.map((r) => r.phase)).toEqual(["onboarding", "execute"]);
    expect(rows.map((r) => r.runs)).toEqual([2, 2]);
  });

  test("a self-iterate round is its own phase key, never a re-run", () => {
    // Same phase, different iteration: two frames the model genuinely
    // reasoned in, and collapsing them would hide the growth this panel
    // exists to make visible.
    const rows = promptWeights([size("execute", 1, 6_807), size("execute", 2, 7_646)]);
    expect(rows).toHaveLength(2);
    expect(rows.map((r) => r.runs)).toEqual([1, 1]);
  });

  test("an event with no payload is skipped rather than counted as a zero row", () => {
    expect(promptWeights([event("prompt.size"), event("agent_phase_started")])).toEqual([]);
  });
});
