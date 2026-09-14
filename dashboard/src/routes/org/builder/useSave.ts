/**
 * Saving the draft, and settling a save whose answer never arrived.
 *
 * ONE REQUEST, THE ONE THE CHECK VALIDATED. A save is `model/transport.ts`'s
 * save request of the draft as it stands: in edit mode a JSON merge patch of
 * what changed, conditional on the base revision with `If-Match`; in create
 * mode the whole document with `If-None-Match: *`. The audit summary rides in
 * the body, signed with a write id.
 *
 * THE WRITE ID BELONGS TO THE DRAFT, NOT TO THE CLICK. It is minted when the
 * review opens (an event handler) and kept for as long as the draft does not
 * change. So a second press after an answer that never arrived sends the
 * SAME id, and settling (`writes.settleUnknownWrite`) recognizes the first
 * attempt if it landed. A fresh id per press would let a landed first
 * attempt read as somebody else's save, and the conflict that follows would
 * replay every operation onto the operator's own revision. While an outcome
 * is unknown the Builder records nothing, for the same reason.
 *
 * A WRITE IN FLIGHT IS NEVER ABORTED. Leaving the lens mid-save unmounts the
 * Builder, but aborting the request would only turn a write that may land
 * into one nobody hears about. The answer is still handled, and a save that
 * landed after the Builder went away is still recorded and clears the kept
 * draft, so a later visit does not offer to replay it.
 */

import { useCallback, useRef, useState, type MutableRefObject } from "react";
import type { Derived } from "~/protocol/index.ts";
import { toDocument, type IndexedDocument } from "./model/document.ts";
import type { KeySource } from "./model/keys.ts";
import type { BuilderState } from "./model/reducer.ts";
import type { CheckOutcome, ConflictReason, SettledCheck } from "./model/scheduler.ts";
import { saveRequest, type BuilderMode, type ConfigTransport } from "./model/transport.ts";
import {
  classifySave,
  newWriteId,
  settleUnknownWrite,
  signedSummary,
  type SaveAttempt,
} from "./model/writes.ts";

export type SavePhase =
  | { readonly kind: "idle" }
  | { readonly kind: "saving" }
  | { readonly kind: "settling" }
  /** Whether the save landed could not be established. */
  | { readonly kind: "unknown"; readonly detail: string }
  /** The engine refused the save; the Builder has been told why. */
  | { readonly kind: "refused"; readonly message: string }
  /** The save did not land and nothing else did either: it can simply be sent again. */
  | { readonly kind: "retry"; readonly message: string };

export interface Landed {
  readonly revisionId: string;
  /** From the write's answer; `null` for a write found by settling. */
  readonly epoch: number | null;
  readonly derived: Derived | null;
  readonly mode: BuilderMode;
  /** The revision active now: the write, or a later one built on it. */
  readonly activeRevisionId: string;
}

export interface SaveEvents {
  onLanded(landed: Landed): void;
  onConflict(conflict: { reason: ConflictReason; currentRevisionId: string | null }): void;
  /** A refusal about the document or the request, for the Builder to place and re-check. */
  onRefused(settled: SettledCheck): void;
}

export interface Save {
  readonly phase: SavePhase;
  /** The write id the review shows, once one is prepared. */
  readonly writeId: string | null;
  /** True while the last attempt's outcome is not known. */
  readonly unsettled: boolean;
  /** Prepares the write id for the draft as it stands. Call it where the review opens. */
  prepare(): string;
  save(summary: string): Promise<void>;
  /** Asks the engine again whether an unanswered save landed. */
  checkAgain(): Promise<void>;
  /** Forgets a finished outcome's message. */
  acknowledge(): void;
}

/** What the review says about a refusal the check would also have given. */
function refusalMessage(outcome: CheckOutcome): string {
  switch (outcome.status) {
    case "problems":
      return "The engine refused the save. The problems it found are marked on the chart; fix them, then save again.";
    case "guarded":
      return "The engine refused this browser's token. Set a token it accepts, then save again.";
    case "readonly":
      return "This process cannot write the configuration because it has no coordination store.";
    default:
      return "The engine refused the save.";
  }
}

export function useSave({
  stateRef,
  transport,
  keys,
  events,
}: {
  stateRef: MutableRefObject<BuilderState>;
  transport: ConfigTransport;
  keys: KeySource;
  events: MutableRefObject<SaveEvents>;
}): Save {
  const [phase, setPhase] = useState<SavePhase>({ kind: "idle" });
  const write = useRef<{ id: string; generation: number } | null>(null);
  const [writeId, setWriteId] = useState<string | null>(null);
  const [unsettled, setUnsettled] = useState(false);
  const unsettledRef = useRef(false);
  const lastAttempt = useRef<SaveAttempt | null>(null);

  const markUnsettled = (value: boolean) => {
    unsettledRef.current = value;
    setUnsettled(value);
  };

  const prepare = useCallback((): string => {
    const generation = stateRef.current.generation;
    // The same draft, or an attempt whose outcome is still unknown, keeps its id.
    if (!write.current || (write.current.generation !== generation && !unsettledRef.current)) {
      write.current = { id: newWriteId(keys), generation };
    }
    setWriteId(write.current.id);
    return write.current.id;
  }, [keys, stateRef]);

  const landed = useCallback(
    (result: Landed) => {
      write.current = null;
      setWriteId(null);
      markUnsettled(false);
      setPhase({ kind: "idle" });
      events.current.onLanded(result);
      // `events` is a ref the Builder keeps current; its identity never changes.
    },
    [events],
  );

  const settle = useCallback(
    async (attempt: SaveAttempt, currentRevisionId: string | null) => {
      setPhase({ kind: "settling" });
      const signal = new AbortController().signal;
      const result = await settleUnknownWrite(transport, attempt, currentRevisionId, signal);
      switch (result.kind) {
        case "landed":
          landed({
            revisionId: result.revisionId,
            epoch: null,
            derived: null,
            mode: attempt.mode,
            activeRevisionId: result.activeRevisionId,
          });
          return;
        case "not_landed": {
          markUnsettled(false);
          const nothingMoved =
            attempt.mode === "edit"
              ? result.currentRevisionId === attempt.baseRevision
              : result.currentRevisionId === null;
          if (nothingMoved) {
            setPhase({
              kind: "retry",
              message:
                "The save did not reach the engine, and nothing was stored. Save again once the engine is reachable.",
            });
            return;
          }
          setPhase({ kind: "idle" });
          events.current.onConflict({
            reason: attempt.mode === "create" ? "already_configured" : "revision_advanced",
            currentRevisionId: result.currentRevisionId,
          });
          return;
        }
        case "unknown":
          setPhase({ kind: "unknown", detail: result.detail });
          return;
      }
    },
    [transport, landed, events],
  );

  const save = useCallback(
    async (summary: string) => {
      const state = stateRef.current;
      const id = prepare();
      const attempt: SaveAttempt = {
        writeId: id,
        mode: state.mode,
        baseRevision: state.base.revision,
      };
      const sent: IndexedDocument = toDocument(state.draft);
      const request = saveRequest(
        { mode: state.mode, baseRevision: state.base.revision, base: state.base.document, sent },
        signedSummary(summary, id),
      );
      lastAttempt.current = attempt;
      setPhase({ kind: "saving" });
      const answer = await transport.send(request, new AbortController().signal);
      const outcome = classifySave(answer, attempt, unsettledRef.current);
      switch (outcome.kind) {
        case "saved":
          landed({
            revisionId: outcome.revisionId,
            epoch: outcome.epoch,
            derived: outcome.derived,
            mode: attempt.mode,
            activeRevisionId: outcome.revisionId,
          });
          return;
        case "unknown":
          markUnsettled(true);
          await settle(attempt, outcome.currentRevisionId);
          return;
        case "refused": {
          // A refusal says nothing about an EARLIER attempt whose answer was
          // lost, so an unsettled outcome stays unsettled through it.
          const refused = outcome.outcome;
          if (refused.status === "conflict") {
            setPhase({ kind: "idle" });
            events.current.onConflict({
              reason: refused.reason,
              currentRevisionId: refused.currentRevisionId,
            });
            return;
          }
          if (refused.status === "clean" || refused.status === "unreachable") {
            // A success that is not a 201 stored nothing this client can name,
            // and a failure the classifier could not place is not a refusal:
            // both are settled as an unknown outcome rather than believed.
            markUnsettled(true);
            await settle(attempt, null);
            return;
          }
          setPhase({ kind: "refused", message: refusalMessage(refused) });
          events.current.onRefused({
            generation: state.generation,
            sent,
            baseRevision: attempt.baseRevision,
            outcome: refused,
          });
          return;
        }
      }
    },
    [stateRef, prepare, transport, landed, settle, events],
  );

  const checkAgain = useCallback(async () => {
    const attempt = lastAttempt.current;
    if (!attempt) return;
    await settle(attempt, null);
  }, [settle]);

  const acknowledge = useCallback(() => {
    setPhase((current) =>
      current.kind === "refused" || current.kind === "retry" ? { kind: "idle" } : current,
    );
  }, []);

  return { phase, writeId, unsettled, prepare, save, checkAgain, acknowledge };
}
