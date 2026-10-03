// @vitest-environment node
/**
 * The check: its requests, what an answer means, the state machine, the save
 * rules, and the driver under a fake clock and a scripted engine.
 *
 * What these protect: the settings check is exactly the write a save would
 * send, without a summary, and a draft that changes no setting reads them
 * instead so a newer revision is still a conflict; the chart is judged on its ROWS against the base,
 * so a read that only moved the position is no conflict, and in create mode on
 * whether it is still empty; a halting answer from either request outranks
 * everything; only the answer for the current generation is used; a
 * superseded request is aborted and its late answer dropped; a burst of
 * changes sends one check; an unreachable engine is retried with growing
 * delays that changes do not shortcut; a conflict and a refused credential
 * stop checking until a reset; and saving is refused, held or allowed per
 * status.
 */

import { describe, expect, test } from "vitest";
import type { ChartRead } from "~/protocol/index.ts";
import { RETRY_AFTER_MAX_MS } from "~/contract/retry.ts";
import { UNANSWERED_RETRY_BASE_MS, UNANSWERED_RETRY_MAX_MS } from "~/protocol/retry.ts";
import { chartPrint, fingerprint, fromChart } from "./document.ts";
import { EMPTY_DRAFT } from "./draft.ts";
import {
  CHECK_DEBOUNCE_MS,
  CheckRunner,
  INITIAL_CHECK,
  classifyChart,
  classifySettings,
  classifySettingsRead,
  combineCheck,
  isCurrentAnswer,
  saveRules,
  transition,
  type CheckState,
  type SettledCheck,
} from "./scheduler.ts";
import {
  etagOfRevision,
  revisionOfEtag,
  settingsCheckRequest,
  settingsSaveRequest,
  type Clock,
  type EngineRequest,
  type EngineTransport,
  type HttpAnswer,
} from "./transport.ts";
import { chartOf, fixtureChart, fixtureSettings } from "./testkit.ts";
import type { PlacedProblem } from "./problems.ts";

describe("requests", () => {
  const base = fixtureSettings();

  test("an edit-mode check is the merge patch a save sends, conditional on the base, without a summary", () => {
    const inputs = {
      mode: "edit" as const,
      baseRevision: "rev-1",
      base,
      draft: { ...base, mission: "Changed" },
    };
    const check = settingsCheckRequest(inputs);
    expect(check).toEqual({
      method: "PATCH",
      path: "/config",
      query: { dry_run: "true" },
      contentType: "application/merge-patch+json",
      headers: { "If-Match": '"rev-1"' },
      body: { mission: "Changed" },
    });
    expect(settingsSaveRequest(inputs, "Update the mission (write abc)")).toEqual({
      ...check,
      query: {},
      body: { mission: "Changed", _summary: "Update the mission (write abc)" },
    });
  });

  test("a create-mode check puts the whole settings document, only where no company exists", () => {
    expect(
      settingsCheckRequest({
        mode: "create",
        baseRevision: null,
        base: null,
        draft: { name: "New" },
      }),
    ).toEqual({
      method: "PUT",
      path: "/config",
      query: { dry_run: "true" },
      contentType: "application/json",
      headers: { "If-None-Match": "*" },
      body: { name: "New" },
    });
  });

  test("an edit-mode request without its base is a defect", () => {
    expect(() =>
      settingsCheckRequest({ mode: "edit", baseRevision: null, base: null, draft: {} }),
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

describe("what the chart read says", () => {
  const chart = fixtureChart();
  const print = fingerprint(chartPrint(chart));

  test("the same rows are no conflict, however far the position moved", () => {
    const later: ChartRead = { ...chart, answer: { level: "linearizable", position: "L@1:999" } };
    expect(classifyChart({ status: 200, body: later }, "edit", print)).toBeNull();
    // Control: other rows are somebody else's save.
    const other: ChartRead = {
      ...chart,
      seats: chart.seats.map((s) => (s.handle === "dev" ? { ...s, goal: "Ship" } : s)),
    };
    expect(classifyChart({ status: 200, body: other }, "edit", print)).toEqual({
      status: "conflict",
      reason: "chart_moved",
      currentRevisionId: null,
    });
  });

  test("in create mode the chart must still be empty", () => {
    expect(classifyChart({ status: 200, body: chartOf({}) }, "create", "")).toBeNull();
    expect(classifyChart({ status: 200, body: chart }, "create", "")).toMatchObject({
      status: "conflict",
      reason: "chart_exists",
    });
  });

  test("a refused read is guarded with the grants it named, and any other failure unreachable", () => {
    expect(
      classifyChart(
        { status: 403, body: { error: "unauthorized", grants: ["state:read", 7] } },
        "edit",
        print,
      ),
    ).toEqual({ status: "guarded", grants: ["state:read"] });
    expect(classifyChart({ status: 401, body: {} }, "edit", print)).toEqual({
      status: "guarded",
      grants: [],
    });
    expect(classifyChart({ status: 0, body: null }, "edit", print)).toEqual({
      status: "unreachable",
      detail: "The engine could not be reached.",
      retryAfter: null,
    });
    expect(
      classifyChart(
        { status: 503, body: { error: "unavailable", detail: "behind the log" } },
        "edit",
        print,
      ),
    ).toEqual({ status: "unreachable", detail: "behind the log", retryAfter: null });
  });

  // THE ENGINE'S HINT RIDES THE OUTCOME, a zero included: it is what the
  // check waits instead of its own backoff.
  test.each([12, 0])("a 503 the engine wrote carries its hint of %i seconds", (retryAfter) => {
    expect(
      classifyChart(
        { status: 503, body: { error: "behind", detail: "behind the log" }, retryAfter },
        "edit",
        print,
      ),
    ).toEqual({ status: "unreachable", detail: "behind the log", retryAfter });
  });
});

describe("what the settings dry run says", () => {
  test("a dry run on the draft's base is taken, carrying its warnings", () => {
    const warnings = [{ kind: "unused", message: "w" }];
    expect(
      classifySettings(
        { status: 200, body: { valid: true, base_revision_id: "r1", warnings } },
        "edit",
        "r1",
      ),
    ).toEqual({ kind: "taken", warnings });
  });

  test("a dry run that validated against another base is a conflict", () => {
    expect(
      classifySettings(
        { status: 200, body: { valid: true, base_revision_id: "r2" } },
        "edit",
        "r1",
      ),
    ).toEqual({
      kind: "outcome",
      outcome: { status: "conflict", reason: "base_moved", currentRevisionId: "r2" },
    });
    expect(
      classifySettings(
        { status: 200, body: { valid: true, base_revision_id: "r2" } },
        "create",
        null,
      ),
    ).toMatchObject({ kind: "outcome", outcome: { reason: "already_configured" } });
    expect(
      classifySettings(
        { status: 200, body: { valid: true, base_revision_id: "" } },
        "create",
        null,
      ),
    ).toMatchObject({ kind: "taken" });
  });

  test("refusals map to the states that halt, failures to unreachable, and a document's problems are carried", () => {
    const outcome = (answer: HttpAnswer) => classifySettings(answer, "edit", "r1");
    expect(outcome({ status: 403, body: { grants: ["config:write"] } })).toEqual({
      kind: "outcome",
      outcome: { status: "guarded", grants: ["config:write"] },
    });
    expect(
      outcome({ status: 409, body: { error: "revision_advanced", current_revision_id: "r9" } }),
    ).toEqual({
      kind: "outcome",
      outcome: { status: "conflict", reason: "revision_advanced", currentRevisionId: "r9" },
    });
    expect(outcome({ status: 409, body: { error: "no_active_revision" } })).toMatchObject({
      outcome: { reason: "no_active_revision" },
    });
    expect(outcome({ status: 503, body: { error: "draining", detail: "restarting" } })).toEqual({
      kind: "outcome",
      outcome: { status: "unreachable", detail: "restarting", retryAfter: null },
    });
    expect(
      outcome({ status: 503, body: { error: "draining", detail: "restarting" }, retryAfter: 30 }),
    ).toEqual({
      kind: "outcome",
      outcome: { status: "unreachable", detail: "restarting", retryAfter: 30 },
    });
    const problem = {
      path: "name",
      segments: ["name"],
      kind: "missing",
      message: "name is required",
    };
    expect(
      outcome({
        status: 400,
        body: { error: "validation_error", hint: "fix it", problems: [problem] },
      }),
    ).toEqual({ kind: "refused", problems: [problem], code: "validation_error", hint: "fix it" });
    expect(outcome({ status: 413, body: { error: "body_too_large", detail: "too big" } })).toEqual({
      kind: "refused",
      problems: [{ path: "", segments: null, kind: "invalid", message: "too big" }],
      code: "body_too_large",
      hint: "",
    });
  });
});

describe("what the settings read says", () => {
  // A DRAFT THAT CHANGES NO SETTING STILL HEARS OF A NEWER REVISION. There is
  // nothing to dry-run, and a draft nobody touched must still stand on the
  // charter a colleague saved.
  test("the base revision says nothing, and a newer one is the dry run's own conflict", () => {
    expect(classifySettingsRead({ status: 200, body: {}, etag: '"r1"' }, "r1")).toBeNull();
    expect(classifySettingsRead({ status: 200, body: {}, etag: 'W/"r2"' }, "r1")).toEqual({
      kind: "outcome",
      outcome: { status: "conflict", reason: "revision_advanced", currentRevisionId: "r2" },
    });
    expect(
      classifySettingsRead({ status: 404, body: { error: "no_active_revision" } }, "r1"),
    ).toMatchObject({ outcome: { status: "conflict", reason: "no_active_revision" } });
  });

  test("a refused read is guarded, and every other failure unreachable", () => {
    expect(classifySettingsRead({ status: 403, body: { grants: ["config:read"] } }, "r1")).toEqual({
      kind: "outcome",
      outcome: { status: "guarded", grants: ["config:read"] },
    });
    expect(classifySettingsRead({ status: 0, body: null }, "r1")).toEqual({
      kind: "outcome",
      outcome: {
        status: "unreachable",
        detail: "The engine could not be reached.",
        retryAfter: null,
      },
    });
    expect(
      classifySettingsRead({ status: 503, body: { error: "unavailable" }, retryAfter: 2 }, "r1"),
    ).toMatchObject({ outcome: { status: "unreachable", retryAfter: 2 } });
    // A 404 that is not the engine's own word for "no company" is a process
    // that does not serve the configuration, never a company that went away.
    expect(classifySettingsRead({ status: 404, body: { error: "not_found" } }, "r1")).toMatchObject(
      { outcome: { status: "unreachable" } },
    );
  });
});

describe("combining a check", () => {
  const warning: PlacedProblem = {
    severity: "warning",
    kind: "dangling_reference",
    message: "w",
    node: "seat:dev",
    field: ["manages", 0],
    link: null,
    source: { path: "", segments: null, kind: "dangling_reference", message: "w" },
  };
  const problem: PlacedProblem = { ...warning, severity: "problem", kind: "invalid" };

  test("a halting answer from either request outranks everything, the credential first", () => {
    const conflict = {
      status: "conflict",
      reason: "chart_moved",
      currentRevisionId: null,
    } as const;
    const guarded = { kind: "outcome", outcome: { status: "guarded", grants: [] } } as const;
    expect(combineCheck(conflict, guarded, [problem])).toEqual({ status: "guarded", grants: [] });
    expect(combineCheck(conflict, null, [problem])).toBe(conflict);
    expect(
      combineCheck(
        null,
        { kind: "outcome", outcome: { status: "unreachable", detail: "x", retryAfter: null } },
        [],
      ),
    ).toEqual({ status: "unreachable", detail: "x", retryAfter: null });
  });

  // THE NEXT CHECK ASKS BOTH REQUESTS AGAIN, so it waits for whichever said
  // it needs longer — and never, when either said waiting will not change it.
  // A request nobody answered says nothing about the one the engine did.
  test.each([
    [12, 30, 30],
    [12, null, 12],
    [null, 0, 0],
    [30, 0, 0],
    [null, null, null],
  ])("a chart hint of %s and a settings hint of %s wait %s", (chart, settings, want) => {
    const unreachable = (retryAfter: number | null) =>
      ({ status: "unreachable", detail: "x", retryAfter }) as const;
    expect(
      combineCheck(unreachable(chart), { kind: "outcome", outcome: unreachable(settings) }, []),
    ).toEqual({ status: "unreachable", detail: "x", retryAfter: want });
  });

  test("the draft's own problems and the settings' findings are judged together", () => {
    expect(combineCheck(null, null, [warning])).toEqual({ status: "clean", findings: [warning] });
    expect(combineCheck(null, null, [problem])).toMatchObject({ status: "problems", code: "" });
    const refused = combineCheck(
      null,
      {
        kind: "refused",
        problems: [{ path: "name", segments: ["name"], kind: "missing", message: "name" }],
        code: "validation_error",
        hint: "fix it",
      },
      [],
    );
    expect(refused).toMatchObject({ status: "problems", code: "validation_error", hint: "fix it" });
    expect(refused.status === "problems" && refused.findings[0]?.node).toBe("company");
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

  // THE ENGINE SAID WHEN, so the retry waits exactly that rather than the
  // backoff, which asked a node twelve seconds behind at one, two, four and
  // eight seconds first — and bounded, like every hint.
  test.each([
    [12, 12_000],
    [600, RETRY_AFTER_MAX_MS],
  ])("an unreachable answer hinting %i seconds is retried then", (retryAfter, wait) => {
    const settled = transition(loaded.state, {
      type: "settled",
      request: 1,
      status: "unreachable",
      now: T0,
      retryAfter,
    });
    expect(settled.effects).toEqual([{ type: "wake", at: T0 + wait }]);
    expect(settled.state).toMatchObject({ status: "unreachable", dueAt: T0 + wait });
  });

  // AND A ZERO IS NOT RETRIED ON A TIMER AT ALL: the engine said waiting will
  // not change it. A change to the draft is then a fresh question, asked at
  // the debounce like any other, rather than a retry nothing scheduled.
  test("an unreachable answer no wait clears schedules nothing, and a change asks afresh", () => {
    const settled = transition(loaded.state, {
      type: "settled",
      request: 1,
      status: "unreachable",
      now: T0,
      retryAfter: 0,
    });
    expect(settled.effects).toEqual([]);
    expect(settled.state).toMatchObject({
      status: "unreachable",
      dueAt: null,
      failures: 0,
      halted: false,
    });
    const idle = transition(settled.state, {
      type: "timer",
      now: T0 + 10 * UNANSWERED_RETRY_MAX_MS,
    });
    expect(idle.effects).toEqual([]);
    const changed = transition(settled.state, { type: "changed", generation: 2, now: T0 + 5 });
    expect(changed.effects).toEqual([{ type: "wake", at: T0 + 5 + CHECK_DEBOUNCE_MS }]);
    expect(changed.state.status).toBe("checking");
  });

  test("a conflict and a refused credential halt checking until a reset", () => {
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
});

describe("saveRules", () => {
  test("refuses, holds or allows saving per status", () => {
    const open = { review: true, save: true, waiting: false, reason: null };
    expect(saveRules("clean", true)).toEqual(open);
    // A save reads the chart again and every write is decided where it lands.
    expect(saveRules("unreachable", true)).toEqual(open);
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
  /** A chart read, a settings dry run, or a plain read of the settings. */
  kind: "chart" | "settings" | "read";
  request: EngineRequest | null;
  signal: AbortSignal;
  resolve: (answer: HttpAnswer) => void;
  reject: (err: unknown) => void;
}

/** An engine whose every request waits until the test answers it. */
class ScriptedTransport implements EngineTransport {
  pending: Pending[] = [];
  private wait(kind: Pending["kind"], request: EngineRequest | null, signal: AbortSignal) {
    return new Promise<HttpAnswer>((resolve, reject) => {
      this.pending.push({ kind, request, signal, resolve, reject });
      signal.addEventListener("abort", () => reject(signal.reason));
    });
  }
  send(request: EngineRequest, signal: AbortSignal) {
    return this.wait("settings", request, signal);
  }
  chart(signal: AbortSignal) {
    return this.wait("chart", null, signal);
  }
  settings(signal: AbortSignal) {
    return this.wait("read", null, signal);
  }
  revision(): Promise<HttpAnswer> {
    throw new Error("not used by the runner");
  }
  /** The requests of one check, in the order they went: the chart, then the settings. */
  of(kind: Pending["kind"]) {
    return this.pending.filter((p) => p.kind === kind);
  }
}

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));
const EMPTY = { status: 200, body: chartOf({}) };

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
      const draft = { ...EMPTY_DRAFT, company: { name: `generation ${g}` } };
      return {
        mode: "create",
        baseRevision: null,
        basePrint: fingerprint(chartPrint(null)),
        draft,
        baseDraft: EMPTY_DRAFT,
        settings: settingsCheckRequest({
          mode: "create",
          baseRevision: null,
          base: null,
          draft: draft.company,
        }),
      };
    },
    onSettled: (s) => settled.push(s),
  });
  const change = () => runner.changed(++generation);
  const reset = () => runner.reset(++generation);
  return { clock, transport, settled, runner, change, reset };
}

describe("CheckRunner", () => {
  test("a check reads the chart and dry-runs the settings, and a burst of changes sends one, for the last generation", async () => {
    const { clock, transport, settled, change, reset } = harness();
    reset();
    expect(transport.pending.map((p) => p.kind)).toEqual(["chart", "settings"]);
    transport.of("chart")[0]!.resolve(EMPTY);
    transport.of("settings")[0]!.resolve({ status: 200, body: { valid: true } });
    await flush();
    expect(settled.map((s) => [s.generation, s.outcome.status])).toEqual([[1, "clean"]]);

    change();
    clock.advance(100);
    change();
    clock.advance(100);
    change();
    clock.advance(CHECK_DEBOUNCE_MS - 1);
    expect(transport.of("settings")).toHaveLength(1);
    clock.advance(1);
    expect(transport.of("settings")).toHaveLength(2);
    expect(transport.of("settings")[1]!.request!.body).toEqual({ name: "generation 4" });
  });

  test("a superseded check is aborted, and its answer never reaches the reducer", async () => {
    const { clock, transport, settled, change, reset } = harness();
    reset();
    const [chart, settings] = transport.pending;
    change();
    expect(chart!.signal.aborted).toBe(true);
    expect(settings!.signal.aborted).toBe(true);
    await flush();
    expect(settled).toEqual([]);
    clock.advance(CHECK_DEBOUNCE_MS);
    transport.of("chart")[1]!.resolve(EMPTY);
    transport.of("settings")[1]!.resolve({
      status: 400,
      body: {
        error: "validation_error",
        problems: [{ path: "name", segments: ["name"], kind: "missing", message: "name" }],
      },
    });
    await flush();
    expect(settled).toHaveLength(1);
    expect(settled[0]).toMatchObject({ generation: 2, outcome: { status: "problems" } });
  });

  test("a chart that already holds a company is a conflict, whatever the settings said", async () => {
    const { transport, settled, reset } = harness();
    reset();
    transport.of("chart")[0]!.resolve({ status: 200, body: fixtureChart() });
    transport.of("settings")[0]!.resolve({ status: 200, body: { valid: true } });
    await flush();
    expect(settled[0]!.outcome).toEqual({
      status: "conflict",
      reason: "chart_exists",
      currentRevisionId: null,
    });
  });

  test("an unreachable engine is retried after the backoff, not at the next change", async () => {
    const { clock, transport, runner, change, reset } = harness();
    reset();
    transport.of("chart")[0]!.resolve({ status: 0, body: null });
    transport.of("settings")[0]!.resolve({ status: 200, body: { valid: true } });
    await flush();
    expect(runner.state.status).toBe("unreachable");
    change();
    clock.advance(UNANSWERED_RETRY_BASE_MS - 1);
    expect(transport.of("chart")).toHaveLength(1);
    clock.advance(1);
    expect(transport.of("chart")).toHaveLength(2);
    expect(transport.of("settings")[1]!.request!.body).toEqual({ name: "generation 2" });
  });

  // THE HINT TRAVELS FROM THE ANSWER TO THE TIMER: a node that said twelve
  // seconds is not asked at the backoff's first second.
  test("an engine that said when is retried then, not after the backoff", async () => {
    const { clock, transport, runner, reset } = harness();
    reset();
    transport.of("chart")[0]!.resolve({
      status: 503,
      body: { error: "behind", detail: "behind the chart's log" },
      retryAfter: 12,
    });
    transport.of("settings")[0]!.resolve({ status: 200, body: { valid: true } });
    await flush();
    expect(runner.state.status).toBe("unreachable");
    clock.advance(12_000 - 1);
    expect(transport.of("chart")).toHaveLength(1);
    clock.advance(1);
    expect(transport.of("chart")).toHaveLength(2);
  });

  test("an engine that said waiting will not change it is not asked again on a timer", async () => {
    const { clock, transport, runner, reset } = harness();
    reset();
    transport.of("chart")[0]!.resolve({
      status: 503,
      body: { error: "log_full", detail: "raise the chart log's ceiling" },
      retryAfter: 0,
    });
    transport.of("settings")[0]!.resolve({ status: 200, body: { valid: true } });
    await flush();
    expect(runner.state.status).toBe("unreachable");
    clock.advance(10 * UNANSWERED_RETRY_MAX_MS);
    expect(transport.of("chart")).toHaveLength(1);
  });

  test("a reset of the same generation, as a change of reader sends, never delivers the replaced answer", async () => {
    const { transport, settled, runner, reset } = harness();
    reset();
    const first = transport.of("chart")[0]!;
    runner.reset(1);
    expect(first.signal.aborted).toBe(true);
    first.resolve({ status: 401, body: { error: "unauthorized" } });
    transport.of("chart")[1]!.resolve(EMPTY);
    transport.of("settings")[1]!.resolve({ status: 200, body: { valid: true } });
    await flush();
    expect(settled.map((s) => s.outcome.status)).toEqual(["clean"]);
  });

  test("a transport that rejects without an abort reports the check unanswered rather than stalling", async () => {
    const { clock, transport, settled, runner, reset } = harness();
    reset();
    transport.of("chart")[0]!.reject(new Error("socket hang up"));
    await flush();
    expect(settled[0]).toMatchObject({
      outcome: { status: "unreachable", detail: "socket hang up" },
    });
    clock.advance(UNANSWERED_RETRY_BASE_MS);
    expect(transport.of("chart")).toHaveLength(2);
    runner.dispose();
    expect(transport.of("chart")[1]!.signal.aborted).toBe(true);
  });

  test("a draft whose shape is wrong is reported with the answer, without asking anything more", async () => {
    const clock = new FakeClock();
    const transport = new ScriptedTransport();
    const settled: SettledCheck[] = [];
    const base = fromChart(fixtureSettings(), fixtureChart());
    const draft = {
      ...base,
      roles: [{ key: "new:x", data: { handle: "Not A Handle", name: "X" } }, ...base.roles],
    };
    const runner = new CheckRunner({
      clock,
      transport,
      prepare: () => ({
        mode: "edit",
        baseRevision: "rev-1",
        basePrint: fingerprint(chartPrint(fixtureChart())),
        draft,
        baseDraft: base,
        settings: null,
      }),
      onSettled: (s) => settled.push(s),
    });
    runner.reset(1);
    // The draft changes no setting: nothing is dry-run, and the settings are read.
    expect(transport.pending.map((p) => p.kind)).toEqual(["chart", "read"]);
    transport.of("chart")[0]!.resolve({ status: 200, body: fixtureChart() });
    transport.of("read")[0]!.resolve({ status: 200, body: fixtureSettings(), etag: '"rev-1"' });
    await flush();
    expect(settled[0]!.outcome).toMatchObject({ status: "problems" });
    expect(
      settled[0]!.outcome.status === "problems" &&
        settled[0]!.outcome.findings.map((f) => [f.node, f.field.join(".")]),
    ).toContainEqual(["new:x", "handle"]);
  });

  test("a draft that changes no setting reads them, and a revision saved meanwhile halts it", async () => {
    const clock = new FakeClock();
    const transport = new ScriptedTransport();
    const settled: SettledCheck[] = [];
    const base = fromChart(fixtureSettings(), fixtureChart());
    const runner = new CheckRunner({
      clock,
      transport,
      prepare: () => ({
        mode: "edit",
        baseRevision: "rev-1",
        basePrint: fingerprint(chartPrint(fixtureChart())),
        draft: base,
        baseDraft: base,
        settings: null,
      }),
      onSettled: (s) => settled.push(s),
    });
    const check = async (etag: string) => {
      runner.reset(settled.length + 1);
      transport.of("chart").at(-1)!.resolve({ status: 200, body: fixtureChart() });
      transport.of("read").at(-1)!.resolve({ status: 200, body: fixtureSettings(), etag });
      await flush();
      return settled.at(-1)!.outcome;
    };
    // The control: the base revision is served, and the check is clean.
    expect(await check('"rev-1"')).toEqual({ status: "clean", findings: [] });
    expect(await check('"rev-2"')).toEqual({
      status: "conflict",
      reason: "revision_advanced",
      currentRevisionId: "rev-2",
    });
    expect(transport.of("settings")).toHaveLength(0);
  });
});
