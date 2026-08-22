// The Activity room: merging the live feed with stored history, and the
// three ways its filters used to lie.
//
// Merging is where this view can go quietly wrong — a row that appears
// twice, a row that jumps as a page lands, or, worst because nothing
// reports it, a hole between the evicted tail of the live window and the
// first row a page returned. Those are the first half of this suite.
//
// The second half is what the filters may claim:
//
//   * the pill counts describe the RETAINED LIVE FEED and say so, rather
//     than being one number above a list that answers a different
//     question;
//   * a multi-value filter is decomposed into one server read per value
//     and merged, and rows are held back until every branch of the read
//     has reached them — the old view pushed a filter into the query
//     only when exactly ONE value was selected and client-filtered a
//     server page otherwise, so paging under two categories silently
//     omitted matching rows;
//   * every filter lives in the URL, so a filtered screen survives a
//     reload and can be handed to somebody as a link.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";

installDom();
// `replaceParams` writes the filter state through the history API. A
// fragment-only replaceState moves `location.hash` and fires no
// hashchange, which is the whole reason the view can use it: the shell
// would otherwise remount the view and lose everything it holds.
globalThis.location.hash = "#/activity";
globalThis.history = {
  replaceState: (_state, _title, url) => {
    globalThis.location.hash = String(url);
  },
};

const base = new URL("../../../src/crewlet/static/dashboard/js/", import.meta.url);
const { createActivityView } = await import(new URL("views/activity.js", base));
const { newestFirst, oldestFirst, tsKey } = await import(new URL("format.js", base));
const { EVENT_CATEGORIES } = await import(new URL("state.js", base));
const { patch } = await import(new URL("patch.js", base));

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

// A full page — anything shorter is the end of that branch's history.
function fullPage(prefix, from, extra = {}) {
  return Array.from({ length: 100 }, (_, i) => ev(`${prefix}${i}`, from + i, extra));
}

/**
 * A view harness: a fake store, a scripted `query`, and the markup after
 * each refresh.
 *
 * `answer` is a function of the query params, not a queue, because the
 * whole point of a multi-value filter is that one click issues SEVERAL
 * reads and the reply depends on which branch asked.
 */
function makeView({ live = [], params = {}, answer = () => [] } = {}) {
  const calls = [];
  const navigated = [];
  const state = { events: live, connected: true, health: { status: "ok" } };
  const store = { state, subscribe: () => () => {} };
  let markup = "";
  const view = createActivityView({
    store,
    params,
    navigate: (hash) => navigated.push(hash),
    refresh: () => {
      markup = view.render(state);
    },
    query: async (what, requested) => {
      calls.push({ what, params: requested });
      return answer(requested);
    },
  });
  view.mount();
  markup = view.render(state);
  return {
    view,
    calls,
    state,
    navigated,
    markup: () => markup,
    act(action, dataset = {}) {
      view.onAction(action, { dataset });
      return markup;
    },
    async loadOlder() {
      view.onAction("load-older", { dataset: {} });
      // One macrotask drains every pending microtask, including the
      // `allSettled` fan-out — a fixed number of `await Promise.resolve()`
      // hops does not, and reports a half-finished load as the answer.
      await new Promise((resolve) => setTimeout(resolve, 0));
      return markup;
    },
  };
}

// The ids the feed rendered, in order.
function renderedIds(markup) {
  return [...markup.matchAll(/data-k="t:(?:_solo_)?([^"]+)"/g)].map((m) => m[1]);
}

// ---- ordering -------------------------------------------------------

test("the ordering key reads naive and aware timestamps as one instant", () => {
  // The API emits both interchangeably. Compared as raw strings the same
  // moment sorts differently depending on who wrote it.
  assert.strictEqual(tsKey("2026-04-01T12:00:00+00:00"), tsKey("2026-04-01T12:00:00"));
});

test("rows sharing a timestamp are broken by id, both ways", () => {
  const a = ev("aaa", 10);
  const b = ev("bbb", 10);
  assert.ok(newestFirst(a, b) > 0, "newest-first did not break the tie");
  assert.ok(oldestFirst(a, b) < 0, "oldest-first did not break the tie");
  assert.strictEqual(newestFirst(a, b), -oldestFirst(a, b));
});

// ---- merging --------------------------------------------------------

test("history is appended beneath the live feed", async () => {
  const h = makeView({
    live: [ev("live1", 1), ev("live2", 2)],
    answer: () => [ev("old1", 100), ev("old2", 200)],
  });
  assert.deepStrictEqual(renderedIds(await h.loadOlder()), [
    "live1",
    "live2",
    "old1",
    "old2",
  ]);
});

test("the cursor is the oldest row of the MERGED set", async () => {
  // Not of the fetched half alone: the live ring evicts as it grows, so
  // a row can leave the live window while still held here, and deriving
  // the cursor from one half silently opens a hole no error would
  // report.
  const pages = [fullPage("o", 100), [ev("older", 500)]];
  const h = makeView({ live: [ev("live1", 1)], answer: () => pages.shift() || [] });
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
  const h = makeView({
    live: [ev("dup", 50)],
    answer: () => [ev("dup", 50), ev("old", 100)],
  });
  assert.deepStrictEqual(renderedIds(await h.loadOlder()), ["dup", "old"]);
});

test("a page landing twice cannot duplicate its rows", async () => {
  const page = [ev("old1", 100), ev("old2", 200)];
  const h = makeView({ live: [ev("live", 1)], answer: () => page.slice() });
  await h.loadOlder();
  assert.deepStrictEqual(renderedIds(await h.loadOlder()), ["live", "old1", "old2"]);
});

test("a row never jumps: position depends only on its own key", async () => {
  const h = makeView({
    live: [ev("newest", 1), ev("middle", 50)],
    answer: () => [ev("oldest", 900), ev("between", 60)],
  });
  assert.deepStrictEqual(renderedIds(await h.loadOlder()), [
    "newest",
    "middle",
    "between",
    "oldest",
  ]);
});

// ---- the end of history ---------------------------------------------

test("a short page ends the paging and says so", async () => {
  const h = makeView({ live: [ev("live", 1)], answer: () => [ev("old", 100)] });
  const markup = await h.loadOlder();
  assert.ok(markup.includes("beginning of the retained history"));
  assert.ok(!markup.includes("Load 100 more"));
});

test("a full page leaves the control available", async () => {
  const h = makeView({ live: [ev("live", 1)], answer: () => fullPage("o", 100) });
  assert.ok((await h.loadOlder()).includes("Load 100 more from history"));
});

test("no event store is said plainly, not drawn as an empty history", async () => {
  // An empty page and "there is no store" look identical to a pager, and
  // announcing "you have reached the beginning of history" on a
  // deployment that simply cannot answer silently excludes every event
  // there is.
  const h = makeView({
    live: [ev("live", 1)],
    answer: () => {
      throw new Error("no_event_store");
    },
  });
  const markup = await h.loadOlder();
  assert.ok(markup.includes("no event store is configured"));
  assert.ok(!markup.includes("beginning of the retained history"));
});

test("an empty filtered list still offers the store", async () => {
  // A seat quiet for longer than the live ring holds is exactly the
  // reader who needs to look further back, and the old view drew the
  // pager only when it already had rows — so arriving from that seat's
  // page gave an empty screen with no way to page.
  const h = makeView({ live: [ev("live", 1)], params: { actor: "PM" } });
  assert.ok(h.markup().includes("Load 100 more from history"));
  assert.ok(h.markup().includes("No events match the selected filters"));
});

// ---- filters reach the server ---------------------------------------

test("a single active filter is pushed into the query, not applied after", async () => {
  // Filtering a paged list client-side silently excludes: a 100-row page
  // holding 2 matches reads as "only 2 exist".
  const h = makeView({ live: [ev("live", 1)] });
  h.act("actor", { actor: "CEO" });
  await h.loadOlder();
  assert.strictEqual(h.calls[0].params.actor, "CEO");
});

test("changing a filter discards the history fetched under the old one", async () => {
  const pages = [[ev("old", 100)], [ev("other", 300)]];
  const h = makeView({ live: [ev("live", 1)], answer: () => pages.shift() || [] });
  await h.loadOlder();
  assert.ok(renderedIds(h.markup()).includes("old"));
  h.act("cat", { cat: "task" });
  await h.loadOlder();
  assert.strictEqual(h.calls[1].params.before, at(1), "the cursor was not reset");
  assert.strictEqual(h.calls[1].params.category, "task");
});

// ---- multi-select fans out ------------------------------------------

test("two categories issue one server read each, not one client-filtered page", async () => {
  const h = makeView({
    live: [ev("live", 1)],
    params: { cat: "task,notification" },
    answer: () => [],
  });
  await h.loadOlder();
  assert.strictEqual(h.calls.length, 2, "a multi-value filter read the store once");
  assert.deepStrictEqual(
    h.calls.map((c) => c.params.category).sort(),
    ["notification", "task"],
    "a branch read the store with no category at all",
  );
});

test("a category and an actor together are one read per pair", async () => {
  const h = makeView({
    live: [ev("live", 1)],
    params: { cat: "task,notification", actor: "CEO" },
  });
  await h.loadOlder();
  assert.strictEqual(h.calls.length, 2);
  for (const call of h.calls) assert.strictEqual(call.params.actor, "CEO");
});

test("selecting every category is not a filter and collapses to one read", async () => {
  const h = makeView({
    live: [ev("live", 1)],
    params: { cat: EVENT_CATEGORIES.map((c) => c.key).join(",") },
  });
  await h.loadOlder();
  assert.strictEqual(h.calls.length, 1);
  assert.strictEqual(h.calls[0].params.category, undefined);
});

test("a fan-out too wide to read honestly refuses, and says why", async () => {
  // 4 categories × 4 seats is sixteen reads of the store for one click.
  // Silently reading a subset is the defect this whole section is about.
  const h = makeView({
    live: [ev("live", 1)],
    params: {
      cat: "task,notification,lifecycle,learning",
      actor: "CEO,PM,CTO,Eng",
    },
  });
  const markup = await h.loadOlder();
  assert.strictEqual(h.calls.length, 0, "it read the store anyway");
  assert.ok(markup.includes("16 separate reads"), markup.slice(0, 400));
});

// ---- the floor ------------------------------------------------------

test("a branch that outran the others cannot show rows the others have not reached", async () => {
  // Task is rare and notifications are constant, so 100 rows of each
  // cover wildly different spans. Rendering the deep branch's rows in
  // the region the shallow one has never read would draw those months as
  // if nothing else had happened in them.
  const deep = fullPage("task", 1000);
  const shallow = fullPage("note", 100, { category: "notification", type: "external_notification" });
  const h = makeView({
    live: [ev("live", 1)],
    params: { cat: "task,notification" },
    answer: (p) => (p.category === "task" ? deep : shallow),
  });
  const markup = await h.loadOlder();
  const ids = renderedIds(markup);
  assert.ok(ids.includes("note0"), "the shallow branch's rows are missing");
  assert.ok(!ids.includes("task0"), "a row below the floor was rendered as if complete");
  assert.ok(markup.includes("100 older rows are loaded and hidden"));
});

test("the next click reads only the branch holding the floor", async () => {
  const deep = fullPage("task", 1000);
  const shallow = fullPage("note", 100, { category: "notification" });
  const h = makeView({
    live: [ev("live", 1)],
    params: { cat: "task,notification" },
    answer: (p) => (p.category === "task" ? deep : shallow),
  });
  await h.loadOlder();
  h.calls.length = 0;
  await h.loadOlder();
  assert.deepStrictEqual(
    h.calls.map((c) => c.params.category),
    ["notification"],
    "it re-read a branch that was already deeper than the floor",
  );
  assert.strictEqual(h.calls[0].params.before, at(199), "the branch resumed at the wrong depth");
});

test("held rows appear as soon as the shallow branch reaches them", async () => {
  const deep = fullPage("task", 1000);
  let notePage = fullPage("note", 100, { category: "notification" });
  const h = makeView({
    live: [ev("live", 1)],
    params: { cat: "task,notification" },
    answer: (p) => (p.category === "task" ? deep : notePage),
  });
  await h.loadOlder();
  assert.ok(!renderedIds(h.markup()).includes("task0"));
  // A short page: the notification branch has no more history, so it
  // stops holding the floor up and the task rows below it are known to
  // be complete.
  notePage = [ev("note-last", 2000, { category: "notification" })];
  const markup = await h.loadOlder();
  const ids = renderedIds(markup);
  assert.ok(ids.includes("task0"), "the held rows never surfaced");
  assert.ok(
    ids.filter((id) => id.startsWith("task")).length > 50,
    "only a handful of the held rows surfaced",
  );
  // The rest is behind the LOCAL pager — rows already in memory — which
  // is a different control from the one that reads the store.
  assert.ok(/Show \d+ older/.test(markup), "the rest is not reachable");
});

test("one branch failing does not discard the pages that landed", async () => {
  const h = makeView({
    live: [ev("live", 1)],
    params: { cat: "task,notification" },
    answer: (p) => {
      if (p.category === "task") throw new Error("timeout");
      return fullPage("note", 100, { category: "notification" });
    },
  });
  const markup = await h.loadOlder();
  assert.ok(markup.includes("Part of that read did not come back"));
  // Nothing below the live tail may be shown — the failed branch has not
  // covered it — but the rows are held, not thrown away.
  assert.deepStrictEqual(renderedIds(markup), ["live"]);
  assert.ok(markup.includes("Load 100 more from history"), "the control was taken away");
});

// ---- what the counts count ------------------------------------------

test("the filter counts name their own scope", () => {
  const h = makeView({ live: [ev("a", 1), ev("b", 2)] });
  assert.ok(
    h.markup().includes("counts: last 2 events"),
    "the counts do not say what they are counting",
  );
});

test("the counts describe the live feed, not the paged history", async () => {
  // They cannot describe the filtered result — that reaches into the
  // event store, whose size no query here can total. Counting the loaded
  // set instead would report a category as huge purely because it is the
  // branch being paged.
  const h = makeView({ live: [ev("a", 1)], answer: () => fullPage("o", 100) });
  await h.loadOlder();
  const markup = h.markup();
  assert.ok(markup.includes("counts: last 1 events"));
  assert.ok(
    /data-cat="task"[\s\S]{0,200}<span class="ct">1<\/span>/.test(markup),
    "a pill counted rows the live feed does not hold",
  );
});

test("failures are counted from the live feed too, and toggle on their own", () => {
  const h = makeView({ live: [ev("a", 1, { failed: true }), ev("b", 2)] });
  assert.ok(h.markup().includes("Failures"));
  const markup = h.act("failed");
  assert.deepStrictEqual(renderedIds(markup), ["a"]);
});

// ---- the URL --------------------------------------------------------

test("the incoming actor seed filters, and shows a pill even at zero matches", () => {
  // Reachable from a URL — the seat page links here rather than
  // rendering a second copy of this list — so the filter can be set
  // before one matching event has loaded. An active filter the reader
  // cannot see is an empty list with no way back out of it.
  const h = makeView({
    live: [ev("e1", 1, { actor: "Agent CEO" }), ev("e2", 2, { actor: "Agent PM" })],
    params: { actor: "Agent CEO" },
  });
  assert.deepStrictEqual(renderedIds(h.markup()), ["e1"]);
  assert.ok(/data-action="actor" data-actor="Agent CEO"/.test(h.markup()));

  const quiet = makeView({ live: [ev("e2", 2, { actor: "Agent PM" })], params: { actor: "Agent CEO" } });
  assert.ok(/data-actor="Agent CEO"/.test(quiet.markup()), "no pill for the active filter");

  // And with no seed, nothing is filtered.
  const open = makeView({
    live: [ev("e1", 1, { actor: "Agent CEO" }), ev("e2", 2, { actor: "Agent PM" })],
  });
  assert.deepStrictEqual(renderedIds(open.markup()), ["e1", "e2"]);
});

test("every filter is restored from the URL", () => {
  const h = makeView({
    live: [
      ev("a", 1, { failed: true }),
      ev("b", 2, { category: "notification" }),
      ev("c", 3, { actor: "PM" }),
    ],
    params: { cat: "task", actor: "CEO", failed: "1", sort: "asc" },
  });
  const markup = h.markup();
  assert.deepStrictEqual(renderedIds(markup), ["a"]);
  assert.ok(markup.includes("Oldest first"), "the sort direction was not restored");
  assert.ok(
    /data-cat="task"[^>]*aria-pressed="true"/.test(markup),
    "the restored category filter is not shown as active",
  );
});

test("toggling a filter writes it to the URL", () => {
  const h = makeView({ live: [ev("a", 1)] });
  h.act("cat", { cat: "task" });
  h.act("actor", { actor: "CEO" });
  h.act("failed");
  h.act("sort");
  const hash = globalThis.location.hash;
  const query = new URLSearchParams(hash.split("?")[1] || "");
  assert.ok(hash.startsWith("#/activity?"), hash);
  assert.strictEqual(query.get("cat"), "task");
  assert.strictEqual(query.get("actor"), "CEO");
  assert.strictEqual(query.get("failed"), "1");
  assert.strictEqual(query.get("sort"), "asc");
});

test("clearing every filter clears the URL with them", () => {
  // "All" means all: it used to leave the actor filter — the one the
  // seat page seeds — in place while lighting up as though the list were
  // unfiltered.
  const h = makeView({
    live: [ev("a", 1)],
    params: { cat: "task", failed: "1", actor: "CEO" },
  });
  const markup = h.act("cat-all");
  const query = new URLSearchParams(globalThis.location.hash.split("?")[1] || "");
  assert.strictEqual(query.get("cat"), null);
  assert.strictEqual(query.get("failed"), null);
  assert.strictEqual(query.get("actor"), null);
  assert.deepStrictEqual(renderedIds(markup), ["a"]);
});

test("a value carrying a comma survives the round trip", () => {
  // The multi-value fields are comma-joined, so the separator has to be
  // escapable — a seat named "Rivera, Marcus" must not become two
  // filters that match nothing.
  const who = "Rivera, Marcus";
  const h = makeView({ live: [ev("a", 1, { actor: who })] });
  h.act("actor", { actor: who });
  const written = new URLSearchParams(globalThis.location.hash.split("?")[1] || "");
  const restored = makeView({
    live: [ev("a", 1, { actor: who }), ev("b", 2, { actor: "Someone Else" })],
    params: { actor: written.get("actor") },
  });
  assert.deepStrictEqual(renderedIds(restored.markup()), ["a"]);
});

// ---- traces ---------------------------------------------------------

test("events sharing a trace collapse into one row and expand on toggle", () => {
  const rows = [
    ev("root", 10, { trace_id: "tr1", span_id: "s1", parent_span_id: "" }),
    ev("kid", 11, { trace_id: "tr1", span_id: "s2", parent_span_id: "s1" }),
  ];
  const h = makeView({ live: rows });
  assert.deepStrictEqual(renderedIds(h.markup()), ["tr1"]);
  assert.ok(h.markup().includes("2 events"));
  assert.ok(!h.markup().includes('data-k="c:kid"'), "children render before expanding");
  assert.ok(h.act("toggle", { key: "tr1" }).includes('data-k="c:kid"'));
});

test("the room patches in place rather than rebuilding itself", () => {
  // Every repeated node is keyed, so a filter toggle re-uses the rows it
  // did not change. Without the keys the patcher matches positionally,
  // and the row a reader had expanded is silently replaced by whichever
  // row happens to land at its index.
  const h = makeView({
    live: [ev("a", 1), ev("b", 2, { category: "notification" })],
  });
  const root = document.createElement("div");
  patch(root, h.markup());
  const listBefore = root.querySelector('[data-k="list"]');
  const rowBefore = root.querySelector('[data-k="t:_solo_a"]');
  assert.ok(listBefore && rowBefore);

  patch(root, h.act("cat", { cat: "task" }));
  assert.strictEqual(root.querySelector('[data-k="list"]'), listBefore, "the list was rebuilt");
  assert.strictEqual(root.querySelector('[data-k="t:_solo_a"]'), rowBefore, "the row was rebuilt");
  assert.strictEqual(root.querySelector('[data-k="t:_solo_b"]'), null, "the filter did not apply");
});

test("a trace links to its own screen; a single event links to its detail", () => {
  const h = makeView({
    live: [
      ev("root", 10, { trace_id: "tr1", span_id: "s1" }),
      ev("kid", 11, { trace_id: "tr1", span_id: "s2", parent_span_id: "s1" }),
      ev("solo", 12, { type: "agent_turn_completed" }),
    ],
  });
  h.act("open-trace", { trace: "tr1" });
  h.act("open", { id: "solo" });
  assert.deepStrictEqual(h.navigated, ["/traces/tr1", "/events/solo"]);
});

await run();
