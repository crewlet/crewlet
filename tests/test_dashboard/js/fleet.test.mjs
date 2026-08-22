// The Fleet view, and the one thing it must never do.
//
// This view exists because `/health` answers about the node that served
// it, so behind a load balancer a refresh tells a different story. It
// reads the lease table instead — and it is the view an operator opens
// when nodes are dying, which is exactly when the API answering it may
// be struggling too.
//
// So the failure that matters here is not a crash. It is the view
// quietly re-rendering a fleet from ten minutes ago under a footer that
// says "Refreshed just now" — the same defect the health surface
// documents at length, in the newest view on the page.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";

installDom();
const base = new URL("../../../src/crewlet/static/dashboard/js/", import.meta.url);
const { createFleetView } = await import(new URL("views/fleet.js", base));

function fleetPayload() {
  return {
    nodes: [
      {
        id: "node-a",
        roles: ["ingress", "seats"],
        labels: { zone: "eu" },
        seats: 2,
        expires_in: 40,
        config_epoch: 7,
        config_status: "ok",
        config_error: "",
      },
    ],
    seats: [{ handle: "ceo", node: "node-a", epoch: 3, expires_in: 40 }],
    duties: [{ duty: "maintenance", node: "node-a", expires_in: 40 }],
    unplaceable: [],
    unmanned_roles: [],
    this_node: "node-a",
  };
}

// A root element the view can render into. `isConnected` is what the
// view checks before painting after an await.
function makeRoot() {
  const el = document.createElement("div");
  el.isConnected = true;
  return el;
}

/**
 * Mount a view whose `fleet` query answers come from `answers`, run
 * `body`, and ALWAYS destroy it.
 *
 * The `finally` is not tidiness. `mount` starts a 15 s poll, and node
 * will not exit while an interval is pending — so a test that threw
 * before its `destroy()` left the whole suite hanging until the pytest
 * wrapper's timeout killed it, reporting a hang instead of the
 * assertion that actually failed.
 *
 * `answers` holds payloads; a `null` entry stands for a query that
 * REJECTS, which is what the socket channel does on every failure —
 * a dead socket, an unreadable lease store, a timeout.
 */
async function withView(answers, body) {
  const el = makeRoot();
  // The view destructures `query` once, so the swap a test performs has
  // to go through a stable function rather than replacing the field.
  let impl = async () => {
    const next = answers.length ? answers.shift() : null;
    if (next === null) throw new Error("fleet_unavailable");
    return next;
  };
  const ctx = {
    query: (...args) => impl(...args),
    // The shell re-renders a view when it asks; here it paints straight
    // into the element so a test can read what a reader would see.
    refresh: () => {
      el.innerHTML = view.render({});
    },
    setQuery: (fn) => {
      impl = fn;
    },
  };
  const view = createFleetView(ctx);
  view.mount(el);
  el.innerHTML = view.render({});
  // Let the initial load() settle.
  await new Promise((resolve) => setTimeout(resolve, 0));
  try {
    await body({ el, view, ctx });
  } finally {
    view.destroy();
  }
}

test("the view returns markup for the shell to patch", async () => {
  // It used to paint with `innerHTML` and return no `render` at all, so
  // the shell's own `patch(root, active.render(state))` threw on mount —
  // in every build, on the one view an operator opens when nodes are
  // dying.
  await withView([fleetPayload()], ({ view }) => {
    assert.strictEqual(typeof view.render, "function");
    assert.match(view.render({}), /node-a/);
  });
});

test("a lagging node is measured against what the fleet is converging on", async () => {
  // An epoch on its own is a number with nothing to compare it to.
  const payload = fleetPayload();
  payload.target_epoch = 10;
  payload.nodes[0].config_epoch = 7;
  await withView([payload], ({ el }) => {
    assert.match(el.innerHTML, /epoch 7/);
    assert.match(el.innerHTML, /3 behind/);
  });
});

test("a node that stopped reporting is not read as merely behind", async () => {
  const payload = fleetPayload();
  payload.nodes[0].config_reported_at = new Date(Date.now() - 600_000).toISOString();
  await withView([payload], ({ el }) => {
    assert.match(el.innerHTML, /stopped reporting/);
  });
});

test("a successful read renders the fleet and says it is fresh", async () => {
  await withView([fleetPayload()], ({ el }) => {
    assert.match(el.innerHTML, /node-a/);
    assert.match(el.innerHTML, /Refreshed/);
    assert.doesNotMatch(el.innerHTML, /Could not refresh/);
  });
});

test("a failed refresh keeps the fleet but stops claiming it is current", async () => {
  // First read succeeds, second fails — the ordinary shape of an API
  // that goes away while the page is open.
  await withView([fleetPayload()], async ({ el, view, ctx }) => {
    assert.match(el.innerHTML, /Refreshed/);

    ctx.setQuery(async () => {
      throw new Error("fleet_unavailable");
    });
    await view.__loadForTest();

    assert.match(el.innerHTML, /node-a/, "the last fleet was thrown away");
    assert.match(
      el.innerHTML,
      /Could not refresh/,
      "a stale fleet was still presented as current",
    );
    assert.match(el.innerHTML, /This view is not live/);
  });
});

test("a failed FIRST read says so instead of spinning forever", async () => {
  // No prior data to fall back on. A skeleton here means "loading", and
  // nothing is loading — it is the most patient lie a page can tell.
  await withView([null], ({ el }) => {
    assert.match(el.innerHTML, /Could not read the fleet/);
  });
});

test("a recovered read drops the warning again", async () => {
  await withView([null], async ({ el, view, ctx }) => {
    assert.match(el.innerHTML, /Could not read the fleet/);

    ctx.setQuery(async () => fleetPayload());
    await view.__loadForTest();

    assert.match(el.innerHTML, /node-a/);
    assert.doesNotMatch(el.innerHTML, /Could not refresh/);
  });
});

test("destroy stops the poll", async () => {
  let calls = 0;
  let after = null;
  await withView([fleetPayload()], ({ view, ctx }) => {
    ctx.setQuery(async () => {
      calls++;
      return fleetPayload();
    });
    view.destroy();
    after = new Promise((resolve) => setTimeout(resolve, 20));
  });
  await after;
  assert.equal(calls, 0, "the interval survived destroy()");
});

await run();
