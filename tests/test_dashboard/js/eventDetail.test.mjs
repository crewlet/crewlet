// The event-detail screen renders arbitrary event payloads, which is the
// one place on the dashboard where an outsider's text (a Slack message, a
// Jira summary, a webhook body) is laid out field by field. Two properties
// matter here and both are one-line mistakes away:
//
//   * the screen must actually render — it is reached by a click from
//     every LLM turn's Source chip, so a throw here dead-ends that path;
//   * every value must be escaped unless a caller deliberately opts out.
//
// `render(ev)` is pure and the view's dependencies are all injected, so
// both are testable without a browser.

import assert from "node:assert";
import { test, run } from "./harness.mjs";

const { createEventDetailView } = await import(
  new URL(
    "../../../src/crewlet/static/dashboard/js/views/eventDetail.js",
    import.meta.url,
  )
);

const TRACE = "3f8a1c9d4b2e6071";

function makeView(event, { onNavigate } = {}) {
  let markup = "";
  const view = createEventDetailView({
    query: async () => event,
    navigate: onNavigate || (() => {}),
    // `load` calls `refresh()` when the query settles; the shell would
    // re-render, so this stands in for that.
    refresh: () => {
      markup = view.render();
    },
    params: { id: event ? event.id : "missing" },
  });
  view.mount();
  return {
    view,
    // One microtask turn is enough: the injected query resolves immediately.
    async markup() {
      await Promise.resolve();
      await Promise.resolve();
      return markup || view.render();
    },
  };
}

const BASE = {
  id: "ev-1",
  type: "external_notification",
  category: "notification",
  source: "notification_service.slack",
  actor: "founder",
  summary: "Message from founder",
  timestamp: "2026-04-01T12:00:00Z",
  trace_id: TRACE,
  payload: { channel_id: "C123", sender: "founder", body: "ship it" },
};

test("an event with a trace renders instead of throwing", async () => {
  // Regression: the view held its fetched event in a closure variable
  // named `raw`, which shadowed the module-level `raw()` marker that
  // `kv` reads to skip escaping. The single `raw(...)` call — on the
  // Trace row, so only events carrying a trace_id reached it — became a
  // call on the event object, and the whole screen died with
  // "raw is not a function".
  const { markup } = makeView(BASE);
  const html = await markup();
  assert.ok(html.includes("Trace"), "the trace row is missing");
  assert.ok(html.includes(TRACE.slice(0, 8)), "the trace id was not rendered");
});

test("an event with no trace still renders", async () => {
  const { markup } = makeView({ ...BASE, trace_id: "" });
  const html = await markup();
  assert.ok(html.includes("Type"), "the summary block is missing");
});

test("every event category renders", async () => {
  // The screen dispatches on type/category into five different bodies;
  // each is reachable from a Source chip, so none may throw.
  const cases = [
    { type: "agent_phase_completed", category: "system", payload: { phase: "plan" } },
    { type: "a2a_message_sent", category: "a2a", payload: { channel_id: "c1" } },
    { type: "external_notification", category: "notification", payload: {} },
    { type: "webhook:slack", category: "webhook", payload: { event: {} } },
    { type: "task_created", category: "task", payload: { task_id: "T-1" } },
  ];
  for (const c of cases) {
    const { markup } = makeView({ ...BASE, ...c });
    const html = await markup();
    assert.ok(html.length > 0, `${c.type} rendered nothing`);
  }
});

test("payload text an outsider chose is escaped", async () => {
  // These fields come off a webhook body. `kv` escapes by default and a
  // caller opts out explicitly; this pins the default.
  const nasty = '<img src=x onerror="alert(1)">';
  const { markup } = makeView({
    ...BASE,
    summary: nasty,
    source: nasty,
    payload: { body: nasty },
  });
  const html = await markup();
  assert.ok(!html.includes("<img src=x"), "an unescaped payload value reached the markup");
  assert.ok(html.includes("&lt;img"), "the value was dropped rather than escaped");
});

test("a missing event says so rather than rendering an empty shell", async () => {
  const { markup } = makeView(null);
  const html = await markup();
  assert.ok(html.includes("Event not found"), "no empty-state for a missing event");
});

run();
