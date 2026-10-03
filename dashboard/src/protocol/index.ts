/**
 * The wire protocol, on its own.
 *
 * This barrel is a real build target, not just an import convenience:
 * `vite.protocol.config.ts` emits it as `static/dashboard/protocol.js`, plain
 * unminified ESM, so `internal/e2e/golden_test.go` can replay a real company's
 * captured socket frames through the client's OWN dispatch table under bare
 * `node`. Nothing in this directory may import React, touch the DOM at module
 * scope, or reach for anything a Node process does not have — and what it
 * takes from `../contract/`, the declarations an engine test holds, it takes
 * by a RELATIVE path, because this build has no `~` alias.
 */

export { Store, MAX_PHASES } from "./store.ts";
export { share } from "./share.ts";
export type { StoreState, Slice } from "./store.ts";
export {
  LiveSocket,
  QueryRefusedError,
  isLogRefusal,
  queryErrorCode,
  queryFailure,
  unavailableRetryMs,
} from "./socket.ts";
export type { QueryFailure } from "./socket.ts";
export {
  retryAfterMs,
  UNANSWERED_RETRY_BASE_MS,
  UNANSWERED_RETRY_MAX_MS,
  unansweredRetryMs,
} from "./retry.ts";
// THE ENGINE'S OWN WAITS AND BOUNDS, declared in the contract and re-exported
// here so the standalone `protocol.js` carries the values its own transport
// is bounded by.
export { RETRY_AFTER_MAX_MS, UNAVAILABLE_RETRY_MS } from "../contract/retry.ts";
export { GATE_REQUEST_TIMEOUT_MS } from "../contract/gate.ts";
export { MAX_EVENTS } from "../contract/wire.ts";
export { api } from "./api.ts";
export {
  rest,
  RestError,
  REQUEST_TIMEOUT_MS,
  isAbort,
  refusedGrants,
  restFailure,
  restRetryMs,
  retryAfterSeconds,
  retryHintOf,
} from "./rest.ts";
export type {
  ReadErrorCode,
  RequestOptions,
  RestFailure,
  RestResponse,
  RetryContext,
  QueryValue,
} from "./rest.ts";
export { keepsOperation, layoutOpID, newGateOpID } from "./gate.ts";
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
// THE WRITE AND THE FLOOR IT RAISES, so the replay can hand a captured
// `/operator/act` answer to the client's own `act` and read the floor back off
// the client's own `SessionFloors` — the floor a later read then names.
export { act, newActOpID } from "./act.ts";
export type { ActErrorCode, ActionArgs, ActionTool, ActOptions, ActResult } from "./act.ts";
export { SessionFloors, domainOf, tabFloors } from "./floors.ts";
export type { LogPosition, SessionDomain } from "./floors.ts";
export type * from "./types.ts";
