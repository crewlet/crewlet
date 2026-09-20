// @vitest-environment node
/**
 * The dry-run check: its requests, what an answer means, the state machine,
 * the save rules, and the driver under a fake clock and a scripted engine.
 *
 * What these protect: a check sends exactly the write a save would, without a
 * summary; only the answer for the current generation is used; a superseded
 * request is aborted and its late answer dropped; a burst of changes sends one
 * check; an unreachable engine is retried with growing delays that changes do
 * not shortcut; a conflict, a refused token and a read-only process stop
 * checking until a reset; and saving is refused, held or allowed per status.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { fromDocument, toDocument, type IndexedDocument } from "./document.ts";
import {
  CHECK_BACKOFF_BASE_MS,
  CHECK_BACKOFF_MAX_MS,
  CHECK_DEBOUNCE_MS,
  CheckRunner,
  INITIAL_CHECK,
  backoffDelay,
  classifyCheck,
  isCurrentAnswer,
  saveRules,
  transition,
  type CheckState,
  type SettledCheck,
} from "./scheduler.ts";
import {
  checkRequest,
  etagOfRevision,
  revisionOfEtag,
  saveRequest,
  type Clock,
  type ConfigRequest,
  type ConfigTransport,
  type HttpAnswer,
} from "./transport.ts";
import { fixtureCompany, fixtureDerived } from "./testkit.ts";

describe("requests", () => {
  const base = fixtureCompany();
  const draftDoc = (): IndexedDocument => {
    const changed: CompanyDocument = { ...base, mission: "Changed" };
    return toDocument(fromDocument(changed, null));
  };

  test("an edit-mode check is the merge patch a save sends, conditional on the base, without a summary", () => {
    const sent = draftDoc();
    const check = checkRequest({ mode: "edit", baseRevision: "rev-1", base, sent });
    expect(check).toEqual({
      method: "PATCH",
      path: "/config",
      query: { dry_run: "true" },
      contentType: "application/merge-patch+json",
      headers: { "If-Match": '"rev-1"' },
      body: { mission: "Changed" },
    });
    const save = saveRequest(
      { mode: "edit", baseRevision: "rev-1", base, sent },
      "Update the mission (write abc)",
    );
    expect(save).toEqual({
      ...check,
      query: {},
      body: { mission: "Changed", _summary: "Update the mission (write abc)" },
    });
  });

  test("a create-mode check puts the whole document, only where no company exists", () => {
    const sent = toDocument(fromDocument({ name: "New", roles: [{ name: "A" }] }, null));
    expect(checkRequest({ mode: "create", baseRevision: null, base: null, sent })).toEqual({
      method: "PUT",
      path: "/config",
      query: { dry_run: "true" },
      contentType: "application/json",
      headers: { "If-None-Match": "*" },
      body: { name: "New", roles: [{ name: "A" }] },
    });
  });

  test("an edit-mode request without its base is a defect", () => {
    expect(() =>
      checkRequest({ mode: "edit", baseRevision: null, base: null, sent: draftDoc() }),
    ).toThrow(RangeError);
  });

  test("entity tags name revisions the way the engine matches them", () => {
    expect(etagOfRevision("abc")).toBe('"abc"');
    expect(revisionOfEtag('"abc"')).toBe("abc");
    expect(revisionOfEtag('W/"abc"')).toBe("abc");
    expect(revisionOfEtag("abc")).toBe("abc");
    expect(revisionOfEtag('""')).toBeNull();
    expect(revisionOfEtag(null)).toBeNull();
  });
});

describe("classifyCheck", () => {
  const derived = fixtureDerived(fixtureCompany());

  test("a dry run on the draft's base is clean, carrying its warnings and derivation", () => {
    expect(
      classifyCheck(
        { status: 200, body: { valid: true, base_revision_id: "r1", warnings: null, derived } },
        "edit",
        "r1",
      ),
    ).toEqual({ status: "clean", warnings: [], derived });
  });

  test("a dry run that validated against another base is a conflict", () => {
    expect(
      classifyCheck({ status: 200, body: { valid: true, base_revision_id: "r2" } }, "edit", "r1"),
    ).toEqual({
      status: "conflict",
      reason: "base_moved",
      currentRevisionId: "r2",
    });
    expect(
      classifyCheck({ status: 200, body: { valid: true, base_revision_id: "r2" } }, "create", null),
    ).toMatchObject({
      status: "conflict",
      reason: "already_configured",
    });
    expect(
      classifyCheck({ status: 200, body: { valid: true, base_revision_id: "" } }, "create", null),
    ).toMatchObject({
      status: "clean",
    });
  });

  test("refusals map to the states that halt, and failures to unreachable", () => {
    const cases: [HttpAnswer, unknown][] = [
      [{ status: 401, body: { error: "unauthorized" } }, { status: "guarded" }],
      [{ status: 403, body: {} }, { status: "guarded" }],
      [
        { status: 503, body: { error: "draining", detail: "restarting" } },
        { status: "unreachable", detail: "restarting" },
      ],
      [
        { status: 0, body: { error: "unreachable" } },
        { status: "unreachable", detail: "unreachable" },
      ],
      [
        { status: 502, body: { error: "unreadable_body" } },
        { status: "unreachable", detail: "unreadable_body" },
      ],
      [
        { status: 409, body: { error: "revision_advanced", current_revision_id: "r9" } },
        { status: "conflict", reason: "revision_advanced", currentRevisionId: "r9" },
      ],
      [
        { status: 412, body: { error: "already_configured", current_revision_id: "r3" } },
        { status: "conflict", reason: "already_configured", currentRevisionId: "r3" },
      ],
      [
        { status: 409, body: { error: "no_active_revision" } },
        { status: "conflict", reason: "no_active_revision", currentRevisionId: null },
      ],
    ];
    for (const [answer, expected] of cases)
      expect(classifyCheck(answer, "edit", "r1"), JSON.stringify(answer)).toEqual(expected);
  });

  test("a refused document carries its problems, and a refusal without any is given one from its detail", () => {
    const problem = {
      path: "roles[0].name",
      segments: ["roles", 0, "name"],
      kind: "missing",
      message: "role 0: name is required",
    };
    expect(
      classifyCheck(
        {
          status: 400,
          body: {
            error: "validation_error",
            detail: "x",
            hint: "fix it",
            problems: [problem],
            derived,
          },
        },
        "edit",
        "r1",
      ),
    ).toEqual({
      status: "problems",
      problems: [problem],
      derived,
      code: "validation_error",
      hint: "fix it",
    });
    expect(
      classifyCheck(
        { status: 413, body: { error: "body_too_large", detail: "the body is over the limit" } },
        "edit",
        "r1",
      ),
    ).toEqual({
      status: "problems",
      problems: [
        { path: "", segments: null, kind: "invalid", message: "the body is over the limit" },
      ],
      derived: null,
      code: "body_too_large",
      hint: "",
    });
  });
});

describe("transition", () => {
  const T0 = 1_000;
  const loaded = transition(INITIAL_CHECK, { type: "reset", generation: 1, now: T0 });

  test("a reset checks at once, without waiting for the debounce", () => {
    expect(loaded.effects).toEqual([{ type: "send", generation: 1, request: 1 }]);
    expect(loaded.state).toMatchObject({
      generation: 1,
      status: "checking",
      inFlight: { generation: 1, request: 1 },
    });
  });

  test("a change aborts the superseded request and waits out the debounce", () => {
    const changed = transition(loaded.state, { type: "changed", generation: 2, now: T0 + 10 });
    expect(changed.effects).toEqual([
      { type: "abort", request: 1 },
      { type: "wake", at: T0 + 10 + CHECK_DEBOUNCE_MS },
    ]);
    expect(changed.state).toMatchObject({ generation: 2, inFlight: null, status: "checking" });

    const early = transition(changed.state, { type: "timer", now: T0 + 100 });
    expect(early.effects).toEqual([{ type: "wake", at: T0 + 10 + CHECK_DEBOUNCE_MS }]);
    const due = transition(changed.state, { type: "timer", now: T0 + 10 + CHECK_DEBOUNCE_MS });
    expect(due.effects).toEqual([{ type: "send", generation: 2, request: 2 }]);
    expect(due.state.inFlight).toEqual({ generation: 2, request: 2 });
  });

  test("an answer for a request that is no longer in flight is dropped", () => {
    const changed = transition(loaded.state, { type: "changed", generation: 2, now: T0 });
    const late = transition(changed.state, {
      type: "settled",
      request: 1,
      status: "clean",
      now: T0 + 5,
    });
    expect(late.state).toBe(changed.state);
  });

  test("a reset of the same generation replaces the request in flight, and the old answer is not the new one", () => {
    const again = transition(loaded.state, { type: "reset", generation: 1, now: T0 + 1 });
    expect(again.effects).toEqual([
      { type: "abort", request: 1 },
      { type: "send", generation: 1, request: 2 },
    ]);
    expect(isCurrentAnswer(again.state, 1)).toBe(false);
    expect(isCurrentAnswer(again.state, 2)).toBe(true);
    const old = transition(again.state, {
      type: "settled",
      request: 1,
      status: "guarded",
      now: T0 + 2,
    });
    expect(old.state).toBe(again.state);
  });

  test("the answer for the current generation settles the status", () => {
    const settled = transition(loaded.state, {
      type: "settled",
      request: 1,
      status: "problems",
      now: T0 + 5,
    });
    expect(settled.state).toMatchObject({
      status: "problems",
      inFlight: null,
      failures: 0,
      halted: false,
    });
    expect(settled.effects).toEqual([]);
  });

  test("an unreachable engine is retried with doubling delays up to the cap, and changes ride the retry", () => {
    let state: CheckState = loaded.state;
    let now = T0;
    const delays: number[] = [];
    for (let failure = 1; failure <= 8; failure++) {
      const settled = transition(state, {
        type: "settled",
        request: state.inFlight!.request,
        status: "unreachable",
        now,
      });
      const wake = settled.effects.find((e) => e.type === "wake");
      if (wake?.type !== "wake") throw new Error("no retry scheduled");
      delays.push(wake.at - now);
      state = settled.state;
      expect(state.status).toBe("unreachable");

      const changed = transition(state, {
        type: "changed",
        generation: state.generation + 1,
        now: now + 1,
      });
      expect(changed.effects).toEqual([]);
      expect(changed.state.dueAt).toBe(wake.at);
      expect(changed.state.status).toBe("unreachable");
      state = changed.state;

      now = wake.at;
      const retried = transition(state, { type: "timer", now });
      expect(retried.effects).toEqual([
        { type: "send", generation: state.generation, request: state.requests + 1 },
      ]);
      state = retried.state;
    }
    expect(delays).toEqual([1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 30_000]);
    const recovered = transition(state, {
      type: "settled",
      request: state.inFlight!.request,
      status: "clean",
      now,
    });
    expect(recovered.state.failures).toBe(0);
  });

  test("a conflict and a refused token halt checking until a reset", () => {
    for (const status of ["conflict", "guarded"] as const) {
      const halted = transition(loaded.state, { type: "settled", request: 1, status, now: T0 });
      expect(halted.state.halted, status).toBe(true);
      const changed = transition(halted.state, { type: "changed", generation: 2, now: T0 + 1 });
      expect(changed.effects, status).toEqual([]);
      expect(changed.state.status, status).toBe(status);
      const reset = transition(changed.state, { type: "reset", generation: 3, now: T0 + 2 });
      expect(reset.effects, status).toEqual([{ type: "send", generation: 3, request: 2 }]);
      expect(reset.state.halted, status).toBe(false);
    }
  });

  test("backoffDelay is anchored to its constants", () => {
    expect(backoffDelay(1)).toBe(CHECK_BACKOFF_BASE_MS);
    expect(backoffDelay(2)).toBe(2 * CHECK_BACKOFF_BASE_MS);
    expect(backoffDelay(1_000)).toBe(CHECK_BACKOFF_MAX_MS);
  });
});

describe("saveRules", () => {
  test("refuses, holds or allows saving per status", () => {
    expect(saveRules("clean", true)).toEqual({
      review: true,
      save: true,
      waiting: false,
      reason: null,
    });
    expect(saveRules("unreachable", true)).toEqual({
      review: true,
      save: true,
      waiting: false,
      reason: null,
    });
    expect(saveRules("checking", true)).toEqual({
      review: true,
      save: false,
      waiting: true,
      reason: null,
    });
    expect(saveRules("problems", true)).toEqual({
      review: true,
      save: false,
      waiting: false,
      reason: "Fix the problems above first.",
    });
    for (const status of ["conflict", "guarded"] as const) {
      expect(saveRules(status, true), status).toMatchObject({ review: false, save: false });
    }
    expect(saveRules("clean", false)).toMatchObject({ review: false, save: false });
  });
});

// ---------------------------------------------------------------------------
// The driver
// ---------------------------------------------------------------------------

class FakeClock implements Clock {
  time = 0;
  private timers: { at: number; fn: () => void; id: number }[] = [];
  private nextId = 0;
  now() {
    return this.time;
  }
  setTimer(fn: () => void, ms: number) {
    const id = ++this.nextId;
    this.timers.push({ at: this.time + ms, fn, id });
    return () => {
      this.timers = this.timers.filter((t) => t.id !== id);
    };
  }
  advance(ms: number) {
    const until = this.time + ms;
    for (;;) {
      this.timers.sort((a, b) => a.at - b.at);
      const next = this.timers[0];
      if (!next || next.at > until) break;
      this.timers.shift();
      this.time = next.at;
      next.fn();
    }
    this.time = until;
  }
}

interface Pending {
  request: ConfigRequest;
  signal: AbortSignal;
  resolve: (answer: HttpAnswer) => void;
  reject: (err: unknown) => void;
}

class ScriptedTransport implements ConfigTransport {
  pending: Pending[] = [];
  send(request: ConfigRequest, signal: AbortSignal): Promise<HttpAnswer> {
    return new Promise((resolve, reject) => {
      this.pending.push({ request, signal, resolve, reject });
      signal.addEventListener("abort", () => reject(signal.reason));
    });
  }
  current(): Promise<HttpAnswer> {
    throw new Error("not used by the runner");
  }
  revision(): Promise<HttpAnswer> {
    throw new Error("not used by the runner");
  }
}

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

function harness() {
  const clock = new FakeClock();
  const transport = new ScriptedTransport();
  const settled: SettledCheck[] = [];
  let generation = 0;
  const runner = new CheckRunner({
    clock,
    transport,
    prepare: (g) => {
      if (g !== generation) return null;
      const sent = toDocument(fromDocument({ name: `generation ${g}` }, null));
      return {
        request: checkRequest({ mode: "create", baseRevision: null, base: null, sent }),
        sent,
        mode: "create",
        baseRevision: null,
      };
    },
    onSettled: (s) => settled.push(s),
  });
  const change = () => runner.changed(++generation);
  const reset = () => runner.reset(++generation);
  return { clock, transport, settled, runner, change, reset };
}

describe("CheckRunner", () => {
  test("a burst of changes sends one check, for the last generation", async () => {
    const { clock, transport, settled, change, reset } = harness();
    reset();
    expect(transport.pending).toHaveLength(1);
    transport.pending[0]!.resolve({ status: 200, body: { valid: true } });
    await flush();
    expect(settled.map((s) => s.generation)).toEqual([1]);

    change();
    clock.advance(100);
    change();
    clock.advance(100);
    change();
    clock.advance(CHECK_DEBOUNCE_MS - 1);
    expect(transport.pending).toHaveLength(1);
    clock.advance(1);
    expect(transport.pending).toHaveLength(2);
    expect(transport.pending[1]!.request.body).toEqual({ name: "generation 4" });
  });

  test("a superseded check is aborted, and its answer never reaches the reducer", async () => {
    const { clock, transport, settled, change, reset } = harness();
    reset();
    const first = transport.pending[0]!;
    change();
    expect(first.signal.aborted).toBe(true);
    first.resolve({ status: 200, body: { valid: true } });
    await flush();
    expect(settled).toEqual([]);
    clock.advance(CHECK_DEBOUNCE_MS);
    transport.pending[1]!.resolve({
      status: 400,
      body: {
        error: "validation_error",
        problems: [{ path: "name", segments: ["name"], kind: "missing", message: "name" }],
      },
    });
    await flush();
    expect(settled).toHaveLength(1);
    expect(settled[0]).toMatchObject({ generation: 2, outcome: { status: "problems" } });
    expect(settled[0]!.sent.document).toEqual({ name: "generation 2" });
  });

  test("an unreachable engine is retried after the backoff, not at the next change", async () => {
    const { clock, transport, runner, change, reset } = harness();
    reset();
    transport.pending[0]!.resolve({ status: 0, body: { error: "unreachable" } });
    await flush();
    expect(runner.state.status).toBe("unreachable");
    change();
    clock.advance(CHECK_BACKOFF_BASE_MS - 1);
    expect(transport.pending).toHaveLength(1);
    clock.advance(1);
    expect(transport.pending).toHaveLength(2);
    expect(transport.pending[1]!.request.body).toEqual({ name: "generation 2" });
  });

  test("a reset of the same generation, as a token change sends, never delivers the replaced answer", async () => {
    const { transport, settled, runner, reset } = harness();
    reset();
    const first = transport.pending[0]!;
    runner.reset(1);
    expect(first.signal.aborted).toBe(true);
    first.resolve({ status: 401, body: { error: "unauthorized" } });
    transport.pending[1]!.resolve({ status: 200, body: { valid: true } });
    await flush();
    expect(settled.map((s) => s.outcome.status)).toEqual(["clean"]);
  });

  test("a transport that rejects without an abort reports the check unanswered rather than stalling", async () => {
    const { clock, transport, settled, runner, reset } = harness();
    reset();
    transport.pending[0]!.reject(new Error("socket hang up"));
    await flush();
    expect(settled[0]).toMatchObject({
      outcome: { status: "unreachable", detail: "socket hang up" },
    });
    clock.advance(CHECK_BACKOFF_BASE_MS);
    expect(transport.pending).toHaveLength(2);
    runner.dispose();
    expect(transport.pending[1]!.signal.aborted).toBe(true);
  });
});
