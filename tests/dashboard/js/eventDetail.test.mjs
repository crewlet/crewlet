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
import { JS_URL } from "./dashboardRoot.mjs";

const { createEventDetailView } = await import(
  new URL("views/eventDetail.js", JS_URL)
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

// ---------------------------------------------------------------------
// Per-source webhook layouts.
//
// GitLab is the most heavily routed source in the project's own example
// company, and it used to fall through to a raw JSON dump. Each case below
// pins the fields an operator reads to answer
// "what happened, who did it, and why did it wake anyone" — the ones the
// engine's own routers key on.
// ---------------------------------------------------------------------

function webhook(source, payload) {
  return {
    ...BASE,
    type: "webhook:test",
    category: "webhook",
    source,
    payload,
  };
}

// The raw-payload block dumps every field of the event, so asserting
// that some value appears *somewhere* in the markup proves nothing — it
// passed just as well when these sources fell through to the dump. Read
// the value out of its own `<dt>/<dd>` pair instead; `null` means the
// row was never rendered.
function row(html, label) {
  const at = html.indexOf(`<dt>${label}</dt>`);
  if (at < 0) return null;
  const dd = html.slice(at).match(/<dd>([\s\S]*?)<\/dd>/);
  return dd ? dd[1] : null;
}

// The text of a labelled `<pre class="code">` block (Comment, Description,
// Commits), or `null` when the branch rendered none.
function block(html, label) {
  const at = html.indexOf(`<div class="block-label">${label}</div>`);
  if (at < 0) return null;
  const pre = html.slice(at).match(/<pre class="code"[^>]*>([\s\S]*?)<\/pre>/);
  return pre ? pre[1] : null;
}

test("a gitlab merge-request update shows the object, actor and the assignee diff", async () => {
  const { markup } = makeView(
    webhook("gitlab", {
      object_kind: "merge_request",
      user: { username: "kai" },
      project: { path_with_namespace: "nimbus/platform" },
      object_attributes: {
        action: "update",
        iid: 42,
        title: "Cache the project map",
        state: "opened",
        source_branch: "feat/cache",
        target_branch: "main",
        description: "Warms the id→identifier cache at boot.",
        url: "https://gitlab.example.com/nimbus/platform/-/merge_requests/42",
      },
      changes: {
        reviewers: { previous: [], current: [{ username: "ada" }] },
      },
    }),
  );
  const html = await markup();
  assert.strictEqual(row(html, "Event"), "merge_request.update");
  assert.strictEqual(row(html, "Actor"), "kai");
  assert.strictEqual(row(html, "Project"), "nimbus/platform");
  assert.strictEqual(row(html, "Merge request"), "!42 Cache the project map");
  assert.strictEqual(row(html, "State"), "opened");
  assert.strictEqual(row(html, "Branches"), "feat/cache → main");
  // The router acts on who was ADDED, so the transition has to be shown
  // rather than only the resulting reviewer list.
  assert.strictEqual(row(html, "Reviewers"), "— → ada");
  assert.strictEqual(row(html, "Changed"), "reviewers");
  assert.ok(
    row(html, "Link").includes(
      'href="https://gitlab.example.com/nimbus/platform/-/merge_requests/42"',
    ),
    "the merge request link is missing",
  );
  assert.strictEqual(
    block(html, "Description"),
    "Warms the id→identifier cache at boot.",
  );
  assert.ok(html.includes("Raw payload"), "the raw payload block was dropped");
});

test("a gitlab failed pipeline names the status and the broken jobs", async () => {
  const { markup } = makeView(
    webhook("gitlab", {
      object_kind: "pipeline",
      user: { username: "kai" },
      project: { path_with_namespace: "nimbus/platform" },
      object_attributes: { id: 991, ref: "main", status: "failed", duration: 63 },
      merge_request: { iid: 42, title: "Cache the project map" },
      builds: [
        { stage: "test", name: "pytest", status: "failed" },
        { stage: "build", name: "wheel", status: "success" },
      ],
    }),
  );
  const html = await markup();
  assert.strictEqual(row(html, "Pipeline"), "#991 on main");
  assert.strictEqual(
    row(html, "Status"),
    '<span class="badge failed">failed</span>',
    "a failed pipeline reads like any other status",
  );
  assert.strictEqual(row(html, "Failed jobs"), "test/pytest");
  assert.strictEqual(row(html, "Duration"), "63s");
  // The MR the pipeline ran for is a sibling key on this hook, not
  // `object_attributes`.
  assert.strictEqual(row(html, "Merge request"), "!42 Cache the project map");
});

test("a gitlab push shows the branch and its commits", async () => {
  const { markup } = makeView(
    webhook("gitlab", {
      object_kind: "push",
      // A push hook has no `user` object — only the flattened username.
      user_username: "ada",
      ref: "refs/heads/main",
      total_commits_count: 2,
      project: { path_with_namespace: "nimbus/platform" },
      commits: [
        { id: "abcdef1234567890", title: "fix(cache): warm the project map" },
        { id: "1234567890abcdef", message: "test(cache): cover the miss\n\nbody" },
      ],
    }),
  );
  const html = await markup();
  assert.strictEqual(row(html, "Actor"), "ada", "the flattened push actor was not read");
  assert.strictEqual(row(html, "Branch"), "main");
  assert.strictEqual(row(html, "Commits"), "2");
  assert.strictEqual(
    block(html, "Commits"),
    "abcdef12 fix(cache): warm the project map\n12345678 test(cache): cover the miss",
    "a commit body was rendered instead of its first line",
  );
});

test("a gitlab note renders the comment and the item it hangs off", async () => {
  const { markup } = makeView(
    webhook("gitlab", {
      object_kind: "note",
      user: { username: "founder" },
      project: { path_with_namespace: "nimbus/platform" },
      object_attributes: {
        note: "@ada can you take this?",
        noteable_type: "MergeRequest",
        url: "https://gitlab.example.com/nimbus/platform/-/merge_requests/42#note_7",
      },
      merge_request: { iid: 42, title: "Cache the project map" },
    }),
  );
  const html = await markup();
  assert.strictEqual(row(html, "Event"), "note");
  assert.strictEqual(row(html, "Merge request"), "!42 Cache the project map");
  assert.strictEqual(block(html, "Comment"), "@ada can you take this?");
});

test("webhook text an outsider chose is escaped in the new branches", async () => {
  const nasty = '<img src=x onerror="alert(1)">';
  for (const ev of [
    webhook("gitlab", {
      object_kind: "issue",
      user: { username: nasty },
      object_attributes: { iid: 1, title: nasty, description: nasty, url: nasty },
    }),
  ]) {
    const { markup } = makeView(ev);
    const html = await markup();
    assert.ok(!html.includes("<img src=x"), "an unescaped payload value reached the markup");
    assert.ok(html.includes("&lt;img"), "the value was dropped rather than escaped");
  }
});

test("a payload url that is not http(s) never becomes a link", async () => {
  // GitLab's `object_attributes.url` lands in an `href`. A payload that
  // is not http(s) — `javascript:` above all — must render as text.
  const { markup } = makeView(
    webhook("gitlab", {
      object_kind: "issue",
      user: { username: "kai" },
      object_attributes: { iid: 1, title: "t", url: "javascript:alert(1)" },
    }),
  );
  const html = await markup();
  assert.ok(!html.includes('href="javascript:'), "a javascript: url became an href");
  assert.strictEqual(
    row(html, "Link"),
    "javascript:alert(1)",
    "the url was dropped rather than shown as text",
  );
});

test("a notification says how it was routed", async () => {
  // `routed_via` is the transport's answer to "why did this wake THIS
  // agent"; it is promoted out of the metadata chip dump, so it must
  // appear exactly once.
  const { markup } = makeView({
    ...BASE,
    source: "notification_service.jira",
    payload: {
      notification_source: "jira",
      sender: "kai",
      subject: "[ENG-42] Warm the project cache",
      metadata: { routed_via: "assignee_added", project: "ENG" },
    },
  });
  const html = await markup();
  assert.strictEqual(row(html, "Routed via"), "assignee_added");
  assert.strictEqual(
    html.split("assignee_added").length - 1,
    1,
    "the routing reason is rendered twice",
  );
});

test("a missing event says so rather than rendering an empty shell", async () => {
  const { markup } = makeView(null);
  const html = await markup();
  assert.ok(html.includes("Event not found"), "no empty-state for a missing event");
});

run();
