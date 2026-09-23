/**
 * How the live socket answers the engine's two close codes and a refused
 * handshake — the half of revalidation that runs in the browser.
 *
 * The engine re-checks an open socket every minute and closes it 4401 when its
 * credential names nobody any more, 4403 when it names somebody who may not
 * have this surface. The repairs are opposite: a 4401 is the ordinary
 * reconnect (the browser may hold a newer cookie), a 4403 is not repaired by
 * anything this tab can do, so the socket must STOP rather than dial the same
 * refusal every thirty seconds under a "reconnecting" banner.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LiveSocket } from "./socket.ts";
import { Store } from "./store.ts";

/** A WebSocket the test drives: every dial is recorded, and nothing opens by itself. */
class ScriptedWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  static dials: ScriptedWebSocket[] = [];
  readyState = ScriptedWebSocket.CONNECTING;
  onopen: (() => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  onclose: ((e: CloseEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(readonly url: string) {
    ScriptedWebSocket.dials.push(this);
  }
  send(): void {}
  close(): void {}
  open(): void {
    this.readyState = ScriptedWebSocket.OPEN;
    this.onopen?.();
  }
  closeWith(code: number, reason = ""): void {
    this.readyState = ScriptedWebSocket.CLOSED;
    this.onclose?.({ code, reason } as CloseEvent);
  }
}

let fetches: string[] = [];
let probeStatus = 426;
let probeBody: unknown = {};
/** How long the plain-HTTP re-ask takes to answer. */
let probeDelayMs = 0;

beforeEach(() => {
  vi.useFakeTimers();
  ScriptedWebSocket.dials = [];
  fetches = [];
  probeStatus = 426;
  probeBody = {};
  probeDelayMs = 0;
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: ScriptedWebSocket });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      fetches.push(url);
      if (url.endsWith("/ws/stream")) {
        if (probeDelayMs > 0) await new Promise((resolve) => setTimeout(resolve, probeDelayMs));
        return new Response(JSON.stringify(probeBody), { status: probeStatus });
      }
      return new Response("{}", { status: 503 });
    }),
  );
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

/** The nth dial, which the case has already caused. */
function dial(n: number): ScriptedWebSocket {
  const sock = ScriptedWebSocket.dials[n];
  if (!sock) throw new Error(`dial ${n} never happened`);
  return sock;
}

function started(): { socket: LiveSocket; store: Store } {
  const store = new Store();
  const socket = new LiveSocket(store);
  socket.start();
  return { socket, store };
}

describe("the engine's close codes", () => {
  test("4403 stops the socket and says why", async () => {
    const { store } = started();
    dial(0).open();
    dial(0).closeWith(4403, "grant withdrawn: state:read");
    await vi.advanceTimersByTimeAsync(120_000);

    expect(store.state.accessRefused).toBe("grant withdrawn: state:read");
    // No re-dial and no REST fallback: both would be refused by the same
    // decision for as long as the tab stayed open.
    expect(ScriptedWebSocket.dials).toHaveLength(1);
    expect(fetches).toHaveLength(0);
  });

  test("4401 reconnects and asks nothing", async () => {
    const { store } = started();
    dial(0).open();
    dial(0).closeWith(4401, "credential no longer accepted");
    await vi.advanceTimersByTimeAsync(2_000);

    expect(ScriptedWebSocket.dials.length).toBeGreaterThan(1);
    expect(store.state.accessRefused).toBeNull();
    expect(store.state.authRejected).toBe(false);
    expect(fetches.filter((u) => u.endsWith("/ws/stream"))).toHaveLength(0);
  });

  test("a 403 handshake stops the socket once the probe says why, and a retry dials again", async () => {
    probeStatus = 403;
    probeBody = { error: "unauthorized", detail: "the live socket needs state:read" };
    const { socket, store } = started();
    dial(0).closeWith(1006);
    await vi.advanceTimersByTimeAsync(0);
    expect(store.state.accessRefused).toBe("the live socket needs state:read");

    const dialled = ScriptedWebSocket.dials.length;
    await vi.advanceTimersByTimeAsync(120_000);
    expect(ScriptedWebSocket.dials).toHaveLength(dialled);

    socket.reconnect();
    expect(ScriptedWebSocket.dials).toHaveLength(dialled + 1);
    dial(dialled).open();
    expect(store.state.accessRefused).toBeNull();
  });

  test("a refusal that lands while a dial is in flight is not undone by that dial's close", async () => {
    // The probe is asynchronous and the reconnect is not waiting for it, so
    // the refusal can arrive with the NEXT dial already out. That dial's own
    // close must not start the loop the refusal just stopped.
    probeStatus = 403;
    probeBody = { error: "seat_unavailable", detail: "ana" };
    probeDelayMs = 5_000;
    const { store } = started();
    dial(0).closeWith(1006);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(ScriptedWebSocket.dials).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(4_000);
    expect(store.state.accessRefused).toBe("ana");

    dial(1).closeWith(1006);
    await vi.advanceTimersByTimeAsync(120_000);
    expect(ScriptedWebSocket.dials).toHaveLength(2);
  });
});
