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

export { Store } from "./store.ts";
export type { StoreState, Slice } from "./store.ts";
export {
  LiveSocket,
  QueryError,
  isLogRefusal,
  queryErrorCode,
  unavailableRetryMs,
} from "./socket.ts";
export { retryAfterMs } from "./retry.ts";
export { api } from "./api.ts";
export {
  rest,
  RestError,
  REQUEST_TIMEOUT_MS,
  isAbort,
  lastAnsweredAt,
  refusedGrants,
  retryAfterSeconds,
  whenRequestsSettle,
} from "./rest.ts";
export { keepsOperation, layoutOpID, newGateOpID } from "./gate.ts";
export type { GateAction } from "./gate.ts";
export type { RequestOptions, RestResponse, QueryValue } from "./rest.ts";
export { auth } from "./auth.ts";
export {
  confirmStepUp,
  currentSessionNeed,
  needSession,
  onSessionNeed,
  sessionNeedsEnrolment,
  sessionRestored,
  setStepUpConfirmer,
} from "./signin.ts";
export type { SessionNeed, StepUpConfirmer } from "./signin.ts";
export { SessionKeepAlive, SESSION_KEEPALIVE_MS } from "./keepalive.ts";
// THE WRITE AND THE FLOOR IT RAISES, so the replay can hand a captured
// `/operator/act` answer to the client's own `act` and read the floor back off
// the client's own `SessionFloors` — the floor a later read then names.
export { act, newActOpID } from "./act.ts";
export type { ActErrorCode, ActResult } from "./act.ts";
export { SessionFloors, domainOf } from "./session.ts";
export type * from "./types.ts";
