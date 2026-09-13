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
 * THE BASE IS KEYED BY THE ENGINE. A document read from `GET /config` carries
 * no handles, so until the first check answers, a seat that declares no
 * handle can only be keyed by its path. The first check runs on the base with
 * an empty log, and its derivation re-keys the base by handle, which is safe
 * precisely because no operation refers to the old keys yet. A restore or an
 * update waits for a keyed base for the same reason (see [isBaseKeyed]).
 */

import type { CompanyDocument, Derived } from "~/protocol/index.ts";
import { allKeys, locate, type Draft, EMPTY_DRAFT } from "./draft.ts";
import { fromDocument, handlesByKey, toDocument, type IndexedDocument } from "./document.ts";
import { COMPANY_KEY, type NodeKey } from "./keys.ts";
import {
  apply,
  describeOperation,
  record,
  touchedKeys,
  type ApplyReport,
  type Intent,
  type Operation,
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
import type { CheckOutcome, SettledCheck } from "./scheduler.ts";
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
    readonly reason: RecordRefusal | "mode" | "not_keyed";
    readonly message: string;
  } | null;
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
  /** The operator token changed: the tab may have changed hands. */
  | { readonly type: "tokenChanged" }
  /**
   * A save landed as `revisionId`. The draft becomes the base until the UI
   * loads that revision, which it does next: the engine's stored document is
   * the one to edit from, not the draft that was sent.
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

/** The state before anything is loaded. */
export const INITIAL_BUILDER: BuilderState = {
  mode: "create",
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
};

/**
 * Whether the base is keyed by the engine's identities, so a log recorded
 * against handles can be replayed onto it. Create mode starts from nothing
 * and needs no derivation.
 */
export function isBaseKeyed(state: BuilderState): boolean {
  return state.mode === "create" || state.base.derived !== null;
}

/**
 * What the dry-run check should hear about a state change: `reset` when the
 * draft now stands on a different base (a load, a save, an adopted update),
 * which checks at once and lifts a halt; `changed` when only the draft moved;
 * `null` when neither did. Keying the base from a check's answer changes
 * neither the base document nor its revision, so it asks for nothing.
 *
 * A change of operator token moves no generation and no base, and resets the
 * check all the same (every answer may differ): its owner calls the runner's
 * `reset` directly when the token changes.
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

/** Whether the draft has anything to save. */
export function hasChanges(state: BuilderState): boolean {
  return state.log.ops.length > 0;
}

/** The engine's handles for the draft's seats, from a check of this very generation. */
function currentHandles(state: BuilderState): ReadonlyMap<NodeKey, string> {
  if (state.check.generation !== state.generation || !state.check.sent) return new Map();
  return handlesByKey(state.check.sent, state.check.derived);
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
      const baseDraft = fromDocument(document, null);
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
      const intent = action.intent;
      if (
        intent.type === "applyTemplate" &&
        (state.mode !== "create" || state.base.document !== null)
      ) {
        return refused(
          state,
          "mode",
          "A template starts a new company. It cannot be applied to a company that exists.",
        );
      }
      const handles = currentHandles(state);
      const result = record(state.draft, intent, { handleOf: (key) => handles.get(key) });
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
        outcome.status === "clean" || outcome.status === "problems"
          ? outcome.derived
          : state.check.derived;
      let next: BuilderState = {
        ...state,
        keep: outcome.status === "guarded" ? false : state.keep,
        check: {
          generation: settled.generation,
          outcome,
          sent: settled.sent,
          problems: placed(settled.sent, outcome),
          derived:
            outcome.status === "clean" || outcome.status === "problems" ? outcome.derived : null,
        },
      };
      // Key the base by handle the first time the engine describes it. Only
      // while the log is empty: no operation refers to the path keys yet.
      if (
        state.mode === "edit" &&
        state.base.derived === null &&
        state.log.ops.length === 0 &&
        state.log.undone.length === 0 &&
        derived &&
        settled.baseRevision === state.base.revision
      ) {
        const baseDraft = fromDocument(state.base.document, derived);
        const sent = toDocument(baseDraft);
        next = {
          ...next,
          base: { ...state.base, derived },
          baseDraft,
          draft: baseDraft,
          check: { ...next.check, sent, problems: placed(sent, outcome), derived },
        };
      }
      return next;
    }

    case "tokenChanged":
      return { ...state, keep: false };

    case "saved":
      return {
        ...state,
        mode: "edit",
        base: {
          document: toDocument(state.draft).document,
          revision: action.revisionId,
          derived: action.derived,
        },
        baseDraft: state.draft,
        log: EMPTY_LOG,
        reports: [],
        generation: state.generation + 1,
        check: EMPTY_CHECK,
        keep: true,
        update: null,
        last: null,
        refusal: null,
      };

    case "updateBegin": {
      if (state.mode !== "edit")
        return refused(state, "mode", "Only a draft of an existing company can be updated.");
      const base: BaseCompany = {
        document: action.document,
        revision: action.revision,
        derived: action.derived,
      };
      const baseDraft = fromDocument(action.document, action.derived);
      const choices = new Map<number, Choice>();
      return {
        ...state,
        update: {
          base,
          baseDraft,
          ops: state.log.ops,
          choices,
          result: rebase(baseDraft, state.log.ops, choices, contextOf(baseDraft, action.derived)),
          restoring: false,
        },
      };
    }

    case "updateChoose": {
      if (!state.update) return state;
      const choices = new Map(state.update.choices);
      if (action.choice === null) choices.delete(action.index);
      else choices.set(action.index, action.choice);
      const { baseDraft, base } = state.update;
      return {
        ...state,
        update: {
          ...state.update,
          choices,
          result: rebase(baseDraft, state.update.ops, choices, contextOf(baseDraft, base.derived)),
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
      if (kept.mode !== state.mode) {
        return refused(
          state,
          "mode",
          "This draft was made for a different mode and cannot be restored here.",
        );
      }
      if (!isBaseKeyed(state)) {
        return refused(
          state,
          "not_keyed",
          "The engine has not described this company yet. Restore the draft once the check finishes.",
        );
      }
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
      const result = rebase(
        state.baseDraft,
        kept.ops,
        choices,
        contextOf(state.baseDraft, state.base.derived),
      );
      const clean = result.entries.every((e) => e.outcome === "applies");
      if (clean && kept.baseRevision === state.base.revision) {
        // Recorded on this very revision and every operation still applies:
        // the draft is the one the operator left, redo stack included.
        return {
          ...state,
          draft: result.draft,
          log: { ops: result.ops, undone: kept.undone },
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

/** The recording context of a base: the handles its derivation reports. */
function contextOf(baseDraft: Draft, derived: Derived | null) {
  const handles = handlesByKey(toDocument(baseDraft), derived);
  return { handleOf: (key: NodeKey) => handles.get(key) };
}

function placed(sent: IndexedDocument, outcome: CheckOutcome): ProblemIndex {
  if (outcome.status === "problems") {
    return placeProblems(sent, { problems: outcome.problems, derived: outcome.derived });
  }
  if (outcome.status === "clean")
    return placeProblems(sent, { warnings: outcome.warnings, derived: outcome.derived });
  return EMPTY_PROBLEMS;
}
