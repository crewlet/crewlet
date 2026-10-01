/**
 * The check: when the builder asks the engine about the draft, what the
 * answer means, and what saving is allowed to do meanwhile.
 *
 * WHAT A CHECK ASKS. The org chart has no dry run — a batch and a content
 * write are decided when they are written — so a check asks the two things
 * that can be asked without writing, and says the rest itself:
 *
 * - IS THE CHART STILL THE ONE THE DRAFT WAS MADE ON? It reads the chart and
 *   compares its rows with the base's ([chartPrint]): a content write is full
 *   post-state and carries no precondition, so a draft saved over rows
 *   somebody else changed would put back what they wrote. That is the lost
 *   update this check exists to catch, and it is a `conflict`, which halts.
 *   In create mode the question is whether the chart is still empty.
 * - WOULD THE SETTINGS BE TAKEN? When the draft changes them (always in
 *   create mode) their dry run is the same request the save sends, and its
 *   problems and a newer revision are reported as they always were. When it
 *   changes none, the check READS them instead and compares the revision with
 *   the base: the save would write no settings, so there is nothing to
 *   validate, but a draft nobody touched still has to stand on the charter a
 *   colleague saved rather than go on showing the one it was loaded with —
 *   and a draft with work on it has to hear of that revision now, not at the
 *   first settings edit, when the conflict would name a change made an hour
 *   earlier.
 * - WHAT DOES THE DRAFT'S OWN SHAPE SAY? `problems.preflight`, synchronously,
 *   with the answer: an address the chart would refuse, a field past its cap.
 *
 * EVERY GENERATION IS CHECKED, AND ONLY ITS OWN ANSWER IS USED. Every change
 * to the draft (an operation, an undo, a redo, a rebase, a discard, a load)
 * moves its GENERATION, and the check that answers for a generation is the
 * only one whose answer is used: a request is aborted as soon as a newer
 * generation supersedes it, and an answer that still arrives for an older one
 * is dropped.
 *
 * A PURE STATE MACHINE, AND A SMALL DRIVER. [transition] takes the state and
 * one event and returns the next state with the effects to perform (send,
 * abort, wake me at). [CheckRunner] performs them with an injected clock and
 * transport. The machine is where every rule is, and every rule is tested
 * without a timer or a socket.
 *
 * ONE CHECK IN FLIGHT. It reads the whole chart and may validate the whole
 * settings document, so two in flight for one tab would only race to be
 * dropped.
 *
 * STATES. `checking` while an answer for the current generation is due;
 * `clean` and `problems` for a checked draft; `conflict` when the engine holds
 * a newer company than the draft's base (the chart's rows changed; a settings
 * 409 or 412, or a dry run reporting a different base); `guarded` when it
 * refused the credential (401 or 403); `unreachable` when a request was never
 * answered or the engine failed (status 0, or a 5xx). A draining node's `503
 * draining` is one of those, deliberately: the drain ends, so the retry
 * reaches a peer behind a load balancer, or this node once it has restarted.
 *
 * TWO OF THEM HALT. `conflict` and `guarded` do not change by asking again:
 * the answer to the next check is the same refusal. So a change to the draft
 * in one of them sends nothing, and checking resumes only on a reset (a load,
 * an updated draft, a change of reader).
 *
 * `unreachable` BACKS OFF. Asking again at every keystroke would hammer an
 * engine that is restarting, or a network that is down, with requests that
 * each wait out the transport's deadline. After a failure the next check waits
 * [unansweredRetryMs] — the one backoff every unanswered request in this
 * dashboard takes (`~/protocol/retry.ts`), doubling per consecutive failure up
 * to a cap — and changes made meanwhile ride that retry instead of scheduling
 * their own.
 *
 * UNLESS THE ENGINE SAID WHEN. A `503` the engine wrote carries a
 * `Retry-After` — a node behind the chart's log says how far, a draining one
 * says thirty seconds — and the retry waits exactly that
 * (`~/protocol/retry.ts`, bounded like every hint), where the backoff asked a
 * node twelve seconds behind at one, two, four and eight seconds first. And a
 * `503` it wrote with NO `Retry-After` is its statement that waiting will not
 * change the answer — a chart log at its ceiling, a record the node cannot
 * decode — so nothing re-asks it on a timer: the check stays `unreachable`
 * with no retry due, and the next change to the draft is asked about as a
 * fresh question, because a person editing is the one thing that may come
 * after an operator's fix.
 */

import type { ChartRead, ConfigProblem, ConfigWarning, DryRunResult } from "~/protocol/index.ts";
import { chartPrint, fingerprint } from "./document.ts";
import type { Draft } from "./draft.ts";
import { isRecord } from "./json.ts";
import { placeSettingsFindings, preflight, type PlacedProblem } from "./problems.ts";
import { retryAfterMs, unansweredRetryMs } from "~/protocol/retry.ts";
import {
  revisionOfEtag,
  type BuilderMode,
  type Clock,
  type CancelTimer,
  type EngineRequest,
  type EngineTransport,
  type HttpAnswer,
} from "./transport.ts";

/**
 * How long the draft must stay unchanged before it is checked.
 *
 * An operation is a discrete act (a dialog confirmed, a drop, a key press;
 * the node editor applies a whole form as one operation, so typing is not a
 * stream of them), and the answer to a single act should arrive while the
 * operator is still looking at what they did. What must coalesce is a burst:
 * a held Ctrl+Z or Shift+Ctrl+Z repeats at the platform's key-repeat interval,
 * about 30 to 50 ms, after an initial delay of 250 ms or more. Three hundred
 * milliseconds sits above the repeat interval, so a held key sends one check
 * when it is released rather than one per repeat, and well under the half
 * second after which a response to an action stops feeling like its result.
 */
export const CHECK_DEBOUNCE_MS = 300;

export type CheckStatus =
  "checking" | "clean" | "problems" | "conflict" | "guarded" | "unreachable";

/** Why the engine holds a company the draft was not built on. */
export type ConflictReason =
  /** `409 revision_advanced`: another settings write activated after the draft's base. */
  | "revision_advanced"
  /** `412 already_configured`: a company exists, and the draft was creating one. */
  | "already_configured"
  /** `409` or `412 no_active_revision`: the draft edits a company the engine no longer holds. */
  | "no_active_revision"
  /** A settings dry run validated against a base other than the draft's. */
  | "base_moved"
  /** The chart's rows are not the ones the draft was made on: somebody else wrote them. */
  | "chart_moved"
  /** The draft creates a company, and the chart already holds seats or units. */
  | "chart_exists";

/** What one answered check means. */
export type CheckOutcome =
  | {
      readonly status: "clean";
      /** Warnings only: the draft's own and the settings dry run's. */
      readonly findings: readonly PlacedProblem[];
    }
  | {
      readonly status: "problems";
      /** At least one problem, beside any warnings. */
      readonly findings: readonly PlacedProblem[];
      /** The settings refusal's code and hint, when it was the settings that were refused. */
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
  | {
      readonly status: "unreachable";
      readonly detail: string;
      /**
       * When the engine said to ask again — [HttpAnswer.retryAfter], seconds,
       * zero for "waiting will not change it" — or null where it said nothing
       * and the check backs off on its own.
       */
      readonly retryAfter: number | null;
    };

const text = (value: unknown): string => (typeof value === "string" ? value : "");
const list = <T>(value: unknown): readonly T[] => (Array.isArray(value) ? (value as T[]) : []);

/** The grants a 401 or a 403 names, parsed here because this directory takes nothing from `~/protocol` at runtime. */
function grantsOf(body: Record<string, unknown>): string[] {
  return list<unknown>(body.grants).filter((g): g is string => typeof g === "string");
}

/** What a settings dry run said, as far as a check is concerned. */
export type SettingsAnswer =
  | {
      readonly kind: "taken";
      readonly warnings: readonly ConfigWarning[];
    }
  | {
      readonly kind: "refused";
      readonly problems: readonly ConfigProblem[];
      readonly code: string;
      readonly hint: string;
    }
  | { readonly kind: "outcome"; readonly outcome: CheckOutcome };

/**
 * What a settings dry run's answer means for a draft built on `baseRevision`.
 * A save's refusal answers with the same codes, and `writes.ts` reads a
 * settings write through the same function.
 */
export function classifySettings(
  answer: HttpAnswer,
  mode: BuilderMode,
  baseRevision: string | null,
): SettingsAnswer {
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
        kind: "outcome",
        outcome: {
          status: "conflict",
          reason: mode === "edit" ? "base_moved" : "already_configured",
          currentRevisionId: answeredBase,
        },
      };
    }
    return { kind: "taken", warnings: list<ConfigWarning>(result.warnings) };
  }
  if (answer.status === 401 || answer.status === 403) {
    return { kind: "outcome", outcome: { status: "guarded", grants: grantsOf(body) } };
  }
  if (answer.status === 409 || answer.status === 412) {
    const reason: ConflictReason =
      code === "no_active_revision"
        ? "no_active_revision"
        : code === "already_configured"
          ? "already_configured"
          : "revision_advanced";
    return { kind: "outcome", outcome: { status: "conflict", reason, currentRevisionId: current } };
  }
  if (answer.status === 0 || answer.status >= 500) {
    return {
      kind: "outcome",
      outcome: {
        status: "unreachable",
        detail: text(body.detail) || code,
        retryAfter: answer.retryAfter ?? null,
      },
    };
  }
  // Every other refusal is about the document or the request that carried it:
  // a validation error, a patch the engine could not apply, a body too large.
  const problems = list<ConfigProblem>(body.problems);
  const detail =
    text(body.detail) || code || `The engine refused the settings with status ${answer.status}.`;
  return {
    kind: "refused",
    problems:
      problems.length > 0
        ? problems
        : [{ path: "", segments: null, kind: "invalid", message: detail }],
    code,
    hint: text(body.hint),
  };
}

/**
 * What a plain read of the settings says about an edit draft that changes none
 * of them: nothing while the engine serves the draft's base revision, and the
 * dry run's own conflicts otherwise — a newer revision is `revision_advanced`,
 * as a 409 is, and a company that is gone is `no_active_revision`.
 */
export function classifySettingsRead(
  answer: HttpAnswer,
  baseRevision: string | null,
): SettingsAnswer | null {
  const body = isRecord(answer.body) ? answer.body : {};
  const outcome = (o: CheckOutcome): SettingsAnswer => ({ kind: "outcome", outcome: o });
  if (answer.status === 200) {
    const current = revisionOfEtag(answer.etag);
    return current === baseRevision
      ? null
      : outcome({ status: "conflict", reason: "revision_advanced", currentRevisionId: current });
  }
  if (answer.status === 401 || answer.status === 403) {
    return outcome({ status: "guarded", grants: grantsOf(body) });
  }
  if (answer.status === 404 && text(body.error) === "no_active_revision") {
    return outcome({ status: "conflict", reason: "no_active_revision", currentRevisionId: null });
  }
  return outcome({
    status: "unreachable",
    detail:
      text(body.detail) ||
      text(body.error) ||
      (answer.status === 0
        ? "The engine could not be reached."
        : `The engine answered the settings read with status ${answer.status}.`),
    retryAfter: answer.retryAfter ?? null,
  });
}

/**
 * What a chart read says about a draft: nothing (the rows are the base's, or
 * in create mode there are none), or the outcome that stops the check.
 */
export function classifyChart(
  answer: HttpAnswer,
  mode: BuilderMode,
  basePrint: string,
): CheckOutcome | null {
  const body = isRecord(answer.body) ? answer.body : {};
  if (answer.status === 401 || answer.status === 403) {
    return { status: "guarded", grants: grantsOf(body) };
  }
  if (answer.status !== 200) {
    return {
      status: "unreachable",
      detail:
        text(body.detail) ||
        text(body.error) ||
        (answer.status === 0
          ? "The engine could not be reached."
          : `The engine answered the chart read with status ${answer.status}.`),
      retryAfter: answer.retryAfter ?? null,
    };
  }
  const chart = body as unknown as ChartRead;
  if (mode === "create") {
    const holds = list(chart.seats).length > 0 || list(chart.units).length > 0;
    return holds ? { status: "conflict", reason: "chart_exists", currentRevisionId: null } : null;
  }
  return fingerprint(chartPrint(chart)) === basePrint
    ? null
    : { status: "conflict", reason: "chart_moved", currentRevisionId: null };
}

/**
 * The outcome of a check from its parts: the chart's word, the settings' (or
 * `null` when there were none to check), and the draft's own problems.
 * A halting answer from either request outranks everything, an unanswered one
 * comes next, and only a draft both requests took is judged on its problems.
 */
export function combineCheck(
  chart: CheckOutcome | null,
  settings: SettingsAnswer | null,
  own: readonly PlacedProblem[],
): CheckOutcome {
  const settingsOutcome = settings?.kind === "outcome" ? settings.outcome : null;
  for (const status of ["guarded", "conflict"] as const) {
    if (chart?.status === status) return chart;
    if (settingsOutcome?.status === status) return settingsOutcome;
  }
  const unanswered = [chart, settingsOutcome].filter(
    (o): o is Extract<CheckOutcome, { status: "unreachable" }> => o?.status === "unreachable",
  );
  if (unanswered.length > 0) {
    // THE CHART'S WORDS, AND BOTH REQUESTS' HINTS: the next check asks both
    // again, so it waits for whichever said it needs longer — and never,
    // when either said waiting will not change it.
    return { ...unanswered[0]!, retryAfter: joinHints(unanswered.map((o) => o.retryAfter)) };
  }
  const findings: PlacedProblem[] = [...own];
  let code = "";
  let hint = "";
  if (settings?.kind === "taken") {
    findings.push(...placeSettingsFindings({ warnings: settings.warnings }));
  } else if (settings?.kind === "refused") {
    findings.push(...placeSettingsFindings({ problems: settings.problems }));
    code = settings.code;
    hint = settings.hint;
  }
  return findings.some((f) => f.severity === "problem")
    ? { status: "problems", findings, code, hint }
    : { status: "clean", findings };
}

/**
 * One retry hint for a check whose requests each said one: zero if either said
 * waiting will not change it, the longer of two that said when, and null only
 * where neither said anything — a request the engine did not answer says
 * nothing about the one it did.
 */
function joinHints(hints: readonly (number | null)[]): number | null {
  if (hints.some((h) => h === 0)) return 0;
  const said = hints.filter((h): h is number => h !== null);
  return said.length > 0 ? Math.max(...said) : null;
}

// ---------------------------------------------------------------------------
// The state machine
// ---------------------------------------------------------------------------

/** The one request in flight: which draft generation it checks, and its own number. */
export interface InFlight {
  readonly generation: number;
  /**
   * Numbered per request, not per generation: a reset checks the SAME
   * generation again (a change of reader moves no generation), and the answer
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
  /**
   * Consecutive unanswered or failed checks that a later retry may clear. A
   * refusal the engine said waiting will not change is not one: nothing
   * re-asks it on a timer, so there is no backoff for it to lengthen.
   */
  readonly failures: number;
  /** A halting status holds: changes send nothing until a reset. */
  readonly halted: boolean;
}

export type CheckEvent =
  /**
   * Check this generation now, whatever is in flight, forgetting any halt: a
   * load, an updated draft, a save, or a change of reader (which moves no
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
      /**
       * For an `unreachable` answer, when the engine said to ask again (see
       * the outcome's `retryAfter`); absent or null where it said nothing.
       */
      readonly retryAfter?: number | null;
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
        const dueAt = state.dueAt ?? event.now + unansweredRetryMs(state.failures);
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
        const hint = event.retryAfter ?? null;
        const failures = state.failures + 1;
        const wait = hint === null ? unansweredRetryMs(failures) : retryAfterMs(hint);
        if (wait === null) {
          // THE ENGINE SAID WAITING WILL NOT CHANGE IT, so no retry is due,
          // and a change to the draft is asked about as a fresh question
          // rather than riding a retry nothing scheduled.
          return {
            state: {
              ...state,
              status: "unreachable",
              inFlight: null,
              failures: 0,
              dueAt: null,
              halted: false,
            },
            effects,
          };
        }
        const dueAt = event.now + wait;
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
 * `unreachable` ALLOWS SAVING: a save reads the chart again before its first
 * write and every write is decided where it lands, so a network that dropped a
 * check is no reason to refuse the operator a write that may well get
 * through. `problems` opens the review so the
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
        reason: "The company changed since you started editing. Update your draft first.",
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
        reason:
          "The engine refused this browser's credential to read the chart or write the settings.",
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
  readonly mode: BuilderMode;
  readonly baseRevision: string | null;
  /** The fingerprint of the chart the draft was made on (`document.fingerprint`). */
  readonly basePrint: string;
  readonly draft: Draft;
  /** The chart the draft was made on, for the checks that compare against it. */
  readonly baseDraft: Draft;
  /**
   * The settings dry run, or `null` when the draft changes no setting — and
   * then the check reads the settings instead ([classifySettingsRead]).
   */
  readonly settings: EngineRequest | null;
}

/** One answered check, for the reducer. */
export interface SettledCheck {
  readonly generation: number;
  readonly baseRevision: string | null;
  readonly basePrint: string;
  readonly outcome: CheckOutcome;
}

export interface CheckRunnerOptions {
  readonly clock: Clock;
  readonly transport: EngineTransport;
  /**
   * What to check for `generation`, built from the draft as it stands; `null`
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
 * [reset] on a load, an adopted update, a save or a change of reader (see
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
        retryAfter: outcome.status === "unreachable" ? outcome.retryAfter : null,
      });
      if (current) {
        this.options.onSettled({
          generation,
          baseRevision: prepared.baseRevision,
          basePrint: prepared.basePrint,
          outcome,
        });
      }
    };
    const { transport } = this.options;
    const chart = transport.chart(controller.signal);
    const settings: Promise<SettingsAnswer | null> = prepared.settings
      ? transport
          .send(prepared.settings, controller.signal)
          .then((answer) => classifySettings(answer, prepared.mode, prepared.baseRevision))
      : transport
          .settings(controller.signal)
          .then((answer) => classifySettingsRead(answer, prepared.baseRevision));
    Promise.all([chart, settings]).then(
      ([chartAnswer, settingsAnswer]) =>
        settle(
          combineCheck(
            classifyChart(chartAnswer, prepared.mode, prepared.basePrint),
            settingsAnswer,
            preflight(prepared.draft, prepared.baseDraft),
          ),
        ),
      // An aborted check is superseded, not unreachable, and `settle` drops
      // it. A transport that rejects for any other reason broke its contract,
      // and the check is reported as unanswered rather than left in flight for
      // ever, which would stop every later check.
      (err: unknown) =>
        settle({
          status: "unreachable",
          detail: err instanceof Error ? err.message : String(err),
          retryAfter: null,
        }),
    );
  }
}
