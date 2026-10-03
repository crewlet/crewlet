/**
 * Changing one scope's token ceilings: read, check, save, and watch it apply.
 *
 * # Two surfaces (`./ceilings.ts`)
 *
 * The COMPANY's ceilings are a `/config` merge patch, and EVERY SAVE IS
 * CHECKED FIRST against the revision it was read from. The engine validates
 * the whole company behind a ceiling — a week below its own day — and some of
 * what it finds is a WARNING rather than a refusal: the document is valid and
 * the ceiling will never refuse a turn. A save sent blind stores that and says
 * "Saved"; a check first is the only place the person can see it before it is
 * theirs. So a change that introduces a warning stops and says it, and saves
 * only when the person says so again; one that introduces none saves straight
 * away.
 *
 * A SEAT's are its org chart runtime half, written by the seat's content
 * write (`PATCH /chart/seats/{handle}`). The chart has no dry run, so there is
 * no check to stop on: the chart refuses a ceiling it will not hold outright
 * (`422`, in its rule's own words), and a seat ceiling the company's leaves
 * idle is what `/chart/check` reports as `budget_idle` once it is there.
 *
 * READ AT THE MOMENT OF THE SAVE, never from what the screen drew. The screen
 * draws the budgets answer, which is what THIS node applied; the write lands on
 * what the surface holds now, which may be a colleague's newer state. A change
 * is therefore worked out against what the read returns — the typed values
 * compared with the ceilings held now. A `/config` write carries the read
 * revision's tag, so anything that lands in between is a conflict to reload
 * rather than an overwrite. A chart content write takes no precondition — the
 * chart arbitrates per object, and a colleague's write to the SAME seat
 * landing first is a `409 stale` — so the seat is read at the press and
 * written straight after it.
 *
 * A CHART WRITE HAS THREE OUTCOMES, and only one of them is a refusal:
 * `applied` (this node applied it), `pending` (durable at a position every
 * node will apply, this one not yet) and `unknown` — nothing could establish
 * whether it landed. An unknown is never a refusal and never a save: it keeps
 * the write and its OPERATION ID, and sending again resends the very same
 * operation, which the chart's ledger answers with the first arrival's
 * outcome. A fresh id would be a second write.
 *
 * A SAVE IS NOT AN APPLY. Until this node has applied the change, the gate
 * still enforces the ceiling before, and so does every figure on the screen. A
 * `/config` save names the epoch its revision activated at, and is applied
 * once the health push reports this node there; a chart write is applied once
 * this node pushes its org again, which follows every chart apply. Then the
 * caller re-reads what it draws.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import {
  dryRunPatch,
  getConfig,
  savePatch,
  type ConfigWriteOutcome,
} from "~/protocol/configWrite.ts";
import { introducedWarnings } from "~/protocol/configAnswer.ts";
import { newActOpID } from "~/protocol/act.ts";
import { isAbort, rest, RestError } from "~/protocol/rest.ts";
import type { ChartSeatRead, ConfigWarning, TokenBudget } from "~/protocol/types.ts";
import {
  ceilingSummary,
  changesFrom,
  chartRefusalWords,
  companyPatch,
  refusalWords,
  seatCeilingBody,
  type CeilingScope,
  type Period,
} from "./ceilings.ts";
import { useEngineHealth, useOrgPushes } from "./store-hooks.ts";

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
   * Stored; `applied` once THIS node runs it. `until` is what says so: the
   * epoch a `/config` revision activated at, or the org push a chart write is
   * followed by.
   */
  | {
      readonly kind: "saved";
      readonly applied: boolean;
      readonly summary: string;
      readonly until: Applies;
    }
  /**
   * Nobody could establish whether a chart write landed. `confirm` resends it
   * under the same operation id (`opId`), which is the only safe retry.
   */
  | { readonly kind: "unknown"; readonly message: string; readonly opId: string }
  | { readonly kind: "refused"; readonly message: string; readonly reload: boolean };

/** What tells a saved change that this node has applied it. */
export type Applies =
  /** A `/config` revision: this node reports the epoch it activated at. */
  | { readonly kind: "epoch"; readonly epoch: number }
  /** A chart write: this node pushes its org again, past the count at the save. */
  | { readonly kind: "push"; readonly after: number };

/** A write checked and ready to send, held while a warning is confirmed or an unknown resent. */
type Prepared =
  | {
      readonly surface: "config";
      readonly etag: string;
      readonly body: Record<string, unknown>;
      readonly summary: string;
    }
  | {
      readonly surface: "chart";
      readonly handle: string;
      /** The operation, minted once and resent by every retry of this write. */
      readonly opId: string;
      readonly body: Record<string, unknown>;
      readonly summary: string;
    };

/** The typed value of each window a form offers. */
export type TypedCeilings = Readonly<Partial<Record<Period, string>>>;

export interface CeilingWrite {
  readonly state: CeilingWriteState;
  /** Check the typed values and, unless the check warns, save them. */
  readonly submit: (typed: TypedCeilings) => void;
  /** Save what the last check warned about, as it was checked — or resend an unknown write. */
  readonly confirm: () => void;
  /** Back to idle, dropping a held write and any request in flight. */
  readonly reset: () => void;
  readonly busy: boolean;
}

export function useCeilingWrite(
  scope: CeilingScope,
  {
    onApplied,
  }: {
    /** Called once this node applies the saved change — re-read what the screen draws. */
    onApplied?: () => void;
  } = {},
): CeilingWrite {
  const [state, setState] = useState<CeilingWriteState>({ kind: "idle" });
  const prepared = useRef<Prepared | null>(null);
  const inflight = useRef<AbortController | null>(null);
  const health = useEngineHealth();
  const pushes = useOrgPushes();
  const pushesRef = useRef(pushes);
  pushesRef.current = pushes;
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
      if (write.surface === "config") {
        const outcome = await savePatch(write.body, write.etag, write.summary, signal);
        prepared.current = null;
        if (outcome.kind === "saved") {
          setState({
            kind: "saved",
            applied: false,
            summary: write.summary,
            until: { kind: "epoch", epoch: outcome.epoch },
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
        return;
      }
      // THE COUNT BEFORE THE WRITE: a push that arrives while it is in flight
      // may already carry it, and one that arrives after certainly does.
      const after = pushesRef.current;
      try {
        await rest.request("PATCH", `/chart/seats/${encodeURIComponent(write.handle)}`, {
          body: write.body,
          headers: { "Idempotency-Key": write.opId },
          signal,
        });
        prepared.current = null;
        // 200 APPLIED AND 202 PENDING ARE BOTH DURABLE: the change is stored
        // and every node will apply it, so both are saved. What tells them
        // apart — whether THIS node has applied it yet — is what `until`
        // watches for either way.
        setState({
          kind: "saved",
          applied: false,
          summary: write.summary,
          until: { kind: "push", after },
        });
      } catch (err) {
        if (isAbort(err)) throw err;
        if (err instanceof RestError && !outcomeUnknown(err)) {
          prepared.current = null;
          setState({ kind: "refused", ...chartRefusalWords(err, scope) });
          return;
        }
        // KEPT, with its id: the only safe retry of an unknown is the same
        // operation.
        prepared.current = write;
        setState({
          kind: "unknown",
          opId: write.opId,
          message:
            "The engine could not say whether the ceiling was saved. Sending it again resends the same change, which the engine answers with whatever became of the first.",
        });
      }
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
          if (scope.kind === "seat") {
            await submitSeat(scope, typed, signal);
            return;
          }
          await submitCompany(typed, signal);
        } catch (err) {
          if (!isAbort(err)) setState(failed(err));
        }
      })();

      // THE SEAT: its chart row with the runtime half, read now.
      async function submitSeat(
        seat: Extract<CeilingScope, { kind: "seat" }>,
        typedValues: TypedCeilings,
        abort: AbortSignal,
      ) {
        let read: ChartSeatRead;
        try {
          read = (
            await rest.request("GET", `/chart/seats/${encodeURIComponent(seat.handle)}`, {
              query: { runtime: "true" },
              signal: abort,
            })
          ).body as ChartSeatRead;
        } catch (err) {
          if (isAbort(err)) throw err;
          if (err instanceof RestError && !outcomeUnknown(err)) {
            setState({ kind: "refused", ...chartRefusalWords(err, seat) });
            return;
          }
          // NOTHING WAS SENT, so nothing is unknown: the read is what failed.
          setState({
            kind: "refused",
            message: `${seat.name}'s ceilings could not be read, so nothing was saved. Try again.`,
            reload: false,
          });
          return;
        }
        // A RUNTIME HALF WITHHELD IS NOT AN EMPTY ONE. Sent back from a read
        // that was not shown it, a write would state the seat had no model
        // chain, no credentials and no ceilings — so it is refused here,
        // naming the grant the half is served under.
        if (!read.runtime) {
          setState({
            kind: "refused",
            message: `Reading ${seat.name}'s ceilings needs config:read, which the credential you presented does not carry, so nothing was saved.`,
            reload: false,
          });
          return;
        }
        const held = budgetOf(read.seat.runtime?.token_budget);
        const changes = changesFrom(typedValues, held);
        if (changes === undefined || Object.keys(changes).length === 0) {
          setState({ kind: "unchanged" });
          return;
        }
        await store(
          {
            surface: "chart",
            handle: seat.handle,
            opId: newActOpID(),
            body: seatCeilingBody(read.seat, changes),
            summary: ceilingSummary(seat, changes, held),
          },
          abort,
        );
      }

      // THE COMPANY: the settings revision, read and checked now.
      async function submitCompany(typedValues: TypedCeilings, abort: AbortSignal) {
        // 1. What the company holds NOW, and the tag of the revision it is in.
        const read = await getConfig(abort);
        if (read.kind !== "document") {
          setState({ kind: "refused", ...refusalWords(read, scope) });
          return;
        }
        const held = budgetOf(read.document.token_budget);

        // 2. What the typed values change, against THAT.
        const changes = changesFrom(typedValues, held);
        if (changes === undefined || Object.keys(changes).length === 0) {
          setState({ kind: "unchanged" });
          return;
        }
        const write: Prepared = {
          surface: "config",
          etag: read.etag,
          body: companyPatch(changes),
          summary: ceilingSummary(scope, changes, held),
        };

        // 3. The check, beside the company's own warnings as they stand, so
        // only what THIS change introduces is shown.
        const check = (body: Record<string, unknown>): Promise<ConfigWriteOutcome> =>
          dryRunPatch(body, read.etag, abort);
        const [before, after] = await Promise.all([check({}), check(write.body)]);
        if (after.kind === "saved") {
          // A CHECK THE ENGINE STORED. No route this page sends a check to
          // answers one with 201, but a 201 means a revision exists, and
          // saying so is the only answer that cannot become a second save.
          setState({
            kind: "saved",
            applied: false,
            summary: write.summary,
            until: { kind: "epoch", epoch: after.epoch },
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
        await store(write, abort);
      }
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
  // was replacing and call it the new one.
  const appliedEpoch = health?.applied_epoch ?? 0;
  useEffect(() => {
    if (state.kind !== "saved" || state.applied) return;
    const reached =
      state.until.kind === "epoch" ? appliedEpoch >= state.until.epoch : pushes > state.until.after;
    if (!reached) return;
    setState({ ...state, applied: true });
    appliedRef.current?.();
  }, [state, appliedEpoch, pushes]);

  const busy = state.kind === "checking" || state.kind === "saving";
  return { state, submit, confirm, reset, busy };
}

/**
 * Whether a chart write's failure leaves its outcome unestablished: no answer
 * came back (status 0), or a 5xx — a gateway that gave up says nothing about
 * the write behind it, and a write the node could not settle says so itself
 * (`503`, `outcome: "unknown"`, `op_id`). Every 4xx is the engine refusing the
 * write as sent, which wrote nothing.
 */
function outcomeUnknown(err: RestError): boolean {
  return err.status === 0 || err.status >= 500;
}

/**
 * A failure no answer classifies — a fault in this page, said as one and
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
