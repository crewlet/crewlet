/**
 * Changing a setting of the company document from the page that draws it:
 * read, check, save, and watch this node apply it.
 *
 * The same four steps a token ceiling takes (`lib/useCeilingWrite.ts`, whose
 * head says why each exists), for any setting a merge patch names:
 *
 *  1. READ AT THE MOMENT OF THE SAVE. A settings page draws what THIS node
 *     applied; the write is conditional on the ACTIVE revision, which may be
 *     a colleague's newer one. So the change is worked out by the caller's
 *     plan against the document the read returns, and carries that revision's
 *     tag (`If-Match`): anything that landed in between is a conflict to
 *     reload, never an overwrite.
 *  2. CHECKED FIRST (`?dry_run=true`), beside the same document unchanged, so
 *     a warning the change INTRODUCES stops the save and is said, and the
 *     engine's refusal of an invalid value is read before anything is stored.
 *  3. SAVED with the summary the plan wrote, which is what the revision
 *     history shows for it.
 *  4. NOT APPLIED UNTIL THIS NODE SAYS SO. The answer names the revision and
 *     the epoch it activated at; `standing` follows where this node stands on
 *     it (`lib/nodeStanding.ts`), and the caller re-reads what it draws once
 *     the node applied it or a later activation passed it.
 *
 * The save is confirmed here, with the toast every write raises
 * (`lib/savedToast.ts`); a screen draws only what the person must act on.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { dryRunPatch, getConfig, savePatch } from "~/protocol/configWrite.ts";
import { introducedWarnings } from "~/protocol/configAnswer.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { ConfigRefusal } from "~/protocol/configAnswer.ts";
import type { CompanyDocument, ConfigProblem, ConfigWarning } from "~/protocol/types.ts";
import { configRefusalWords, type RefusalWords } from "./configRefusal.ts";
import { nodeStanding, type Standing } from "./nodeStanding.ts";
import { useLastHealth } from "./useLastHealth.ts";
import { useSavedToast, type SavedWords } from "./savedToast.ts";

/** One change to the document: the merge patch that makes it, and its audit summary. */
export interface SettingChange {
  readonly patch: Record<string, unknown>;
  readonly summary: string;
}

/**
 * What a press changes, worked out against the document as it is NOW. Null
 * when the document already holds what was asked for, and `"conflict"` when it
 * no longer holds what the change was made to (a rule edited or removed since
 * the form was filled from it), which is a reload, never a write over it.
 */
export type SettingPlan = (document: CompanyDocument) => SettingChange | null | "conflict";

/** What the last change came to. */
export type SettingWriteState =
  | { readonly kind: "idle" }
  | { readonly kind: "checking" }
  /** The document already holds what was asked for: nothing to save. */
  | { readonly kind: "unchanged" }
  /** The check passed and found what the change would introduce. */
  | { readonly kind: "warned"; readonly warnings: readonly ConfigWarning[] }
  | { readonly kind: "saving" }
  | {
      readonly kind: "saved";
      readonly revisionId: string;
      readonly epoch: number;
      /** Where this node stands on the saved revision. */
      readonly standing: Standing["kind"];
      readonly summary: string;
    }
  | {
      readonly kind: "refused";
      readonly message: string;
      readonly reload: boolean;
      /** The engine's problems with the document, for a form that places each under its field. */
      readonly problems?: readonly ConfigProblem[];
    };

export interface SettingWrite {
  readonly state: SettingWriteState;
  /** Check the change and, unless the check warns, save it. */
  readonly submit: (plan: SettingPlan) => void;
  /** Save what the last check warned about, as it was checked. */
  readonly confirm: () => void;
  /** Back to idle, dropping a warned write and any request in flight. */
  readonly reset: () => void;
  readonly busy: boolean;
}

/** A write checked and ready to store, held while a warning is confirmed. */
interface Prepared extends SettingChange {
  readonly etag: string;
}

export function useSettingWrite({
  reread,
  onApplied,
  problemWords,
  said,
}: {
  /** How a conflict ends, in the page's words: "Reload to see the limits as they are now." */
  reread: string;
  /** Called once this node applies the saved revision, or a later one passes it. */
  onApplied?: () => void;
  /**
   * The engine's refusal of the document, in the page's words for the one
   * setting it changes, in place of each problem's line for an operator: the
   * page knows which setting a problem is about, and what it is called.
   */
  problemWords?: (problems: readonly ConfigProblem[]) => string;
  /** The save's toast in the page's words; the revision's summary otherwise. */
  said?: SavedWords;
}): SettingWrite {
  const [state, setState] = useState<SettingWriteState>({ kind: "idle" });
  const prepared = useRef<Prepared | null>(null);
  const inflight = useRef<AbortController | null>(null);
  const health = useLastHealth();
  const appliedRef = useRef(onApplied);
  appliedRef.current = onApplied;
  const wordsRef = useRef(problemWords);
  wordsRef.current = problemWords;
  const refusalWords = useCallback(
    (
      refusal: ConfigRefusal | { readonly kind: "none" },
    ): RefusalWords & { readonly problems?: readonly ConfigProblem[] } => {
      const words = wordsRef.current;
      if (refusal.kind === "problems") {
        const said = words ? { message: words(refusal.problems), reload: false } : null;
        return { ...(said ?? configRefusalWords(refusal, reread)), problems: refusal.problems };
      }
      return configRefusalWords(refusal, reread);
    },
    [reread],
  );

  // A NEW PRESS SUPERSEDES THE OLD, and leaving the page abandons both.
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
      const outcome = await savePatch(write.patch, write.etag, write.summary, signal);
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
      if (outcome.kind === "unchanged") {
        // THE ENGINE ALREADY HOLDS IT: the write left the document as it
        // was, and stored nothing to apply.
        setState({ kind: "unchanged" });
        return;
      }
      if (outcome.kind === "valid") {
        // A 200 to a write is a check the engine took for one: nothing stored.
        setState({
          kind: "refused",
          message: "The engine checked the change but stored nothing. Try again.",
          reload: false,
        });
        return;
      }
      setState({ kind: "refused", ...refusalWords(outcome) });
    },
    [refusalWords],
  );

  const submit = useCallback(
    (plan: SettingPlan) => {
      const signal = begin();
      prepared.current = null;
      setState({ kind: "checking" });
      void (async () => {
        try {
          const read = await getConfig(signal);
          if (read.kind !== "document") {
            setState({ kind: "refused", ...refusalWords(read) });
            return;
          }
          const change = plan(read.document);
          if (change === null) {
            setState({ kind: "unchanged" });
            return;
          }
          if (change === "conflict") {
            setState({
              kind: "refused",
              ...configRefusalWords(
                { kind: "conflict", reason: "revision_advanced", currentRevisionId: null },
                reread,
              ),
            });
            return;
          }
          const write: Prepared = { ...change, etag: read.etag };
          // The company's own warnings as they stand, beside the change's, so
          // only what THIS change introduces is shown. The baseline is the
          // empty patch, a check of the company as it is.
          const [before, after] = await Promise.all([
            dryRunPatch({}, read.etag, signal),
            dryRunPatch(write.patch, read.etag, signal),
          ]);
          if (after.kind === "saved") {
            // A check the engine stored: a revision exists, and saying so is
            // the only answer that cannot become a second save.
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
            setState({ kind: "refused", ...refusalWords(after) });
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
    [begin, reread, refusalWords, store],
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

  // APPLIED WHEN THIS NODE SAYS SO, by its last health push and by none
  // before the first (`useCeilingWrite` has the reasons).
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
  useSavedToast(state.kind === "saved" ? state : null, said);

  const busy = state.kind === "checking" || state.kind === "saving";
  return { state, submit, confirm, reset, busy };
}

/** A fault in this page rather than a refusal the engine made. */
function failed(err: unknown): SettingWriteState {
  return {
    kind: "refused",
    message: `The change could not be sent: ${err instanceof Error ? err.message : String(err)}. Reload before trying again.`,
    reload: true,
  };
}
