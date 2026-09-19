/**
 * The rules that decide what a turn's non-phase events MEAN.
 *
 * Each case here is one of the ways the flat "Everything else this turn
 * published" list misled a reader: a duplicate presented as news, a sentinel
 * given the same weight as a failure, a healthy turn unable to say it was
 * healthy, and a type this build has never seen quietly disappearing.
 */

import { describe, expect, test } from "vitest";
import {
  ABSORBED,
  bandOf,
  collapseRuns,
  prefetchBlocks,
  promptWeights,
  tellStory,
} from "./turnstory.ts";
import type { PromptWeight } from "./turnstory.ts";
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

  test("all seven blocks are named, hit or not", () => {
    // The event's own summary is "2/7 hits", which is right for a feed and
    // useless on the screen about this turn: WHICH two is the whole question.
    const blocks = prefetchBlocks(prefetch());
    expect(blocks).toHaveLength(7);
    expect(blocks.filter((b) => b.hit).map((b) => b.label)).toEqual([
      "Personal memory",
      "Who it was with",
    ]);
  });

  test("the thread the turn was woken in counts its messages", () => {
    // A block the list omits simply never renders, and nothing anywhere goes
    // red — this list is hand-written with no gate behind it, so the seventh
    // block needs its own case or it can vanish from the screen silently.
    const blocks = prefetchBlocks(prefetch({ thread_context_hit: true, thread_context_posts: 12 }));
    const thread = blocks.find((b) => b.label === "The thread so far")!;
    expect(thread.hit).toBe(true);
    expect(thread.note).toBe("12 messages handed over");
  });

  test("a thread that could not be read reads differently from an absent one", () => {
    // hit=true with zero messages is the block telling the seat to go and read
    // the thread itself, which is nothing like a trigger that had no thread.
    const unreadable = prefetchBlocks(prefetch({ thread_context_hit: true }));
    expect(unreadable.find((b) => b.label === "The thread so far")!.note).toBe(
      "the thread could not be read from that node",
    );
    expect(prefetchBlocks(prefetch()).find((b) => b.label === "The thread so far")!.note).toBe("");
  });

  test("the thread block is never gated", () => {
    // It is what makes a thin trigger thick: the trigger body is still a bare
    // pointer, so the flag stays set, and this block still ran.
    const blocks = prefetchBlocks(
      prefetch({ trigger_requires_recon: true, thread_context_hit: false }),
    );
    expect(blocks.find((b) => b.label === "The thread so far")!.gated).toBe("");
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

  test("no prefetch event means no blocks, not seven empty ones", () => {
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
        system_chars: 24_000,
        user_chars: 2_800,
        message_chars: 0,
        tool_chars: 3_800,
        tool_count: 11,
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
      size("execute", 1, 6_807, { system_chars: 24_000 }),
      size("execute", 1, 6_807, { system_chars: 24_000 }),
      size("execute", 1, 6_616, { system_chars: 23_000 }),
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

  // THE WIRE KEYS ARE `system_chars` / `user_chars`, AND THAT IS LOAD-BEARING.
  //
  // The measurement is bytes and the keys say chars — the names disagree on
  // purpose, because the key is a peer contract frozen by ADR-0006 while the
  // label was the thing that lied. Renaming the reader to the honest spelling
  // is the tempting tidy-up and it is silent: every row already in the store
  // carries the old key, `Number(undefined ?? 0)` is 0, and the panel renders
  // "0 B" beside a truthful token count rather than declining to draw. This
  // case is what makes that tidy-up fail loudly instead.
  test("a payload keyed the honest way reads zero, which is why the reader keeps the frozen key", () => {
    const [row] = promptWeights([
      event("prompt.size", {
        payload: {
          turn_id: "t-1",
          phase: "execute",
          iteration: 1,
          approximate_tokens: 6807,
          system_bytes: 24_000,
          user_bytes: 2_800,
        },
      }),
    ]);
    expect(row!.systemBytes).toBe(0);
    expect(row!.userBytes).toBe(0);
    // And the token count still arrives, which is exactly what makes the
    // zero read as a fact rather than as a row that failed to load.
    expect(row!.approximateTokens).toBe(6807);
  });

  test("every term the engine measured reaches the row", () => {
    // The tool-definition array is the term this event was blind to, and the
    // dominant one: a measured turn reported ~6,900 tokens here against the
    // provider's 205,000. Asserting a NON-ZERO value is the point — the parse
    // coerces with `?? 0`, so a key that never arrives renders a permanent
    // zero that a "renders 0" assertion could never catch.
    const [w] = promptWeights([size("execute", 2, 7_400)]) as [PromptWeight];
    expect(w.phase).toBe("execute");
    expect(w.iteration).toBe(2);
    expect(w.approximateTokens).toBe(7_400);
    expect(w.systemBytes).toBe(24_000);
    expect(w.userBytes).toBe(2_800);
    expect(w.toolBytes).toBe(3_800);
    expect(w.toolCount).toBe(11);
  });

  test("a resumed phase's conversation is carried, not folded into the user term", () => {
    // A detached coding run re-enters its saved messages, so the engine sends
    // no system or user text at all and reports the seed instead. Folding it
    // into `userBytes` would make one column mean two different things
    // depending on whether the phase was resumed.
    const [w] = promptWeights([
      size("execute", 1, 12_950, { system_chars: 0, user_chars: 0, message_chars: 48_000 }),
    ]) as [PromptWeight];
    expect(w.messageBytes).toBe(48_000);
    expect(w.systemBytes).toBe(0);
    expect(w.userBytes).toBe(0);
  });

  test("an older engine's row reads as zero rather than NaN", () => {
    // A rolling upgrade puts a node that never measured the tool array on the
    // same stream. Its rows must render — `NaN B` in a column is worse than a
    // zero, because it reads as a broken screen rather than a quiet term.
    const [w] = promptWeights([
      event("prompt.size", {
        payload: { phase: "review", iteration: 1, approximate_tokens: 900, system_chars: 3_600 },
      }),
    ]) as [PromptWeight];
    expect(w.toolBytes).toBe(0);
    expect(w.toolCount).toBe(0);
    expect(w.messageBytes).toBe(0);
    expect(Number.isNaN(w.toolBytes)).toBe(false);
  });
});

/**
 * A repeat is drawn once and counted.
 *
 * `ProviderFallback.SummaryFor` renders the same sentence for every attempt —
 * the phase and the iteration that tell them apart are on the payload and not
 * in the line — so a turn that lost its chain on every phase drew eight
 * byte-identical rows and the rows that said something else had to be found
 * among them.
 */
describe("a run of identical rows", () => {
  const row = (type: string, summary: string, timestamp: string, failed = false): EventRecord =>
    ({ id: `${type}-${timestamp}`, type, summary, timestamp, failed }) as unknown as EventRecord;

  const chain = "default failed (auth) — no provider left in the chain";

  test("draws one row, counted, spanning the first and the last", () => {
    const runs = collapseRuns([
      row("provider_fallback", chain, "2026-09-13T15:42:15Z"),
      row("provider_fallback", chain, "2026-09-13T15:42:16Z"),
      row("provider_fallback", chain, "2026-09-13T15:42:18Z"),
    ]);
    expect(runs).toHaveLength(1);
    expect(runs[0]?.count).toBe(3);
    // THE FIRST KEYS THE ROW and the last closes the span: a row that opened
    // on the last event would date the run by its end.
    expect(runs[0]?.event.timestamp).toBe("2026-09-13T15:42:15Z");
    expect(runs[0]?.last.timestamp).toBe("2026-09-13T15:42:18Z");
  });

  test("never merges across a row that says something else", () => {
    // The axis is time — every row renders its own instant — so a merge that
    // reached over the `llm_unavailable` between two fallbacks would either
    // lie about when the run happened or reorder the band to make it true.
    const runs = collapseRuns([
      row("provider_fallback", chain, "2026-09-13T15:42:15Z"),
      row("llm_unavailable", "LLM unavailable for Agent CEO", "2026-09-13T15:42:16Z", true),
      row("provider_fallback", chain, "2026-09-13T15:42:18Z"),
    ]);
    expect(runs.map((r) => r.count)).toEqual([1, 1, 1]);
  });

  test("keeps a failure apart from a line that merely reads the same", () => {
    // Two rows a reader cannot tell apart are what this exists for; two that
    // share a sentence while one of them failed are not — the failed one
    // draws a glyph and a red edge the other does not.
    const runs = collapseRuns([
      row("provider_fallback", chain, "2026-09-13T15:42:15Z", false),
      row("provider_fallback", chain, "2026-09-13T15:42:16Z", true),
    ]);
    expect(runs).toHaveLength(2);
  });

  test("leaves a band with nothing repeated exactly as it was", () => {
    const rows = [
      row("provider_fallback", chain, "2026-09-13T15:42:15Z"),
      row("turn.guard_breach", "guard max_iter", "2026-09-13T15:48:37Z", true),
    ];
    expect(collapseRuns(rows).map((r) => r.event)).toEqual(rows);
    expect(collapseRuns([])).toEqual([]);
  });
});
