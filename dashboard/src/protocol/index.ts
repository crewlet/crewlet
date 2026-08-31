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
export { LiveSocket } from "./socket.ts";
export { api } from "./api.ts";
export { apiToken, storeToken, clearToken, requestToken, onTokenRequested } from "./authToken.ts";
export type * from "./types.ts";
