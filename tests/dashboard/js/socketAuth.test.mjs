// How the dashboard reacts when the engine refuses it.
//
// Reads are open by default, so most deployments never reach any of
// this. The ones that set `api.auth.allow_anonymous_read: false` reach
// all of it on the first page load, and the failure mode it guards is
// the one that actually shipped: the engine logged `ws_auth_failed` on
// every dial while the browser showed "Not connected to the engine —
// retrying", a sentence describing a wait that was never going to end.
//
// A browser exposes no status code for a failed WebSocket upgrade, and
// no close code either: a rejected handshake never opened, so there is
// no close frame to carry one. Every refusal arrives as 1006 — exactly
// what a stopped engine looks like.
//
// This suite used to drive `onclose({code: 1008})` and assert the client
// reacted. No browser does that, so the tests passed against a client
// that could not detect a real refusal at all. The distinction is drawn
// by a plain GET of the same path, which answers 401 for a refused
// credential and 426 for an accepted one, so the fake below has to model
// BOTH halves: the close, and the probe that follows it.

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
  // A refused handshake, as a browser actually reports one: the
  // connection never opened, so the close carries 1006 and nothing else.
  // What tells this apart from an outage is the probe the client makes
  // afterwards, which `serverRefuses` below arms.
  reject() {
    this.readyState = FakeWebSocket.CLOSED;
    if (this.onclose) this.onclose({ code: 1006 });
  }
  // A refusal delivered late, on an OPEN socket, as a close frame. The
  // engine does not do this today; the client honours it, so it is
  // covered.
  revoke() {
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

// Whether the engine is refusing this browser's credential. The client
// cannot learn that from the close code, so it asks over HTTP — and this
// is the half of the fake that answers.
let refusing = false;

globalThis.fetch = async (url) => {
  const path = String(url);
  if (path.startsWith("/ws/stream")) {
    // 426 Upgrade Required is the HEALTHY answer to a plain GET of the
    // socket path: the guard let it through and only the missing Upgrade
    // header stopped it. 401 is the refusal.
    return { status: refusing ? 401 : 426, ok: false };
  }
  // The degraded poll fires from every close; it is not the subject here.
  throw new Error("offline");
};

// The probe is a fetch, so a close does not resolve into a prompt within
// the same tick. Every test that drives a refusal has to let the
// microtask queue drain first.
const settle = () => new Promise((r) => setTimeout(r, 0));

/** Drive a refused handshake and wait for the client to work it out. */
async function refuse(sock) {
  refusing = true;
  sock.reject();
  await settle();
}

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
  refusing = false;
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

test("a refused handshake prompts, and re-dials with the new token", async () => {
  const { store, last } = dial();
  answer = "fresh-token";
  await refuse(last());
  assert.strictEqual(prompts, 1);
  // The re-dial is immediate rather than waiting out the backoff: the
  // reader just supplied the missing thing and is looking at the page.
  assert.ok(sockets.length > 1, "no re-dial after the token was entered");
  assert.ok(last().url.includes("token=fresh-token"), last().url);
  assert.strictEqual(store.state.authRejected, false);
});

test("a successful open clears a previous rejection", async () => {
  const { store, last } = dial();
  answer = "fresh-token";
  await refuse(last());
  refusing = false;
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

test("a dismissed prompt is not reopened by the reconnect backoff", async () => {
  const { last } = dial();
  answer = null;
  await refuse(last());
  assert.strictEqual(prompts, 1);
  await refuse(last());
  await refuse(last());
  assert.strictEqual(prompts, 1, "the backoff reopened the dialog");
});

test("a token the engine also refuses gets asked about again", async () => {
  // The ask-once latch exists for the backoff above, not to make a
  // second refusal silent: a reader who mistyped the token would never
  // be asked again and would sit on a page that never says why.
  const { last } = dial();
  answer = "wrong-token";
  await refuse(last());
  assert.strictEqual(prompts, 1);
  answer = "right-token";
  await refuse(last());
  assert.strictEqual(prompts, 2);
  assert.ok(last().url.includes("token=right-token"), last().url);
});

test("a dismissed prompt leaves the banner saying why, not 'retrying'", async () => {
  const { store, last } = dial();
  answer = null;
  await refuse(last());

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

// ---- the bug this suite was blind to ----

test("a stale token on an open-read engine still reaches the reader", async () => {
  // THE REPORTED FAILURE, end to end. `allow_anonymous_read: true`, so
  // the dashboard would have connected with no credential at all — but
  // a token left in localStorage by an earlier deployment is sent, the
  // engine refuses a credential that is present and wrong, and the
  // handshake 401s.
  //
  // Every piece of the repair already existed: the banner, the dialog,
  // "forget this token" in the health popover and the command palette.
  // None of it ran, because the trigger was a close code no browser
  // sends for a refused handshake. The reader saw "retrying" for ever
  // with the one broken thing unmentioned and uncorrectable.
  const { store, last } = dial({ token: "stale-from-last-deployment" });
  answer = null;
  await refuse(last());
  assert.strictEqual(prompts, 1, "the reader was never told the token was wrong");
  assert.strictEqual(store.state.authRejected, true);
});

test("an outage is not reported as a refused token", async () => {
  // The other half, and the reason this cannot simply prompt on every
  // failed dial: a stopped engine closes exactly the same way. What
  // separates them is the probe, which does not answer 401 here.
  const { store, last } = dial({ token: "s3cret" });
  refusing = false;
  last().reject();
  await settle();
  assert.strictEqual(prompts, 0, "an outage raised a token dialog");
  assert.strictEqual(store.state.authRejected, false);
});

test("a probe that cannot be made raises no dialog", async () => {
  // Offline, or a proxy refusing the request. Neither is something the
  // reader fixes by typing a token, and a dialog here would fire on
  // every backoff of a genuinely disconnected page.
  const { store, last } = dial({ token: "s3cret" });
  const answering = globalThis.fetch;
  globalThis.fetch = async () => {
    throw new Error("offline");
  };
  try {
    last().reject();
    await settle();
    assert.strictEqual(prompts, 0);
    assert.strictEqual(store.state.authRejected, false);
  } finally {
    globalThis.fetch = answering;
  }
});

test("the probe sends the credential in a header, not the query string", async () => {
  // The handshake has no choice — a WebSocket constructor cannot set
  // headers — but a fetch does, and a token in a URL is a token in every
  // proxy's access log between the browser and the engine.
  const { last } = dial({ token: "s3cret" });
  let seen = null;
  const answering = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    seen = { url: String(url), init };
    return { status: 401, ok: false };
  };
  try {
    last().reject();
    await settle();
  } finally {
    globalThis.fetch = answering;
  }
  assert.ok(seen, "no probe was made");
  assert.ok(!seen.url.includes("token="), seen.url);
  assert.strictEqual(seen.init.headers.Authorization, "Bearer s3cret");
});

test("a refusal delivered on an open socket is still honoured", async () => {
  // close(1008) after the upgrade. The engine does not do this today —
  // it refuses the handshake — but a socket whose credential is revoked
  // mid-session would, and the client should not need changing then.
  const { store, last } = dial({ token: "s3cret" });
  answer = null;
  last().open();
  last().revoke();
  await settle();
  assert.strictEqual(prompts, 1);
  assert.strictEqual(store.state.authRejected, true);
});

await run();
for (const socket of built) socket.stop();
