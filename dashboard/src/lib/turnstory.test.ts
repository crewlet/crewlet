/**
 * What the Turn screen does with a turn's rows once the engine has filed them.
 *
 * WHICH BAND A ROW IS IN IS NOT TESTED HERE. The engine files every row of the
 * `turn` answer (`types.BandOf`) and its cases — a phase start absorbed, a
 * failed run what went wrong, a type a newer build publishes falling through,
 * a band entry that could never fill — are Go tests now, in
 * `internal/events/types/turnband_test.go` beside the sets they pin. What is
 * left for this file is the client's half: collecting the filed rows without
 * deciding anything twice, collapsing a run of identical rows, and reading the
 * payloads the screen tabulates.
 */

import { describe, expect, test } from "vitest";
import { collapseRuns, isFailed, prefetchBlocks, promptWeights, tellStory } from "./turnstory.ts";
import type { PromptWeight } from "./turnstory.ts";
import type { EventRecord, TurnBand, TurnEventRow } from "~/protocol/index.ts";

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

/** A row of a turn's answer, filed in `band` as the engine would file it. */
function filed(
  type: string,
  band: TurnBand | string,
  over: Partial<EventRecord> = {},
): TurnEventRow {
  return { ...event(type, over), band: band as TurnBand };
}

describe("collecting a turn's rows", () => {
  test("each row lands where the engine filed it, and nowhere else", () => {
    // THE CLIENT DECIDES NOTHING A SECOND TIME: a row the engine called
    // `did` is listed under what else it did whatever its type would suggest,
    // because a second rule here is how the screen and a member's story of the
    // same turn would come to disagree.
    const story = tellStory([
      filed("provider_fallback", "went_wrong"),
      filed("prefetch_summary", "given"),
      filed("knowledge_read", "did"),
      filed("episode_written", "left_behind"),
      filed("something.unbanded", "rest"),
    ]);
    expect(story.wentWrong.map((e) => e.type)).toEqual(["provider_fallback"]);
    expect(story.given.map((e) => e.type)).toEqual(["prefetch_summary"]);
    expect(story.did.map((e) => e.type)).toEqual(["knowledge_read"]);
    expect(story.leftBehind.map((e) => e.type)).toEqual(["episode_written"]);
    expect(story.rest.map((e) => e.type)).toEqual(["something.unbanded"]);
  });

  test("an absorbed row is not listed — the answer's inventory says where it went", () => {
    // Six of the twelve rows on the turn the bands were built against were
    // `agent_phase_started`, one per phase card directly above. The screen
    // prints where each kind went from the answer's own `absorbed`, so a row
    // collected here as well would be drawn twice.
    const story = tellStory([filed("agent_phase_started", "absorbed")]);
    const listed = [
      ...story.wentWrong,
      ...story.given,
      ...story.did,
      ...story.leftBehind,
      ...story.rest,
    ];
    expect(listed).toHaveLength(0);
  });

  test("a band this build does not know is still drawn, under the rest", () => {
    // The registry is additive-only and so is the band set: a newer engine's
    // band must not make its rows vanish from an older dashboard's page.
    const story = tellStory([filed("later.type", "a_band_from_a_later_build")]);
    expect(story.rest.map((e) => e.type)).toEqual(["later.type"]);
  });

  test("the order a band lists is the order the turn published", () => {
    // The axis of every band is time; collecting must not reorder it.
    const story = tellStory([
      filed("provider_fallback", "went_wrong", { id: "first" }),
      filed("prefetch_summary", "given"),
      filed("provider_fallback", "went_wrong", { id: "second" }),
    ]);
    expect(story.wentWrong.map((e) => e.id)).toEqual(["first", "second"]);
  });
});

describe("a failed row", () => {
  test("is read from the store's flag or from the payload's own", () => {
    // It reaches the client two ways: the payload's own field while the event
    // is live, and the `failed` tag the store writer stamps, which is all that
    // survives into history. Reading only one makes the same turn red on one
    // surface and clean on another.
    expect(isFailed(event("sandbox_run_completed", { failed: true }))).toBe(true);
    expect(isFailed(event("skill_used", { payload: { failed: true } }))).toBe(true);
    expect(isFailed(event("skill_used"))).toBe(false);
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
    const blocks = prefetchBlocks(
      prefetch({ thread_context_hit: true, thread_context_read: true, thread_context_posts: 12 }),
    );
    const thread = blocks.find((b) => b.label === "The thread so far")!;
    expect(thread.hit).toBe(true);
    expect(thread.note).toBe("12 messages handed over");
  });

  test("the three zero-message thread states read as three different things", () => {
    // Both of the block's zero-message paths render non-empty prose into the
    // prompt, so hit and bytes look identical on a thread that was READ and
    // empty and on one no backend answered for. Reading the first as the
    // second tells an operator that a healthy node cannot reach its own chat
    // surface — which is why the engine reports whether it was read at all.
    const note = (over: Record<string, unknown>) =>
      prefetchBlocks(prefetch(over)).find((b) => b.label === "The thread so far")!.note;

    expect(note({ thread_context_hit: true })).toBe("the thread could not be read from that node");
    expect(note({ thread_context_hit: true, thread_context_read: true })).toBe(
      "read, and there was nothing earlier",
    );
    // And a trigger with no thread at all claims nothing about one.
    expect(note({})).toBe("");
  });

  test("a thread too long to read says the newest messages are missing", () => {
    // The count reads the same on a truncated thread as on a whole one, and
    // the messages that are missing are the NEWEST — the one that woke the
    // turn included. A seat answering a message it never saw is exactly the
    // turn this screen is opened to explain.
    const blocks = prefetchBlocks(
      prefetch({
        thread_context_hit: true,
        thread_context_read: true,
        thread_context_posts: 30,
        thread_context_stopped_short: true,
      }),
    );
    expect(blocks.find((b) => b.label === "The thread so far")!.note).toBe(
      "30 messages handed over, but not the newest — the thread was too long to read",
    );
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
  // A DISTINCT id PER EVENT, because the rows are keyed on it now and a
  // shared id would make the uniqueness case below pass on a build that
  // hands every row the same one.
  let seq = 0;
  function size(
    phase: string,
    iteration: number,
    tokens: number,
    over: Record<string, unknown> = {},
  ): EventRecord {
    seq++;
    return event("prompt.size", {
      id: `ev-size-${seq}`,
      payload: {
        turn_id: "t-1",
        phase,
        iteration,
        approximate_tokens: tokens,
        system_bytes: 24_000,
        user_bytes: 2_800,
        message_bytes: 0,
        tool_bytes: 3_800,
        tool_count: 11,
        ...over,
      },
    });
  }

  /** The re-entry half of a suspended executor: no opening, a parked
      conversation instead. The shape `Runner.Resume` publishes. */
  function reentry(phase: string, iteration: number, tokens: number): EventRecord {
    return size(phase, iteration, tokens, {
      system_bytes: 0,
      user_bytes: 0,
      message_bytes: 3_329,
    });
  }

  test("a turn that ran once gets one row per phase, in the order it published", () => {
    // The control. Without it every case below passes on a build that
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
    expect(rows.every((r) => !r.resumed)).toBe(true);
  });

  test("a suspended executor's two measurements are two rows, not one with a count", () => {
    // THE CASE THE COLLAPSE GOT WRONG. A repeated `phase|iteration` inside
    // ONE turn is never a re-run — a redelivered trigger runs under a new
    // run id (`adr/0017`) and the turn query is `WHERE turn_id = ?`, so the
    // attempts never meet here. It is a SUSPEND: the executor's opening
    // frame, then the parked conversation the resume re-enters with. The
    // figures below are the pair a real suspend and resume published.
    const rows = promptWeights([size("execute", 1, 1_753), reentry("execute", 1, 1_814)]);
    expect(rows).toHaveLength(2);
    // The opening frame SURVIVES. Collapsed, this row was the one that
    // vanished, and with it the only evidence the phase opened at all: the
    // panel showed System 0 B over a 24,000-byte system prompt.
    expect(rows[0]!.systemBytes).toBe(24_000);
    expect(rows[0]!.messageBytes).toBe(0);
    expect(rows[0]!.resumed).toBe(false);
    // And the re-entry says what it is, which is what makes its 0/0 read as
    // a fact about a re-entered phase rather than as a failed render.
    expect(rows[1]!.resumed).toBe(true);
    expect(rows[1]!.systemBytes).toBe(0);
    expect(rows[1]!.messageBytes).toBe(3_329);
  });

  test("every row carries its own event id, because the phase key is not unique", () => {
    // The renderer keys on this. Keyed on `phase|iteration` instead, the two
    // rows above are one duplicate React key and the re-entry's figures
    // reconcile onto the opening's row — the collapse back, by accident.
    const rows = promptWeights([size("execute", 1, 1_753), reentry("execute", 1, 1_814)]);
    expect(new Set(rows.map((r) => r.id)).size).toBe(2);
    expect(rows.every((r) => r.id !== "")).toBe(true);
  });

  test("rows come back in publish order, so they read down the page as the turn ran", () => {
    // A resumed turn re-enters its parked phase and then carries on, so the
    // executor's two halves are not adjacent to the rounds around them by
    // accident — the order they were published in is the order they ran.
    const rows = promptWeights([
      size("execute", 1, 6_807),
      size("review", 1, 2_071),
      size("execute", 2, 7_646),
      reentry("execute", 2, 9_100),
      size("review", 2, 2_400),
    ]);
    expect(rows.map((r) => `${r.phase}|${r.iteration}${r.resumed ? "+resumed" : ""}`)).toEqual([
      "execute|1",
      "review|1",
      "execute|2",
      "execute|2+resumed",
      "review|2",
    ]);
  });

  test("a self-iterate round is its own row, and so is every round before it", () => {
    // Same phase, different iteration: two frames the model genuinely
    // reasoned in, and folding them would hide the growth this panel exists
    // to make visible.
    const rows = promptWeights([size("execute", 1, 6_807), size("execute", 2, 7_646)]);
    expect(rows.map((r) => r.approximateTokens)).toEqual([6_807, 7_646]);
  });

  test("an event with no payload is skipped rather than counted as a zero row", () => {
    expect(promptWeights([event("prompt.size"), event("agent_phase_started")])).toEqual([]);
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
      size("execute", 1, 12_950, { system_bytes: 0, user_bytes: 0, message_bytes: 48_000 }),
    ]) as [PromptWeight];
    expect(w.messageBytes).toBe(48_000);
    expect(w.systemBytes).toBe(0);
    expect(w.userBytes).toBe(0);
    // And that term is ALSO what marks the row a re-entry: the engine ships
    // no flag for it, because a phase that opens its own conversation reports
    // this at 0 by construction.
    expect(w.resumed).toBe(true);
  });

  test("a payload missing a term reads as zero rather than NaN", () => {
    // The parse defaults a missing key to zero — `NaN B` in a column is worse
    // than a zero, because it reads as a broken screen rather than a quiet
    // term.
    const [w] = promptWeights([
      event("prompt.size", {
        payload: { phase: "review", iteration: 1, approximate_tokens: 900, system_bytes: 3_600 },
      }),
    ]) as [PromptWeight];
    expect(w.toolBytes).toBe(0);
    expect(w.toolCount).toBe(0);
    expect(w.messageBytes).toBe(0);
    expect(Number.isNaN(w.toolBytes)).toBe(false);
    // And it reads as an OPENING, which is the safe direction: no message
    // term, no re-entry claimed — claiming one would label a row "resumed" on
    // the strength of a key the writer never wrote.
    expect(w.resumed).toBe(false);
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
