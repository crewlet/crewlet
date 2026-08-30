// The engine-health surface.
//
// Everything asserted here was already on the wire and reached no
// screen. The failure mode that motivated the whole surface is the one
// pinned first: an engine with no active company configuration — one
// accepting and discarding every inbound webhook — rendered exactly
// like a healthy idle engine, on every screen at once.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";
import { JS_URL } from "./dashboardRoot.mjs";

installDom();
const { dotClass, providerTrouble, renderHealthPopover, bannerFor, bannerTone } =
  await import(
    new URL("health.js", JS_URL)
  );
const { emptyOrPending } = await import(
  new URL("ui.js", JS_URL)
);

// ---- the dot ----

test("a disconnected socket outranks whatever health last said", () => {
  // The last health tick is by definition stale once the socket drops.
  assert.strictEqual(dotClass({ status: "ok" }, false), "down");
});

test("an unconfigured engine is not green", () => {
  assert.strictEqual(dotClass({ status: "unconfigured" }, true), "warn");
});

test("a draining engine is amber", () => {
  assert.strictEqual(dotClass({ status: "shutting_down" }, true), "drain");
});

test("a status the client does not recognise is never rendered as healthy", () => {
  // This is why the mapping is a table and not a ternary chain: the
  // chain's final `else` sent every unrecognised status to green, so a
  // state added on the server would have ARRIVED on screen as healthy.
  assert.strictEqual(dotClass({ status: "wedged" }, true), "warn");
  assert.strictEqual(dotClass({}, true), "warn");
  assert.strictEqual(dotClass(null, true), "warn");
});

// ---- the banner ----

test("an unconfigured engine gets a banner, not just a dot", () => {
  const text = bannerFor({ status: "unconfigured", configured: false }, true, []);
  assert.ok(text.includes("No company configuration is active"));
  assert.ok(text.includes("webhooks"), "the consequence is not stated");
  assert.strictEqual(bannerTone({ configured: false }, true), "warn");
});

test("a healthy engine gets no banner at all", () => {
  assert.strictEqual(bannerFor({ status: "ok", configured: true }, true, []), "");
});

test("a lost socket outranks the unconfigured banner", () => {
  // Both are true at once when an unconfigured engine goes away, and
  // only one of them tells the reader that what is on screen is stale.
  const text = bannerFor({ configured: false }, false, []);
  assert.ok(text.includes("Not connected"));
  assert.strictEqual(bannerTone({ configured: false }, false), "down");
});

test("every banner that has text also has a tone to paint it", () => {
  // The stylesheet declares `.degraded.down` and `.degraded.warn` and
  // no bare default, so a tone the CSS does not know renders as plain
  // body text — a correctly-worded warning nobody would read as one.
  const cases = [
    [{ status: "ok", configured: true }, true],
    [{ status: "unconfigured", configured: false }, true],
    [{ status: "shutting_down", configured: true }, true],
    [{ configured: false }, false],
    [{}, false],
  ];
  for (const [health, connected] of cases) {
    const text = bannerFor(health, connected, []);
    const tone = bannerTone(health, connected);
    assert.strictEqual(
      !!text,
      !!tone,
      `text and tone disagree for ${JSON.stringify(health)} connected=${connected}`,
    );
    if (tone) assert.ok(tone === "down" || tone === "warn", `unstyled tone ${tone}`);
  }
});

test("a disconnected banner dates the state it is still showing", () => {
  const text = bannerFor({}, false, [{ timestamp: new Date().toISOString() }]);
  assert.ok(text.includes("last state received"));
});

// ---- provider trouble ----

test("recovered fallbacks are counted here, not as feed failures", () => {
  // `provider_fallback` means the chain RECOVERED. Counting it in the
  // feed's red would fire the sidebar's alert badge on turns that
  // succeeded; "a provider is degrading" is a health question.
  const trouble = providerTrouble([
    { type: "provider_fallback" },
    { type: "provider_fallback" },
    { type: "llm_unavailable" },
    { type: "task_created" },
  ]);
  assert.strictEqual(trouble.fallbacks, 2);
  assert.strictEqual(trouble.unavailable, 1);
  assert.strictEqual(trouble.retained, 4, "the window is not the row count");
});

test("no events is not an error", () => {
  assert.deepStrictEqual(providerTrouble(undefined), {
    fallbacks: 0,
    unavailable: 0,
    retained: 0,
  });
});

// ---- the popover ----

const OK = {
  status: "ok",
  configured: true,
  engine: true,
  version: "0.1.0",
  queue: "jetstream",
  event_store: "durable",
  feed_hydrated: true,
  in_flight: 2,
  engine_started_at: new Date(Date.now() - 3600_000).toISOString(),
};

test("a healthy engine reports every wiring fact it knows", () => {
  const html = renderHealthPopover(OK, null, [], true);
  assert.ok(html.includes("jetstream"));
  assert.ok(html.includes("durable"));
  assert.ok(html.includes("history survives a restart"));
  assert.ok(html.includes("seeded from history"));
});

test("a memory event store says what it will cost on a restart", () => {
  // "A store is wired" is not "history survives a restart" — with no
  // database the CLI still wraps in-memory legs in a composite store,
  // so the presence check an operator would make answers yes.
  const html = renderHealthPopover({ ...OK, event_store: "memory" }, null, [], true);
  assert.ok(html.includes("will not survive a restart"));
});

test("no engine renders an em dash, never a confident zero", () => {
  // "Nothing is running" and "this process has no engine to ask" are
  // different answers, and the standalone API can only give the second.
  const html = renderHealthPopover(
    { ...OK, engine: false, in_flight: undefined },
    null,
    [],
    true,
  );
  assert.ok(html.includes("no engine in this API process"));
  assert.ok(html.includes(">—<"), "the value was not an em dash");
});

test("dropped envelopes offer the only repair there is", () => {
  // A dropped envelope is gone: the server discards the OLDEST queued
  // frame and never re-sends it, so a fresh handshake snapshot is the
  // only way back to a correct page.
  const html = renderHealthPopover(
    OK,
    { dropped: 3, queued: 0, capacity: 256, clients: 2, connected_at: new Date().toISOString() },
    [],
    true,
  );
  assert.ok(html.includes('data-action="reconnect"'));
});

test("no drops means no reconnect prompt", () => {
  const html = renderHealthPopover(
    OK,
    { dropped: 0, queued: 0, capacity: 256, clients: 1, connected_at: "" },
    [],
    true,
  );
  assert.ok(!html.includes('data-action="reconnect"'));
});

test("a disconnected popover claims nothing about the engine", () => {
  // Dropping the socket CLEARS the health slice — correctly, a
  // five-second-old tick is not evidence about now — so every field
  // arrives `undefined`. Read as `=== false ? bad : good`, that
  // rendered "Configuration: active" on a page that could not see the
  // engine at all: unknown-reads-as-healthy, in the surface built to
  // prevent exactly that.
  const html = renderHealthPopover({ status: "unknown" }, null, [], false);
  assert.ok(html.includes("Disconnected") || html.includes("disconnected"));
  assert.ok(!html.includes(">active<"), "claimed a configuration it cannot see");
  assert.ok(!html.includes("seeded from history"), "claimed a seeded feed");
  assert.ok(html.includes("not known while disconnected"));
});

test("the per-socket section waits rather than inventing zeros", () => {
  const html = renderHealthPopover(OK, null, [], true);
  assert.ok(html.includes("This connection"));
  assert.ok(!html.includes("Tabs connected"));
});

test("the popover states the retained window it counted over", () => {
  // The feed is a bounded ring and a busy org fills it in minutes, so a
  // duration would be a lie; the row count is the honest denominator.
  const html = renderHealthPopover(OK, null, [{ type: "provider_fallback" }], true);
  assert.ok(html.includes("in the last 1 retained events"));
});

// ---- the empty states the flag now explains ----

test("an unconfigured engine explains every empty list, not just one", () => {
  const html = emptyOrPending(
    { connected: true, health: { configured: false } },
    () => "SKELETON",
    "user",
    "No agents configured",
  );
  assert.ok(html.includes("No company configuration is active"));
  assert.ok(!html.includes("No agents configured"));
});

test("a configured engine with an empty list says so plainly", () => {
  const html = emptyOrPending(
    { connected: true, health: { configured: true } },
    () => "SKELETON",
    "user",
    "No agents configured",
  );
  assert.ok(html.includes("No agents configured"));
});

test("a disconnected client concludes nothing", () => {
  const html = emptyOrPending(
    { connected: false, health: { configured: false } },
    () => "SKELETON",
    "user",
    "No agents configured",
  );
  assert.strictEqual(html, "SKELETON");
});

test("the skeleton is a thunk, so it is not built on the branch not taken", () => {
  let built = 0;
  emptyOrPending({ connected: true, health: { configured: true } }, () => {
    built++;
    return "SKELETON";
  }, "user", "No agents configured");
  assert.strictEqual(built, 0);
});

test("the popover says whether this browser is holding a token", () => {
  // The answer to "why does it never ask me for a token": because it
  // already has one. The credential lives in localStorage and rides
  // every handshake and every query, so a reader who set one once is
  // authenticated on every later visit — correct, and previously
  // invisible. Nothing on any screen said a credential was being held,
  // and nothing could drop it, so the only way to find out was devtools.
  const held = renderHealthPopover(OK, null, [], true, { held: true });
  assert.match(held, /API token/);
  assert.match(held, /held/);
  assert.match(held, /data-action="clear-token"/);

  const none = renderHealthPopover(OK, null, [], true, { held: false });
  assert.match(none, /not set/);
  assert.match(none, /data-action="set-token-chrome"/);
  assert.ok(
    !/data-action="clear-token"/.test(none),
    "offered to forget a token that is not there",
  );
});

test("a popover rendered with no auth argument claims nothing", () => {
  // Every other field here is three-valued for the same reason: an
  // absent fact must not render as a confident one.
  //
  // This assertion used to check only that "held" was absent, which the
  // "not set" branch satisfies — so it passed while the popover was
  // stating, to a caller that had not looked, that no credential exists.
  // Checking one side of a two-sided claim is how a test certifies the
  // half of the bug it was written to catch.
  const html = renderHealthPopover(OK, null, [], true);
  assert.ok(!/>held</.test(html), "invented a credential nobody reported");
  assert.ok(!/not set/.test(html), "declared a credential missing without looking");
  assert.match(html, /not reported/);
  assert.ok(
    !/data-action="clear-token"|data-action="set-token-chrome"/.test(html),
    "offered an action for a state nobody established",
  );
});

run();
