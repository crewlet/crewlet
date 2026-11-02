/**
 * Changing one scope's token ceilings: read, check, save, and watch it apply.
 *
 * EVERY SAVE IS CHECKED FIRST, against the revision it was read from. The
 * engine validates the whole company behind a ceiling — a seat ceiling above
 * the company's, a week below its own day — and some of what it finds is a
 * WARNING rather than a refusal: the document is valid and the ceiling will
 * never refuse a turn. A save sent blind stores that and says "Saved"; a check
 * first is the only place the person can see it before it is theirs. So a
 * change that introduces a warning stops and says it, and saves only when the
 * person says so again; one that introduces none saves straight away.
 *
 * READ AT THE MOMENT OF THE SAVE, never from what the screen drew. The screen
 * draws the budgets answer, which is the epoch THIS node applied; the write is
 * conditional on the ACTIVE revision, which may be a colleague's newer one. A
 * change is therefore worked out against what the read returns — the typed
 * values compared with the ceilings the document holds now — and the write
 * carries that revision's tag, so anything that lands in between is a
 * conflict to reload rather than an overwrite.
 *
 * A SAVE IS NOT AN APPLY. The answer names the revision and the epoch it
 * activated at; until this node has applied it, the gate still enforces the
 * ceiling before, and so does every figure on the screen. The state follows
 * where this node stands on it (`nodeStanding`, the rule every follower of a
 * config write reads): applying until the health push says this node applied
 * it, and then the caller re-reads what it draws — or refused, when this node's
 * attempt at it failed, said as that rather than as an apply still running,
 * since the applied epoch a refusal leaves where it was would have said
 * "applying" for the life of the page while the gate went on enforcing the
 * ceiling before. A save a later activation passed before this node applied
 * it is re-read like an apply: there is nothing more to wait for.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import {
  dryRunEntity,
  dryRunPatch,
  getConfig,
  getEntity,
  putEntity,
  savePatch,
  type ConfigWriteOutcome,
} from "~/protocol/configWrite.ts";
import { introducedWarnings } from "~/protocol/configAnswer.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { ConfigWarning, TokenBudget } from "~/protocol/types.ts";
import {
  ceilingSummary,
  changesFrom,
  documentCeilings,
  documentPatch,
  refusalWords,
  seatWithCeilings,
  type CeilingChanges,
  type CeilingScope,
  type Period,
} from "./ceilings.ts";
import { nodeStanding, type Standing } from "./nodeStanding.ts";
import { useLastHealth } from "./useLastHealth.ts";

/** What the last change came to. */
export type CeilingWriteState =
  | { readonly kind: "idle" }
  /** Reading the scope and checking the change against it. */
  | { readonly kind: "checking" }
  /** The typed values are the ceilings already held: nothing to save. */
  | { readonly kind: "unchanged" }
  /** The check passed and found what the change would introduce. */
  | { readonly kind: "warned"; readonly warnings: readonly ConfigWarning[] }
  | { readonly kind: "saving" }
  /**
   * Stored as `revisionId` and activated at `epoch`. `standing` is where THIS
   * node — the one whose budgets answer the screen draws — stands on it:
   * `applying`, `refused` (it runs the ceiling before), or settled by
   * `applied` or `passed`, after which the caller has been asked to re-read.
   */
  | {
      readonly kind: "saved";
      readonly revisionId: string;
      readonly epoch: number;
      readonly standing: Standing["kind"];
      readonly summary: string;
    }
  | { readonly kind: "refused"; readonly message: string; readonly reload: boolean };

/** A write checked and ready to store, held while a warning is confirmed. */
interface Prepared {
  readonly etag: string;
  readonly body: Record<string, unknown>;
  readonly summary: string;
}

/** The typed value of each window a form offers. */
export type TypedCeilings = Readonly<Partial<Record<Period, string>>>;

export interface CeilingWrite {
  readonly state: CeilingWriteState;
  /** Check the typed values and, unless the check warns, save them. */
  readonly submit: (typed: TypedCeilings) => void;
  /** Save what the last check warned about, as it was checked. */
  readonly confirm: () => void;
  /** Back to idle, dropping a warned write and any request in flight. */
  readonly reset: () => void;
  readonly busy: boolean;
}

export function useCeilingWrite(
  scope: CeilingScope,
  {
    onApplied,
  }: {
    /**
     * Called once this node applies the saved revision, or a later activation
     * passes it — re-read what the screen draws.
     */
    onApplied?: () => void;
  } = {},
): CeilingWrite {
  const [state, setState] = useState<CeilingWriteState>({ kind: "idle" });
  const prepared = useRef<Prepared | null>(null);
  const inflight = useRef<AbortController | null>(null);
  const health = useLastHealth();
  const appliedRef = useRef(onApplied);
  appliedRef.current = onApplied;

  // A NEW PRESS SUPERSEDES THE OLD, and leaving the screen abandons both: an
  // answer arriving for a field nobody is looking at would set state on a
  // component that is gone, or on the next press's field.
  const begin = useCallback(() => {
    inflight.current?.abort();
    const controller = new AbortController();
    inflight.current = controller;
    return controller.signal;
  }, []);
  useEffect(() => () => inflight.current?.abort(), []);

  const store = useCallback(
    async (write: Prepared, signal: AbortSignal) => {
      setState({ kind: "saving" });
      const outcome =
        scope.kind === "seat"
          ? await putEntity("roles", scope.handle, write.body, write.etag, write.summary, signal)
          : await savePatch(write.body, write.etag, write.summary, signal);
      prepared.current = null;
      if (outcome.kind === "saved") {
        setState({
          kind: "saved",
          revisionId: outcome.revisionId,
          epoch: outcome.epoch,
          standing: "applying",
          summary: write.summary,
        });
        return;
      }
      if (outcome.kind === "valid") {
        // A 200 to a write is a check the engine took for one, which stored
        // nothing; saying "saved" over it would be the one lie this avoids.
        setState({
          kind: "refused",
          message: "The engine checked the change but stored nothing. Try again.",
          reload: false,
        });
        return;
      }
      setState({ kind: "refused", ...refusalWords(outcome, scope) });
    },
    [scope],
  );

  const submit = useCallback(
    (typed: TypedCeilings) => {
      const signal = begin();
      prepared.current = null;
      setState({ kind: "checking" });
      void (async () => {
        try {
          // 1. What the scope holds NOW, and the tag of the revision it is in.
          let etag: string;
          let held: TokenBudget;
          let unchanged: Record<string, unknown>;
          let build: (changes: CeilingChanges) => Record<string, unknown>;
          if (scope.kind !== "seat") {
            const read = await getConfig(signal);
            if (read.kind !== "document") {
              setState({ kind: "refused", ...refusalWords(read, scope) });
              return;
            }
            etag = read.etag;
            held = budgetOf(documentCeilings(scope, read.document));
            unchanged = {};
            build = (changes) => documentPatch(scope, changes);
          } else {
            const read = await getEntity("roles", scope.handle, signal);
            if (read.kind !== "entity") {
              setState({ kind: "refused", ...refusalWords(read, scope) });
              return;
            }
            etag = read.etag;
            held = budgetOf(read.entity.token_budget);
            unchanged = read.entity;
            build = (changes) => seatWithCeilings(read.entity, changes);
          }

          // 2. What the typed values change, against THAT.
          const changes = changesFrom(typed, held);
          if (changes === undefined || Object.keys(changes).length === 0) {
            setState({ kind: "unchanged" });
            return;
          }
          const write: Prepared = {
            etag,
            body: build(changes),
            summary: ceilingSummary(scope, changes, held),
          };

          // 3. The check, beside the company's own warnings as they stand, so
          // only what THIS change introduces is shown.
          const check = (body: Record<string, unknown>): Promise<ConfigWriteOutcome> =>
            scope.kind === "seat"
              ? dryRunEntity("roles", scope.handle, body, etag, signal)
              : dryRunPatch(body, etag, signal);
          const [before, after] = await Promise.all([check(unchanged), check(write.body)]);
          if (after.kind === "saved") {
            // A CHECK THE ENGINE STORED. No route this page sends a check to
            // answers one with 201, but a 201 means a revision exists, and
            // saying so is the only answer that cannot become a second save.
            setState({
              kind: "saved",
              revisionId: after.revisionId,
              epoch: after.epoch,
              standing: "applying",
              summary: write.summary,
            });
            return;
          }
          if (after.kind !== "valid") {
            setState({ kind: "refused", ...refusalWords(after, scope) });
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

          // 4. Nothing to confirm: store it.
          await store(write, signal);
        } catch (err) {
          if (!isAbort(err)) setState(failed(err));
        }
      })();
    },
    [begin, scope, store],
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

  // APPLIED WHEN THIS NODE SAYS SO — the node whose budgets answer the screen
  // draws — and not a moment before, or the re-read would draw the ceiling it
  // was replacing and call it the new one. Applied and passed settle the save
  // and ask for that re-read once; a refusal is followed on, since a node
  // that restarts attempts the revision again, and a later activation it
  // applies, or passes, settles the save as either does. BY ITS LAST PUSH
  // (`useLastHealth`), and by none before the first: a socket that drops says
  // nothing about the save, and read as a standing it put a refused ceiling
  // back to the saved figure, applying, until the socket came back.
  const standing =
    state.kind === "saved" && health !== null
      ? nodeStanding(health, { revisionId: state.revisionId, epoch: state.epoch }).kind
      : null;
  useEffect(() => {
    if (state.kind !== "saved" || standing === null || standing === state.standing) return;
    if (state.standing === "applied" || state.standing === "passed") return;
    setState({ ...state, standing });
    if (standing === "applied" || standing === "passed") appliedRef.current?.();
  }, [state, standing]);

  const busy = state.kind === "checking" || state.kind === "saving";
  return { state, submit, confirm, reset, busy };
}

/**
 * A failure no answer classifies — every refusal is a value (`configWrite.ts`
 * rejects only on an abort), so this is a fault in this page, said as one and
 * never as a refusal the engine made.
 */
function failed(err: unknown): CeilingWriteState {
  return {
    kind: "refused",
    message: `The change could not be sent: ${err instanceof Error ? err.message : String(err)}. Reload before trying again.`,
    reload: true,
  };
}

/** A stored `token_budget:` as numbers, whatever shape it arrived in. */
function budgetOf(value: unknown): TokenBudget {
  const out: Partial<Record<Period, number>> = {};
  if (value && typeof value === "object" && !Array.isArray(value)) {
    for (const [key, ceiling] of Object.entries(value)) {
      if ((key === "day" || key === "week" || key === "month") && typeof ceiling === "number") {
        out[key] = ceiling;
      }
    }
  }
  return out;
}
