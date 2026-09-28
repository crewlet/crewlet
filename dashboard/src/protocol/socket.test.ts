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
import { currentSessionNeed, sessionRestored } from "./session.ts";
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
let fetchInits: (RequestInit | undefined)[] = [];
let probeStatus = 426;
let probeBody: unknown = {};
/** How long the plain-HTTP re-ask takes to answer. */
let probeDelayMs = 0;

beforeEach(() => {
  vi.useFakeTimers();
  ScriptedWebSocket.dials = [];
  fetches = [];
  fetchInits = [];
  probeStatus = 426;
  probeBody = {};
  probeDelayMs = 0;
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: ScriptedWebSocket });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      fetches.push(url);
      fetchInits.push(init);
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
  sessionRestored();
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
    // THE CONTROL for the enrolment case: a refusal on authority is the
    // administrator's to repair, and sends nobody to sign in.
    expect(currentSessionNeed()).toBeNull();

    const dialled = ScriptedWebSocket.dials.length;
    await vi.advanceTimersByTimeAsync(120_000);
    expect(ScriptedWebSocket.dials).toHaveLength(dialled);

    socket.reconnect();
    expect(ScriptedWebSocket.dials).toHaveLength(dialled + 1);
    dial(dialled).open();
    expect(store.state.accessRefused).toBeNull();
  });

  // A HANDSHAKE THAT NAMES NOBODY is a browser that needs to sign in, which
  // is said where the application can hear it; the socket itself draws
  // nothing.
  test("a 401 handshake asks for a sign-in", async () => {
    probeStatus = 401;
    probeBody = { error: "invalid_token" };
    const { store } = started();
    dial(0).closeWith(1006);
    await vi.advanceTimersByTimeAsync(0);
    expect(store.state.authRejected).toBe(true);
    expect(currentSessionNeed()).toBe("sign_in");
    expect(store.state.accessRefused).toBeNull();
  });

  // A SESSION THAT MAY ONLY ENROL is refused the socket until it has, and
  // its repair is the person's own: the enrolment, not an administrator.
  test("a session that may only enrol stops the socket and asks for the enrolment", async () => {
    probeStatus = 403;
    probeBody = { error: "second_factor_enrolment_required" };
    const { socket, store } = started();
    dial(0).closeWith(1006);
    await vi.advanceTimersByTimeAsync(0);
    expect(currentSessionNeed()).toBe("second_factor");
    expect(store.state.accessRefused).toBeNull();

    const dialled = ScriptedWebSocket.dials.length;
    await vi.advanceTimersByTimeAsync(120_000);
    expect(ScriptedWebSocket.dials).toHaveLength(dialled);

    // The enrolment ends by re-dialling with the whole session it opened.
    socket.reconnect();
    expect(ScriptedWebSocket.dials).toHaveLength(dialled + 1);
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

// THE COOKIE IS THE CREDENTIAL, on the dial and on the probe alike. A
// handshake URL is written into every proxy's access log, which is why the
// engine reads no credential there; a client that put one there anyway would
// be leaking it to a log for nothing.
describe("what a dial presents", () => {
  test("the handshake URL carries no credential, and the probe no header of its own", async () => {
    probeStatus = 401;
    started();
    expect(dial(0).url).toBe(`ws://${location.host}/ws/stream`);
    dial(0).closeWith(1006);
    await vi.advanceTimersByTimeAsync(0);
    const probe = fetchInits[fetches.findIndex((url) => url.endsWith("/ws/stream"))];
    expect(probe?.credentials).toBe("same-origin");
    expect(new Headers(probe?.headers).has("Authorization")).toBe(false);
  });
});
