/**
 * The wire protocol, on its own.
 *
 * This barrel is a real build target, not just an import convenience:
 * `vite.protocol.config.ts` emits it as `static/dashboard/protocol.js`, plain
 * unminified ESM, so `internal/e2e/golden_test.go` can replay a real company's
 * captured socket frames through the client's OWN dispatch table under bare
 * `node`. Nothing in this directory may import React, touch the DOM at module
 * scope, or reach for anything a Node process does not have.
 */

export { Store, MAX_EVENTS } from "./store.ts";
export type { StoreState, Slice } from "./store.ts";
export {
  LiveSocket,
  QueryRefusedError,
  isLogRefusal,
  queryErrorCode,
  queryFailure,
  unavailableRetryMs,
} from "./socket.ts";
export { RETRY_AFTER_MAX_MS, retryAfterMs, UNAVAILABLE_RETRY_MS } from "./retry.ts";
export type { QueryFailure } from "./socket.ts";
export { api } from "./api.ts";
export {
  rest,
  RestError,
  REQUEST_TIMEOUT_MS,
  isAbort,
  refusedGrants,
  restFailure,
  restRetryMs,
} from "./rest.ts";
export type { RequestOptions, RestFailure, RestResponse, QueryValue } from "./rest.ts";
export {
  GATE_ACTIONS,
  GATE_ACTIONS_KEEPING_OPERATION,
  GATE_REQUEST_TIMEOUT_MS,
  keepsOperation,
  layoutOpID,
  newGateOpID,
} from "./gate.ts";
export type { GateAction } from "./gate.ts";
export { auth } from "./auth.ts";
export {
  confirmStepUp,
  currentSessionNeed,
  needSession,
  onSessionNeed,
  sessionNeedsEnrolment,
  sessionRestored,
  setStepUpConfirmer,
} from "./session.ts";
export type { SessionNeed, StepUpConfirmer, StepUpWindow } from "./session.ts";
export type * from "./types.ts";
