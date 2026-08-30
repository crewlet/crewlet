// The rooms built for the reorganised dashboard, and the claims each one
// must not make.
//
// These views mostly render facts, and the way a view like this fails is
// not by throwing — it is by stating something it cannot see: a budget
// drawn at the bottom of its cap because the shared counter could not be
// read, an integration called healthy because no traffic arrived, a
// sandbox run that is waiting on a person and says nothing about it.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";
import { JS_URL } from "./dashboardRoot.mjs";

installDom();
const base = JS_URL;
const { createWorkView } = await import(new URL("views/work.js", base));
const { createSpendView } = await import(new URL("views/spend.js", base));
const { createIntegrationsView } = await import(
  new URL("views/integrations.js", base)
);
const { createMissionView } = await import(new URL("views/mission.js", base));

function state(extra = {}) {
  return {
    connected: true,
    agents: [],
    events: [],
    sandboxes: [],
    org: {},
    tools: [],
    tokens: null,
    budget: {},
    health: { configured: true },
    ...extra,
  };
}

/** Mount a view with a stubbed query channel and always destroy it. */
async function withView(factory, answers, body) {
  let impl = async (what) => {
    if (!(what in answers)) throw new Error("unknown_query");
    const value = answers[what];
    if (value === null) throw new Error("failed");
    return value;
  };
  const ctx = {
    store: { state: state() },
    query: (...args) => impl(...args),
    refresh: () => {},
    navigate: () => {},
    params: {},
    setQuery: (fn) => {
      impl = fn;
    },
  };
  const view = factory(ctx);
  if (view.mount) view.mount(document.createElement("div"));
  await new Promise((resolve) => setTimeout(resolve, 0));
  try {
    await body({ view, ctx });
  } finally {
    if (view.destroy) view.destroy();
  }
}

// ---------------------------------------------------------------------
// Work
// ---------------------------------------------------------------------

test("work: a reseed run says its box is gone and its branch is not", async () => {
  // The state with no surface anywhere before this room existed: the box
  // was reclaimed, the seat is free, nothing is in the feed. It looked
  // exactly like work that had finished.
  await withView(
    createWorkView,
    {
      sandbox_runs: {
        runs: [
          {
            turn_id: "t1",
            role: "Engineer",
            status: "reseed",
            question: "Which auth provider for the admin panel?",
            branch: "feat/admin-auth",
            coding_agent: "claude-code",
            answerable_in_chat: true,
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /Waiting on an answer/);
      assert.match(html, /Which auth provider/);
      assert.match(html, /re-seed from the pushed branch/);
      assert.match(html, /feat\/admin-auth/);
    },
  );
});

test("work: a paused box says it is being billed for", async () => {
  await withView(
    createWorkView,
    {
      sandbox_runs: {
        runs: [
          {
            turn_id: "t2",
            role: "Engineer",
            status: "awaiting_clarification",
            question: "Drop or dead-letter?",
            answerable_in_chat: true,
          },
        ],
      },
    },
    ({ view }) => {
      assert.match(view.render(state()), /billed for/);
    },
  );
});

test("work: a run no chat can answer says so", async () => {
  // A run started by a schedule stored a conversation key no inbound
  // message can reproduce. Telling somebody to "reply in the thread"
  // would send them to a thread that does not exist.
  await withView(
    createWorkView,
    {
      sandbox_runs: {
        runs: [
          {
            turn_id: "t3",
            role: "Engineer",
            status: "awaiting_clarification",
            question: "q",
            answerable_in_chat: false,
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /no conversation to reply in/);
      assert.doesNotMatch(html, /answer by replying where the task came from/);
    },
  );
});

test("work: no pending store is said, not shown as an empty board", async () => {
  await withView(createWorkView, {}, async ({ view, ctx }) => {
    ctx.setQuery(async () => {
      throw new Error("no_pending_store");
    });
    await view.__loadForTest();
    assert.match(view.render(state()), /No pending-run store/);
  });
});

// ---------------------------------------------------------------------
// Spend & Budgets
// ---------------------------------------------------------------------

test("spend: an unreadable counter is not drawn as zero spend", async () => {
  await withView(
    createSpendView,
    { budgets: { durable: false, org: {}, seats: [] } },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /No shared usage counter/);
      assert.doesNotMatch(html, /of 0 tokens/);
    },
  );
});

test("spend: exhaustion is read from the refusal, not from used >= max", async () => {
  await withView(
    createSpendView,
    {
      budgets: {
        durable: true,
        org: { max_tokens: 100000, durable_used: 99000, refused_at: "" },
        seats: [
          {
            agent_id: "a1",
            role: "CEO",
            handle: "ceo",
            max_tokens: 100000,
            durable_used: 99000,
            refused_at: "2026-08-21T09:14:00Z",
            live_used: 99000,
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      // The org is at 99% and NOT refused, so it is not called exhausted.
      assert.doesNotMatch(html, /Refusing charges/);
      // The seat was refused, so it is — at the same ratio.
      assert.match(html, /refused/);
    },
  );
});

test("spend: the durable counter is labelled as the one that enforces", async () => {
  await withView(
    createSpendView,
    {
      budgets: {
        durable: true,
        org: { max_tokens: 500, durable_used: 100 },
        seats: [],
      },
    },
    ({ view }) => {
      assert.match(view.render(state()), /every node and every restart/);
    },
  );
});

// ---------------------------------------------------------------------
// Integrations
// ---------------------------------------------------------------------

test("integrations: silence is reported as silence, never as health", async () => {
  await withView(
    createIntegrationsView,
    {
      integrations: {
        traffic_known: true,
        traffic_since: null,
        integrations: [
          {
            key: "gitlab",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/gitlab",
            secret_present: true,
            routes: true,
            seats: ["swe"],
            inbound: 0,
            last_at: null,
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /nothing has arrived/);
      assert.match(html, /not the same as broken/);
    },
  );
});

test("integrations: a missing signing secret is a finding", async () => {
  // A webhook route with no secret answers 503 to every delivery. The
  // sender sees a retry; the operator saw nothing at all.
  await withView(
    createIntegrationsView,
    {
      integrations: {
        traffic_known: true,
        traffic_since: null,
        integrations: [
          {
            key: "github",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/github",
            secret_present: false,
            routes: false,
            seats: [],
            inbound: 0,
            last_at: null,
          },
        ],
      },
    },
    ({ view }) => {
      assert.match(view.render(state()), /missing — every delivery is being turned away/);
    },
  );
});

test("integrations: a secret that did not resolve is not a secret", async () => {
  // The failure every other surface hides. The config names a ${VAR}, so
  // secret_present is true and the vendor's settings page shows a healthy
  // hook — while the route answers 503 to every delivery because nothing
  // answered the variable. Rendering that as "configured" is the whole bug.
  await withView(
    createIntegrationsView,
    {
      integrations: {
        traffic_known: true,
        traffic_since: null,
        integrations: [
          {
            key: "gitlab",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/gitlab",
            secret_present: true,
            secret_usable: false,
            routes: true,
            seats: ["swe"],
            inbound: 0,
            last_at: null,
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /did not resolve to a usable key/);
      assert.match(html, /turned away with a 503/);
    },
  );
});

test("integrations: a resolved secret says so, and an unknown one does not claim to", async () => {
  await withView(
    createIntegrationsView,
    {
      integrations: {
        traffic_known: true,
        traffic_since: null,
        integrations: [
          {
            key: "gitlab",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/gitlab",
            secret_present: true,
            secret_usable: true,
            routes: true,
            seats: ["swe"],
            inbound: 0,
            last_at: null,
          },
          {
            key: "jira",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/jira",
            secret_present: true,
            secret_usable: null,
            routes: true,
            seats: [],
            inbound: 0,
            last_at: null,
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /configured and resolved/);
      assert.match(html, /not visible from this process/);
      assert.doesNotMatch(html, /did not resolve to a usable key/);
    },
  );
});

test("integrations: a surface with no shared secret is not missing one", async () => {
  await withView(
    createIntegrationsView,
    {
      integrations: {
        traffic_known: true,
        window_hours: 24,
        integrations: [
          {
            key: "mattermost",
            configured: true,
            enabled: true,
            inbound_kind: "websocket",
            inbound_path: "",
            secret_present: null,
            seats: ["swe"],
            inbound: 3,
            routed: 3,
            skipped: 0,
            coalesced: 0,
            last_at: "2026-08-21T12:00:00Z",
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /the seat's own token/);
      assert.doesNotMatch(html, /missing/);
      assert.match(html, /no public URL needed/);
    },
  );
});

// A branded surface is one an operator recognises without reading. The
// label alone cannot prove that: integrationMeta title-cases an unknown
// key, so a missing entry still renders the word "Atlassian" — with the
// generic inbox glyph and the fallback pink, sitting between the Jira and
// Confluence cards it is the parent of. The icon and the colour are the
// half of the badge that goes wrong silently, so they are what this reads.
test("integrations: the Atlassian organization is branded, not title-cased", async () => {
  await withView(
    createIntegrationsView,
    {
      integrations: {
        traffic_known: true,
        traffic_since: null,
        integrations: [
          {
            key: "atlassian",
            configured: true,
            enabled: true,
            // No route of its own: Cloud arrives through the Forge app
            // and Data Center on the jira and confluence routes, each of
            // which is its own card.
            inbound_kind: "webhook",
            inbound_path: "",
            org_id: "${ATLASSIAN_ORG_ID}",
            url: "https://acme.example.com",
            secret_present: null,
            secret_usable: null,
            routes: null,
            seats: ["swe"],
            inbound: 0,
            last_at: null,
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /#i-building/);
      assert.match(html, /--int-color:var\(--accent-ink\)/);
      assert.match(html, />\s*Atlassian\s*</);
      // The row exists to answer "which agents get an account", so the
      // seat has to reach the chip that navigates to it.
      assert.match(html, /data-seat="swe"/);
      // No credential of its own: the organization key is the operator's,
      // arrives from the environment for one run and is never stored.
      assert.match(html, /the seat's own token/);
      assert.doesNotMatch(html, /missing/);
    },
  );
});

test("integrations: counts that cannot be taken are not shown as zero", async () => {
  await withView(
    createIntegrationsView,
    {
      integrations: {
        traffic_known: false,
        window_hours: 24,
        integrations: [
          {
            key: "slack",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/slack/{handle}",
            secret_present: true,
            seats: [],
            inbound: 0,
            routed: 0,
            skipped: 0,
            coalesced: 0,
            last_at: "",
          },
        ],
      },
    },
    ({ view }) => {
      assert.match(view.render(state()), /no event store — traffic cannot be counted/);
    },
  );
});

// ---------------------------------------------------------------------
// Mission Control
// ---------------------------------------------------------------------

test("mission: the attention queue leads the page", async () => {
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(
    state({
      agents: [{ role: "Analyst", handle: "analyst", state: "afk", afk_reason: "stall" }],
    }),
  );
  assert.match(html, /Needs you/);
  // Before anything else on the page: the first question is what needs a
  // person, not whether the engine is well.
  assert.ok(
    html.indexOf("Needs you") < html.indexOf("Engine"),
    "the engine band was drawn above the attention queue",
  );
});

test("mission: the oldest waiting item is dated, so somebody can tell nobody looked", () => {
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(
    state({
      sandboxes: [
        {
          turn_id: "t1",
          role: "Engineer",
          status: "awaiting_input",
          question: "Which backoff?",
          updated_at: "2026-08-20T08:00:00Z",
          started_at: "2026-08-20T08:00:00Z",
        },
      ],
    }),
  );
  assert.match(html, /Oldest has been waiting/);
});

test("mission: a quiet company still gets the panel", () => {
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(state());
  assert.match(html, /Nothing is waiting on you/);
  // The sentence is not the panel. An earlier rewrite of this room kept
  // the words and dropped the `<section class="panel">` around them, and
  // every assertion here still passed while the attention rows — which
  // carry separators and no surface of their own — sat directly on the
  // page ground.
  assert.match(html, /class="panel att-panel"/);
});

test("mission: the attention rows are inside the panel, not beside it", () => {
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(
    state({
      agents: [
        { role: "Analyst", handle: "analyst", state: "afk", afk_reason: "stall" },
      ],
    }),
  );
  const open_ = html.indexOf('class="panel att-panel"');
  const row = html.indexOf('class="att-row');
  const close = html.indexOf("</section>", open_);
  assert.ok(open_ !== -1 && row !== -1, "no attention panel, or no rows in it");
  assert.ok(open_ < row && row < close, "an attention row escaped the panel");
});

test("mission: a seat doing detached code work counts as working", () => {
  // The seat is freed the moment a run detaches, so a count keyed on
  // live turns alone showed a company doing nothing while its most
  // expensive work was under way. The board that used to carry this
  // belongs to Work now; the invariant survives in the engine band's
  // seats figure, which goes through `effectiveAgentState`.
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(
    state({
      org: { name: "N", roles: [{ name: "Engineer", kind: "agent" }], units: [] },
      agents: [{ role: "Engineer", handle: "eng", state: "idle" }],
      sandboxes: [
        {
          turn_id: "t1",
          role: "Engineer",
          status: "running",
          coding_agent: "opencode",
          task: "port the retry queue",
        },
      ],
    }),
  );
  assert.match(html, /Seats working/);
  assert.match(html, />1 <span class="ms-on">of 1</, "the detached seat was counted idle");
});

test("mission: a disconnected page reports no engine numbers at all", () => {
  // `store.setConnected(false)` wipes only the health slice; agents,
  // events, tokens and budget all stay frozen. The page this replaced
  // printed every one of them at full confidence.
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(
    state({
      connected: false,
      agents: [{ role: "CEO", handle: "ceo", state: "working" }],
      health: {},
    }),
  );
  assert.ok(!/Seats working<\/span>\s*<span class="ms-clause-value">\d/.test(html),
    "a seat count was drawn on a page that cannot see the engine");
  assert.match(html, /not a quiet hour/i);
});

test("mission: every cost tile carries its footnote", () => {
  // The strip's key is `foot`. A rewrite of this band passed `sub`, which
  // the renderer simply ignored — so every figure lost the line that says
  // what span it covers, and nothing failed. A tile whose value is a bare
  // number with no span under it is the kind of figure this dashboard
  // exists not to print.
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(
    state({
      tokens: {
        totals: { total_tokens: 4200, input_tokens: 3000, output_tokens: 1200, calls: 9 },
      },
      budget: { org: { used: 900, max: 1000 } },
    }),
  );
  assert.match(html, /3\.0k in · 1\.2k out · 9 calls/);
  assert.match(html, /900 of 1\.0k/);
});

test("mission: a seat name in a cost tile is escaped", () => {
  // `foot` is inserted as markup so callers can put a badge in it, which
  // makes every caller responsible for its own data. Seat names come from
  // company config.
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(
    state({
      agents: [
        {
          role: "<img src=x onerror=1>",
          handle: "x",
          state: "idle",
          budget: { used: 10, max: 10, refused_at: "2026-08-20T08:00:00Z" },
        },
      ],
    }),
  );
  assert.ok(!html.includes("<img src=x"), "a seat name reached the DOM as markup");
  assert.match(html, /&lt;img src=x/);
});

test("mission: a seat whose teardown was never proven is surfaced", () => {
  // runtime.py calls unproven_seconds "the one to alert on" and it
  // reached no screen in the product.
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(
    state({ health: { status: "ok", configured: true, engine: true,
                      seats: { unproven: ["ceo"], unproven_seconds: { ceo: 612.4 } } } }),
  );
  assert.match(html, /Stuck/);
  assert.match(html, /teardown never confirmed/);
});

test("mission: a teardown that just failed once is not called stuck", () => {
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(
    state({ health: { status: "ok", configured: true, engine: true,
                      seats: { unproven: ["ceo"], unproven_seconds: { ceo: 4 } } } }),
  );
  assert.ok(!/teardown never confirmed/.test(html), "a 4-second retry was called stranded");
});


test("integrations: an ingest-only surface is not a working one", async () => {
  // Verifying and storing a delivery is the first half; a parser turning it
  // into a notification is the second, and a vendor can have one without the
  // other. Everywhere else those look identical — configured, secret
  // present, deliveries arriving — so this row is the only place an operator
  // can see a surface ingesting into a void.
  await withView(
    createIntegrationsView,
    {
      integrations: {
        traffic_known: true,
        traffic_since: "2026-08-20T09:00:00Z",
        integrations: [
          {
            key: "github",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/github",
            secret_present: true,
            routes: false,
            seats: [],
            inbound: 42,
            last_at: "2026-08-24T09:00:00Z",
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /ingest only/);
      assert.match(html, /wake nobody/);
      // The traffic line must still read as traffic: 42 deliveries arrived,
      // and the finding is what happened to them, not that nothing came.
      assert.match(html, /42 inbound/);
    },
  );
});

test("integrations: an API with no engine says so rather than 'no'", async () => {
  // routes === null is "this process cannot see an engine", which is the
  // honest answer from a standalone API and must never render as a finding.
  await withView(
    createIntegrationsView,
    {
      integrations: {
        traffic_known: true,
        traffic_since: null,
        integrations: [
          {
            key: "gitlab",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/gitlab",
            secret_present: true,
            routes: null,
            seats: [],
            inbound: 0,
            last_at: null,
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /not visible from this process/);
      assert.doesNotMatch(html, /ingest only/);
    },
  );
});

await run();

// "128 inbound" alone cannot tell a working integration from one whose every
// delivery reaches nobody. The two outcome counts are what separate them.
test("integrations: what became of the deliveries is shown beside how many arrived", async () => {
  await withView(
    createIntegrationsView,
    {
      integrations: {
        traffic_known: true,
        traffic_since: "2026-06-01T00:00:00Z",
        integrations: [
          {
            key: "gitlab",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/gitlab",
            secret_present: true,
            routes: true,
            seats: ["swe"],
            inbound: 128,
            skipped: 30,
            coalesced: 2,
            last_at: "2026-06-08T07:31:10Z",
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /128 inbound/);
      assert.match(html, /30 dropped/);
      assert.match(html, /2 merged/);
    },
  );
});

// NULL IS NOT ZERO. A node that could not read its event log must not be
// rendered as one where every delivery woke a seat — and a healthy
// integration with nothing dropped shows no outcome clause at all, because a
// "0 dropped" on every row is noise that trains a reader to skip the line.
test("integrations: outcome counts that are null or zero render nothing", async () => {
  await withView(
    createIntegrationsView,
    {
      integrations: {
        traffic_known: true,
        traffic_since: "2026-06-01T00:00:00Z",
        integrations: [
          {
            key: "gitlab",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/gitlab",
            secret_present: true,
            routes: true,
            seats: ["swe"],
            inbound: 128,
            skipped: null,
            coalesced: 0,
            last_at: "2026-06-08T07:31:10Z",
          },
        ],
      },
    },
    ({ view }) => {
      const html = view.render(state());
      assert.match(html, /128 inbound/);
      assert.doesNotMatch(html, /dropped/);
      assert.doesNotMatch(html, /merged/);
    },
  );
});
