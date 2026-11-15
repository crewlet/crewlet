/**
 * One change to a seat's or a team's schedules: read, checked, saved.
 *
 * READ AT THE MOMENT OF THE SAVE, never from what the screen drew, for the
 * reason `lib/useCeilingWrite.ts` gives: the screen draws this node's applied
 * company, the write is conditional on the ACTIVE revision, and anything that
 * landed in between is a conflict to reload rather than an overwrite.
 *
 * CHECKED FIRST, and a check that finds a WARNING the change introduces (the
 * document is valid and something in it will not do what it says) stops and
 * says so, saving only when the person says so again. A problem the check
 * refuses is drawn beside the field it is about ([locatedProblems]).
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { dryRunEntity, getEntity, putEntity } from "~/protocol/configWrite.ts";
import { introducedWarnings } from "~/protocol/configAnswer.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { ConfigWarning } from "~/protocol/types.ts";
import type { ActivatedRevision } from "~/lib/nodeStanding.ts";
import {
  applyChange,
  entityPath,
  locatedProblems,
  scheduleRefusalWords,
  type DraftField,
  type ScheduleChange,
  type ScheduleOwner,
} from "./scheduleWrite.ts";

export type ScheduleWriteState =
  | { readonly kind: "idle" }
  | { readonly kind: "checking" }
  /** The change is what the document already says: nothing to save. */
  | { readonly kind: "unchanged" }
  /** The engine refused the change: by field where it named one, the rest whole. */
  | {
      readonly kind: "problems";
      readonly fields: Partial<Record<DraftField, string>>;
      readonly general: readonly string[];
    }
  /** The check passed and found what the change would introduce. */
  | { readonly kind: "warned"; readonly warnings: readonly ConfigWarning[] }
  | { readonly kind: "saving" }
  /** Stored, and activated as `revisionId` at `epoch`: what a follower of this node's apply reads. */
  | {
      readonly kind: "saved";
      readonly done: string;
      readonly revisionId: string;
      readonly epoch: number;
    }
  | { readonly kind: "refused"; readonly message: string; readonly reload: boolean };

interface Prepared {
  readonly etag: string;
  readonly body: Record<string, unknown>;
  readonly summary: string;
  readonly done: string;
}

export interface ScheduleWrite {
  readonly state: ScheduleWriteState;
  /** Check the change and, unless the check warns, save it. */
  readonly submit: (change: ScheduleChange) => void;
  /** Save what the last check warned about, as it was checked. */
  readonly confirm: () => void;
  readonly reset: () => void;
  readonly busy: boolean;
}

export function useScheduleWrite(
  owner: ScheduleOwner | null,
  {
    onSaved,
  }: {
    /**
     * Told once the change is stored, with what it did, said as done, and the
     * revision it activated: a screen drawing the schedule follows this node
     * applying it (`ScheduleActions`), since what it draws is this node's
     * applied company and changes only once the node applies the revision.
     */
    onSaved?: (done: string, activated: ActivatedRevision) => void;
  } = {},
): ScheduleWrite {
  const [state, setState] = useState<ScheduleWriteState>({ kind: "idle" });
  const prepared = useRef<Prepared | null>(null);
  const inflight = useRef<AbortController | null>(null);
  const savedRef = useRef(onSaved);
  savedRef.current = onSaved;

  const begin = useCallback(() => {
    inflight.current?.abort();
    const controller = new AbortController();
    inflight.current = controller;
    return controller.signal;
  }, []);
  useEffect(() => () => inflight.current?.abort(), []);

  const store = useCallback(
    async (write: Prepared, signal: AbortSignal) => {
      if (!owner) return;
      setState({ kind: "saving" });
      const outcome = await putEntity(
        entityPath(owner),
        owner.id,
        write.body,
        write.etag,
        write.summary,
        signal,
      );
      prepared.current = null;
      if (outcome.kind === "saved") {
        const activated = { revisionId: outcome.revisionId, epoch: outcome.epoch };
        setState({ kind: "saved", done: write.done, ...activated });
        savedRef.current?.(write.done, activated);
        return;
      }
      if (outcome.kind === "unchanged") {
        // THE ENGINE ALREADY HOLDS IT: nothing was stored to apply.
        setState({ kind: "unchanged" });
        return;
      }
      if (outcome.kind === "valid") {
        setState({
          kind: "refused",
          message: "The engine checked the change but saved nothing. Try again.",
          reload: false,
        });
        return;
      }
      setState({ kind: "refused", ...scheduleRefusalWords(outcome, owner) });
    },
    [owner],
  );

  const submit = useCallback(
    (change: ScheduleChange) => {
      if (!owner) return;
      const signal = begin();
      prepared.current = null;
      setState({ kind: "checking" });
      void (async () => {
        try {
          const path = entityPath(owner);
          const read = await getEntity(path, owner.id, signal);
          if (read.kind !== "entity") {
            setState({ kind: "refused", ...scheduleRefusalWords(read, owner) });
            return;
          }
          const applied = applyChange(read.entity, owner, change);
          if (applied.kind === "unchanged") {
            setState({ kind: "unchanged" });
            return;
          }
          if (applied.kind === "refused") {
            setState({ kind: "refused", message: applied.message, reload: true });
            return;
          }
          const write: Prepared = {
            etag: read.etag,
            body: applied.entity,
            summary: applied.summary,
            done: applied.done,
          };
          // THE COMPANY'S WARNINGS AS THEY STAND beside the change's, so only
          // what THIS change introduces is shown.
          const [before, after] = await Promise.all([
            dryRunEntity(path, owner.id, read.entity, read.etag, signal),
            dryRunEntity(path, owner.id, write.body, read.etag, signal),
          ]);
          if (after.kind === "problems") {
            const unitName = typeof read.entity.name === "string" ? read.entity.name : "";
            setState({
              kind: "problems",
              ...locatedProblems(after.problems, owner, applied.index, unitName),
            });
            return;
          }
          if (after.kind === "saved") {
            // A CHECK THE ENGINE STORED: saying so is the only answer that
            // cannot become a second save.
            const activated = { revisionId: after.revisionId, epoch: after.epoch };
            setState({ kind: "saved", done: write.done, ...activated });
            savedRef.current?.(write.done, activated);
            return;
          }
          if (after.kind !== "valid") {
            setState({ kind: "refused", ...scheduleRefusalWords(after, owner) });
            return;
          }
          const warnings = introducedWarnings(
            before.kind === "valid" ? before.warnings : [],
            after.warnings,
          );
          if (warnings.length > 0) {
            prepared.current = write;
            setState({ kind: "warned", warnings });
            return;
          }
          await store(write, signal);
        } catch (err) {
          if (!isAbort(err)) setState(failed(err));
        }
      })();
    },
    [begin, owner, store],
  );

  const confirm = useCallback(() => {
    const write = prepared.current;
    if (!write) return;
    const signal = begin();
    void store(write, signal).catch((err: unknown) => {
      if (!isAbort(err)) setState(failed(err));
    });
  }, [begin, store]);

  const reset = useCallback(() => {
    inflight.current?.abort();
    prepared.current = null;
    setState({ kind: "idle" });
  }, []);

  const busy = state.kind === "checking" || state.kind === "saving";
  return { state, submit, confirm, reset, busy };
}

/** A failure no answer classifies: a fault in this page, said as one. */
function failed(err: unknown): ScheduleWriteState {
  return {
    kind: "refused",
    message: `The change could not be sent: ${err instanceof Error ? err.message : String(err)}. Reload before trying again.`,
    reload: true,
  };
}
