/**
 * The dry-run check: when the builder asks the engine about the draft, what
 * the answer means, and what saving is allowed to do meanwhile.
 *
 * THE ENGINE IS THE VALIDATOR, SO IT IS ASKED OFTEN AND ASKED EXACTLY. Every
 * change to the draft (an operation, an undo, a redo, a rebase, a discard, a
 * load) moves its GENERATION, and the check that answers for a generation is
 * the only one whose answer is used: a request is aborted as soon as a newer
 * generation supersedes it, and an answer that still arrives for an older one
 * is dropped. Each answer travels with the document that was sent, because
 * the problems in it name paths in THAT document.
 *
 * A PURE STATE MACHINE, AND A SMALL DRIVER. [transition] takes the state and
 * one event and returns the next state with the effects to perform (send,
 * abort, wake me at). [CheckRunner] performs them with an injected clock and
 * transport. The machine is where every rule is, and every rule is tested
 * without a timer or a socket.
 *
 * ONE REQUEST IN FLIGHT. A dry run validates and derives the whole company, so
 * two in flight for one tab would only race to be dropped.
 *
 * STATES. `checking` while an answer for the current generation is due;
 * `clean` and `problems` for a validated draft; `conflict` when the engine
 * holds a newer revision than the draft's base (a 409, a 412, or a dry run
 * reporting a different base); `guarded` when it refused the credential (401
 * or 403); `unreachable` when the request was never answered or the engine
 * failed (status 0, or a 5xx). A draining node's `503 draining` is one of
 * those, deliberately: the drain ends, so the retry reaches a peer behind a
 * load balancer, or this node once it has restarted.
 *
 * TWO OF THEM HALT. `conflict` and `guarded` do not change by asking again:
 * the answer to the next check is the same refusal. So a change to the draft
 * in one of them sends nothing, and checking resumes only on a reset (a load,
 * an updated draft, a token change).
 *
 * `unreachable` BACKS OFF. Asking again at every keystroke would hammer an
 * engine that is restarting, or a network that is down, with requests that
 * each wait out the transport's deadline. After a failure the next check waits
 * [backoffDelay], doubling per consecutive failure up to a cap, and changes
 * made meanwhile ride that retry instead of scheduling their own.
 */

import type { ConfigProblem, ConfigWarning, Derived, DryRunResult } from "~/protocol/index.ts";
import type { IndexedDocument } from "./document.ts";
import { isRecord } from "./json.ts";
import type {
  BuilderMode,
  Clock,
  CancelTimer,
  ConfigRequest,
  ConfigTransport,
  HttpAnswer,
} from "./transport.ts";

/**
 * How long the draft must stay unchanged before it is checked.
 *
 * An operation is a discrete act (a dialog confirmed, a drop, a key press;
 * the node editor applies a whole form as one operation, so typing is not a
 * stream of them), and the answer to a single act should arrive while the
 * operator is still looking at what they did. What must coalesce is a burst:
 * a held Alt+Down or Ctrl+Z repeats at the platform's key-repeat interval,
 * about 30 to 50 ms, after an initial delay of 250 ms or more. Three hundred
 * milliseconds sits above the repeat interval, so a held key sends one check
 * when it is released rather than one per repeat, and well under the half
 * second after which a response to an action stops feeling like its result.
 */
export const CHECK_DEBOUNCE_MS = 300;

/**
 * The wait before the first retry after an unanswered or failed check. One
 * second is long enough not to spin against a refused connection and short
 * enough to recover as soon as a restarting engine accepts one.
 */
export const CHECK_BACKOFF_BASE_MS = 1_000;

/**
 * The longest wait between retries: `REQUEST_TIMEOUT_MS` in `protocol/rest.ts`,
 * the longest a single attempt may itself take, so an engine that recovers is
 * never noticed later than one more attempt would have taken to fail.
 */
export const CHECK_BACKOFF_MAX_MS = 30_000;

/** The wait before retry `failures` (1 for the first). */
export function backoffDelay(failures: number): number {
  const exponent = Math.max(0, failures - 1);
  return Math.min(CHECK_BACKOFF_MAX_MS, CHECK_BACKOFF_BASE_MS * 2 ** Math.min(exponent, 30));
}

export type CheckStatus =
  "checking" | "clean" | "problems" | "conflict" | "guarded" | "unreachable";

/** Why the engine holds a revision the draft was not built on. */
export type ConflictReason =
  /** `409 revision_advanced`: another write activated after the draft's base. */
  | "revision_advanced"
  /** `412 already_configured`: a company exists, and the draft was creating one. */
  | "already_configured"
  /** `409` or `412 no_active_revision`: the draft edits a company the engine no longer holds. */
  | "no_active_revision"
  /** A dry run validated against a base other than the draft's. */
  | "base_moved";

/** What one answered check means. */
export type CheckOutcome =
  | {
      readonly status: "clean";
      readonly warnings: readonly ConfigWarning[];
      readonly derived: Derived | null;
    }
  | {
      readonly status: "problems";
      /** Never empty: a refusal carrying none is given one at document level from its detail. */
      readonly problems: readonly ConfigProblem[];
      readonly derived: Derived | null;
      readonly code: string;
      readonly hint: string;
    }
  | {
      readonly status: "conflict";
      readonly reason: ConflictReason;
      readonly currentRevisionId: string | null;
    }
  | {
      readonly status: "guarded";
      /**
       * The grants the refusal named, any ONE of which would admit this
       * reader — empty for a 401, where nothing the engine accepted was
       * presented. Read from the answer, never written here.
       */
      readonly grants: readonly string[];
    }
  | { readonly status: "unreachable"; readonly detail: string };

const text = (value: unknown): string => (typeof value === "string" ? value : "");
const list = <T>(value: unknown): readonly T[] => (Array.isArray(value) ? (value as T[]) : []);
const derivedOf = (value: unknown): Derived | null =>
  isRecord(value) ? (value as unknown as Derived) : null;

/**
 * What an answer to a check means for a draft built on `baseRevision`.
 *
 * Also used for a save's refusal: a write answers with the same codes, and a
 * 201 is classified by `writes.ts`, which is the only place a success differs.
 */
export function classifyCheck(
  answer: HttpAnswer,
  mode: BuilderMode,
  baseRevision: string | null,
): CheckOutcome {
  const body = isRecord(answer.body) ? answer.body : {};
  const code = text(body.error);
  const current = text(body.current_revision_id) || null;

  if (answer.status >= 200 && answer.status < 300) {
    const result = body as Partial<DryRunResult>;
    const answeredBase = text(result.base_revision_id);
    if (
      mode === "edit" ? answeredBase !== "" && answeredBase !== baseRevision : answeredBase !== ""
    ) {
      return {
        status: "conflict",
        reason: mode === "edit" ? "base_moved" : "already_configured",
        currentRevisionId: answeredBase,
      };
    }
    return {
      status: "clean",
      warnings: list<ConfigWarning>(result.warnings),
      derived: derivedOf(result.derived),
    };
  }
  if (answer.status === 401 || answer.status === 403) {
    // The envelope's `grants`, parsed here rather than through
    // `refusedGrants` because this directory takes nothing from
    // `~/protocol` at runtime (see boundary.test.ts).
    const grants = list<unknown>(body.grants).filter((g): g is string => typeof g === "string");
    return { status: "guarded", grants };
  }
  if (answer.status === 409 || answer.status === 412) {
    const reason: ConflictReason =
      code === "no_active_revision"
        ? "no_active_revision"
        : code === "already_configured"
          ? "already_configured"
          : "revision_advanced";
    return { status: "conflict", reason, currentRevisionId: current };
  }
  if (answer.status === 0 || answer.status >= 500) {
    return { status: "unreachable", detail: text(body.detail) || code };
  }
  // Every other refusal is about the document or the request that carried it:
  // a validation error, a patch the engine could not apply, a body too large.
  const problems = list<ConfigProblem>(body.problems);
  const detail =
    text(body.detail) || code || `The engine refused the check with status ${answer.status}.`;
  return {
    status: "problems",
    problems:
      problems.length > 0
        ? problems
        : [{ path: "", segments: null, kind: "invalid", message: detail }],
    derived: derivedOf(body.derived),
    code,
    hint: text(body.hint),
  };
}

// ---------------------------------------------------------------------------
// The state machine
// ---------------------------------------------------------------------------

/** The one request in flight: which draft generation it checks, and its own number. */
export interface InFlight {
  readonly generation: number;
  /**
   * Numbered per request, not per generation: a reset checks the SAME
   * generation again (a token change moves no generation), and the answer
   * to the request it replaced must not be taken for the answer to the new
   * one.
   */
  readonly request: number;
}

export interface CheckState {
  /** The draft generation the machine last heard of. */
  readonly generation: number;
  readonly status: CheckStatus;
  readonly inFlight: InFlight | null;
  /** How many requests have been sent: the next request's number. */
  readonly requests: number;
  /** When the next request is due, if one is scheduled. */
  readonly dueAt: number | null;
  /** Consecutive unanswered or failed checks. */
  readonly failures: number;
  /** A halting status holds: changes send nothing until a reset. */
  readonly halted: boolean;
}

export type CheckEvent =
  /**
   * Check this generation now, whatever is in flight, forgetting any halt: a
   * load, an updated draft, a save, or a token change (which moves no
   * generation but may change every answer).
   */
  | { readonly type: "reset"; readonly generation: number; readonly now: number }
  /** The draft changed. */
  | { readonly type: "changed"; readonly generation: number; readonly now: number }
  /** The wake-up the machine asked for. */
  | { readonly type: "timer"; readonly now: number }
  /** A request settled. */
  | {
      readonly type: "settled";
      readonly request: number;
      readonly status: Exclude<CheckStatus, "checking">;
      readonly now: number;
    };

export type CheckEffect =
  | { readonly type: "send"; readonly generation: number; readonly request: number }
  | { readonly type: "abort"; readonly request: number }
  /** Arm the one timer for `at`, replacing any armed one. */
  | { readonly type: "wake"; readonly at: number };

export interface Transition {
  readonly state: CheckState;
  readonly effects: readonly CheckEffect[];
}

/** Before the first load: nothing to check yet. */
export const INITIAL_CHECK: CheckState = {
  generation: -1,
  status: "checking",
  inFlight: null,
  requests: 0,
  dueAt: null,
  failures: 0,
  halted: false,
};

const HALTING: ReadonlySet<CheckStatus> = new Set(["conflict", "guarded"]);

/** The next state of the check, and what to do about it. */
export function transition(state: CheckState, event: CheckEvent): Transition {
  const effects: CheckEffect[] = [];
  const abort = (keep: (inFlight: InFlight) => boolean): InFlight | null => {
    if (state.inFlight === null || keep(state.inFlight)) return state.inFlight;
    effects.push({ type: "abort", request: state.inFlight.request });
    return null;
  };
  const send = (base: CheckState): Transition => {
    const inFlight = { generation: base.generation, request: base.requests + 1 };
    effects.push({ type: "send", ...inFlight });
    return {
      state: { ...base, status: "checking", inFlight, requests: inFlight.request, dueAt: null },
      effects,
    };
  };

  switch (event.type) {
    case "reset": {
      abort(() => false);
      return send({
        ...state,
        generation: event.generation,
        inFlight: null,
        failures: 0,
        halted: false,
      });
    }

    case "changed": {
      if (event.generation === state.generation) return { state, effects };
      const inFlight = abort((f) => f.generation === event.generation);
      const base = { ...state, generation: event.generation, inFlight };
      if (state.halted) return { state: base, effects };
      if (state.failures > 0) {
        // Ride the retry that is already scheduled.
        const dueAt = state.dueAt ?? event.now + backoffDelay(state.failures);
        if (state.dueAt === null) effects.push({ type: "wake", at: dueAt });
        return { state: { ...base, dueAt }, effects };
      }
      const dueAt = event.now + CHECK_DEBOUNCE_MS;
      effects.push({ type: "wake", at: dueAt });
      return { state: { ...base, status: "checking", dueAt }, effects };
    }

    case "timer": {
      if (state.dueAt === null) return { state, effects };
      if (event.now < state.dueAt) {
        effects.push({ type: "wake", at: state.dueAt });
        return { state, effects };
      }
      if (state.inFlight !== null) return { state: { ...state, dueAt: null }, effects };
      return send(state);
    }

    case "settled": {
      if (event.request !== state.inFlight?.request) return { state, effects };
      if (state.inFlight.generation !== state.generation) {
        // Superseded while it was out: the change that superseded it has
        // already scheduled the check that counts.
        return { state: { ...state, inFlight: null }, effects };
      }
      if (event.status === "unreachable") {
        const failures = state.failures + 1;
        const dueAt = event.now + backoffDelay(failures);
        effects.push({ type: "wake", at: dueAt });
        return {
          state: {
            ...state,
            status: "unreachable",
            inFlight: null,
            failures,
            dueAt,
            halted: false,
          },
          effects,
        };
      }
      return {
        state: {
          ...state,
          status: event.status,
          inFlight: null,
          failures: 0,
          dueAt: null,
          halted: HALTING.has(event.status),
        },
        effects,
      };
    }
  }
}

/**
 * Whether the answer to `request` is the one the machine is waiting for, for
 * the generation it is on: the only answer the reducer may be given.
 */
export function isCurrentAnswer(state: CheckState, request: number): boolean {
  return state.inFlight?.request === request && state.inFlight.generation === state.generation;
}

// ---------------------------------------------------------------------------
// Save rules
// ---------------------------------------------------------------------------

/** What the Review and save control may do. */
export interface SaveRules {
  /** Whether Review and save opens. */
  readonly review: boolean;
  /** Whether the review's Save button is enabled. */
  readonly save: boolean;
  /** Whether Save is waiting for a check to settle before it enables. */
  readonly waiting: boolean;
  /** Why saving is not available, as a sentence to show; `null` when it is. */
  readonly reason: string | null;
}

/**
 * What saving may do in a check status.
 *
 * `unreachable` ALLOWS SAVING: the write validates exactly as the check would
 * have, so a network that dropped a check is no reason to refuse the operator
 * a write that may well get through. `problems` opens the review so the
 * operator can read the changes, with Save disabled, in both modes: the
 * engine will refuse exactly what it just reported. The three halting states
 * refuse even the review, because what it would review is not what the
 * engine holds or will accept.
 */
export function saveRules(status: CheckStatus, hasChanges: boolean): SaveRules {
  if (!hasChanges)
    return { review: false, save: false, waiting: false, reason: "There are no changes to save." };
  switch (status) {
    case "conflict":
      return {
        review: false,
        save: false,
        waiting: false,
        reason: "The configuration changed since you started editing. Update your draft first.",
      };
    case "guarded":
      return {
        review: false,
        save: false,
        waiting: false,
        // THE BANNER SAYS WHICH GRANT, from the refusal itself; this is the
        // disabled button's reason, and it used to name "an operator
        // token", which a signed-in reader holding the wrong grants has no
        // use for.
        reason: "The engine refused this browser's credential to write the configuration.",
      };
    case "problems":
      return { review: true, save: false, waiting: false, reason: "Fix the problems above first." };
    case "checking":
      return { review: true, save: false, waiting: true, reason: null };
    case "clean":
    case "unreachable":
      return { review: true, save: true, waiting: false, reason: null };
  }
}

// ---------------------------------------------------------------------------
// The driver
// ---------------------------------------------------------------------------

/** What the runner needs to check one generation of the draft. */
export interface PreparedCheck {
  readonly request: ConfigRequest;
  readonly sent: IndexedDocument;
  readonly mode: BuilderMode;
  readonly baseRevision: string | null;
}

/** One answered check, for the reducer. */
export interface SettledCheck {
  readonly generation: number;
  readonly sent: IndexedDocument;
  readonly baseRevision: string | null;
  readonly outcome: CheckOutcome;
}

export interface CheckRunnerOptions {
  readonly clock: Clock;
  readonly transport: ConfigTransport;
  /**
   * The request for `generation`, built from the draft as it stands; `null`
   * when the draft has already moved past that generation.
   */
  readonly prepare: (generation: number) => PreparedCheck | null;
  /** Called once per answer that is still current. */
  readonly onSettled: (settled: SettledCheck) => void;
  /** Called after every transition, for a status display. */
  readonly onState?: (state: CheckState) => void;
}

/**
 * Performs the machine's effects: one timer, one request, one abort
 * controller. The owner calls [changed] when the draft's generation moves,
 * [reset] on a load, an adopted update, a save or a token change (see
 * `reducer.checkTrigger`), and [dispose] when the builder goes away.
 */
export class CheckRunner {
  private current: CheckState = INITIAL_CHECK;
  private cancelTimer: CancelTimer | null = null;
  private inFlight: { readonly request: number; readonly controller: AbortController } | null =
    null;
  private disposed = false;

  constructor(private readonly options: CheckRunnerOptions) {}

  get state(): CheckState {
    return this.current;
  }

  reset(generation: number): void {
    this.dispatch({ type: "reset", generation, now: this.options.clock.now() });
  }

  changed(generation: number): void {
    this.dispatch({ type: "changed", generation, now: this.options.clock.now() });
  }

  dispose(): void {
    this.disposed = true;
    this.cancelTimer?.();
    this.cancelTimer = null;
    this.inFlight?.controller.abort();
    this.inFlight = null;
  }

  private dispatch(event: CheckEvent): void {
    if (this.disposed) return;
    const { state, effects } = transition(this.current, event);
    this.current = state;
    for (const effect of effects) this.perform(effect);
    this.options.onState?.(this.current);
  }

  private perform(effect: CheckEffect): void {
    switch (effect.type) {
      case "wake": {
        this.cancelTimer?.();
        const delay = Math.max(0, effect.at - this.options.clock.now());
        this.cancelTimer = this.options.clock.setTimer(() => {
          this.cancelTimer = null;
          this.dispatch({ type: "timer", now: this.options.clock.now() });
        }, delay);
        return;
      }
      case "abort":
        if (this.inFlight?.request === effect.request) {
          this.inFlight.controller.abort();
          this.inFlight = null;
        }
        return;
      case "send":
        this.send(effect.generation, effect.request);
        return;
    }
  }

  private send(generation: number, request: number): void {
    const prepared = this.options.prepare(generation);
    if (!prepared) {
      // The draft moved on before the request left, so the change that moved
      // it is already on its way to the machine. Settle this request as
      // superseded rather than leave its slot taken, or that change's check
      // could never go out.
      this.current = { ...this.current, inFlight: null };
      return;
    }
    const controller = new AbortController();
    this.inFlight = { request, controller };
    const settle = (outcome: CheckOutcome) => {
      if (this.inFlight?.request === request) this.inFlight = null;
      if (controller.signal.aborted || this.disposed) return;
      const current = isCurrentAnswer(this.current, request);
      this.dispatch({
        type: "settled",
        request,
        status: outcome.status,
        now: this.options.clock.now(),
      });
      if (current) {
        this.options.onSettled({
          generation,
          sent: prepared.sent,
          baseRevision: prepared.baseRevision,
          outcome,
        });
      }
    };
    this.options.transport.send(prepared.request, controller.signal).then(
      (answer) => settle(classifyCheck(answer, prepared.mode, prepared.baseRevision)),
      // An aborted check is superseded, not unreachable, and `settle` drops
      // it. A transport that rejects for any other reason broke its contract,
      // and the check is reported as unanswered rather than left in flight for
      // ever, which would stop every later check.
      (err: unknown) =>
        settle({ status: "unreachable", detail: err instanceof Error ? err.message : String(err) }),
    );
  }
}
