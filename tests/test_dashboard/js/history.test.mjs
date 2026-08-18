// Paging the Activity feed into stored history.
//
// The live feed is a bounded ring the server keeps; everything older
// lives in the event store. Merging the two is where this can go quietly
// wrong: a row that appears twice, a row that jumps as a page lands, or
// — worst, because nothing reports it — a hole between the evicted tail
// of the live window and the first row a page returned.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";

installDom();
const { createEventsView } = await import(
  new URL("../../../src/crewlet/static/dashboard/js/views/events.js", import.meta.url)
);
const { newestFirst, oldestFirst, tsKey } = await import(
  new URL("../../../src/crewlet/static/dashboard/js/format.js", import.meta.url)
);
const { arrangeSpans } = await import(
  new URL("../../../src/crewlet/static/dashboard/js/traceNodes.js", import.meta.url)
);

const T0 = Date.parse("2026-04-01T12:00:00Z");
const at = (secondsAgo) => new Date(T0 - secondsAgo * 1000).toISOString();

function ev(id, secondsAgo, extra = {}) {
  return {
    id,
    type: "task_created",
    category: "task",
    actor: "CEO",
    summary: id,
    timestamp: at(secondsAgo),
    trace_id: "",
    ...extra,
  };
}

// A view harness: a fake store, a scripted `query`, and the rendered
// markup after each refresh.
function makeView(liveEvents, pages) {
  const calls = [];
  const state = { events: liveEvents, connected: true, health: { status: "ok" } };
  const store = {
    state,
    subscribe: () => () => {},
  };
  let markup = "";
  const view = createEventsView({
    store,
    navigate: () => {},
    refresh: () => {
      markup = view.render(state);
    },
    query: async (what, params) => {
      calls.push({ what, params });
      return pages.shift() || [];
    },
  });
  view.mount();
  markup = view.render(state);
  return {
    view,
    calls,
    state,
    markup: () => markup,
    async loadOlder() {
      view.onAction("load-older", { dataset: {} });
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
      return markup;
    },
  };
}

// The ids the feed rendered, in order.
function renderedIds(markup) {
  return [...markup.matchAll(/data-k="t:(?:_solo_)?([^"]+)"/g)].map((m) => m[1]);
}

// ---- ordering ----

test("the ordering key reads naive and aware timestamps as one instant", () => {
  // The API emits both interchangeably. Compared as raw strings the same
  // moment sorts differently depending on who wrote it.
  const aware = "2026-04-01T12:00:00+00:00";
  const naive = "2026-04-01T12:00:00";
  assert.strictEqual(tsKey(aware), tsKey(naive));
});

test("rows sharing a timestamp are broken by id, both ways", () => {
  const a = ev("aaa", 10);
  const b = ev("bbb", 10);
  assert.ok(newestFirst(a, b) > 0, "newest-first did not break the tie");
  assert.ok(oldestFirst(a, b) < 0, "oldest-first did not break the tie");
  // And the two are exact opposites, so a trace and a feed can never
  // disagree about which of two simultaneous rows came first.
  assert.strictEqual(newestFirst(a, b), -oldestFirst(a, b));
});

// ---- merging ----

test("history is appended beneath the live feed", async () => {
  const h = makeView([ev("live1", 1), ev("live2", 2)], [[ev("old1", 100), ev("old2", 200)]]);
  const markup = await h.loadOlder();
  assert.deepStrictEqual(renderedIds(markup), ["live1", "live2", "old1", "old2"]);
});

test("the cursor is the oldest row of the MERGED set", async () => {
  // Not of `older` alone: the live ring evicts as it grows, so a row can
  // leave the live window while still held here, and deriving the cursor
  // from one half silently opens a hole no error would report.
  //
  // A FULL page, so paging continues to a second trip — a short page is
  // the end of the history and correctly stops.
  const full = Array.from({ length: 100 }, (_, i) => ev(`o${i}`, 100 + i));
  const h = makeView([ev("live1", 1)], [full, [ev("older", 500)]]);
  await h.loadOlder();
  assert.strictEqual(h.calls[0].params.before, at(1), "first page ignored the live tail");
  assert.strictEqual(h.calls[0].params.before_id, "live1");
  await h.loadOlder();
  assert.strictEqual(h.calls[1].params.before, at(199), "cursor was not the oldest held row");
  assert.strictEqual(h.calls[1].params.before_id, "o99");
});

test("a row in both halves appears once", async () => {
  // Queries are re-sent verbatim across a reconnect, so a page can land
  // twice; and the live feed legitimately overlaps the first page.
  const h = makeView([ev("dup", 50)], [[ev("dup", 50), ev("old", 100)]]);
  const markup = await h.loadOlder();
  assert.deepStrictEqual(renderedIds(markup), ["dup", "old"]);
});

test("a page landing twice cannot duplicate its rows", async () => {
  const page = [ev("old1", 100), ev("old2", 200)];
  const h = makeView([ev("live", 1)], [page, page.slice()]);
  await h.loadOlder();
  const markup = await h.loadOlder();
  assert.deepStrictEqual(renderedIds(markup), ["live", "old1", "old2"]);
});

test("a row never jumps: position depends only on its own key", async () => {
  const h = makeView(
    [ev("newest", 1), ev("middle", 50)],
    [[ev("oldest", 900), ev("between", 60)]],
  );
  const markup = await h.loadOlder();
  assert.deepStrictEqual(renderedIds(markup), [
    "newest",
    "middle",
    "between",
    "oldest",
  ]);
});

// ---- the end of history ----

test("a short page ends the paging and says so", async () => {
  const h = makeView([ev("live", 1)], [[ev("old", 100)]]);
  const markup = await h.loadOlder();
  assert.ok(markup.includes("beginning of the retained history"));
  assert.ok(!markup.includes("Load 100 more"));
});

test("a full page leaves the control available", async () => {
  const full = Array.from({ length: 100 }, (_, i) => ev(`o${i}`, 100 + i));
  const h = makeView([ev("live", 1)], [full]);
  const markup = await h.loadOlder();
  assert.ok(markup.includes("Load 100 more from history"));
});

test("no event store is said plainly, not drawn as an empty history", async () => {
  // An empty page and "there is no store" look identical to a pager, and
  // announcing "you have reached the beginning of history" on a
  // deployment that simply cannot answer is a figure that silently
  // excludes every event there is.
  const state = { events: [ev("live", 1)], connected: true, health: {} };
  const store = { state, subscribe: () => () => {} };
  let markup = "";
  const view = createEventsView({
    store,
    navigate: () => {},
    refresh: () => {
      markup = view.render(state);
    },
    query: async () => {
      throw new Error("no_event_store");
    },
  });
  view.mount();
  view.onAction("load-older", { dataset: {} });
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  assert.ok(markup.includes("no event store is configured"));
  assert.ok(!markup.includes("beginning of the retained history"));
});

test("changing a filter discards the history fetched under the old one", async () => {
  const h = makeView([ev("live", 1)], [[ev("old", 100)], [ev("other", 300)]]);
  await h.loadOlder();
  assert.ok(renderedIds(h.markup()).includes("old"));
  h.view.onAction("cat", { dataset: { cat: "task" } });
  // The next page must start from the top again, under the new filter.
  await h.loadOlder();
  assert.strictEqual(h.calls[1].params.before, at(1));
  assert.strictEqual(h.calls[1].params.category, "task");
});

test("a single active filter is pushed into the query, not applied after", async () => {
  // Filtering a paged list client-side silently excludes: a 100-row page
  // holding 2 matches reads as "only 2 exist".
  const h = makeView([ev("live", 1)], [[]]);
  h.view.onAction("actor", { dataset: { actor: "CEO" } });
  await h.loadOlder();
  assert.strictEqual(h.calls[0].params.actor, "CEO");
});

// ---- span arrangement ----

test("a trace nests children under their parent span", () => {
  const rows = [
    { id: "root", span_id: "s1", parent_span_id: "" },
    { id: "child", span_id: "s2", parent_span_id: "s1" },
    { id: "grandchild", span_id: "s3", parent_span_id: "s2" },
  ];
  assert.deepStrictEqual(
    arrangeSpans(rows).map((n) => [n.event.id, n.depth]),
    [
      ["root", 0],
      ["child", 1],
      ["grandchild", 2],
    ],
  );
});

test("an orphan renders at the root rather than vanishing", () => {
  // A trace can be truncated at the cap, or a parent can have been
  // written by a process whose events did not survive.
  const rows = [
    { id: "a", span_id: "s1", parent_span_id: "" },
    { id: "orphan", span_id: "s9", parent_span_id: "missing" },
  ];
  assert.deepStrictEqual(
    arrangeSpans(rows).map((n) => n.event.id),
    ["a", "orphan"],
  );
});

test("sibling order is the order given, which is causal", () => {
  const rows = [
    { id: "root", span_id: "s1", parent_span_id: "" },
    { id: "first", span_id: "s2", parent_span_id: "s1" },
    { id: "second", span_id: "s3", parent_span_id: "s1" },
  ];
  assert.deepStrictEqual(
    arrangeSpans(rows).map((n) => n.event.id),
    ["root", "first", "second"],
  );
});

test("a cycle in the span graph cannot lose a row or hang", () => {
  const rows = [
    { id: "a", span_id: "s1", parent_span_id: "s2" },
    { id: "b", span_id: "s2", parent_span_id: "s1" },
  ];
  const out = arrangeSpans(rows);
  assert.strictEqual(out.length, 2);
});

test("events with no spans at all still render, in order", () => {
  const rows = [
    { id: "a", span_id: "", parent_span_id: "" },
    { id: "b", span_id: "", parent_span_id: "" },
  ];
  assert.deepStrictEqual(
    arrangeSpans(rows).map((n) => [n.event.id, n.depth]),
    [
      ["a", 0],
      ["b", 0],
    ],
  );
});

run();
