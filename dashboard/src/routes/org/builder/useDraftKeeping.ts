/**
 * Keeping the operation log across a reload, and offering it back.
 *
 * `model/persistence.ts` decides what is stored and validates what is read;
 * this hook decides WHEN, against the company that was just loaded.
 *
 * NOTHING IS WRITTEN UNTIL THE KEPT DRAFT IS DECIDED. A freshly loaded
 * Builder has an empty log, and the storage plan for an empty log is to
 * clear, so writing before the decision would erase the very draft about to
 * be offered. The decision waits for a base the engine has keyed (in edit
 * mode, the first check's derivation), because a log recorded against
 * handles replays onto nothing else.
 *
 * WHAT THE DECISION IS (spec 3.3):
 * - the same revision offers Keep or Discard, and the Builder records nothing
 *   until the operator picks one;
 * - a different revision runs the update-my-draft flow through the reducer's
 *   `restore`, and the kept draft stays stored until that flow ends;
 * - a draft kept for the other mode is discarded, and the operator is told;
 * - a draft this page itself kept a moment ago (the operator switched lens
 *   and came back) is restored without asking: the offer exists for a draft
 *   somebody may have walked away from, not for a lens switch.
 *
 * WHAT CLEARS IT: whatever makes the plan say so (a save, a discard, an empty
 * log), and a state that may no longer be kept (`keep` false after a token
 * change or a refused token), which also withdraws an offer still on screen.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import {
  clearDraft,
  keepDraft,
  persistencePlan,
  restoreDraft,
  restoreOffer,
  type DraftStorage,
  type KeptDraft,
} from "./model/persistence.ts";
import { isBaseKeyed, type BuilderAction, type BuilderState } from "./model/reducer.ts";

/**
 * When this page last kept a draft, by its `savedAt`. Module state, because a
 * lens switch unmounts the Builder and the next mount is the one that has to
 * recognize the draft as its own.
 */
let keptByThisPage: number | null = null;

export interface KeepNotice {
  readonly tone: "neutral" | "caution";
  readonly message: string;
}

export interface DraftKeeping {
  /** A kept draft of this revision, waiting for Keep or Discard. */
  readonly offer: KeptDraft | null;
  /** True until the kept draft, if any, is decided. */
  readonly pending: boolean;
  readonly notice: KeepNotice | null;
  keep(): void;
  discard(): void;
  /** Clears the kept draft and withdraws any offer: the tab may have changed hands. */
  forget(): void;
  dismissNotice(): void;
}

const REFUSED: KeepNotice = {
  tone: "caution",
  message: "This browser refuses to keep a draft, so unsaved changes will not survive a reload.",
};

export function useDraftKeeping({
  state,
  dispatch,
  loaded,
  storage,
  now,
}: {
  state: BuilderState;
  dispatch: (action: BuilderAction) => void;
  loaded: boolean;
  storage: DraftStorage | null;
  /** Milliseconds since the epoch, for `savedAt`. */
  now: () => number;
}): DraftKeeping {
  const [decided, setDecided] = useState(false);
  const [offer, setOffer] = useState<KeptDraft | null>(null);
  // Two notices, because they end differently: what the decision found stays
  // until dismissed, and what storage refused lasts until storage accepts.
  const [decisionNotice, setDecisionNotice] = useState<KeepNotice | null>(null);
  const [storageNotice, setStorageNotice] = useState<KeepNotice | null>(null);
  // The state a restore was dispatched from: its outcome is read off the
  // first state after it, never off the state it was dispatched in.
  const restoringFrom = useRef<BuilderState | null>(null);

  const restore = useCallback(
    (kept: KeptDraft) => {
      restoringFrom.current = state;
      dispatch({ type: "restore", kept });
    },
    [dispatch, state],
  );

  // Decide the kept draft once the base can take it.
  useEffect(() => {
    if (decided || offer || restoringFrom.current || !loaded || !isBaseKeyed(state)) return;
    const restored = restoreDraft(storage);
    switch (restored.kind) {
      case "none":
      case "unavailable":
        setDecided(true);
        return;
      case "refused":
        setStorageNotice(REFUSED);
        setDecided(true);
        return;
      case "discarded":
        setDecisionNotice({
          tone: "neutral",
          message:
            "A kept draft could not be read by this version of the dashboard, so it was discarded.",
        });
        setDecided(true);
        return;
      case "restored": {
        const kept = restored.kept;
        const decision = restoreOffer(kept, { mode: state.mode, revision: state.base.revision });
        if (decision.kind === "discard_mode_changed") {
          clearDraft(storage);
          setDecisionNotice({
            tone: "caution",
            message:
              decision.kept === "create"
                ? "A draft for creating a company was discarded, because this engine now has a company."
                : "A kept draft of the previous company was discarded, because no configuration is active on this engine now.",
          });
          setDecided(true);
          return;
        }
        if (decision.kind === "update" || kept.savedAt === keptByThisPage) {
          restore(kept);
          return;
        }
        setOffer(kept);
      }
    }
  }, [decided, offer, loaded, state, storage, restore]);

  // Read a restore's outcome: adopted, refused, or an update that has ended.
  useEffect(() => {
    const from = restoringFrom.current;
    if (!from || state === from || state.update?.restoring) return;
    restoringFrom.current = null;
    const nothingRestored = state.log.ops.length === 0 && state.log.undone.length === 0;
    if (nothingRestored && state.refusal) {
      setDecisionNotice({ tone: "caution", message: state.refusal.message });
    }
    setDecided(true);
  }, [state]);

  // Keep the log, or clear it, once decided; withdraw everything when the
  // state may no longer be kept.
  useEffect(() => {
    if (!loaded) return;
    if (!state.keep) {
      clearDraft(storage);
      if (!decided) {
        setOffer(null);
        restoringFrom.current = null;
        setDecided(true);
      }
      return;
    }
    if (!decided) return;
    const plan = persistencePlan(
      {
        mode: state.mode,
        baseRevision: state.base.revision,
        log: state.log,
        keep: state.keep,
      },
      now(),
    );
    const result = plan.action === "clear" ? clearDraft(storage) : keepDraft(storage, plan.kept);
    if (plan.action === "keep" && result === "kept") keptByThisPage = plan.kept.savedAt;
    if (plan.action === "clear" && result === "cleared") keptByThisPage = null;
    setStorageNotice(
      result === "refused"
        ? REFUSED
        : result === "too_large"
          ? {
              tone: "caution",
              message:
                "This draft holds more changes than can be kept across a reload. Save it in steps, or it is lost if the tab reloads.",
            }
          : null,
    );
    // `state.log`, `keep`, `mode` and the base revision are what the plan
    // reads; a check answer, or the base being keyed, moves none of them.
  }, [loaded, decided, state.log, state.keep, state.mode, state.base.revision, storage, now]);

  const keep = useCallback(() => {
    if (!offer) return;
    setOffer(null);
    restore(offer);
  }, [offer, restore]);

  const discard = useCallback(() => {
    clearDraft(storage);
    setOffer(null);
    setDecided(true);
  }, [storage]);

  const forget = useCallback(() => {
    clearDraft(storage);
    setOffer(null);
    restoringFrom.current = null;
    setDecided(true);
  }, [storage]);

  const dismissNotice = useCallback(() => setDecisionNotice(null), []);

  return {
    offer,
    pending: !decided,
    notice: storageNotice ?? decisionNotice,
    keep,
    discard,
    forget,
    dismissNotice,
  };
}
