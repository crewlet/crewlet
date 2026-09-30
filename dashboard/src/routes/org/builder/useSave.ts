/**
 * Saving the draft: sending its plan step by step, and what each answer leads
 * to.
 *
 * THE PLAN IS THE DRAFT'S, MADE WHEN THE SAVE STARTS (`model/save.ts`): the
 * settings write, the structural batches, the removals and one content write
 * per changed object, each named with an operation id derived from the save's
 * write id. The run first reads the chart once more and compares its rows
 * with the draft's base, because a content write is full post-state with no
 * precondition of its own — a draft saved over rows a colleague changed a
 * minute ago would put back what they wrote. Then it sends the steps in order
 * and stops at the first one that does not land.
 *
 * THE WRITE ID BELONGS TO THE PLAN, NOT TO THE CLICK. It is minted when the
 * review opens (an event handler), and every step's id derives from it, so a
 * RETRY of a step whose outcome is unknown sends the SAME id. For a chart step
 * that is what makes the retry safe: the chart's ledger recognises an id it
 * has seen and answers the first arrival's outcome. For the settings step it
 * is what settling recognises the write by (`writes.settleUnknownWrite`). A
 * new SAVE is a new plan, and takes a new id: an id the ledger has seen names
 * the body it saw, and a different body sent under it would be answered with
 * the first one's outcome.
 *
 * WHAT STOPS A SAVE, AND WHAT THEN:
 *
 * - UNKNOWN: nothing could establish whether the step landed. The run holds
 *   there, editing pauses, and Retry sends the same step again; nothing after
 *   it is sent until it is known.
 * - REFUSED: the engine will not take the step as sent — the rule and, for a
 *   batch, the operation it names. Nothing after it is sent. What landed
 *   before it stays landed, so the Builder reads the company back and carries
 *   the rest of the draft onto it ([SaveEvents.onStopped]).
 * - A CONFLICT: somebody else wrote first (the chart's rows moved, a newer
 *   settings revision, a write that lost a race on one object). The draft is
 *   updated onto what is there, never written over it.
 *
 * A WRITE IN FLIGHT IS NEVER ABORTED. Leaving the lens mid-save unmounts the
 * Builder, but aborting would only turn a write that may land into one nobody
 * hears about, so the run goes on and its events still fire. Before its first
 * write the run marks the kept log with its write id and the nodes it creates
 * (`persistence.markPendingWrite`), so a tab reloaded mid-save carries the
 * log onto whatever landed rather than creating those nodes twice. A page that
 * mounts again while a run of its own is still out waits for it
 * ([saveInFlight]) before deciding what to do with the kept log.
 */

import { useCallback, useRef, useState, type MutableRefObject } from "react";
import { needsSentence } from "~/lib/refusal.ts";
import { chartPrint, fingerprint } from "./model/document.ts";
import type { NodeKey } from "./model/keys.ts";
import { locate } from "./model/draft.ts";
import type { PendingWrite } from "./model/persistence.ts";
import type { BuilderState } from "./model/reducer.ts";
import {
  classifyStep,
  createdBy,
  furthestPosition,
  landed,
  planSave,
  type Creation,
  type SaveStep,
  type StepOutcome,
} from "./model/save.ts";
import type { ConflictReason } from "./model/scheduler.ts";
import type { BuilderMode, EngineTransport } from "./model/transport.ts";
import { newWriteId } from "./runtime.ts";
import { readChart, settleUnknownWrite, signedSummary, type SaveAttempt } from "./model/writes.ts";

/** One save, as it is being sent. */
export interface SaveRun {
  readonly writeId: string;
  readonly mode: BuilderMode;
  readonly baseRevision: string | null;
  readonly steps: readonly SaveStep[];
  /** One per step, `undefined` until the step has an answer. */
  readonly outcomes: readonly (StepOutcome | undefined)[];
}

export type SavePhase =
  | { readonly kind: "idle" }
  /** Reading the chart once more before the first write. */
  | { readonly kind: "confirming" }
  /** Sending step `at`. */
  | { readonly kind: "saving"; readonly at: number }
  /** A settings write's answer was lost: reading the revisions to find it. */
  | { readonly kind: "settling"; readonly at: number }
  /**
   * The run stopped at step `at` (or before the first, at -1: the chart could
   * not be read to confirm the base). `unknown` is the one a retry resolves.
   */
  | {
      readonly kind: "stopped";
      readonly at: number;
      readonly unknown: boolean;
      readonly message: string;
      /** The node the refusal names, when it names one. */
      readonly node: NodeKey | null;
    };

/** What a run that landed every step wrote, for the after-save strip. */
export interface Finished {
  readonly mode: BuilderMode;
  readonly settings: {
    readonly revisionId: string;
    readonly parentRevisionId: string | null;
    /** From the write's answer; `null` for a write found by settling. */
    readonly epoch: number | null;
  } | null;
  /** The furthest position the chart writes reached; `null` when none was sent. */
  readonly chartPosition: string | null;
  /** Whether every chart write answered `applied`: this node has applied them all. */
  readonly chartAppliedHere: boolean;
}

/** A run that stopped, and what it had written before it did. */
export interface Stopped {
  /** Whether any step landed: the company is not the draft's base any more. */
  readonly anyLanded: boolean;
  /** The nodes the landed steps created, which the rebase resolves the draft's adds onto. */
  readonly landed: readonly Creation[];
  /** What the settings step activated, when it landed. */
  readonly settings: Finished["settings"];
  readonly chartPosition: string | null;
  readonly chartAppliedHere: boolean;
}

export interface SaveEvents {
  /** The first write is about to go: the kept log is marked with the save first. */
  onSending(pending: PendingWrite): void;
  /** Nothing about the save is unknown any more: the mark comes off the kept log. */
  onSettled(): void;
  /** Every step landed. */
  onFinished(finished: Finished): void;
  /** A step was refused, or a conflict stopped the run after something landed. */
  onStopped(stopped: Stopped): void;
  /** Somebody else wrote first, and nothing of this save landed. */
  onConflict(conflict: { reason: ConflictReason; currentRevisionId: string | null }): void;
}

export interface Save {
  readonly phase: SavePhase;
  /** The run being sent or stopped; `null` before the first press and after it is acknowledged. */
  readonly run: SaveRun | null;
  /** The write id the review shows, once one is prepared. */
  readonly writeId: string | null;
  /** True while a step's outcome is not known. */
  readonly unsettled: boolean;
  /** Prepares the write id for the draft as it stands. Call it where the review opens. */
  prepare(): string;
  save(summary: string): Promise<void>;
  /** Sends the step whose outcome is unknown again, under the same id, and goes on. */
  retry(): Promise<void>;
  /** Forgets a stopped run, once the reader has seen why. */
  acknowledge(): void;
}

/** The runs this page has out, by write id: module state, because a run outlives its Builder. */
const outstanding = new Map<string, Promise<void>>();

/** Settles once no run this page started is still sending. */
export function saveInFlight(): Promise<void> | null {
  if (outstanding.size === 0) return null;
  return Promise.allSettled([...outstanding.values()]).then(() => undefined);
}

/** The sentence a stopped run shows. */
function stopMessage(
  outcome: StepOutcome,
  step: SaveStep,
  nameOf: (key: NodeKey) => string,
): string {
  if (outcome.kind === "unknown") {
    return `Whether ${stepLabel(step, nameOf)} was written could not be confirmed. ${outcome.detail} Retry sends the same write again: the engine recognises its id and answers what happened to it rather than writing it twice.`;
  }
  if (outcome.kind !== "refused") return "";
  // THE NODE IS NAMED WHERE THE STEP DOES NOT NAME IT: a batch is many
  // operations, and the refusal says which; a content write is about one
  // object its label already names, and naming it twice read "the seat CEO
  // (CEO)".
  const batch = step.kind === "structure" || step.kind === "removal";
  const about = batch && outcome.node ? ` (${nameOf(outcome.node)})` : "";
  if (outcome.status === 401 || outcome.status === 403) {
    if (outcome.code === "step_up_required") {
      return `The engine asks you to confirm who you are before ${stepLabel(step, nameOf)}${about}: sign in again, then save.`;
    }
    const fields =
      outcome.fields.length > 0 ? ` It changes ${outcome.fields.join(", ")}, which takes it.` : "";
    return `${needsSentence(`Saving ${stepLabel(step, nameOf)}${about}`, outcome.grants)}${fields}`;
  }
  return `The engine refused ${stepLabel(step, nameOf)}${about}: ${outcome.detail}`;
}

/** What a step writes, as a phrase. */
export function stepLabel(step: SaveStep, nameOf: (key: NodeKey) => string): string {
  switch (step.kind) {
    case "settings":
      return "the settings";
    case "structure":
      return step.nodes.length === 1
        ? "the change to the chart's structure"
        : `${step.nodes.length} changes to the chart's structure`;
    case "removal":
      return step.nodes.length === 1 ? "the removal" : `${step.nodes.length} removals`;
    case "unit":
      return `the unit ${nameOf(step.nodes[0]!)}`;
    case "seat":
      return `the seat ${nameOf(step.nodes[0]!)}`;
  }
}

export function useSave({
  stateRef,
  transport,
  events,
}: {
  stateRef: MutableRefObject<BuilderState>;
  transport: EngineTransport;
  events: MutableRefObject<SaveEvents>;
}): Save {
  const [phase, setPhase] = useState<SavePhase>({ kind: "idle" });
  const [run, setRun] = useState<SaveRun | null>(null);
  const runRef = useRef<SaveRun | null>(null);
  const confirmed = useRef(false);
  /** The settings step's earlier attempt was unknown: a 409 or 412 now may be its own write. */
  const settingsUnknown = useRef(false);
  /** The prepared id, the draft it was prepared for, and whether a plan was sent under it. */
  const write = useRef<{ id: string; generation: number; used: boolean } | null>(null);
  const [writeId, setWriteId] = useState<string | null>(null);
  const [unsettled, setUnsettledState] = useState(false);
  const unsettledRef = useRef(false);
  const setUnsettled = (value: boolean) => {
    unsettledRef.current = value;
    setUnsettledState(value);
  };

  const setRunState = (next: SaveRun | null) => {
    runRef.current = next;
    setRun(next);
  };

  const prepare = useCallback((): string => {
    const generation = stateRef.current.generation;
    // A RUN WHOSE OUTCOME IS UNKNOWN KEEPS ITS ID, whatever else happens: its
    // retry is the only way on, and it must resend what it sent. Otherwise an
    // id already sent under, or prepared for another draft, is replaced.
    const stale = !write.current || write.current.used || write.current.generation !== generation;
    if (stale && !unsettledRef.current) {
      write.current = { id: newWriteId(), generation, used: false };
    }
    setWriteId(write.current!.id);
    return write.current!.id;
  }, [stateRef]);

  const nameOf = useCallback(
    (key: NodeKey) => {
      const { draft, baseDraft } = stateRef.current;
      const found = locate(draft, key) ?? locate(baseDraft, key);
      return found?.node.data.name || "an unnamed node";
    },
    [stateRef],
  );

  /** Sends the run's steps from `from` until one does not land. */
  const drive = useCallback(
    async (from: number) => {
      let current = runRef.current;
      if (!current) return;
      const signal = new AbortController().signal;
      const record = (at: number, outcome: StepOutcome) => {
        const outcomes = [...current!.outcomes];
        outcomes[at] = outcome;
        current = { ...current!, outcomes };
        setRunState(current);
      };
      const landedCreations = () =>
        current!.steps.flatMap((step, i) => (landed(current!.outcomes[i]) ? step.creates : []));
      const settingsLanded = (): Finished["settings"] => {
        const at = current!.steps.findIndex((s) => s.kind === "settings");
        const outcome = at < 0 ? undefined : current!.outcomes[at];
        if (outcome?.kind !== "applied") return null;
        return {
          revisionId: outcome.revisionId ?? "",
          parentRevisionId: current!.baseRevision,
          epoch: outcome.epoch ?? null,
        };
      };
      const chartAppliedHere = () =>
        current!.steps.every(
          (step, i) => step.kind === "settings" || current!.outcomes[i]?.kind !== "pending",
        );
      const stop = (at: number, outcome: StepOutcome) => {
        const step = current!.steps[at]!;
        const unknown = outcome.kind === "unknown";
        setUnsettled(unknown);
        if (!unknown) events.current.onSettled();
        setPhase({
          kind: "stopped",
          at,
          unknown,
          message: stopMessage(outcome, step, nameOf),
          node: outcome.kind === "refused" ? outcome.node : null,
        });
        if (unknown) return;
        const anyLanded = current!.outcomes.some(landed);
        if (outcome.kind === "conflict" && !anyLanded) {
          setPhase({ kind: "idle" });
          setRunState(null);
          events.current.onConflict({
            reason:
              step.kind === "settings"
                ? current!.mode === "create"
                  ? "already_configured"
                  : "revision_advanced"
                : "chart_moved",
            currentRevisionId: outcome.currentRevisionId,
          });
          return;
        }
        if (anyLanded) {
          events.current.onStopped({
            anyLanded,
            landed: landedCreations(),
            settings: settingsLanded(),
            chartPosition: furthestPosition(current!.outcomes),
            chartAppliedHere: chartAppliedHere(),
          });
        }
      };

      for (let at = from; at < current.steps.length; at++) {
        const step = current.steps[at]!;
        setPhase({ kind: "saving", at });
        let outcome = classifyStep(await transport.send(step.request, signal), step);
        if (step.kind === "settings") {
          const ambiguous =
            outcome.kind === "unknown" || (outcome.kind === "conflict" && settingsUnknown.current);
          if (ambiguous) {
            settingsUnknown.current = true;
            setPhase({ kind: "settling", at });
            const attempt: SaveAttempt = {
              writeId: current.writeId,
              mode: current.mode,
              baseRevision: current.baseRevision,
            };
            const settled = await settleUnknownWrite(
              transport,
              attempt,
              outcome.kind === "conflict" ? outcome.currentRevisionId : null,
              signal,
            );
            if (settled.kind === "landed") {
              outcome = { kind: "applied", revisionId: settled.revisionId };
            } else if (settled.kind === "not_landed") {
              const nothingMoved =
                current.mode === "edit"
                  ? settled.currentRevisionId === current.baseRevision
                  : settled.currentRevisionId === null;
              outcome = nothingMoved
                ? {
                    kind: "unknown",
                    opId: step.id,
                    detail: "The settings did not reach the engine, and nothing was stored.",
                  }
                : { kind: "conflict", detail: "", currentRevisionId: settled.currentRevisionId };
            } else {
              outcome = { kind: "unknown", opId: step.id, detail: settled.detail };
            }
            if (outcome.kind !== "unknown") settingsUnknown.current = false;
          }
        }
        record(at, outcome);
        if (!landed(outcome)) {
          stop(at, outcome);
          return;
        }
      }

      // Every step landed.
      write.current = null;
      setWriteId(null);
      setUnsettled(false);
      setPhase({ kind: "idle" });
      const finished: Finished = {
        mode: current.mode,
        settings: settingsLanded(),
        chartPosition: furthestPosition(current.outcomes),
        chartAppliedHere: chartAppliedHere(),
      };
      setRunState(null);
      events.current.onSettled();
      events.current.onFinished(finished);
    },
    [transport, events, nameOf],
  );

  /** Reads the chart once more and compares it with the draft's base before the first write. */
  const confirm = useCallback(async (): Promise<boolean> => {
    const current = runRef.current;
    if (!current) return false;
    setPhase({ kind: "confirming" });
    const reading = await readChart(transport, new AbortController().signal);
    if (reading.kind !== "read") {
      const { status } = reading.answer;
      setPhase({
        kind: "stopped",
        at: -1,
        unknown: status === 0 || status >= 500,
        message:
          status === 401 || status === 403
            ? "The engine refused to show this reader the chart, so the save could not confirm the chart is still the one this draft was made on."
            : "The chart could not be read to confirm it is still the one this draft was made on, so nothing was written. Retry once the engine answers.",
        node: null,
      });
      return false;
    }
    const state = stateRef.current;
    const moved =
      current.mode === "create"
        ? reading.chart.seats.length > 0 || reading.chart.units.length > 0
        : fingerprint(chartPrint(reading.chart)) !== state.base.print;
    if (moved) {
      setPhase({ kind: "idle" });
      setRunState(null);
      events.current.onConflict({
        reason: current.mode === "create" ? "chart_exists" : "chart_moved",
        currentRevisionId: null,
      });
      return false;
    }
    confirmed.current = true;
    return true;
  }, [transport, stateRef, events]);

  const start = useCallback(
    (from: number) => {
      const current = runRef.current;
      if (!current) return Promise.resolve();
      const handled = (async () => {
        if (!confirmed.current) {
          if (!(await confirm())) return;
          // MARKED BEFORE THE FIRST WRITE: a tab that reloads while the run
          // is out must find the save, and what it creates, beside the log.
          events.current.onSending({ write: current.writeId, creates: createdBy(current.steps) });
        }
        await drive(from);
      })();
      outstanding.set(current.writeId, handled);
      return handled.finally(() => {
        if (outstanding.get(current.writeId) === handled) outstanding.delete(current.writeId);
      });
    },
    [confirm, drive, events],
  );

  const save = useCallback(
    async (summary: string) => {
      const state = stateRef.current;
      const id = prepare();
      write.current = { id, generation: state.generation, used: true };
      const plan = planSave({
        mode: state.mode,
        baseRevision: state.base.revision,
        baseSettings: state.base.settings,
        baseDraft: state.baseDraft,
        draft: state.draft,
        runtimeVisible: state.base.runtimeVisible,
        writeId: id,
        summary: signedSummary(summary, id),
      });
      confirmed.current = false;
      settingsUnknown.current = false;
      setRunState({
        writeId: id,
        mode: state.mode,
        baseRevision: state.base.revision,
        steps: plan.steps,
        outcomes: plan.steps.map(() => undefined),
      });
      await start(0);
    },
    [stateRef, prepare, start],
  );

  const retry = useCallback(async () => {
    const current = runRef.current;
    if (!current || phase.kind !== "stopped" || !phase.unknown) return;
    await start(Math.max(0, phase.at));
  }, [phase, start]);

  const acknowledge = useCallback(() => {
    if (phase.kind === "stopped" && phase.unknown) return;
    setPhase((current) => (current.kind === "stopped" ? { kind: "idle" } : current));
    if (phase.kind === "stopped") setRunState(null);
  }, [phase]);

  return { phase, run, writeId, unsettled, prepare, save, retry, acknowledge };
}
