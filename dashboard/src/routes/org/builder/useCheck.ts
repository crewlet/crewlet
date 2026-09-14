/**
 * The dry-run check, driven by the draft.
 *
 * `model/scheduler.ts` is the state machine and its driver; this hook owns
 * the driver's lifetime and tells it when the draft moved. One runner per
 * mounted Builder: a remount (React's StrictMode runs every effect twice)
 * disposes the old runner, aborting its request, before the new one checks.
 *
 * WHAT MOVES THE CHECK. `reducer.checkTrigger` compares consecutive states:
 * a new base resets the check (it runs at once and lifts a halt), a moved
 * draft notifies it (it runs after the debounce). A change of operator token
 * moves neither the base nor the draft, and every answer may differ under the
 * new token, so the Builder asks for a reset with [requestReset] before it
 * dispatches `tokenChanged`, and the reset rides the state change that
 * follows.
 *
 * WHAT IS SENT is built from the state as it stands when the request leaves,
 * never from a state captured earlier: `prepare` reads the latest state, and
 * answers `null` for a generation the draft has already left, which the
 * runner treats as superseded.
 */

import { useCallback, useEffect, useMemo, useRef, useState, type MutableRefObject } from "react";
import { toDocument } from "./model/document.ts";
import { checkTrigger, type BuilderAction, type BuilderState } from "./model/reducer.ts";
import {
  CheckRunner,
  INITIAL_CHECK,
  type CheckState,
  type PreparedCheck,
} from "./model/scheduler.ts";
import { checkRequest, type Clock, type ConfigTransport } from "./model/transport.ts";

/** The check request for `generation` of `state`, or `null` when there is nothing to check. */
export function prepareCheck(state: BuilderState, generation: number): PreparedCheck | null {
  if (state.generation !== generation) return null;
  if (state.mode === "edit" && (state.base.document === null || state.base.revision === null)) {
    return null;
  }
  const sent = toDocument(state.draft);
  return {
    request: checkRequest({
      mode: state.mode,
      baseRevision: state.base.revision,
      base: state.base.document,
      sent,
    }),
    sent,
    mode: state.mode,
    baseRevision: state.base.revision,
  };
}

export interface Check {
  /** The machine's state: its status drives the toolbar and the save rules. */
  readonly machine: CheckState;
  /** Checks the current generation now, forgetting any halt. */
  reset(): void;
  /** Resets the check on the next state change instead of classifying it. */
  requestReset(): void;
}

export function useCheck({
  state,
  stateRef,
  dispatch,
  loaded,
  transport,
  clock,
}: {
  state: BuilderState;
  stateRef: MutableRefObject<BuilderState>;
  dispatch: (action: BuilderAction) => void;
  /** False until the first company (or its absence) is loaded: nothing is checked before. */
  loaded: boolean;
  transport: ConfigTransport;
  clock: Clock;
}): Check {
  const runner = useRef<CheckRunner | null>(null);
  const loadedRef = useRef(loaded);
  loadedRef.current = loaded;
  const [machine, setMachine] = useState<CheckState>(INITIAL_CHECK);
  const resetPending = useRef(false);

  useEffect(() => {
    const created = new CheckRunner({
      clock,
      transport,
      prepare: (generation) => prepareCheck(stateRef.current, generation),
      onSettled: (settled) => dispatch({ type: "checked", settled }),
      onState: setMachine,
    });
    runner.current = created;
    // A remount of a loaded Builder starts a fresh runner, and the draft on
    // screen has not been checked by it.
    if (loadedRef.current) created.reset(stateRef.current.generation);
    return () => {
      created.dispose();
      if (runner.current === created) runner.current = null;
    };
  }, [clock, transport, dispatch, stateRef]);

  const previous = useRef(state);
  useEffect(() => {
    const prev = previous.current;
    previous.current = state;
    if (!loaded || prev === state) return;
    if (resetPending.current) {
      resetPending.current = false;
      runner.current?.reset(state.generation);
      return;
    }
    const trigger = checkTrigger(prev, state);
    if (trigger === "reset") runner.current?.reset(state.generation);
    else if (trigger === "changed") runner.current?.changed(state.generation);
  }, [state, loaded]);

  const reset = useCallback(() => {
    runner.current?.reset(stateRef.current.generation);
  }, [stateRef]);
  const requestReset = useCallback(() => {
    resetPending.current = true;
  }, []);

  return useMemo(() => ({ machine, reset, requestReset }), [machine, reset, requestReset]);
}
