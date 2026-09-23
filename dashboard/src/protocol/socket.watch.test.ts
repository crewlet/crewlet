/**
 * How the live socket watches a seat, what an `inbox_changed` frame does, and
 * what a query frame it sends carries.
 *
 * A watch lives in the engine's routing index for ONE socket, so the tab has to
 * say it again on every open — a reconnect that forgot it would leave a person
 * learning about their work a poll interval late, with nothing on screen to say
 * the push had stopped. And the engine answers a watch it could not decide
 * (`unavailable`) differently from one it refused: the first clears on its own
 * and is asked again, the second is a decision and asking again is noise.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LiveSocket, QueryRefusedError, UNAVAILABLE_RETRY_MS } from "./socket.ts";
import { Store } from "./store.ts";

/** A WebSocket the test drives and whose outgoing frames it reads. */
class RecordingWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  static dials: RecordingWebSocket[] = [];
  readyState = RecordingWebSocket.CONNECTING;
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  onclose: ((e: CloseEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(readonly url: string) {
    RecordingWebSocket.dials.push(this);
  }
  send(data: string): void {
    this.sent.push(data);
  }
  close(): void {}
  open(): void {
    this.readyState = RecordingWebSocket.OPEN;
    this.onopen?.();
  }
  closeWith(code: number): void {
    this.readyState = RecordingWebSocket.CLOSED;
    this.onclose?.({ code, reason: "" } as CloseEvent);
  }
  /** The `watch` frames this socket carried, as the seat each named. */
  watches(): string[] {
    return this.sent
      .map((raw) => JSON.parse(raw) as { kind: string; seat?: string })
      .filter((frame) => frame.kind === "watch")
      .map((frame) => frame.seat ?? "");
  }
}

beforeEach(() => {
  vi.useFakeTimers();
  RecordingWebSocket.dials = [];
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: RecordingWebSocket });
  // The degraded-mode snapshot a dropped socket falls back to; nothing here is
  // about it, so it answers nothing.
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("{}", { status: 503 })),
  );
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function dial(n: number): RecordingWebSocket {
  const sock = RecordingWebSocket.dials[n];
  if (!sock) throw new Error(`dial ${n} never happened`);
  return sock;
}

function started(): { socket: LiveSocket; store: Store } {
  const store = new Store();
  const socket = new LiveSocket(store);
  socket.start();
  return { socket, store };
}

describe("watching a seat", () => {
  test("the watch is sent when the socket opens, and again on every new one", async () => {
    const { socket } = started();
    // ASKED BEFORE THE SOCKET IS OPEN, which is the order the shell asks in:
    // the viewer is known from a query that can land before the handshake.
    socket.watch("ana");
    expect(dial(0).watches()).toEqual([]);
    dial(0).open();
    expect(dial(0).watches()).toEqual(["ana"]);

    dial(0).closeWith(1006);
    await vi.advanceTimersByTimeAsync(1_000);
    dial(1).open();
    expect(dial(1).watches()).toEqual(["ana"]);
  });

  test("a cleared watch is said on this socket and never on the next", async () => {
    const { socket } = started();
    dial(0).open();
    socket.watch("ana");
    socket.watch("");
    expect(dial(0).watches()).toEqual(["ana", ""]);

    dial(0).closeWith(1006);
    await vi.advanceTimersByTimeAsync(1_000);
    dial(1).open();
    expect(dial(1).watches()).toEqual([]);
  });

  test("a watch the engine could not decide is asked again; a refused one is not", async () => {
    const { socket } = started();
    dial(0).open();
    socket.watch("ana");

    socket.onMessage(JSON.stringify({ kind: "error", what: "watch", error: "unavailable" }));
    await vi.advanceTimersByTimeAsync(UNAVAILABLE_RETRY_MS);
    expect(dial(0).watches()).toEqual(["ana", "ana"]);

    socket.onMessage(JSON.stringify({ kind: "error", what: "watch", error: "unauthorized" }));
    await vi.advanceTimersByTimeAsync(10 * UNAVAILABLE_RETRY_MS);
    expect(dial(0).watches()).toEqual(["ana", "ana"]);
  });
});

describe("an inbox_changed frame", () => {
  test("moves the counter of the seat it names, and one naming nobody moves nothing", () => {
    const { socket, store } = started();
    const frame = (data: unknown) =>
      socket.onMessage(JSON.stringify({ kind: "inbox_changed", seat: "ana", data }));
    frame({ handle: "ana", unread_delta: 1, subject: "t-1", reason: "assignee" });
    frame({ handle: "ana", unread_delta: 2, subject: "t-2", reason: "mention" });
    expect(store.state.inboxMoves).toEqual({ ana: 2 });

    // A FRAME NAMING NOBODY MOVES NOTHING: a counter under "" is a seat no
    // screen asks about, and a slice that moved for it would wake them all.
    const before = store.version("inboxMoves");
    frame({ unread_delta: 1 });
    frame({ handle: "", unread_delta: 1 });
    frame(null);
    expect(store.state.inboxMoves).toEqual({ ana: 2 });
    expect(store.version("inboxMoves")).toBe(before);
  });
});

// A QUERY FRAME CARRIES NO CREDENTIAL. The engine decides every question by
// the principal the handshake resolved and reads no token off a frame, so one
// in the frame is the reader's bearer copied into every message for nothing —
// and into whatever a proxy or a browser extension logs of the socket.
describe("a query frame", () => {
  test("carries the question and not the token", () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    socket.setToken("fixture-token-long-enough-to-pass");
    socket.start();
    dial(0).open();
    void socket.query("fleet");
    const frames = dial(0).sent.map((raw) => JSON.parse(raw) as Record<string, unknown>);
    const query = frames.find((frame) => frame.kind === "query");
    expect(query).toBeDefined();
    expect(Object.keys(query!).sort()).toEqual(["id", "kind", "params", "what"]);
  });

  // A REFUSAL ON AUTHORITY SAYS WHY over the socket as it does over REST: the
  // engine's error frame carries the rule and the grants that would have
  // admitted the reader, and the rejection a screen catches carries them too.
  // The code stays the rejection's message, which is what every reader that
  // only branches on the code already tests.
  test("a refusal carries its reason and its grants to the screen", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    socket.start();
    dial(0).open();
    const asked = socket.query("events");
    const frames = dial(0).sent.map((raw) => JSON.parse(raw) as { kind: string; id?: number });
    const id = frames.find((frame) => frame.kind === "query")?.id;
    socket.onMessage(
      JSON.stringify({
        kind: "error",
        id,
        what: "events",
        error: "unauthorized",
        reason: "no_grant",
        grants: ["audit:read"],
      }),
    );
    const refused = await asked.then(
      () => null,
      (err: unknown) => err,
    );
    expect(refused).toBeInstanceOf(QueryRefusedError);
    expect((refused as QueryRefusedError).message).toBe("unauthorized");
    expect((refused as QueryRefusedError).refusal).toEqual({
      reason: "no_grant",
      grants: ["audit:read"],
    });
  });
});
