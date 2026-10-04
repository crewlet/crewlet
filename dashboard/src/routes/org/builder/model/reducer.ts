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
 * MODE GUARDS LIVE HERE, because this is the one door every operation comes
 * through, whether a person dispatched it, a restore replayed it or an update
 * rebased it. `applyTemplate` is refused unless the builder is creating a
 * company from nothing: in edit mode a template would replace every seat and
 * unit of a running company in one merge patch, and every seat's identity and
 * memory with them.
 *
 * THE BASE IS KEYED AS IT LOADS. Every seat the engine stores declares its
 * handle and every unit its key, so the document `GET /config` serves is
 * keyed by those identities at once (`document.fromDocument`) and an edit can
 * be recorded before any check has answered. What the first check of the base
 * adds is the engine's DERIVATION of it — who reports to whom, what each unit
 * inherits — which is kept as the base's own (`BaseCompany.derived`) for the
 * review to compare the draft against.
 */

import type { CompanyDocument, Derived } from "~/protocol/index.ts";
import { allKeys, locate, type Draft, EMPTY_DRAFT } from "./draft.ts";
import {
  buildPatch,
  fromDocument,
  rekeying,
  toDocument,
  type CheckedDocument,
  type IndexedDocument,
} from "./document.ts";
import { COMPANY_KEY, type NodeKey } from "./keys.ts";
import {
  apply,
  describeOperation,
  evaluate,
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
import { EMPTY_PROBLEMS, placeProblems, type ProblemIndex } from "./problems.ts";
import { STEP_UP_REQUIRED, type CheckOutcome, type SettledCheck } from "./scheduler.ts";
import type { KeptDraft } from "./persistence.ts";
import type { BuilderMode } from "./transport.ts";

/** The company the draft was built on. */
export interface BaseCompany {
  /** `null` in create mode. */
  readonly document: CompanyDocument | null;
  /** The active revision; `null` in create mode. */
  readonly revision: string | null;
  /** The engine's derivation of `document`, once a check has reported it. */
  readonly derived: Derived | null;
}

/** What the last current check said. */
export interface CheckView {
  /** The generation it answered; `-1` before any answer. */
  readonly generation: number;
  readonly outcome: CheckOutcome | null;
  /** The document it was sent, and its path index. */
  readonly sent: IndexedDocument | null;
  readonly problems: ProblemIndex;
  /** The draft's derivation, from the last answer that carried one. */
  readonly derived: Derived | null;
}

/** An update of the draft onto a newer revision, waiting for the operator's choices. */
export interface PendingUpdate {
  readonly base: BaseCompany;
  readonly baseDraft: Draft;
  /** The log being sorted: the draft's own, or a kept one being restored. */
  readonly ops: readonly Operation[];
  readonly choices: ReadonlyMap<number, Choice>;
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
    readonly reason: RecordRefusal | "mode" | "not_loaded" | "has_changes";
    readonly message: string;
  } | null;
  /**
   * The keys the last save moved, old key to new: a save moves each node it
   * created from the key it was minted with to its identity, the handle or
   * unit key it was given. A surface still holding an old key (an open
   * dialog, the selection) reads its node through this in the very render the
   * keys change, where a key followed a render later would find no node there
   * and draw it as gone.
   */
  readonly rekeyed: ReadonlyMap<NodeKey, NodeKey>;
}

export type BuilderAction =
  /** The company as `GET /config` served it (edit), or nothing to edit (create). */
  | {
      readonly type: "load";
      readonly mode: BuilderMode;
      readonly document: CompanyDocument | null;
      readonly revision: string | null;
    }
  | { readonly type: "record"; readonly intent: Intent }
  | { readonly type: "undo" }
  | { readonly type: "redo" }
  | { readonly type: "discard" }
  | { readonly type: "checked"; readonly settled: SettledCheck }
  /** Somebody else signed in on this tab: the tab may have changed hands. */
  | { readonly type: "readerChanged" }
  /**
   * A save landed as `revisionId`, and `derived` is the derivation its answer
   * carried. The draft becomes the base, keyed by that derivation, until the
   * UI loads that revision, which it does next: the engine's stored document
   * is the one to edit from, not the draft that was sent.
   */
  | { readonly type: "saved"; readonly revisionId: string; readonly derived: Derived | null }
  /** Start updating the draft onto a newer revision (see `writes.readyToUpdate`). */
  | {
      readonly type: "updateBegin";
      readonly document: CompanyDocument;
      readonly revision: string;
      readonly derived: Derived;
    }
  | { readonly type: "updateChoose"; readonly index: number; readonly choice: Choice | null }
  | { readonly type: "updateConfirm" }
  | { readonly type: "updateCancel" }
  /** Restore a kept draft onto the loaded, keyed base. */
  | { readonly type: "restore"; readonly kept: KeptDraft };

const EMPTY_CHECK: CheckView = {
  generation: -1,
  outcome: null,
  sent: null,
  problems: EMPTY_PROBLEMS,
  derived: null,
};

/**
 * The state before anything is loaded.
 *
 * AN EDIT OF A COMPANY NOT LOADED YET, deliberately not create mode: before a
 * load the builder knows neither whether a company exists nor who its seats
 * are, so the doors stay shut on both counts: an edit waits for the load
 * (which would drop it anyway), and a template, which only ever starts a
 * company from nothing, is refused.
 */
export const INITIAL_BUILDER: BuilderState = {
  mode: "edit",
  base: { document: null, revision: null, derived: null },
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
};

/**
 * What the dry-run check should hear about a state change: `reset` when the
 * draft now stands on a different base (a load, a save, an adopted update),
 * which checks at once and lifts a halt; `changed` when only the draft moved;
 * `null` when neither did. Keeping the base's derivation from a check's answer
 * changes neither the base document nor its revision, so it asks for nothing.
 *
 * A change of reader moves no generation and no base, and resets the check
 * all the same (every answer may differ): its owner calls the runner's
 * `reset` directly when somebody else signs in.
 */
export function checkTrigger(prev: BuilderState, next: BuilderState): "reset" | "changed" | null {
  if (
    prev.mode !== next.mode ||
    prev.base.document !== next.base.document ||
    prev.base.revision !== next.base.revision
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
 * leave a log but no change, and in edit mode a save is a merge patch of what
 * changed: an empty one still stores and activates a new revision, which
 * every node applies exactly as it applies a credential rotation, and writes
 * an audit entry for nothing. So the draft is compared with its base as the
 * patch compares them. In create mode the base is nothing, so what is
 * compared is whether the draft holds anything the builder writes at all.
 */
export function hasChanges(state: BuilderState): boolean {
  const patch = buildPatch(state.base.document, toDocument(state.draft).document);
  return Object.keys(patch).length > 0;
}

/**
 * The last check's document and derivation, when that check answered with
 * one. Any generation: what a derivation still says about the draft is the
 * reader's to decide (see `chartModel.ts`).
 */
export function checkedDocument(check: CheckView): CheckedDocument | null {
  return check.sent && check.derived ? { sent: check.sent, derived: check.derived } : null;
}

/** What recording an intent against a state would answer, before anything is dispatched. */
export type RecordAnswer =
  | Recorded
  | { readonly ok: false; readonly refusal: "mode" | "not_loaded"; readonly message: string };

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
    (state.mode !== "create" || state.base.document !== null)
  ) {
    return {
      ok: false,
      refusal: "mode",
      message: "A template starts a new company. It cannot be applied to a company that exists.",
    };
  }
  if (!isLoaded(state)) {
    return { ok: false, refusal: "not_loaded", message: NOT_LOADED };
  }
  return record(state.draft, intent);
}

const NOT_LOADED = "The company has not loaded yet. Make the change once it has.";

/** Whether a company has been loaded to edit, or create mode was entered: the draft has a base. */
function isLoaded(state: BuilderState): boolean {
  return state.mode === "create" || state.base.document !== null;
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

export function builderReducer(state: BuilderState, action: BuilderAction): BuilderState {
  switch (action.type) {
    case "load": {
      const document = action.mode === "edit" ? action.document : null;
      const baseDraft = fromDocument(document);
      return {
        ...INITIAL_BUILDER,
        mode: action.mode,
        base: {
          document,
          revision: action.mode === "edit" ? action.revision : null,
          derived: null,
        },
        baseDraft,
        draft: baseDraft,
        generation: state.generation + 1,
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
      const derived =
        outcome.status === "clean" || outcome.status === "problems" ? outcome.derived : null;
      const next: BuilderState = {
        ...state,
        // A REFUSAL MAY BE THE TAB CHANGING HANDS, so the log stops being
        // kept — except a step-up the person declined, which is the same
        // person at the keyboard choosing not to confirm yet.
        keep:
          outcome.status === "guarded" && outcome.code !== STEP_UP_REQUIRED ? false : state.keep,
        check: {
          generation: settled.generation,
          outcome,
          sent: settled.sent,
          problems: placed(settled.sent, outcome),
          derived:
            outcome.status === "clean" || outcome.status === "problems" ? outcome.derived : null,
        },
      };
      // The first check of the base itself describes the BASE: keep that
      // derivation as the base's own, for the review to compare against.
      // Only while the log is empty, when the draft that check was sent is
      // the base.
      if (
        state.mode === "edit" &&
        state.base.derived === null &&
        state.log.ops.length === 0 &&
        state.log.undone.length === 0 &&
        derived &&
        settled.baseRevision === state.base.revision
      ) {
        return { ...next, base: { ...state.base, derived } };
      }
      return next;
    }

    case "readerChanged":
      return { ...state, keep: false };

    case "saved": {
      // The saved document becomes the base, KEYED LIKE ANY BASE: by the
      // derivation the write answered with, which describes exactly this
      // document. Keeping the draft's own keys instead would carry the keys
      // minted for the nodes this save created into every later operation,
      // and those name nothing once the revision is read back.
      const document = toDocument(state.draft).document;
      const baseDraft = fromDocument(document);
      return {
        ...state,
        mode: "edit",
        base: { document, revision: action.revisionId, derived: action.derived },
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
      if (state.mode !== "edit" || state.base.document === null)
        return refused(state, "mode", "Only a draft of an existing company can be updated.");
      const base: BaseCompany = {
        document: action.document,
        revision: action.revision,
        derived: action.derived,
      };
      const baseDraft = fromDocument(action.document);
      const choices = new Map<number, Choice>();
      return {
        ...state,
        update: {
          base,
          baseDraft,
          ops: state.log.ops,
          choices,
          result: rebase(baseDraft, state.log.ops, choices),
          restoring: false,
        },
      };
    }

    case "updateChoose": {
      if (!state.update) return state;
      const choices = new Map(state.update.choices);
      if (action.choice === null) choices.delete(action.index);
      else choices.set(action.index, action.choice);
      const { baseDraft } = state.update;
      return {
        ...state,
        update: {
          ...state.update,
          choices,
          result: rebase(baseDraft, state.update.ops, choices),
        },
      };
    }

    case "updateConfirm": {
      const update = state.update;
      if (!update || update.result.pending > 0) return state;
      return {
        ...state,
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
      };
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
      if (kept.mode !== state.mode) {
        return refused(
          state,
          "mode",
          "This draft was made for a different mode and cannot be restored here.",
        );
      }
      if (!isLoaded(state)) return refused(state, "not_loaded", NOT_LOADED);
      if (
        kept.ops.some((op) => op.type === "applyTemplate") &&
        (state.mode !== "create" || state.base.document !== null)
      ) {
        return refused(
          state,
          "mode",
          "A template starts a new company. It cannot be applied to a company that exists.",
        );
      }
      const choices = new Map<number, Choice>();
      const result = rebase(state.baseDraft, kept.ops, choices);
      const clean = result.entries.every((e) => e.outcome === "applies");
      if (clean && kept.baseRevision === state.base.revision) {
        // Recorded on this very revision and every operation still applies:
        // the draft is the one the operator left, redo stack included, as
        // long as that stack redoes. It was read from storage like the rest,
        // and a redo that does not apply would throw in this reducer.
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
          base: state.base,
          baseDraft: state.baseDraft,
          ops: kept.ops,
          choices,
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

function placed(sent: IndexedDocument, outcome: CheckOutcome): ProblemIndex {
  if (outcome.status === "problems") {
    return placeProblems(sent, { problems: outcome.problems, derived: outcome.derived });
  }
  if (outcome.status === "clean")
    return placeProblems(sent, { warnings: outcome.warnings, derived: outcome.derived });
  return EMPTY_PROBLEMS;
}
