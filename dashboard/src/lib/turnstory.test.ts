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
  absorbedGroups,
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

  test("both readers of the rule put an absorbed row in the same band", () => {
    // THE CASE NOTHING HELD, which is how the two drifted apart: `bandOf`
    // answered `rest` for all five of these while `tellStory` filed every one
    // of them under `absorbed` before the call, and no case compared the two
    // answers — so the screen and the function disagreed about every type in
    // the map and the suite stayed green.
    for (const type of Object.keys(ABSORBED)) {
      expect(bandOf(event(type)), type).toBe("absorbed");
      const story = tellStory([event(type)]);
      expect(story.absorbed, type).toHaveLength(1);
      expect(story.rest, type).toHaveLength(0);
    }
  });

  test("a phase record that failed is still its own phase card's", () => {
    // ABSORPTION OUTRANKS THE FAILURE TAXONOMY, which is the one precedence
    // that looks like an exception: the destination draws the failure itself
    // — the phase card carries the error kind and the error text — so banding
    // it `went_wrong` too would put a red row above the card that already
    // says so, and add one to a count of problems that gained no problem.
    expect(bandOf(event("agent_phase_completed", { failed: true }))).toBe("absorbed");
  });

  test("a type that merely collides with Object.prototype is not absorbed", () => {
    // `"toString" in ABSORBED` is TRUE on an object literal, so the `in` this
    // rule used to be written with would absorb such a type to a destination
    // that does not exist and drop it off the screen — the one thing the
    // residual band is here to stop. No type in `internal/events/types`
    // collides today; the point is that the check cannot be what decides it.
    expect(bandOf(event("toString"))).toBe("rest");
    expect(tellStory([event("toString")]).rest).toHaveLength(1);
  });
});

describe("where the absorbed rows went", () => {
  test("grouped by type, counted, and each carrying its destination", () => {
    // BY TYPE, because the destination is the claim and the type is what
    // makes it checkable: `agent_turn_completed` and `turn_completed` share
    // one destination, and merged into a single line a reader cannot tell
    // which pair of records it stands for.
    const groups = absorbedGroups([
      event("agent_phase_started", { timestamp: "1" }),
      event("agent_phase_completed", { timestamp: "2" }),
      event("agent_phase_started", { timestamp: "3" }),
      event("agent_phase_completed", { timestamp: "4" }),
      event("agent_turn_completed", { timestamp: "5" }),
      event("turn_completed", { timestamp: "6" }),
    ]);
    expect(groups.map((g) => [g.type, g.count])).toEqual([
      ["agent_phase_started", 2],
      ["agent_phase_completed", 2],
      ["agent_turn_completed", 1],
      ["turn_completed", 1],
    ]);
    // The words are the map's own, so the screen states the claim this file
    // makes rather than a second paraphrase of it.
    expect(groups.map((g) => g.destination)).toEqual([
      ABSORBED.agent_phase_started,
      ABSORBED.agent_phase_completed,
      ABSORBED.agent_turn_completed,
      ABSORBED.turn_completed,
    ]);
  });

  test("across the whole turn, not only where the repeats are adjacent", () => {
    // The opposite of `collapseRuns`, and deliberately: the axis of a BAND is
    // time and nothing here prints an instant. A turn's starts and records
    // interleave one for one, so a consecutive-only rule would report a group
    // per row and say nothing at all — which is what the case above would
    // read as if this rule were borrowed from that one.
    const groups = absorbedGroups([
      event("agent_phase_started", { timestamp: "1" }),
      event("agent_phase_completed", { timestamp: "2" }),
      event("agent_phase_started", { timestamp: "3" }),
    ]);
    expect(groups).toHaveLength(2);
    expect(groups[0]!.count).toBe(2);
  });

  test("a row that is a row is not in it", () => {
    // Handed a whole turn or handed `Story.absorbed`, it answers the same,
    // because it files on the map rather than on where the caller got the
    // rows.
    const turn = [
      event("agent_phase_started", { timestamp: "1" }),
      event("provider_fallback", { timestamp: "2" }),
      event("episode_written", { timestamp: "3" }),
    ];
    expect(absorbedGroups(turn).map((g) => g.type)).toEqual(["agent_phase_started"]);
    expect(absorbedGroups(tellStory(turn).absorbed).map((g) => g.type)).toEqual([
      "agent_phase_started",
    ]);
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

describe("a band entry is a query predicate", () => {
  // Every row these sets sort came from `EventLog.Turn`, which is
  // `WHERE turn_id = ?`. Eight types were banded whose payloads declare no
  // `turn_id` key at all, so they could never be in the answer and each band
  // failed EMPTY — the one way a panel cannot say it is broken.
  //
  // These cases pin what the screen does with them NOW, and the wire half is
  // held by `internal/events/types/turnbands_client_test.go`: this file cannot
  // read a Go struct tag, so it asserts the banding and the engine asserts the
  // key. Neither half is the whole claim on its own.

  test("an A2A ask is not claimed by 'What else it did'", () => {
    // The band advertised "colleagues" for these three and an ask has never
    // been drawn under that heading on any turn this engine has run: none of
    // the three carries a turn id, so the query never returns one. Banding
    // them again is a promise the wire cannot keep — until the engine stamps
    // the id, which the Go gate's roster is what announces.
    for (const type of ["a2a_channel_opened", "a2a_message_sent", "a2a_channel_closed"]) {
      expect(bandOf(event(type)), type).toBe("rest");
    }
  });

  test("the scheduler's cron fire is the trigger, not work the turn did", () => {
    // `task_assigned` is published by internal/schedule to WAKE a seat, so it
    // precedes every turn id there could be. It is already on the screen as
    // the brief.
    expect(bandOf(event("task_assigned"))).toBe("rest");
  });

  test("a record that no turn ran is not this turn's failure", () => {
    // Both say a delivery was never worked — one at the dispatcher, one at
    // the notification gate — so neither names a turn, and both sat in
    // "What went wrong" contributing nothing to a count the header prints.
    expect(bandOf(event("turn_trigger_skipped"))).toBe("rest");
    expect(bandOf(event("notification_skipped"))).toBe("rest");
  });

  test("work spanning many turns is not what THIS turn left behind", () => {
    // The curator duty promotes a unit's skill off a cluster of many seats'
    // turns, so there is no single right turn to name.
    expect(bandOf(event("skill_promoted"))).toBe("rest");
  });

  test("but a failed one is still loud, exactly as an unknown type is", () => {
    // Falling through to `rest` is about the BAND, never about the weight: the
    // failure taxonomy is the engine's and outranks every set in this file, so
    // a de-banded type that comes back failed is still what went wrong.
    expect(bandOf(event("skill_telemetry_write_failed", { failed: true }))).toBe("went_wrong");
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
        system_chars: 24_000,
        user_chars: 2_800,
        message_chars: 0,
        tool_chars: 3_800,
        tool_count: 11,
        ...over,
      },
    });
  }

  /** The re-entry half of a suspended executor: no opening, a parked
      conversation instead. The shape `Runner.Resume` publishes. */
  function reentry(phase: string, iteration: number, tokens: number): EventRecord {
    return size(phase, iteration, tokens, {
      system_chars: 0,
      user_chars: 0,
      message_chars: 3_329,
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
    // And that term is ALSO what marks the row a re-entry: the engine ships
    // no flag for it, because a phase that opens its own conversation reports
    // this at 0 by construction.
    expect(w.resumed).toBe(true);
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
    // And it reads as an OPENING, which is the safe direction: an absent
    // message term is a build that measured none, so there is no re-entry to
    // claim — and claiming one would label a row "resumed" on the strength of
    // a key the writer never wrote.
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
