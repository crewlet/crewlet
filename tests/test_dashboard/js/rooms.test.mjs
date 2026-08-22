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

installDom();
const base = new URL("../../../src/crewlet/static/dashboard/js/", import.meta.url);
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
        window_hours: 24,
        integrations: [
          {
            key: "gitlab",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/gitlab",
            secret_present: true,
            seats: ["swe"],
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
      const html = view.render(state());
      assert.match(html, /no traffic seen in 24h/);
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
        window_hours: 24,
        integrations: [
          {
            key: "github",
            configured: true,
            enabled: true,
            inbound_kind: "webhook",
            inbound_path: "/webhooks/github",
            secret_present: false,
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
      assert.match(view.render(state()), /missing — every delivery is being turned away/);
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
  // Before the pulse: the first question is what needs a person, not
  // whether anything is happening.
  assert.ok(
    html.indexOf("Needs you") < html.indexOf("Company pulse"),
    "the pulse was drawn above the attention queue",
  );
});

test("mission: a quiet company still gets the panel", () => {
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(state());
  assert.match(html, /Nothing is waiting on you/);
});

test("mission: a detached sandbox run is in flight, not idle", () => {
  // The seat is freed the moment a run detaches, so a board keyed on
  // live turns alone showed a company doing nothing while its most
  // expensive work was under way.
  const view = createMissionView({ store: { state: state() } });
  const html = view.render(
    state({
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
  assert.match(html, /In flight/);
  assert.match(html, /opencode/);
});

await run();
