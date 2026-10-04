/**
 * How the live socket answers the engine's two close codes, a refused
 * handshake and every other close — the half of revalidation that runs in the
 * browser.
 *
 * The engine closes an open socket when the identity estate moves under it:
 * 4401 when its session ended, 4403 when its credential names somebody who may
 * not have this surface. The repairs are opposite: a 4401 is a reconnect once
 * this tab's own requests have landed (the browser may hold a newer cookie,
 * and after a step-up it is about to), a 4403 is not repaired by anything this
 * tab can do, so the socket must STOP rather than dial the same refusal every
 * thirty seconds under a "reconnecting" banner. Anything else is the backoff.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { rest } from "./rest.ts";
import { currentSessionNeed, sessionRestored } from "./signin.ts";
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
/** The headers the plain-HTTP re-ask answers with — a `Retry-After`, say. */
let probeHeaders: Record<string, string> = {};
/** How long the plain-HTTP re-ask takes to answer. */
let probeDelayMs = 0;
/** What the degraded-mode snapshot read answers: by default nothing the engine wrote. */
let snapshotAnswer: () => Promise<Response> = async () => new Response("{}", { status: 503 });

beforeEach(() => {
  vi.useFakeTimers();
  ScriptedWebSocket.dials = [];
  fetches = [];
  fetchInits = [];
  probeStatus = 426;
  probeBody = {};
  probeHeaders = {};
  probeDelayMs = 0;
  snapshotAnswer = async () => new Response("{}", { status: 503 });
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: ScriptedWebSocket });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      fetches.push(url);
      fetchInits.push(init);
      if (url.endsWith("/ws/stream")) {
        if (probeDelayMs > 0) await new Promise((resolve) => setTimeout(resolve, probeDelayMs));
        return new Response(JSON.stringify(probeBody), {
          status: probeStatus,
          headers: probeHeaders,
        });
      }
      return snapshotAnswer();
    }),
  );
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
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

  // A STEP-UP REPLACES THE SESSION IT WAS MADE FROM, so the engine closes
  // every socket the old one opened — and the answer that sets the new cookie
  // may still be on its way. A dial before it lands carries the ended cookie,
  // and its refusal would send somebody who just proved who they are to the
  // sign-in form.
  test("4401 dials again once this tab's own requests have landed, not before", async () => {
    let land!: () => void;
    snapshotAnswer = () =>
      new Promise<Response>((resolve) => {
        land = () => resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      });
    started();
    dial(0).open();
    const stepUp = rest.post("/auth/step-up", { password: "correct horse battery" });
    await vi.advanceTimersByTimeAsync(0);
    dial(0).closeWith(4401, "session replaced");
    await vi.advanceTimersByTimeAsync(5_000);
    expect(ScriptedWebSocket.dials).toHaveLength(1);

    land();
    await stepUp;
    await vi.advanceTimersByTimeAsync(0);
    expect(ScriptedWebSocket.dials).toHaveLength(2);
  });

  // 1013 IS "TRY AGAIN LATER": an ordinary reconnect on the backoff, which
  // asks nobody to sign in and withdraws nothing.
  test("1013 dials again on the backoff and asks nobody to sign in", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0.5);
    const { store } = started();
    dial(0).open();
    dial(0).closeWith(1013, "node restarting");
    await vi.advanceTimersByTimeAsync(499);
    expect(ScriptedWebSocket.dials).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(ScriptedWebSocket.dials).toHaveLength(2);
    expect(store.state.authRejected).toBe(false);
    expect(store.state.accessRefused).toBeNull();
    expect(currentSessionNeed()).toBeNull();
    // A HANDSHAKE THAT COMPLETED is not re-asked over HTTP: it was no refusal.
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

// A HANDSHAKE THE ENGINE COULD NOT DECIDE YET SAYS WHEN TO DIAL AGAIN. A node
// that cannot read its identity estate answers the handshake `503` with a
// `Retry-After`, and the loop went on doubling its own wait whatever that
// said: a dial at one second on the first refusal, before the node had said
// it could answer, and sixteen or thirty seconds a few refusals in, where it
// had said two. Only the engine's own `503` says so, and a zero is no wait at
// all for THIS node — which is the backoff's case, since the next dial may
// reach another.
describe("a handshake the engine could not decide yet", () => {
  const dials = () => ScriptedWebSocket.dials.length;
  const engineBusy = (retryAfter?: number) => {
    probeStatus = 503;
    probeBody = {
      error: "identity_unavailable",
      message: "This node cannot read its identity estate right now.",
    };
    probeHeaders = retryAfter === undefined ? {} : { "Retry-After": String(retryAfter) };
  };

  test("is dialled again when its Retry-After says, not at the backoff's second", async () => {
    engineBusy(12);
    started();
    dial(0).closeWith(1006);
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(11_999);
    expect(dials()).toBe(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(dials()).toBe(2);
  });

  // SOONER THAN THE BACKOFF HAS GROWN TO, as well as later than it started:
  // every refused dial is answered by the hint, never by a wait that doubles.
  test("keeps the hint's cadence however many dials it refuses", async () => {
    engineBusy(2);
    started();
    for (let n = 0; n < 6; n++) {
      dial(n).closeWith(1006);
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(1_999);
      expect(dials()).toBe(n + 1);
      await vi.advanceTimersByTimeAsync(1);
      expect(dials()).toBe(n + 2);
    }
  });

  // THE CONTROLS: a `503` the engine wrote with no `Retry-After`, and one
  // something in front of it wrote, keep the backoff — first dial at a second.
  test.each([
    ["a zero", () => engineBusy()],
    [
      "a proxy's 503",
      () => {
        probeStatus = 503;
        probeBody = {};
        probeHeaders = { "Retry-After": "12" };
      },
    ],
  ])("after %s keeps the backoff", async (_, answer) => {
    answer();
    started();
    dial(0).closeWith(1006);
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(dials()).toBe(2);
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

// THE DEGRADED-MODE POLL WAITS WHAT THE ENGINE SAID, as every other re-ask
// does. It read the snapshot every five seconds whatever came back, so a node
// that answered "come back in twelve" was asked twice first, and one whose
// identity estate refused every guarded read until an operator acted — a 503
// the engine wrote with no Retry-After — was asked for as long as the socket
// stayed down. Anything the engine did not write is the ordinary tick.
describe("the degraded-mode poll", () => {
  const snapshotReads = () => fetches.filter((url) => url.endsWith("/stream/snapshot")).length;
  const refusal = (headers: Record<string, string>) => async () =>
    new Response(JSON.stringify({ error: "identity_unavailable" }), { status: 503, headers });

  /** A socket that opened and then dropped, which starts the poll. */
  async function dropped(): Promise<LiveSocket> {
    const { socket } = started();
    dial(0).open();
    dial(0).closeWith(1006);
    await vi.advanceTimersByTimeAsync(0);
    return socket;
  }

  test("a 503 the engine wrote is asked again when its Retry-After says", async () => {
    snapshotAnswer = refusal({ "Retry-After": "12" });
    await dropped();
    expect(snapshotReads()).toBe(1);
    await vi.advanceTimersByTimeAsync(11_999);
    expect(snapshotReads()).toBe(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(snapshotReads()).toBe(2);
  });

  test("a 503 the engine wrote with no Retry-After is not asked again on a timer", async () => {
    snapshotAnswer = refusal({});
    await dropped();
    await vi.advanceTimersByTimeAsync(120_000);
    expect(snapshotReads()).toBe(1);
  });

  test("a read nothing at the engine answered is the ordinary tick", async () => {
    snapshotAnswer = () => Promise.reject(new TypeError("Failed to fetch"));
    await dropped();
    await vi.advanceTimersByTimeAsync(5_000);
    expect(snapshotReads()).toBe(2);
  });

  // A READ IN FLIGHT WHEN ITS RUN ENDS SCHEDULES NOTHING. `stop()` ends the
  // run with the socket closed, so "is the socket back" cannot be what says
  // so: a read that landed after it started a new chain for the life of the
  // tab, with nothing to render into.
  test("a read that lands after the client stopped schedules no more", async () => {
    snapshotAnswer = () =>
      new Promise((resolve) =>
        setTimeout(() => resolve(new Response("{}", { status: 503 })), 1_000),
      );
    const socket = await dropped();
    socket.stop();
    await vi.advanceTimersByTimeAsync(120_000);
    expect(snapshotReads()).toBe(1);
  });

  test("a snapshot read keeps the tick, and the poll ends when the socket is back", async () => {
    snapshotAnswer = async () =>
      new Response(JSON.stringify({ agents: [], events: [] }), { status: 200 });
    await dropped();
    await vi.advanceTimersByTimeAsync(5_000);
    expect(snapshotReads()).toBe(2);
    const reconnected = ScriptedWebSocket.dials.at(-1)!;
    reconnected.open();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(snapshotReads()).toBe(2);
  });
});

// FULL JITTER: a node that closes every socket at once brings every tab back
// spread over the whole of the backoff's window, never in lockstep at its top.
describe("the backoff", () => {
  const dials = () => ScriptedWebSocket.dials.length;

  test("draws a dial at zero when the draw is zero", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);
    started();
    dial(0).closeWith(1006);
    await vi.advanceTimersByTimeAsync(0);
    expect(dials()).toBe(2);
  });

  // AT THE TOP OF THE DRAW, each wait is just short of a ceiling that doubles
  // from a second and stops at thirty.
  test("waits no longer than a ceiling that doubles to thirty seconds", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0.999);
    started();
    const ceilings = [1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000];
    for (const [n, ceiling] of ceilings.entries()) {
      dial(n).closeWith(1006);
      await vi.advanceTimersByTimeAsync(Math.floor(ceiling * 0.999) - 1);
      expect(dials()).toBe(n + 1);
      await vi.advanceTimersByTimeAsync(1);
      expect(dials()).toBe(n + 2);
    }
  });
});
