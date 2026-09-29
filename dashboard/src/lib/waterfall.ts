/**
 * One turn as a WATERFALL: every span of work it did, on one clock.
 *
 * A PURE MODEL OVER VALUES — the turn's events, the phase records in hand and
 * the time now — and the only place a span's placement is decided. The screen
 * draws what this returns and decides nothing about time; the suite holds the
 * placement rules without a browser, which is the reason a ranking or a layout
 * in this product is a function over values rather than a component.
 *
 * # What a span is, and where its clock comes from
 *
 * EVERY INSTANT IS ONE THE ENGINE MEASURED, never a subtraction of two
 * publishers' arrival stamps. The tool loop times each round's model call
 * (`rounds[]`) and each tool call (`tool_executions[].started_at`,
 * `duration_ms`) where the call is made; a phase and a coding run carry their
 * own `started_at` and `duration_ms`; the prefetch its own. What this model
 * adds is arrangement:
 *
 *  - the TURN from its opening record to its last span (or now, while it runs);
 *  - its CONTEXT — the prefetch — before the first phase;
 *  - each of its own PHASES, with each ROUND's model call under it and that
 *    round's TOOL CALLS after it. A call the engine did not stamp but did time
 *    is run SERIALLY from the model's answer, in the order the round made them
 *    — which is the order the loop runs them in — and marked `placed`, so a
 *    reader can tell a stamped instant from an arranged one;
 *  - a delegate's WORKERS and the round-cap JUDGE under the round whose call
 *    spawned them (`host_round`), or under their phase where the record does
 *    not say which round;
 *  - each detached CODING RUN as its own span, keyed by its `launch_id` — two
 *    launches in one turn are two spans — from its announcement to its
 *    published record, or to now while it is still out;
 *  - the REFLECTION pass after the last phase, with its workers;
 *  - "review pending" between an executor that finished and a reviewer that
 *    has not started, while the turn runs.
 *
 * A TURN THAT PARKED ON A CODING RUN IS ONE TURN: its executor publishes one
 * record when it finally finishes, carrying every round from before the park
 * and after it at their own instants, so the phase spans the run it waited on
 * and the run sits beside it.
 *
 * # What cannot be placed is still shown
 *
 * A record an older engine wrote has no instants at all, and an agent-mode
 * executor's rounds ran inside somebody else's loop. Those are UNTIMED rows —
 * listed under the waterfall as "not placed on this clock" — rather than bars
 * drawn at an invented position, or dropped. The list says PLACED rather than
 * timed because a call can be measured and still have nowhere to go: timed
 * but unstamped, in a round with no model call of its own to follow. Whether
 * a call was measured at all is [callMeasured]'s, which every surface asks.
 */

import type { EventRecord } from "~/protocol/index.ts";
import { tsKey } from "./format.ts";
import { phaseStart, type PhaseRecord, type ToolCall } from "./phases.ts";

export type SpanKind =
  | "turn"
  | "context"
  | "phase"
  | "model"
  | "tool"
  | "worker"
  | "judge"
  | "run"
  | "reflection"
  | "pending";

export interface Span {
  /** Stable across renders and pushes, and what `span=` names. */
  id: string;
  kind: SpanKind;
  label: string;
  /** A second, quieter phrase: a model, a server, a coding agent. */
  sub: string;
  /** Nesting depth; the turn is 0. */
  depth: number;
  /** Milliseconds since the epoch. */
  start: number;
  /** For an open span, the `now` it was built at. */
  end: number;
  /** Still running: its end is now, and moves. */
  open: boolean;
  failed: boolean;
  /** The phase record this span is part of, "" for a turn-level span. */
  phaseKey: string;
  /** The round, for a model, tool, worker or judge span; 0 otherwise. */
  round: number;
  /** The call's index in its phase's `tools`, for a tool span; -1 otherwise. */
  toolIndex: number;
  /** The coding run's job, for a run span; "" otherwise. */
  launchId: string;
  /** A tool call placed from its duration rather than stamped by the engine. */
  placed: boolean;
}

/** A span of work nothing timed. */
export interface Untimed {
  id: string;
  kind: SpanKind;
  label: string;
  sub: string;
  phaseKey: string;
}

export interface Waterfall {
  /** The window every span falls in; 0/0 when nothing is timed. */
  from: number;
  to: number;
  /** In reading order: each span followed by what ran under it. */
  spans: Span[];
  untimed: Untimed[];
  /** Something is still running, so the window's end is now. */
  open: boolean;
}

export interface WaterfallInput {
  /** The turn's own events, in any order. */
  events: readonly EventRecord[];
  /** Every phase record in hand, workers and coding runs included. */
  phases: readonly PhaseRecord[];
  /** The instant open spans end at. */
  now: number;
  /** The turn is still running — a phase in flight, or parked on a run. */
  running: boolean;
  /** The turn is parked on a detached coding run. */
  parked: boolean;
}

interface Node {
  span: Span;
  children: Node[];
}

const at = (s: string | undefined): number => (s ? tsKey(s) : 0);

function payload(ev: EventRecord): Record<string, unknown> {
  return (ev.payload as Record<string, unknown> | undefined) ?? {};
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

/** What a phase is called on a bar: its name, and its iteration past the first. */
/**
 * Whether the engine MEASURED a tool call — stamped its start, or timed it.
 *
 * ONE ANSWER FOR EVERY SURFACE THAT SAYS "not timed", because two derivations
 * disagreed on the same call: a structured submission is stamped and runs
 * inside a millisecond, so its `duration_ms` is a measured 0 — the Timeline
 * drew it (it is stamped) while the Tools tab, testing the duration alone,
 * listed the same call as never timed.
 */
export function callMeasured(call: Pick<ToolCall, "startedAt" | "durationMs">): boolean {
  return at(call.startedAt) > 0 || call.durationMs > 0;
}

export function phaseLabel(p: Pick<PhaseRecord, "phase" | "iteration">): string {
  const name = p.phase ? p.phase[0]!.toUpperCase() + p.phase.slice(1) : "Phase";
  return p.iteration > 1 ? `${name} · iteration ${p.iteration}` : name;
}

function span(over: Partial<Span> & Pick<Span, "id" | "kind" | "label" | "start" | "end">): Span {
  return {
    sub: "",
    depth: 0,
    open: false,
    failed: false,
    phaseKey: "",
    round: 0,
    toolIndex: -1,
    launchId: "",
    placed: false,
    ...over,
  };
}

/** A phase's own start: the engine's stamp, or its landing less its length. */
function phaseBegan(p: PhaseRecord): number {
  return at(p.clockStart) || phaseStart(p);
}

/**
 * A phase and everything timed under it, or null where nothing about it is
 * timed.
 */
function phaseNode(
  p: PhaseRecord,
  workers: readonly PhaseRecord[],
  now: number,
  untimed: Untimed[],
): Node | null {
  const began = phaseBegan(p);
  const children: Node[] = [];
  const byRound = new Map<number, Node>();
  const rounds = [...p.timedRounds];
  for (const r of rounds) {
    const start = at(r.startedAt);
    if (start <= 0) continue;
    const node: Node = {
      span: span({
        id: `${p.key}.r${r.round}`,
        kind: "model",
        label: `Round ${r.round}`,
        sub: r.model || p.model,
        start,
        end: start + r.durationMs,
        phaseKey: p.key,
        round: r.round,
      }),
      children: [],
    };
    byRound.set(r.round, node);
    children.push(node);
  }
  // THE ROUND IN FLIGHT, on a live phase: its provider call has begun and has
  // not answered, so it is a model span that ends now.
  //
  // ONLY WHEN THE START IS NEWER THAN EVERY ROUND ON RECORD. `round_started_at`
  // is the LATEST round's start, and it stays that round's while the round's
  // tools run — so a frame whose start equals the last recorded round's is
  // that round's tools, not a round in flight. Numbering it "the next round"
  // drew the running model call from the previous round's instant: minutes
  // early on a slow model, and under a round that had already answered.
  if (p.live && p.roundStartedAt) {
    const start = at(p.roundStartedAt);
    const round = rounds.reduce((n, r) => Math.max(n, r.round), 0) + 1;
    const lastStart = rounds.reduce((t, r) => Math.max(t, at(r.startedAt)), 0);
    if (start > lastStart && !byRound.has(round)) {
      const node: Node = {
        span: span({
          id: `${p.key}.r${round}`,
          kind: "model",
          label: `Round ${round}`,
          sub: p.model,
          start,
          end: Math.max(start, now),
          open: true,
          phaseKey: p.key,
          round,
        }),
        children: [],
      };
      byRound.set(round, node);
      children.push(node);
    }
  }
  // THE TOOL CALLS, after their round's model call. Stamped where the
  // engine stamped them; otherwise run one after another from the model's
  // answer in the order the round made them, which is the order the loop runs
  // them; otherwise untimed.
  const cursors = new Map<number, number>();
  p.tools.forEach((call, i) => {
    const host = byRound.get(call.round);
    const id = `${p.key}.r${call.round}.t${i}`;
    const stamped = at(call.startedAt);
    let start = 0;
    let placed = false;
    if (stamped > 0) {
      start = stamped;
    } else if (host && call.durationMs > 0) {
      start = cursors.get(call.round) ?? host.span.end;
      placed = true;
    }
    if (start <= 0) {
      untimed.push({ id, kind: "tool", label: call.name, sub: call.server, phaseKey: p.key });
      return;
    }
    const end = start + call.durationMs;
    cursors.set(call.round, Math.max(cursors.get(call.round) ?? 0, end));
    const node: Node = {
      span: span({
        id,
        kind: "tool",
        label: call.name,
        sub: call.server,
        start,
        end,
        failed: call.failed,
        phaseKey: p.key,
        round: call.round,
        toolIndex: i,
        placed,
      }),
      children: [],
    };
    (host ? host.children : children).push(node);
  });
  // THE CALL RUNNING NOW, which has no row in `tools` until it returns.
  if (p.live && p.runningCall) {
    const start = at(p.runningCall.startedAt);
    if (start > 0) {
      const host = byRound.get(p.runningCall.round);
      const node: Node = {
        span: span({
          id: `${p.key}.r${p.runningCall.round}.running`,
          kind: "tool",
          label: p.runningCall.name,
          start,
          end: Math.max(start, now),
          open: true,
          phaseKey: p.key,
          round: p.runningCall.round,
        }),
        children: [],
      };
      (host ? host.children : children).push(node);
    }
  }
  // THE WORKERS AND THE JUDGE, under the round that spawned them.
  for (const w of workers) {
    if (w.hostPhase !== p.phase || w.hostIteration !== p.iteration) continue;
    const wBegan = phaseBegan(w);
    const kind: SpanKind = w.phase === "judge" ? "judge" : "worker";
    const label = kind === "judge" ? "Round-cap judge" : w.worker || w.taskId || "Worker";
    const wEnd = w.live ? now : w.durationMs > 0 ? wBegan + w.durationMs : 0;
    if (wBegan <= 0 || wEnd <= 0) {
      untimed.push({ id: w.key, kind, label, sub: w.model, phaseKey: w.key });
      continue;
    }
    const node: Node = {
      span: span({
        id: w.key,
        kind,
        label,
        sub: w.model,
        start: wBegan,
        end: Math.max(wBegan, wEnd),
        open: w.live,
        failed: w.failed,
        phaseKey: w.key,
        round: w.hostRound,
      }),
      children: [],
    };
    const host = w.hostRound > 0 ? byRound.get(w.hostRound) : undefined;
    (host ? host.children : children).push(node);
  }

  const ownEnd = p.live ? now : p.durationMs > 0 && began > 0 ? began + p.durationMs : 0;
  const bounds = [ownEnd, ...children.flatMap(ends)].filter((t) => t > 0);
  const starts = [began, ...children.flatMap(starts_)].filter((t) => t > 0);
  if (!starts.length || !bounds.length) {
    untimed.push({ id: p.key, kind: "phase", label: phaseLabel(p), sub: p.model, phaseKey: p.key });
    for (const c of children) flattenUntimed(c, untimed);
    return null;
  }
  const start = Math.min(...starts);
  return {
    span: span({
      id: p.key,
      kind: "phase",
      label: phaseLabel(p),
      sub: p.model,
      start,
      end: Math.max(start, ...bounds),
      open: p.live,
      failed: p.failed,
      phaseKey: p.key,
    }),
    children,
  };
}

function ends(n: Node): number[] {
  return [n.span.end, ...n.children.flatMap(ends)];
}

function starts_(n: Node): number[] {
  return [n.span.start, ...n.children.flatMap(starts_)];
}

function flattenUntimed(n: Node, out: Untimed[]) {
  out.push({
    id: n.span.id,
    kind: n.span.kind,
    label: n.span.label,
    sub: n.span.sub,
    phaseKey: n.span.phaseKey,
  });
  for (const c of n.children) flattenUntimed(c, out);
}

/** The coding runs this turn launched, one span per job. */
function runNodes(
  events: readonly EventRecord[],
  records: readonly PhaseRecord[],
  now: number,
  stillOut: boolean,
  untimed: Untimed[],
): Node[] {
  const out: Node[] = [];
  const seen = new Set<string>();
  const failures = events
    .filter((e) => e.type === "sandbox_run_failed")
    .map((e) => at(e.timestamp))
    .filter((t) => t > 0)
    .sort((a, b) => a - b);
  const starts = events
    .filter((e) => e.type === "sandbox_run_started")
    .sort((a, b) => at(a.timestamp) - at(b.timestamp));
  starts.forEach((ev, i) => {
    const p = payload(ev);
    const launch = str(p.launch_id);
    const record = launch ? records.find((r) => r.launchId === launch) : undefined;
    const id = `run:${launch || ev.id}`;
    const label = "Coding run";
    const sub = str(p.coding_agent);
    if (record) {
      seen.add(record.key);
      const node = recordRun(record, id);
      if (node) out.push(node);
      else untimed.push({ id, kind: "run", label, sub, phaseKey: record.key });
      return;
    }
    const start = at(str(p.started_at)) || at(ev.timestamp);
    // A RUN WITH NO RECORD YET is still out, or it failed: a failure is the
    // first `sandbox_run_failed` after its start and before the next one.
    const next = starts[i + 1] ? at(starts[i + 1]!.timestamp) : Infinity;
    const failedAt = failures.find((t) => t >= start && t < next);
    if (failedAt !== undefined) {
      out.push({
        span: span({
          id,
          kind: "run",
          label,
          sub,
          start,
          end: failedAt,
          failed: true,
          launchId: launch,
        }),
        children: [],
      });
    } else if (stillOut && i === starts.length - 1) {
      out.push({
        span: span({
          id,
          kind: "run",
          label,
          sub,
          start,
          end: Math.max(start, now),
          open: true,
          launchId: launch,
        }),
        children: [],
      });
    } else {
      untimed.push({ id, kind: "run", label, sub, phaseKey: "" });
    }
  });
  // A RECORD WITH NO ANNOUNCEMENT in hand — its start fell out of a capped
  // read — is still a run, drawn from its own clock.
  for (const r of records) {
    if (seen.has(r.key)) continue;
    const id = `run:${r.launchId || r.key}`;
    const node = recordRun(r, id);
    if (node) out.push(node);
    else
      untimed.push({ id, kind: "run", label: "Coding run", sub: r.codingAgent, phaseKey: r.key });
  }
  return out;
}

function recordRun(r: PhaseRecord, id: string): Node | null {
  const start = phaseBegan(r);
  if (start <= 0 || r.durationMs <= 0) return null;
  return {
    span: span({
      id,
      kind: "run",
      label: "Coding run",
      sub: r.codingAgent,
      start,
      end: start + r.durationMs,
      failed: r.failed,
      phaseKey: r.key,
      launchId: r.launchId,
    }),
    children: [],
  };
}

/** Order siblings by when they began, and flatten depth-first. */
function flatten(nodes: Node[], depth: number, out: Span[]) {
  const sorted = [...nodes].sort((a, b) => a.span.start - b.span.start || a.span.end - b.span.end);
  for (const n of sorted) {
    out.push({ ...n.span, depth });
    flatten(n.children, depth + 1, out);
  }
}

export function buildWaterfall(input: WaterfallInput): Waterfall {
  const { events, phases, now, running, parked } = input;
  const untimed: Untimed[] = [];
  const top: Node[] = [];

  const workers = phases.filter((p) => p.hostPhase);
  const runs = phases.filter((p) => p.phase === "sandbox" && !p.hostPhase);
  const reflection = phases.filter((p) => p.phase === "auxiliary" && !p.hostPhase);
  const own = phases.filter(
    (p) => !p.hostPhase && p.phase !== "sandbox" && p.phase !== "auxiliary",
  );

  // THE CONTEXT: every prefetch the turn ran, from its own clock.
  for (const ev of events) {
    if (ev.type !== "prefetch_summary") continue;
    const p = payload(ev);
    const start = at(str(p.started_at));
    const length = num(p.duration_ms);
    if (start <= 0) {
      untimed.push({
        id: `context:${ev.id}`,
        kind: "context",
        label: "Context",
        sub: "",
        phaseKey: "",
      });
      continue;
    }
    top.push({
      span: span({
        id: `context:${ev.id}`,
        kind: "context",
        label: "Context",
        sub: "prefetch",
        start,
        end: start + length,
      }),
      children: [],
    });
  }

  for (const p of own) {
    const node = phaseNode(p, workers, now, untimed);
    if (node) top.push(node);
  }
  top.push(...runNodes(events, runs, now, parked, untimed));

  // THE REFLECTION PASS, after the last phase: its workers, closed by its
  // own sentinel where that has landed.
  if (reflection.length || events.some((e) => e.type === "reflection_completed")) {
    const children: Node[] = [];
    for (const w of reflection) {
      const began = phaseBegan(w);
      const end = w.live ? now : w.durationMs > 0 ? began + w.durationMs : 0;
      const label = w.worker || "Reflection worker";
      if (began <= 0 || end <= 0) {
        untimed.push({ id: w.key, kind: "worker", label, sub: w.model, phaseKey: w.key });
        continue;
      }
      children.push({
        span: span({
          id: w.key,
          kind: "worker",
          label,
          sub: w.model,
          start: began,
          end,
          open: w.live,
          failed: w.failed,
          phaseKey: w.key,
        }),
        children: [],
      });
    }
    const closed = events.find((e) => e.type === "reflection_completed");
    const opened = events.find((e) => e.type === "agent_turn_completed");
    const starts = [...children.map((c) => c.span.start), at(opened?.timestamp)].filter(
      (t) => t > 0,
    );
    const endsAt = [...children.map((c) => c.span.end), at(closed?.timestamp)].filter((t) => t > 0);
    if (starts.length && endsAt.length) {
      const start = Math.min(...starts);
      top.push({
        span: span({
          id: "reflection",
          kind: "reflection",
          label: "Reflection",
          sub: "after the last phase",
          start,
          end: Math.max(start, ...endsAt),
          open: !closed && children.some((c) => c.span.open),
        }),
        children,
      });
    }
  }

  // REVIEW PENDING: the executor has finished, nothing is in flight, and no
  // reviewer has started — while the turn runs and is not waiting on a run.
  if (running && !parked && !own.some((p) => p.live)) {
    const last = [...own].sort((a, b) => phaseBegan(b) - phaseBegan(a))[0];
    if (last && last.phase === "execute" && last.durationMs > 0) {
      const start = phaseBegan(last) + last.durationMs;
      if (start > 0 && start <= now) {
        top.push({
          span: span({
            id: "pending:review",
            kind: "pending",
            label: "Review pending",
            start,
            end: now,
            open: true,
          }),
          children: [],
        });
      }
    }
  }

  const children: Span[] = [];
  flatten(top, 1, children);
  const timed = children.filter((s) => s.end >= s.start && s.start > 0);
  const opening = events
    .filter((e) => e.type === "agent_turn_started" && payload(e).resumed !== true)
    .map((e) => at(str(payload(e).started_at)) || at(e.timestamp))
    .filter((t) => t > 0);
  const closing = events
    .filter((e) => e.type === "turn_completed")
    .map((e) => {
      const p = payload(e);
      const start = at(str(p.started_at));
      return start > 0 && num(p.duration_ms) > 0 ? start + num(p.duration_ms) : 0;
    })
    .filter((t) => t > 0);
  const starts = [...opening, ...timed.map((s) => s.start)];
  if (!starts.length) {
    return { from: 0, to: 0, spans: [], untimed, open: running };
  }
  const from = Math.min(...starts);
  const to = running
    ? Math.max(now, ...timed.map((s) => s.end))
    : Math.max(from, ...closing, ...timed.map((s) => s.end));
  // THE TURN ENDS WHERE THE ENGINE SAYS IT DID. Its own record measures the
  // turn (`turn_completed`), and the reflection pass runs AFTER that — so a
  // turn row stretched to cover it printed one length beside a header whose
  // wall clock printed the measured one, two durations for "the turn" on one
  // screen. The window still reaches the reflection, which trails the bar.
  const measured = closing.length ? Math.max(...closing) : 0;
  const turn = span({
    id: "turn",
    kind: "turn",
    label: "Turn",
    start: from,
    end: !running && measured > from ? measured : to,
    open: running,
    failed: events.some((e) => e.type === "agent_turn_completed" && payload(e).failed === true),
  });
  return { from, to, spans: [turn, ...timed], untimed, open: running };
}

/** Where on the axis an instant falls, as a fraction of the window. */
export function fraction(t: number, from: number, to: number): number {
  if (to <= from) return 0;
  return Math.min(1, Math.max(0, (t - from) / (to - from)));
}

/**
 * Tick marks for an axis `span` milliseconds long: a round step from the
 * 1-2-5 ladder that gives at most `most` intervals, and the offsets on it.
 */
export function ticks(spanMs: number, most = 6): { step: number; marks: number[] } {
  if (spanMs <= 0) return { step: 0, marks: [0] };
  const ladder = [1, 2, 5];
  let step = 1;
  for (let exp = 0; exp < 12; exp++) {
    const found = ladder.map((m) => m * 10 ** exp).find((s) => spanMs / s <= most);
    if (found !== undefined) {
      step = found;
      break;
    }
  }
  const marks: number[] = [];
  for (let t = 0; t <= spanMs + 1e-9; t += step) marks.push(t);
  return { step, marks };
}

/**
 * An axis mark's label, in ONE FORMAT across the ruler: unit letters with no
 * space, the way the phase cards and the turns list write a duration —
 * "500ms", "1.5s", "10s", "1m 40s", "2h". A step under a second counts
 * milliseconds only below one second, then seconds with as many decimals as
 * the step needs, so "1000 ms" never stands beside "1.2s". Whole units on a
 * whole step rather than a formatter's precision, which printed "5.0 s"
 * beside "10 s" on one ruler.
 */
export function tickLabel(t: number, step: number): string {
  if (t === 0) return "0";
  if (t < 1_000) return `${Math.round(t)}ms`;
  if (step < 1_000) {
    const places = Math.min(3, Math.ceil(-Math.log10(step / 1_000) - 1e-9));
    return `${Number((t / 1_000).toFixed(places))}s`;
  }
  const s = Math.round(t / 1_000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const rest = s % 60;
  if (m < 60) return rest ? `${m}m ${rest}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return m % 60 ? `${h}h ${m % 60}m` : `${h}h`;
}
