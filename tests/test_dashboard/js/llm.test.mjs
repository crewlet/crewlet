// A failed LLM call must render its failure.
//
// The report this pins: "when an llm turn has failed, the detail doesn't
// show the failure but rather just says no response". Every layer that
// dropped the failure on its way to the screen has a test here.

import assert from "node:assert";
import { test, run } from "./harness.mjs";

const base = new URL("../../../src/crewlet/static/dashboard/js/", import.meta.url);
const { responseBody, failureOf, failureLabel, failureBlock, stripThink } =
  await import(new URL("llm.js", base));
const { recordFromEvent, recordFromLiveCall, recordFromPhase } = await import(
  new URL("records.js", base)
);

test("a failed record renders its error, not a placeholder", () => {
  const record = {
    failed: true,
    error: "All 2 providers exhausted; last error: 429 rate limited",
    error_kind: "rate_limit",
    response: "",
    tool_executions: [],
  };
  const html = responseBody(record.response, record.tool_executions, {
    failure: failureOf(record),
    inProgress: false,
  });
  assert.ok(html.includes("Rate limited"), "the failure class is not named");
  assert.ok(html.includes("429 rate limited"), "the error message is missing");
  assert.ok(!html.includes("No response text yet"), "still claims to be waiting");
});

test("partial output survives alongside the failure", () => {
  const html = responseBody("I looked at the repo and", [], {
    failure: { kind: "timeout", message: "timed out after 600s" },
    inProgress: false,
  });
  assert.ok(html.includes("Timed out"));
  assert.ok(html.includes("I looked at the repo and"), "partial work was dropped");
});

test("a finished call with no text says so, rather than 'yet'", () => {
  const html = responseBody("", [], { inProgress: false });
  assert.ok(html.includes("produced no response text"));
  assert.ok(!html.includes("No response text yet"));
});

test("an in-flight call still reads as in-flight", () => {
  const html = responseBody("", [], { inProgress: true });
  assert.ok(html.includes("No response text yet"));
});

test("failureOf reads both record shapes", () => {
  // History rows carry flat fields; the live projection carries a nested
  // error object. Both have to resolve to the same thing.
  assert.deepStrictEqual(
    failureOf({ failed: true, error: "boom", error_kind: "auth" }),
    { kind: "auth", message: "boom" },
  );
  assert.deepStrictEqual(
    failureOf({ error: { kind: "auth", message: "boom" } }),
    { kind: "auth", message: "boom" },
  );
  assert.strictEqual(failureOf({ response: "fine" }), null);
});

test("an unknown failure class still gets a readable label", () => {
  assert.strictEqual(failureLabel("rate_limit"), "Rate limited");
  assert.strictEqual(failureLabel("some_new_kind"), "Some New Kind");
  assert.strictEqual(failureLabel(""), "Failed");
});

test("the failure block escapes the error text", () => {
  const html = failureBlock({ kind: "fatal", message: "<script>bad()</script>" });
  assert.ok(!html.includes("<script>"), "error text was not escaped");
  assert.ok(html.includes("&lt;script&gt;"));
});

test("a phase record passes the failure through, not a field whitelist", () => {
  // The bug class: four hand-written field lists, each able to drop a
  // field the others kept. These normalizers spread the payload, so a
  // field added upstream arrives without touching them.
  const record = recordFromEvent({
    type: "agent_phase_completed",
    timestamp: "2026-08-18T10:00:00Z",
    payload: {
      phase: "execute",
      failed: true,
      error: "boom",
      error_kind: "connection",
      system_prompt: "sys",
      user_prompt: "usr",
      some_future_field: 42,
    },
  });
  assert.strictEqual(record.failed, true);
  assert.strictEqual(record.error_kind, "connection");
  assert.strictEqual(record.some_future_field, 42, "payload was filtered");
  assert.deepStrictEqual(record.prompt_messages, [
    { role: "system", content: "sys" },
    { role: "user", content: "usr" },
  ]);
});

test("a turn record keeps its decision and failure", () => {
  const record = recordFromEvent({
    type: "agent_turn_completed",
    timestamp: "2026-08-18T10:00:00Z",
    payload: { decision: "failed", failed: true, error: "no provider" },
  });
  assert.strictEqual(record.phase, "turn");
  assert.strictEqual(record.decision, "failed");
  assert.strictEqual(failureOf(record).message, "no provider");
});

test("recordFromEvent ignores events that are not LLM calls", () => {
  assert.strictEqual(recordFromEvent({ type: "task_started", payload: {} }), null);
});

test("a live call that failed is no longer in flight", () => {
  const live = recordFromLiveCall({
    turn_id: "t1",
    phase: "plan",
    in_progress: false,
    failed: true,
    error: { kind: "auth", message: "401" },
  });
  assert.strictEqual(live._live, false);
  assert.strictEqual(live._failed, true);
  assert.strictEqual(failureOf(live).kind, "auth");
});

test("a live call still running is in flight", () => {
  const live = recordFromLiveCall({ turn_id: "t1", phase: "plan", in_progress: true });
  assert.strictEqual(live._live, true);
  assert.strictEqual(failureOf(live), null);
});

test("no live call means no record", () => {
  assert.strictEqual(recordFromLiveCall(null), null);
  assert.strictEqual(recordFromLiveCall({}), null);
});

test("a phase payload with no failure produces no failure", () => {
  const record = recordFromPhase({ phase: "plan", response: "ok" }, "2026-08-18T10:00:00Z");
  assert.strictEqual(failureOf(record), null);
});

// --- Reasoning that streams -------------------------------------------
//
// The engine writes a model's thinking into the response as
// `<think>...</think>` (see events.types.format_reasoning_and_content),
// on the LIVE per-round update as well as the finished phase record.
// The live row and the turn you expand afterwards therefore run the same
// text through the same renderer.

test("reasoning renders as a Reasoning block, live or finished", () => {
  const html = responseBody("<think>Weighing the options.</think>\n\nHere goes.", [], {
    inProgress: true,
  });
  assert.ok(html.includes("Reasoning"), "no reasoning block");
  assert.ok(html.includes("Weighing the options."), "the thinking was dropped");
  assert.ok(html.includes("Here goes."), "the visible answer was dropped");
  assert.ok(!html.includes("&lt;think&gt;"), "the markers leaked into the body");
});

test("reasoning-only output still renders (a thinking model mid-round)", () => {
  const html = responseBody("<think>Still deciding…</think>", [], { inProgress: true });
  assert.ok(html.includes("Reasoning"));
  assert.ok(html.includes("Still deciding…"));
  assert.ok(!html.includes("No response text yet"), "claims nothing arrived");
});

test("stripThink leaves the reasoning text but takes the markers", () => {
  // One-line previews have nowhere to draw a block; they must not print
  // the raw markers either.
  assert.strictEqual(stripThink("<think>why</think>then"), "whythen");
  assert.strictEqual(stripThink("<think>why</think>then", " "), " why then");
  assert.strictEqual(stripThink(null), "");
});

test("a live record keys off its coordinates, not its moving timestamp", () => {
  // `updated_at` advances on every streamed round. If the toggle key
  // moved with it, every round would hand the row a fresh identity and
  // re-open whatever the reader had just collapsed.
  const first = recordFromLiveCall({
    turn_id: "t1",
    phase: "plan",
    iteration: 0,
    updated_at: "2026-08-18T10:00:00Z",
    in_progress: true,
  });
  const later = recordFromLiveCall({
    turn_id: "t1",
    phase: "plan",
    iteration: 0,
    updated_at: "2026-08-18T10:00:09Z",
    in_progress: true,
  });
  assert.strictEqual(first._key, later._key);
  assert.notStrictEqual(first.timestamp, later.timestamp);
});

run();
