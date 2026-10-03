/**
 * Keeping the operation log across a reload, and offering it back.
 *
 * `model/persistence.ts` decides what is stored and validates what is read;
 * this hook decides WHEN, against the company that was just loaded.
 *
 * NOTHING IS WRITTEN UNTIL THE KEPT DRAFT IS DECIDED. A freshly loaded
 * Builder has an empty log, and the storage plan for an empty log is to
 * clear, so writing before the decision would erase the very draft about to
 * be offered. The decision waits for the company to be loaded, because a log
 * replays onto nothing else.
 *
 * IT ALSO WAITS FOR THE READER — who this tab is read by (`lib/reader.ts`) —
 * because a draft is kept FOR somebody, and one kept for anybody else is
 * discarded without being offered or mentioned.
 *
 * WHAT THE DECISION IS (spec 3.3):
 * - a draft kept for another reader is discarded, and nothing is said: it is
 *   a colleague's unsaved work, and not this reader's to hear about;
 * - the same company (settings revision and chart rows) offers Keep or
 *   Discard, and the Builder records nothing until the operator picks one;
 * - a moved company runs the update-my-draft flow through the reducer's
 *   `restore`, and the kept draft stays stored until that flow ends;
 * - a draft kept for the other mode is discarded, and the operator is told —
 *   except a create draft whose save was out, which is carried onto the
 *   company that save made;
 * - a draft this page itself kept a moment ago (the operator left Edit org
 *   and came back) is restored without asking: the offer exists for a draft
 *   somebody may have walked away from, not for a trip away and back.
 *
 * WHAT CLEARS IT: whatever makes the plan say so (a save, a discard, an empty
 * log), and a state that may no longer be kept (`keep` false after a change
 * of reader or a refused credential), which also withdraws an offer still on
 * screen.
 *
 * A KEPT DRAFT A SAVE WAS SENT FOR IS CARRIED ONTO WHAT LANDED. A save marks
 * the kept log with its write id and the nodes it creates before its first
 * write (`markWrite`), and the mark comes off once nothing about the save is
 * unknown. A draft found still marked is a save whose outcome this tab lost,
 * perhaps after the operator left the builder, and part of it may have landed:
 * it is restored as an update, never offered as Keep, and the rebase resolves
 * its own creations onto the nodes the chart holds (`history.rebase`). A run
 * this page still has OUT is waited for first (`useSave.saveInFlight`): its
 * writes may not have landed yet, and deciding beside it would carry the log
 * onto a company that is still changing under it.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import {
  clearDraft,
  keepDraft,
  markPendingWrite,
  persistencePlan,
  restoreDraft,
  restoreOffer,
  type DraftStorage,
  type KeptDraft,
  type PendingWrite,
} from "./model/persistence.ts";
import type { BuilderAction, BuilderState } from "./model/reducer.ts";
import { saveInFlight } from "./useSave.ts";

/**
 * When this page last kept a draft, by its `savedAt`. Module state, because a
 * trip away from Edit org unmounts the Builder and the next mount is the one
 * that has to recognize the draft as its own.
 */
let keptByThisPage: number | null = null;

export interface KeepNotice {
  readonly tone: "neutral" | "warning";
  readonly message: string;
}

export interface DraftKeeping {
  /** A kept draft of this revision, waiting for Keep or Discard. */
  readonly offer: KeptDraft | null;
  /**
   * Whether the draft as it stands is found again after a reload or a trip
   * off the builder: false while storage refuses it or it is over the cap.
   */
  readonly survives: boolean;
  /** True until the kept draft, if any, is decided. */
  readonly pending: boolean;
  /**
   * A save this page sent before the builder was left is still out, and the
   * decision waits for it. Editing waits too: the kept draft is what that save
   * is carried onto if it stops part way, and a new log written over it
   * meanwhile would lose whatever of it did not land.
   */
  readonly waiting: boolean;
  /**
   * Marks the kept draft with a save being sent, or clears the mark once
   * nothing about the save is unknown (`null`). Works on storage directly, so
   * a save settling after the builder was left still marks what it must.
   */
  markWrite(pending: PendingWrite | null): void;
  readonly notice: KeepNotice | null;
  keep(): void;
  discard(): void;
  /** Clears the kept draft and withdraws any offer: the tab may have changed hands. */
  forget(): void;
  dismissNotice(): void;
}

const REFUSED: KeepNotice = {
  tone: "warning",
  message:
    "This browser refuses to keep a draft, so unsaved changes will not survive a reload or leaving the builder.",
};

export function useDraftKeeping({
  state,
  dispatch,
  loaded,
  storage,
  now,
  reader,
}: {
  state: BuilderState;
  dispatch: (action: BuilderAction) => void;
  loaded: boolean;
  storage: DraftStorage | null;
  /** Milliseconds since the epoch, for `savedAt`. */
  now: () => number;
  /** The principal this tab is read by, or null until it is known. */
  reader: string | null;
}): DraftKeeping {
  const [decided, setDecided] = useState(false);
  const [offer, setOffer] = useState<KeptDraft | null>(null);
  // A run of this page's own still out: the decision waits for it.
  const [waiting, setWaiting] = useState(false);
  // The save every keep carries while it is out, so a rewrite of the log
  // never drops the mark the save set.
  const pendingWrite = useRef<PendingWrite | null>(null);
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

  // Decide the kept draft once the company is loaded, and once no run of this
  // page's own is still writing to it.
  useEffect(() => {
    if (decided || offer || waiting || restoringFrom.current || !loaded || reader === null) return;
    const running = saveInFlight();
    if (running) {
      setWaiting(true);
      void running.then(() => setWaiting(false));
      return;
    }
    const restored = restoreDraft(storage);
    switch (restored.kind) {
      case "none":
        setDecided(true);
        return;
      // NO STORAGE IS STORAGE THAT REFUSED. The tab's storage is `null` only
      // when the browser threw on the accessor itself (a sandboxed frame,
      // blocked site data), and the operator loses the draft on a reload
      // exactly as they do when a write is refused, so they are told the same.
      case "unavailable":
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
        const decision = restoreOffer(kept, {
          mode: state.mode,
          revision: state.base.revision,
          print: state.base.print,
          reader,
        });
        if (decision.kind === "discard_other_reader") {
          clearDraft(storage);
          setDecided(true);
          return;
        }
        if (decision.kind === "discard_mode_changed") {
          clearDraft(storage);
          setDecisionNotice({
            tone: "warning",
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
  }, [decided, offer, waiting, loaded, reader, state, storage, restore]);

  // Read a restore's outcome: adopted, refused, or an update that has ended.
  useEffect(() => {
    const from = restoringFrom.current;
    if (!from || state === from || state.update?.restoring) return;
    restoringFrom.current = null;
    const nothingRestored = state.log.ops.length === 0 && state.log.undone.length === 0;
    if (nothingRestored && state.refusal) {
      setDecisionNotice({ tone: "warning", message: state.refusal.message });
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
    // DECIDED IMPLIES A READER, since the decision waits for one; the check
    // is what lets the plan carry it without a cast.
    if (!decided || reader === null) return;
    const plan = persistencePlan(
      {
        mode: state.mode,
        baseRevision: state.base.revision,
        basePrint: state.base.print,
        log: state.log,
        keep: state.keep,
        pending: pendingWrite.current,
        reader,
      },
      now(),
    );
    const result = plan.action === "clear" ? clearDraft(storage) : keepDraft(storage, plan.kept);
    if (plan.action === "keep" && result === "kept") keptByThisPage = plan.kept.savedAt;
    if (plan.action === "clear" && result === "cleared") keptByThisPage = null;
    setStorageNotice(
      result === "refused" || result === "unavailable"
        ? REFUSED
        : result === "too_large"
          ? {
              tone: "warning",
              message:
                "This draft holds more changes than can be kept across a reload. Save it in steps, or it is lost if the tab reloads or you leave the builder.",
            }
          : null,
    );
    // `state.log`, `keep`, `mode` and the base's identity are what the plan
    // reads; a check answer, or a new derivation, moves none of them.
  }, [
    loaded,
    decided,
    state.log,
    state.keep,
    state.mode,
    state.base.revision,
    state.base.print,
    storage,
    now,
    reader,
  ]);

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

  const markWrite = useCallback(
    (pending: PendingWrite | null) => {
      pendingWrite.current = pending;
      markPendingWrite(storage, pending);
    },
    [storage],
  );

  const dismissNotice = useCallback(() => setDecisionNotice(null), []);

  return {
    offer,
    survives: storageNotice === null,
    pending: !decided,
    waiting,
    notice: storageNotice ?? decisionNotice,
    keep,
    discard,
    forget,
    markWrite,
    dismissNotice,
  };
}
