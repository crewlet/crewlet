/**
 * The builder's state, and the one reducer that changes it.
 *
 * PURE, AND SAFE TO RUN TWICE. React runs a reducer twice under StrictMode, so
 * nothing here mints a key, reads a clock or touches storage: a new node's key
 * arrives inside the intent, minted by the event handler, and the effects of a
 * state (checking it, keeping its log) are derived from it by the UI through
 * `scheduler.ts` and `persistence.ts`.
 *
 * THE DRAFT IS `replay(baseDraft, log.ops)`, always. Recording applies the one
 * new operation to the current draft (which is that replay, extended), an
 * undo replays from the base, and every change that moves the draft moves its
 * GENERATION, which is what lets a check's answer say which draft it is about.
 *
 * THE BASE IS TWO READINGS. The settings revision `GET /config` served, and
 * the chart `GET /chart` served — keyed the moment it is read, since every
 * seat it holds carries its handle and every unit its key. What identifies the
 * base is the revision and the chart's rows ([BaseCompany.print]); the chart's
 * position is not an identity, because every read moves it.
 *
 * MODE GUARDS LIVE HERE, because this is the one door every operation comes
 * through, whether a person dispatched it, a restore replayed it or an update
 * rebased it. `applyTemplate` is refused unless the builder is creating a
 * company from nothing: in edit mode a template would replace every seat and
 * unit of a running company, and every seat's identity and memory with them.
 * And a kind change is refused to a reader the chart did not show the runtime
 * half: the change strips what the new kind does not carry, and much of that
 * lives in a half this reader cannot see or write.
 */

import type { ChartRead, CompanyDocument, Derived } from "~/protocol/index.ts";
import { allKeys, locate, type Draft, EMPTY_DRAFT } from "./draft.ts";
import { chartPrint, describes, fingerprint, fromChart, rekeying } from "./document.ts";
import { COMPANY_KEY, seatKey, unitKey, type NodeKey } from "./keys.ts";
import {
  apply,
  describeOperation,
  evaluate,
  expandTemplate,
  record,
  touchedKeys,
  type ApplyReport,
  type Intent,
  type Operation,
  type Recorded,
  type RecordRefusal,
} from "./operations.ts";
import {
  EMPTY_LOG,
  pushOperation,
  rebase,
  redoOperation,
  replay,
  undoOperation,
  type Choice,
  type Log,
  type Rebased,
} from "./history.ts";
import { EMPTY_PROBLEMS, indexProblems, type ProblemIndex } from "./problems.ts";
import type { CheckOutcome, SettledCheck } from "./scheduler.ts";
import type { KeptDraft } from "./persistence.ts";
import { hasSaveSteps, type Creation } from "./save.ts";
import type { BuilderMode } from "./transport.ts";

/** The company the draft was built on. */
export interface BaseCompany {
  /** The settings revision's document; `null` in create mode. */
  readonly settings: CompanyDocument | null;
  /** The active settings revision; `null` in create mode. */
  readonly revision: string | null;
  /** The chart's rows, as a fingerprint of their canonical print (`document.chartPrint`). */
  readonly print: string;
  /**
   * Whether the chart served this reader each object's runtime half. When it
   * did not, the draft holds none, and nothing the builder saves states one.
   */
  readonly runtimeVisible: boolean;
  /**
   * The engine's derivation of this chart, from the org projection, while it
   * describes exactly these rows (`document.describes`); `null` otherwise.
   */
  readonly derived: Derived | null;
}

/** What the last current check said. */
export interface CheckView {
  /** The generation it answered; `-1` before any answer. */
  readonly generation: number;
  readonly outcome: CheckOutcome | null;
  readonly problems: ProblemIndex;
}

/** An update of the draft onto a newer company, waiting for the operator's choices. */
export interface PendingUpdate {
  /** The mode the draft is in once the update is confirmed. */
  readonly mode: BuilderMode;
  readonly base: BaseCompany;
  readonly baseDraft: Draft;
  /** The log being sorted: the draft's own, or a kept one being restored. */
  readonly ops: readonly Operation[];
  readonly choices: ReadonlyMap<number, Choice>;
  /** A save's own creations: the key the draft minted, and the node the chart now holds. */
  readonly aliases: ReadonlyMap<NodeKey, NodeKey>;
  readonly result: Rebased;
  /**
   * Set when the update restores a kept draft. Cancelling it leaves the draft
   * on screen as it was, which for a restore is the base with no operations.
   */
  readonly restoring: boolean;
}

/** The last change to the draft, for the live region and focus. */
export interface LastChange {
  readonly kind: "applied" | "undone" | "redone";
  readonly op: Operation;
  /** One sentence describing the operation. */
  readonly description: string;
  /** The node focus moves to. */
  readonly focus: NodeKey;
}

export interface BuilderState {
  readonly mode: BuilderMode;
  readonly base: BaseCompany;
  readonly baseDraft: Draft;
  readonly draft: Draft;
  readonly log: Log;
  /** What each operation of `log.ops` did, aligned. */
  readonly reports: readonly ApplyReport[];
  /** Moves on every change to the draft. */
  readonly generation: number;
  readonly check: CheckView;
  /** Whether the log may be kept in storage (see `persistence.ts`). */
  readonly keep: boolean;
  readonly update: PendingUpdate | null;
  readonly last: LastChange | null;
  /** Why the last dispatched intent was not recorded. */
  readonly refusal: {
    readonly reason: RecordRefusal | "mode" | "runtime_hidden" | "has_changes";
    readonly message: string;
  } | null;
  /**
   * The keys the last change of base moved, old key to new: a save or an
   * update moves a created node from the key it was minted with to its
   * address, and a renamed node to its new one. A surface still holding an old
   * key (an open dialog, the selection) reads its node through this in the
   * very render the keys change, where a key followed a render later would
   * find no node there and draw it as gone.
   */
  readonly rekeyed: ReadonlyMap<NodeKey, NodeKey>;
  /** The org projection's latest derivation, which [BaseCompany.derived] is judged from. */
  readonly orgDerived: Derived | null;
}

/** A reading of the company: its settings revision and its chart. */
export interface CompanyReading {
  readonly settings: CompanyDocument | null;
  readonly revision: string | null;
  readonly chart: ChartRead | null;
}

export type BuilderAction =
  /** The company as the engine served it (edit), or nothing to edit (create). */
  | ({ readonly type: "load"; readonly mode: BuilderMode } & CompanyReading)
  | { readonly type: "record"; readonly intent: Intent }
  | { readonly type: "undo" }
  | { readonly type: "redo" }
  | { readonly type: "discard" }
  | { readonly type: "checked"; readonly settled: SettledCheck }
  /** The reader changed (a sign-in as somebody else): the tab may have changed hands. */
  | { readonly type: "tokenChanged" }
  /** The org projection pushed a derivation. */
  | { readonly type: "derived"; readonly derived: Derived | null }
  /**
   * A save wrote every step, and this is the company read back: it becomes
   * the base, and the draft is that base with nothing left to write.
   */
  | ({ readonly type: "saved" } & CompanyReading)
  /**
   * Start updating the draft onto a newer company — somebody else's save, or
   * part of this draft's own (`landed`: the nodes its steps created, which a
   * save that stopped part way hands the rebase, see `history.rebase`).
   */
  | ({
      readonly type: "updateBegin";
      readonly landed?: readonly Creation[];
    } & CompanyReading)
  | { readonly type: "updateChoose"; readonly index: number; readonly choice: Choice | null }
  | { readonly type: "updateConfirm" }
  | { readonly type: "updateCancel" }
  /** Restore a kept draft onto the loaded base. */
  | { readonly type: "restore"; readonly kept: KeptDraft };

const EMPTY_CHECK: CheckView = { generation: -1, outcome: null, problems: EMPTY_PROBLEMS };

const EMPTY_BASE: BaseCompany = {
  settings: null,
  revision: null,
  print: fingerprint(chartPrint(null)),
  runtimeVisible: true,
  derived: null,
};

/**
 * The state before anything is loaded.
 *
 * AN EDIT OF A COMPANY NOBODY HAS READ YET, deliberately not create mode.
 * Before a load the builder knows neither whether a company exists nor who
 * its seats are, and a template, which only ever starts a company from
 * nothing, is refused.
 */
export const INITIAL_BUILDER: BuilderState = {
  mode: "edit",
  base: EMPTY_BASE,
  baseDraft: EMPTY_DRAFT,
  draft: EMPTY_DRAFT,
  log: EMPTY_LOG,
  reports: [],
  generation: 0,
  check: EMPTY_CHECK,
  keep: true,
  update: null,
  last: null,
  refusal: null,
  rekeyed: new Map(),
  orgDerived: null,
};

/** The base a reading makes, and the draft it keys. */
function baseOf(
  reading: CompanyReading,
  mode: BuilderMode,
  orgDerived: Derived | null,
): { base: BaseCompany; baseDraft: Draft } {
  const settings = mode === "edit" ? reading.settings : null;
  // CREATE MODE STANDS ON NOTHING, whatever the read found: a company is being
  // made from nothing, and a chart that already holds rows is the check's to
  // report as the conflict it is, never the base a create builds on.
  const chart = mode === "edit" ? reading.chart : null;
  const baseDraft = fromChart(settings, chart);
  return {
    base: {
      settings,
      revision: mode === "edit" ? reading.revision : null,
      print: fingerprint(chartPrint(chart)),
      runtimeVisible: reading.chart?.runtime ?? true,
      derived: describes(baseDraft, orgDerived) ? orgDerived : null,
    },
    baseDraft,
  };
}

/**
 * What the check should hear about a state change: `reset` when the draft now
 * stands on a different base (a load, a save, an adopted update), which checks
 * at once and lifts a halt; `changed` when only the draft moved; `null` when
 * neither did. A new derivation from the org projection changes neither.
 *
 * A change of reader moves no generation and no base, and resets the
 * check all the same (every answer may differ): its owner calls the runner's
 * `reset` directly when the reader changes.
 */
export function checkTrigger(prev: BuilderState, next: BuilderState): "reset" | "changed" | null {
  if (
    prev.mode !== next.mode ||
    prev.base.settings !== next.base.settings ||
    prev.base.revision !== next.base.revision ||
    prev.base.print !== next.base.print
  ) {
    return "reset";
  }
  return prev.generation !== next.generation ? "changed" : null;
}

/**
 * Whether the draft has anything to save.
 *
 * WHAT A SAVE WOULD WRITE, NOT HOW MANY OPERATIONS PRODUCED IT. Operations
 * that cancel out (a goal edited and edited back, a seat added and removed)
 * leave a log but no change, and a save of no change would still write: a
 * settings revision every node applies as it applies a credential rotation, an
 * audit entry for nothing. So the draft is compared with its base as the save
 * plan compares them. In create mode the settings are always written: they are
 * what makes the company exist.
 */
export function hasChanges(state: BuilderState): boolean {
  return hasSaveSteps({
    mode: state.mode,
    baseRevision: state.base.revision,
    baseSettings: state.base.settings,
    baseDraft: state.baseDraft,
    draft: state.draft,
    runtimeVisible: state.base.runtimeVisible,
  });
}

/** What recording an intent against a state would answer, before anything is dispatched. */
export type RecordAnswer =
  | Recorded
  | {
      readonly ok: false;
      readonly refusal: "mode" | "runtime_hidden";
      readonly message: string;
    };

/** Whether an intent writes a node's runtime half. */
function writesRuntime(intent: Intent): boolean {
  switch (intent.type) {
    case "changeKind":
      return true;
    case "addSeat":
    case "addUnit":
      return intent.data.runtime !== undefined;
    case "updateSeat":
    case "updateUnit":
      return intent.set.some((s) => s.path[0] === "runtime");
    case "setScheduleEnabled":
      return true;
    case "edit":
      return intent.intents.some(writesRuntime);
    default:
      return false;
  }
}

/**
 * Records an intent against the state's draft exactly as dispatching it would,
 * guards included, without changing the state.
 *
 * ONE DOOR, ASKED TWICE. A dialog that confirms an operation needs to know
 * whether the reducer will take it before it closes, so it can keep the
 * refusal on screen beside the fields that caused it; and a dialog that shows
 * what an operation will clear or strip needs the operation itself. Both ask
 * here, and the reducer records through the same function, so what a dialog
 * was told and what the reducer does cannot drift apart.
 */
export function recordIntent(state: BuilderState, intent: Intent): RecordAnswer {
  if (
    intent.type === "applyTemplate" &&
    (state.mode !== "create" || state.base.settings !== null)
  ) {
    return {
      ok: false,
      refusal: "mode",
      message: "A template starts a new company. It cannot be applied to a company that exists.",
    };
  }
  if (!state.base.runtimeVisible && writesRuntime(intent)) {
    return {
      ok: false,
      refusal: "runtime_hidden",
      message:
        intent.type === "changeKind"
          ? "Changing a seat's kind removes what the new kind does not carry, and much of that is in the seat's runtime half, which the engine does not show this reader. It takes the grant that reads the company's configuration."
          : "That is part of the runtime half, which the engine does not show this reader. It takes the grant that reads the company's configuration.",
    };
  }
  return record(state.draft, intent);
}

/** Where focus goes after an operation that was just applied to `before`. */
function focusAfter(op: Operation, before: Draft, after: Draft): NodeKey {
  if (op.type === "remove") {
    const found = locate(before, op.target);
    if (found) return found.index > 0 ? found.siblings[found.index - 1]!.key : found.parent;
  }
  return firstExisting(touchedKeys(op), after);
}

function firstExisting(keys: readonly NodeKey[], draft: Draft): NodeKey {
  const present = allKeys(draft);
  return keys.find((k) => present.has(k)) ?? COMPANY_KEY;
}

const refused = (
  state: BuilderState,
  reason: NonNullable<BuilderState["refusal"]>["reason"],
  message: string,
): BuilderState => ({
  ...state,
  refusal: { reason, message },
});

/** The node key a creation names once the chart holds it. */
function addressKeyOf(creation: Creation): NodeKey {
  return creation.kind === "seat" ? seatKey(creation.address) : unitKey(creation.address);
}

/**
 * A log with every template replaced by the operations it amounts to, each
 * recorded against the draft the log had reached where the template stood.
 * What a create draft carries into a company that now exists
 * (`operations.expandTemplate`).
 */
function withoutTemplates(baseDraft: Draft, ops: readonly Operation[]): Operation[] {
  if (!ops.some((op) => op.type === "applyTemplate")) return [...ops];
  const out: Operation[] = [];
  let at = baseDraft;
  for (const op of ops) {
    const parts = op.type === "applyTemplate" ? expandTemplate(at, op) : [op];
    for (const part of parts) {
      out.push(part);
      at = apply(at, part).draft;
    }
  }
  return out;
}

/** Starts an update: the rebase, and whether it can be adopted with nothing to choose. */
function beginUpdate(
  state: BuilderState,
  reading: CompanyReading,
  options: {
    readonly ops: readonly Operation[];
    readonly fromMode: BuilderMode;
    readonly restoring: boolean;
    readonly creates: readonly Creation[];
  },
): PendingUpdate {
  // A company that now holds settings is a company to edit, whatever the
  // draft set out to do; a create draft becomes an edit of what it made.
  const mode: BuilderMode = reading.settings !== null ? "edit" : state.mode;
  const { base, baseDraft } = baseOf(reading, mode, state.orgDerived);
  const ops =
    options.fromMode === "create" && mode === "edit"
      ? withoutTemplates(EMPTY_DRAFT, options.ops)
      : options.ops;
  const aliases = new Map<NodeKey, NodeKey>();
  for (const creation of options.creates) aliases.set(creation.key, addressKeyOf(creation));
  const choices = new Map<number, Choice>();
  return {
    mode,
    base,
    baseDraft,
    ops,
    choices,
    aliases,
    result: rebase(baseDraft, ops, choices, aliases),
    restoring: options.restoring,
  };
}

/** Adopts an update: the rebased draft on its base. */
function adoptUpdate(state: BuilderState, update: PendingUpdate): BuilderState {
  return {
    ...state,
    mode: update.mode,
    base: update.base,
    baseDraft: update.baseDraft,
    draft: update.result.draft,
    log: { ops: update.result.ops, undone: [] },
    reports: update.result.reports,
    generation: state.generation + 1,
    check: EMPTY_CHECK,
    keep: true,
    update: null,
    last: null,
    refusal: null,
    rekeyed: new Map([...rekeying(state.draft, update.result.draft), ...update.result.rekeyed]),
  };
}

export function builderReducer(state: BuilderState, action: BuilderAction): BuilderState {
  switch (action.type) {
    case "load": {
      const { base, baseDraft } = baseOf(action, action.mode, state.orgDerived);
      return {
        ...INITIAL_BUILDER,
        mode: action.mode,
        base,
        baseDraft,
        draft: baseDraft,
        generation: state.generation + 1,
        orgDerived: state.orgDerived,
        rekeyed: rekeying(state.draft, baseDraft),
      };
    }

    case "record": {
      const result = recordIntent(state, action.intent);
      if (!result.ok) return refused(state, result.refusal, result.message);
      const { draft, report } = apply(state.draft, result.op);
      return {
        ...state,
        draft,
        log: pushOperation(state.log, result.op),
        reports: [...state.reports, report],
        generation: state.generation + 1,
        keep: true,
        last: {
          kind: "applied",
          op: result.op,
          description: describeOperation(result.op, state.draft),
          focus: focusAfter(result.op, state.draft, draft),
        },
        refusal: null,
      };
    }

    case "undo": {
      const undone = undoOperation(state.log);
      if (!undone) return state;
      const { draft, reports } = replay(state.baseDraft, undone.log.ops);
      return {
        ...state,
        draft,
        log: undone.log,
        reports,
        generation: state.generation + 1,
        last: {
          kind: "undone",
          op: undone.op,
          description: describeOperation(undone.op, draft),
          focus: firstExisting(touchedKeys(undone.op), draft),
        },
        refusal: null,
      };
    }

    case "redo": {
      const redone = redoOperation(state.log);
      if (!redone) return state;
      const { draft, report } = apply(state.draft, redone.op);
      return {
        ...state,
        draft,
        log: redone.log,
        reports: [...state.reports, report],
        generation: state.generation + 1,
        last: {
          kind: "redone",
          op: redone.op,
          description: describeOperation(redone.op, state.draft),
          focus: focusAfter(redone.op, state.draft, draft),
        },
        refusal: null,
      };
    }

    case "discard":
      return {
        ...state,
        draft: state.baseDraft,
        log: EMPTY_LOG,
        reports: [],
        generation: state.generation + 1,
        update: null,
        last: null,
        refusal: null,
      };

    case "checked": {
      const { settled } = action;
      if (settled.generation !== state.generation) return state;
      const outcome = settled.outcome;
      return {
        ...state,
        keep: outcome.status === "guarded" ? false : state.keep,
        check: {
          generation: settled.generation,
          outcome,
          problems:
            outcome.status === "clean" || outcome.status === "problems"
              ? indexProblems(outcome.findings)
              : EMPTY_PROBLEMS,
        },
      };
    }

    case "tokenChanged":
      return { ...state, keep: false };

    case "derived": {
      const derived = describes(state.baseDraft, action.derived) ? action.derived : null;
      if (action.derived === state.orgDerived && derived === state.base.derived) return state;
      return {
        ...state,
        orgDerived: action.derived,
        base: derived === state.base.derived ? state.base : { ...state.base, derived },
      };
    }

    case "saved": {
      const { base, baseDraft } = baseOf(action, "edit", state.orgDerived);
      return {
        ...state,
        mode: "edit",
        base,
        baseDraft,
        draft: baseDraft,
        rekeyed: rekeying(state.draft, baseDraft),
        log: EMPTY_LOG,
        reports: [],
        generation: state.generation + 1,
        check: EMPTY_CHECK,
        keep: true,
        update: null,
        last: null,
        refusal: null,
      };
    }

    case "updateBegin": {
      const update = beginUpdate(state, action, {
        ops: state.log.ops,
        fromMode: state.mode,
        restoring: false,
        creates: action.landed ?? [],
      });
      // A SAVE THAT STOPPED PART WAY carries on from what landed at once when
      // nothing is left to choose: its own creations are already resolved,
      // and what remains is the draft still to save.
      if (action.landed && update.result.pending === 0) return adoptUpdate(state, update);
      return { ...state, update };
    }

    case "updateChoose": {
      if (!state.update) return state;
      const choices = new Map(state.update.choices);
      if (action.choice === null) choices.delete(action.index);
      else choices.set(action.index, action.choice);
      const { baseDraft, ops, aliases } = state.update;
      return {
        ...state,
        update: {
          ...state.update,
          choices,
          result: rebase(baseDraft, ops, choices, aliases),
        },
      };
    }

    case "updateConfirm": {
      const update = state.update;
      if (!update || update.result.pending > 0) return state;
      return adoptUpdate(state, update);
    }

    case "updateCancel":
      return state.update ? { ...state, update: null } : state;

    case "restore": {
      const { kept } = action;
      if (state.log.ops.length > 0 || state.log.undone.length > 0) {
        // A restore replaces the log, so over work in progress it would drop
        // that work without a word. The offer to restore comes before editing;
        // after it the operator discards first.
        return refused(
          state,
          "has_changes",
          "This draft already has changes. Discard them before restoring the kept draft.",
        );
      }
      // A CREATE DRAFT WHOSE SAVE WAS SENT meets the company that save may have
      // made: carried onto it as an edit, its own creations resolved onto what
      // landed. Any other change of mode is not this draft's to cross.
      const crossing = kept.mode === "create" && state.mode === "edit" && kept.write !== undefined;
      if (kept.mode !== state.mode && !crossing) {
        return refused(
          state,
          "mode",
          "This draft was made for a different mode and cannot be restored here.",
        );
      }
      if (
        !crossing &&
        kept.ops.some((op) => op.type === "applyTemplate") &&
        (state.mode !== "create" || state.base.settings !== null)
      ) {
        return refused(
          state,
          "mode",
          "A template starts a new company. It cannot be applied to a company that exists.",
        );
      }
      const creates = kept.creates ?? [];
      const ops = crossing ? withoutTemplates(EMPTY_DRAFT, kept.ops) : kept.ops;
      const aliases = new Map<NodeKey, NodeKey>();
      for (const creation of creates) aliases.set(creation.key, addressKeyOf(creation));
      const choices = new Map<number, Choice>();
      const result = rebase(state.baseDraft, ops, choices, aliases);
      const clean = result.entries.every((e) => e.outcome === "applies");
      if (
        clean &&
        !crossing &&
        kept.baseRevision === state.base.revision &&
        kept.basePrint === state.base.print
      ) {
        // Recorded on this very base and every operation still applies: the
        // draft is the one the operator left, redo stack included, as long as
        // that stack redoes. It was read from storage like the rest, and a
        // redo that does not apply would throw in this reducer.
        return {
          ...state,
          draft: result.draft,
          log: { ops: result.ops, undone: redoes(result.draft, kept.undone) ? kept.undone : [] },
          reports: result.reports,
          generation: state.generation + 1,
          keep: true,
          update: null,
          last: null,
          refusal: null,
        };
      }
      return {
        ...state,
        update: {
          mode: state.mode,
          base: state.base,
          baseDraft: state.baseDraft,
          ops,
          choices,
          aliases,
          result,
          restoring: true,
        },
        refusal: null,
      };
    }
  }
}

/**
 * Whether a redo stack redoes from `draft`: each operation, most recent undo
 * first, applies to the draft the ones redone before it produce.
 */
function redoes(draft: Draft, undone: readonly Operation[]): boolean {
  let at = draft;
  for (let i = undone.length - 1; i >= 0; i--) {
    const op = undone[i]!;
    if (evaluate(at, op).kind !== "applies") return false;
    at = apply(at, op).draft;
  }
  return true;
}
