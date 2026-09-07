/**
 * The rules that decide what a turn's non-phase events MEAN.
 *
 * Each case here is one of the ways the flat "Everything else this turn
 * published" list misled a reader: a duplicate presented as news, a sentinel
 * given the same weight as a failure, a healthy turn unable to say it was
 * healthy, and a type this build has never seen quietly disappearing.
 */

import { describe, expect, test } from "vitest";
import { ABSORBED, bandOf, prefetchBlocks, tellStory } from "./turnstory.ts";
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
