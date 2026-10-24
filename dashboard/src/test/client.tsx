/**
 * The frame's client for a suite that mounts one screen or control without
 * the frame: a store holding whatever the suite applies to it, and a socket
 * that dials nothing, so a component reading the store (which of Crewlet's
 * services the company runs, the org chart) reads the suite's answer.
 */

import type { ReactNode } from "react";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

/**
 * A socket that never opens, for a store nothing is pushed into. Exported for
 * a suite that builds its own client (to answer the socket's queries itself).
 */
export class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** The client around `children`, over `store` (an empty one unless the suite hands its own). */
export function TestClient({ store, children }: { store?: Store; children: ReactNode }) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const held = store ?? new Store();
  return (
    <ClientContext.Provider value={{ store: held, socket: new LiveSocket(held) }}>
      {children}
    </ClientContext.Provider>
  );
}
