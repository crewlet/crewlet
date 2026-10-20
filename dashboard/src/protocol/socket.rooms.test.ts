/**
 * How the live socket holds an artifact's room: one join however many screens
 * listen, a rejoin on every open that names the `through` the tab last held, a
 * refusal and a withdrawal said to every listener, an undecided join asked
 * again on the engine's own clock, the pushes routed by the artifact their
 * payload names, and presence said only into a room the engine put this
 * socket in.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { MAX_ARTIFACT_ROOMS } from "../contract/wire.ts";
import { LiveSocket, QueryError, type RoomListener } from "./socket.ts";
import { Store } from "./store.ts";

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
  frames(kind: string): Record<string, unknown>[] {
    return this.sent
      .map((raw) => JSON.parse(raw) as Record<string, unknown>)
      .filter((frame) => frame.kind === kind);
  }
  push(frame: unknown): void {
    this.onmessage?.({ data: JSON.stringify(frame) } as MessageEvent);
  }
}

const A = "3182200c-c21d-5011-bfa6-79a51219e345";
const B = "9b2f3a10-0c4e-5d2b-8f6a-2a1b3c4d5e6f";

beforeEach(() => {
  vi.useFakeTimers();
  RecordingWebSocket.dials = [];
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: RecordingWebSocket });
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

function opened(): { socket: LiveSocket; sock: RecordingWebSocket } {
  const socket = new LiveSocket(new Store());
  socket.start();
  const sock = dial(0);
  sock.open();
  return { socket, sock };
}

function joinedAnswer(id: unknown, through = 7) {
  return {
    kind: "result",
    id,
    what: "artifact_join",
    data: {
      artifact: A,
      session: "s1",
      role: "contributor",
      roster: { members: [] },
      through,
      resync: true,
    },
  };
}

/** A listener whose every handler is a mock the test reads. */
function listener() {
  const heard = {
    joined: vi.fn<NonNullable<RoomListener["joined"]>>(),
    refused: vi.fn<NonNullable<RoomListener["refused"]>>(),
    changed: vi.fn<NonNullable<RoomListener["changed"]>>(),
    data: vi.fn<NonNullable<RoomListener["data"]>>(),
    presence: vi.fn<NonNullable<RoomListener["presence"]>>(),
  };
  return heard satisfies RoomListener;
}

describe("joining", () => {
  test("two screens on one artifact make one join, and the last to go leaves", () => {
    const { socket, sock } = opened();
    const first = socket.room(A, listener());
    const second = socket.room(A, listener());
    expect(sock.frames("artifact_join")).toHaveLength(1);
    first();
    expect(sock.frames("artifact_leave")).toHaveLength(0);
    second();
    expect(sock.frames("artifact_leave")).toEqual([{ kind: "artifact_leave", artifact: A }]);
  });

  test("a join made before the socket opens is sent on the open", () => {
    const socket = new LiveSocket(new Store());
    socket.start();
    socket.room(A, listener());
    const sock = dial(0);
    expect(sock.frames("artifact_join")).toHaveLength(0);
    sock.open();
    expect(sock.frames("artifact_join")).toEqual([
      expect.objectContaining({ kind: "artifact_join", artifact: A }),
    ]);
  });

  test("every listener hears the answer, and a reconnect joins again naming what it held", () => {
    const { socket, sock } = opened();
    const one = listener();
    socket.room(A, one);
    const [join] = sock.frames("artifact_join");
    expect(join).not.toHaveProperty("since");
    sock.push(joinedAnswer(join!.id, 7));
    expect(one.joined).toHaveBeenCalledWith(
      expect.objectContaining({ through: 7, role: "contributor" }),
    );
    sock.push({ kind: "artifact_data", data: { artifact: A, deltas: [], through: 9 } });

    sock.closeWith(1006);
    vi.advanceTimersByTime(60_000);
    const again = dial(1);
    again.open();
    expect(again.frames("artifact_join")).toEqual([
      expect.objectContaining({ artifact: A, since: 9 }),
    ]);
  });

  test("a ninth room is refused at once, as the engine would refuse it", () => {
    const { socket, sock } = opened();
    for (let i = 0; i < MAX_ARTIFACT_ROOMS; i++) {
      socket.room(`00000000-0000-5000-8000-00000000000${i}`, listener());
    }
    const ninth = listener();
    socket.room(B, ninth);
    expect(sock.frames("artifact_join")).toHaveLength(MAX_ARTIFACT_ROOMS);
    expect(ninth.refused).toHaveBeenCalledTimes(1);
    expect((ninth.refused.mock.calls[0]![0] as QueryError).message).toBe("bad_params");
  });
});

describe("refusals", () => {
  test("a refused join is said to every listener, and nothing is pushed to the room", () => {
    const { socket, sock } = opened();
    const one = listener();
    socket.room(A, one);
    const [join] = sock.frames("artifact_join");
    sock.push({
      kind: "error",
      id: join!.id,
      what: "artifact_join",
      error: "not_found",
      artifact: A,
    });
    expect(one.refused).toHaveBeenCalledTimes(1);
    expect((one.refused.mock.calls[0]![0] as QueryError).message).toBe("not_found");
    sock.push({ kind: "artifact_changed", data: { artifact: A, what: "version", head: 3 } });
    expect(one.changed).not.toHaveBeenCalled();
  });

  test("a room withdrawn while the reader is in it stops its pushes", () => {
    const { socket, sock } = opened();
    const one = listener();
    socket.room(A, one);
    sock.push(joinedAnswer(sock.frames("artifact_join")[0]!.id));
    sock.push({ kind: "error", what: "artifact_join", error: "not_found", artifact: A });
    expect(one.refused).toHaveBeenCalledTimes(1);
    sock.push({ kind: "artifact_data", data: { artifact: A, deltas: [] } });
    expect(one.data).not.toHaveBeenCalled();
  });

  test("an undecided join is asked again on the engine's clock, and only then", () => {
    const { socket, sock } = opened();
    socket.room(A, listener());
    const [join] = sock.frames("artifact_join");
    sock.push({
      kind: "error",
      id: join!.id,
      what: "artifact_join",
      error: "unavailable",
      artifact: A,
      retry_after: 4,
    });
    vi.advanceTimersByTime(3_900);
    expect(sock.frames("artifact_join")).toHaveLength(1);
    vi.advanceTimersByTime(200);
    expect(sock.frames("artifact_join")).toHaveLength(2);
  });

  test("an undecided join whose engine says waiting will not help is not asked again", () => {
    const { socket, sock } = opened();
    socket.room(A, listener());
    const [join] = sock.frames("artifact_join");
    sock.push({
      kind: "error",
      id: join!.id,
      what: "artifact_join",
      error: "unavailable",
      artifact: A,
      retry_after: 0,
    });
    vi.advanceTimersByTime(120_000);
    expect(sock.frames("artifact_join")).toHaveLength(1);
  });
});

describe("pushes and presence", () => {
  test("each push reaches the listeners of the artifact its payload names, and no other", () => {
    const { socket, sock } = opened();
    const a = listener();
    const b = listener();
    socket.room(A, a);
    socket.room(B, b);
    const [ja, jb] = sock.frames("artifact_join");
    sock.push(joinedAnswer(ja!.id));
    sock.push({ ...joinedAnswer(jb!.id), data: { ...joinedAnswer(jb!.id).data, artifact: B } });
    sock.push({ kind: "artifact_changed", data: { artifact: A, what: "version", head: 2 } });
    sock.push({ kind: "artifact_presence", data: { artifact: B, upsert: [], leave: ["x"] } });
    expect(a.changed).toHaveBeenCalledTimes(1);
    expect(b.changed).not.toHaveBeenCalled();
    expect(b.presence).toHaveBeenCalledWith(expect.objectContaining({ leave: ["x"] }));
    expect(a.presence).not.toHaveBeenCalled();
  });

  test("a room push is not counted as a kind this build does not know", () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    socket.onMessage(JSON.stringify({ kind: "artifact_changed", data: { artifact: A } }));
    socket.onMessage(JSON.stringify({ kind: "artifact_data", data: { artifact: A } }));
    socket.onMessage(JSON.stringify({ kind: "artifact_presence", data: { artifact: A } }));
    expect([...store.unknownPushes.keys()]).toEqual([]);
  });

  test("presence is said only once the engine has put the socket in the room", () => {
    const { socket, sock } = opened();
    socket.room(A, listener());
    socket.presence(A, "viewing", {});
    expect(sock.frames("presence")).toHaveLength(0);
    sock.push(joinedAnswer(sock.frames("artifact_join")[0]!.id));
    socket.presence(A, "editing", { collection: "vendors", doc: "acme", field: "score" });
    expect(sock.frames("presence")).toEqual([
      {
        kind: "presence",
        artifact: A,
        activity: "editing",
        focus: { collection: "vendors", doc: "acme", field: "score" },
      },
    ]);
  });
});
