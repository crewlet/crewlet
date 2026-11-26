/**
 * The dictation socket: the same-origin `/ws/dictation`, carrying the tab's
 * reader as `/ws/stream` does, since a handshake carries no header.
 */
import { readerQuery } from "./signin.ts";

const PATH = "/ws/dictation";

/** Whether this browser can open the socket at all. */
export function canOpenDictationSocket(): boolean {
  return typeof WebSocket !== "undefined";
}

/** Opens one dictation's socket. */
export function openDictationSocket(): WebSocket {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  return new WebSocket(`${proto}://${location.host}${PATH}${readerQuery()}`);
}
