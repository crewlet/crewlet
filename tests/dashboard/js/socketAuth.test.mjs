// How the dashboard reacts when the engine refuses it.
//
// Reads are open by default, so most deployments never reach any of
// this. The ones that set `api.auth.allow_anonymous_read: false` reach
// all of it on the first page load, and the failure mode it guards is
// the one that actually shipped: the engine logged `ws_auth_failed` on
// every dial while the browser showed "Not connected to the engine —
// retrying", a sentence describing a wait that was never going to end.
//
// A browser exposes no status code for a failed WebSocket upgrade. The
// close code is the entire signal, which is why every test here is
// really about telling 1008 from 1006.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";
import { JS_URL } from "./dashboardRoot.mjs";

installDom();

// ---- the fake browser bits socket.js and authToken.js reach for ----

const sockets = [];

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;

  constructor(url) {
    this.url = url;
    this.readyState = FakeWebSocket.CONNECTING;
    this.sent = [];
    sockets.push(this);
  }
  send(frame) {
    this.sent.push(frame);
  }
  close() {
    this.readyState = FakeWebSocket.CLOSED;
    if (this.onclose) this.onclose({ code: 1000 });
  }
  // Test-side drivers.
  open() {
    this.readyState = FakeWebSocket.OPEN;
    if (this.onopen) this.onopen();
  }
  reject() {
    this.readyState = FakeWebSocket.CLOSED;
    if (this.onclose) this.onclose({ code: 1008 });
  }
  drop() {
    this.readyState = FakeWebSocket.CLOSED;
    if (this.onclose) this.onclose({ code: 1006 });
  }
}
globalThis.WebSocket = FakeWebSocket;

const stored = new Map();
globalThis.localStorage = {
  getItem: (k) => (stored.has(k) ? stored.get(k) : null),
  setItem: (k, v) => stored.set(k, v),
};

let prompts = 0;
let answer = null;

// The degraded poll fires from every close; it is not the subject here.
globalThis.fetch = async () => {
  throw new Error("offline");
};

const { Store } = await import(
  new URL("store.js", JS_URL)
);
const { LiveSocket } = await import(
  new URL("socket.js", JS_URL)
);
const { bannerFor, bannerTone } = await import(
  new URL("health.js", JS_URL)
);

// Every LiveSocket ever built here. A live one holds a ping interval, a
// reconnect timeout and a 5-second degraded poll, so leaving them
// running keeps node's event loop alive and the suite never exits —
// which reads as a hang rather than as a pass.
const built = [];

function dial({ token = "" } = {}) {
  sockets.length = 0;
  prompts = 0;
  stored.clear();
  if (token) stored.set("crewlet_api_token", token);
  const store = new Store();
  const socket = new LiveSocket(store);
  built.push(socket);
  socket.setToken(token);
  // The shell owns the dialog; the socket only knows a refusal from an
  // outage. This models what `askForToken` in app.js does with an
  // answer — anything the socket needs to do on its own has to happen
  // without help from here.
  socket.onAuthRejected(() => {
    prompts++;
    if (!answer) return;
    stored.set("crewlet_api_token", answer);
    socket.setToken(answer);
    store.setAuthRejected(false);
    socket.reconnect();
  });
  socket.start();
  return { store, socket, last: () => sockets[sockets.length - 1] };
}

// ---- the handshake credential ----

test("the handshake carries the stored token", () => {
  // A `WebSocket` constructor cannot send an Authorization header, so
  // the query form is the only one a browser has. Without this the
  // richest endpoint in the API was the one endpoint the dashboard
  // could not authenticate to at all.
  const { last } = dial({ token: "s3cret" });
  assert.ok(last().url.endsWith("/ws/stream?token=s3cret"), last().url);
});

test("a token is sent on the first dial, not only after a rejection", () => {
  // Waiting to be told would spend a full backoff cycle disconnected on
  // every single page load of a guarded engine.
  const { last } = dial({ token: "s3cret" });
  assert.strictEqual(sockets.length, 1);
  assert.ok(last().url.includes("token="));
});

test("no stored token means no query string at all", () => {
  // The open-read deployment, which is the default one: an empty
  // `?token=` would be a credential the server has to reject.
  const { last } = dial();
  assert.ok(!last().url.includes("?"), last().url);
});

test("the token is URL-encoded", () => {
  const { last } = dial({ token: "a b/c&d" });
  assert.ok(last().url.endsWith("?token=a%20b%2Fc%26d"), last().url);
});

// ---- telling a refusal from an outage ----

test("an ordinary drop is not treated as a refusal", () => {
  const { store, last } = dial({ token: "s3cret" });
  last().open();
  last().drop();
  assert.strictEqual(store.state.authRejected, false);
  assert.strictEqual(prompts, 0);
});

test("a refused handshake prompts, and re-dials with the new token", () => {
  const { store, last } = dial();
  answer = "fresh-token";
  last().reject();
  assert.strictEqual(prompts, 1);
  // The re-dial is immediate rather than waiting out the backoff: the
  // reader just supplied the missing thing and is looking at the page.
  assert.ok(sockets.length > 1, "no re-dial after the token was entered");
  assert.ok(last().url.includes("token=fresh-token"), last().url);
  assert.strictEqual(store.state.authRejected, false);
});

test("a successful open clears a previous rejection", () => {
  const { store, last } = dial();
  answer = "fresh-token";
  last().reject();
  last().open();
  assert.strictEqual(store.state.authRejected, false);
  assert.strictEqual(store.state.connected, true);
});

test("stopping the client stops the degraded poll too", async () => {
  // `stop()` clears the timers and THEN closes the socket, whose close
  // handler restarts the fallback — so the poll outlived the client it
  // belonged to and kept fetching for the life of the tab, with no
  // socket and nothing to render into.
  const { socket } = dial();
  let fetches = 0;
  const offline = globalThis.fetch;
  globalThis.fetch = async () => {
    fetches++;
    throw new Error("offline");
  };
  try {
    socket.stop();
    const after = fetches;
    await new Promise((r) => setTimeout(r, 30));
    assert.strictEqual(fetches, after, "the poll kept running after stop()");
    assert.strictEqual(socket.fallbackTimer, 0);
  } finally {
    globalThis.fetch = offline;
  }
});

test("a dismissed prompt is not reopened by the reconnect backoff", () => {
  const { last } = dial();
  answer = null;
  last().reject();
  assert.strictEqual(prompts, 1);
  last().reject();
  last().reject();
  assert.strictEqual(prompts, 1, "the backoff reopened the dialog");
});

test("a token the engine also refuses gets asked about again", () => {
  // The ask-once latch exists for the backoff above, not to make a
  // second refusal silent: a reader who mistyped the token would never
  // be asked again and would sit on a page that never says why.
  const { last } = dial();
  answer = "wrong-token";
  last().reject();
  assert.strictEqual(prompts, 1);
  answer = "right-token";
  last().reject();
  assert.strictEqual(prompts, 2);
  assert.ok(last().url.includes("token=right-token"), last().url);
});

test("a dismissed prompt leaves the banner saying why, not 'retrying'", () => {
  const { store, last } = dial();
  answer = null;
  last().reject();

  assert.strictEqual(store.state.authRejected, true);
  const text = bannerFor(store.state.health, store.state.connected, [], true);
  assert.match(text, /refused/);
  assert.ok(
    !/retrying/.test(text),
    "a refused token was reported as an outage that would resolve itself",
  );
  // Amber, not red: nothing failed. The engine is healthy and is doing
  // exactly what it was configured to do.
  assert.strictEqual(bannerTone(store.state.health, false, true), "warn");
});

await run();
for (const socket of built) socket.stop();
