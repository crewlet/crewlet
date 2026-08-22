// Activity: the company's event log — trace-grouped, over the live feed
// AND the stored history beneath it.
//
// The live feed is a bounded ring (store.js MAX_EVENTS) which a busy org
// fills in minutes. Everything older lives in the event store, and the
// `events` query reads it. Merging the two is where this goes quietly
// wrong, so three rules hold the merge together and none of them may be
// relaxed:
//
//   * ONE ordering key, `(instant, id)` descending — `newestFirst`. Raw
//     ISO strings are not safely comparable (the API emits naive and
//     aware forms for the same instant) and a shared timestamp needs the
//     id to break it. A row's position depends only on its own key, so
//     nothing jumps when a page lands.
//   * Dedupe by id, with the LIVE row winning: it carries `payload` and
//     `topic`, which the store's list rows do not.
//   * The read cursor is recomputed from the MERGED set every time,
//     never kept as a running variable. The live ring evicts as it
//     grows, so a row can leave the live window while still held here,
//     and a row never paged can leave both — deriving the cursor from
//     one half silently opens a hole no error would report.
//
// ---------------------------------------------------------------------
// What this view knows, and what it therefore may claim
// ---------------------------------------------------------------------
//
// The store's `events` query takes ONE category and ONE actor (see
// `events_query` in api/routes/events.py). The UI offers multi-select,
// so a selection is decomposed into the cross product of its values —
// one server read per (category, actor) pair, here called a BRANCH — and
// the results are merged under the rules above. Branches are pairwise
// disjoint (two distinct branches differ on a field both constrain), so
// a row belongs to exactly one of them.
//
// That fan-out alone would still lie, because branches drain at
// different rates: 100 rows of a rare category can reach back three
// months while 100 rows of a busy one covers two hours, and the region
// in between would render as "no notifications happened" when in truth
// nothing had asked. So the view tracks a FLOOR — the shallowest depth
// any open branch has been read to — and shows only what sits at or
// above it. The claim the screen makes is therefore exact:
//
//     every event matching this filter, from now down to the floor.
//
// The pager moves the floor, and moving it means reading every branch
// that is currently holding it up (reading a deeper branch cannot move
// it). With one branch — no filter, one category, one seat — the floor
// is simply the oldest row held and nothing is ever hidden, which is the
// behaviour this view has always had.
//
// The failures toggle has no server-side equivalent at all, so it stays
// a client-side narrowing. Under the floor rule that is honest: within
// the covered window the view holds EVERY matching event, so narrowing
// it to the failed ones yields exactly the failures in that window.

import {
  esc,
  escAttr,
  fmtDuration,
  newestFirst,
  relTime,
  shortId,
} from "../format.js";
import { icon } from "../icons.js";
import { catClass, EVENT_CATEGORIES } from "../state.js";
import {
  emptyOrPending,
  eventSummary,
  sectionHead,
  skeletonRows,
} from "../ui.js";
import { isInspectable, traceNode } from "../traceNodes.js";
import { replaceParams } from "../router.js";

// Trace groups rendered before the local pager. The feed grows as events
// stream, so rendering all of them puts a wall of identical rows on
// screen and a few hundred nodes into every patch. Roughly three
// screens — enough to scroll through what just happened without paging.
const PAGE = 60;

// Rows fetched per branch per trip to the store. Larger than PAGE so one
// read usually yields more than one screenful, and well under the
// server's MAX_EVENT_LIMIT (500) so the page a caller asks for is the
// page it gets — the short-page test below depends on that.
const HISTORY_PAGE = 100;

// The most branches one click may fan out to. A branch is a server read
// of up to HISTORY_PAGE rows, so this caps a single click at 1200 rows;
// it also covers every realistic selection (4 categories × 3 seats,
// 6 × 2, 12 × 1). Beyond it the view says plainly that it cannot page
// the store for this filter rather than quietly reading a subset — the
// root fix is a server filter that accepts several values per field.
const MAX_BRANCHES = 12;

const CATEGORY_KEYS = EVENT_CATEGORIES.map((c) => c.key);

// ---------------------------------------------------------------------
// URL codec
// ---------------------------------------------------------------------
// Filters live in the query string, so a filtered screen survives a
// reload and can be handed to somebody as a link. Multi-value fields are
// comma-joined because `replaceParams` sets one value per key.
//
// A value containing a comma (or a percent, which is how a comma is
// escaped) is percent-encoded per item and decoded on the way back;
// anything else is written as-is, so the common link — the seat page's
// `#/activity?actor=Agent%20CEO` — stays readable and keeps working.

const encodeItem = (value) => (/[,%]/.test(value) ? encodeURIComponent(value) : value);

function decodeItem(value) {
  if (!value.includes("%")) return value;
  try {
    return decodeURIComponent(value);
  } catch {
    // A hand-written URL with a bare `%`. Its literal text is a better
    // answer than dropping the filter.
    return value;
  }
}

function splitParam(value) {
  return String(value == null ? "" : value)
    .split(",")
    .map((part) => part.trim())
    .filter(Boolean)
    .map(decodeItem);
}

function joinParam(values) {
  return [...values].map(encodeItem).join(",");
}

function flagParam(value) {
  const raw = String(value == null ? "" : value);
  return raw === "1" || raw === "true";
}

// ---------------------------------------------------------------------
// Depth comparisons
// ---------------------------------------------------------------------
// A "depth" is a row: the point in `(instant, id)` order that some read
// reached. The two combinators read `null` differently, deliberately,
// and the difference is the whole floor rule — so they are named for
// which one they are rather than for the comparison they make.

/** The further-back of two depths; a missing side is simply absent. */
function deeperOf(a, b) {
  if (!a) return b;
  if (!b) return a;
  return newestFirst(a, b) > 0 ? a : b;
}

/**
 * The shallowest of two depths — the floor combinator.
 *
 * Here `null` is not "absent" but "nothing is covered", which is
 * shallower than any row and therefore wins.
 */
function shallowestOf(a, b) {
  if (!a || !b) return null;
  return newestFirst(a, b) <= 0 ? a : b;
}

function sameDepth(a, b) {
  if (!a || !b) return !a && !b;
  return newestFirst(a, b) === 0;
}

const categoryOf = (ev) => ev.category || "system";
// The actor a row can be FILTERED by, which is the field the server
// matches on (`Event.actor` already falls through role → source →
// agent_id, so it is never empty on a stored row). The old view fell
// back to `source` here, which offered pills the server could not
// answer: selecting one read as "the beginning of history" on the first
// page. Client predicate and server filter are now the same field.
const actorOf = (ev) => ev.actor || "";

export function createActivityView({ navigate, refresh, query, store, params }) {
  const expanded = new Set();

  // Every filter is seeded from the URL, so a reload — and a shared
  // link — lands on the same screen. `?actor=<role>` is the one another
  // view mints: the seat page links here instead of rendering a second
  // copy of this list.
  const cats = new Set(splitParam(params && params.cat));
  const actors = new Set(splitParam(params && params.actor));
  // Failures cut across every category — a dead phase is `system`, a
  // dead task is `task` — so this is its own toggle rather than another
  // category pill. It is the one filter an operator reaches for under
  // pressure, so it sits first and stays first.
  let failedOnly = flagParam(params && params.failed);
  let sortAsc = String((params && params.sort) || "") === "asc";
  let shown = PAGE;

  // Stored history fetched beneath the live feed, oldest-growing.
  let older = [];
  // Branch keys whose history ran out (a short page). The one thing
  // about the read that cannot be re-derived from the rows held: a short
  // page is an event, not a property of the data.
  const ended = new Set();
  let loading = false;
  // Set when the server says it has no event store. Cached rather than
  // re-asked, and cleared on reconnect: a reconnect may land on a
  // different process.
  let noStore = false;
  // A branch read that failed for any other reason (a timeout, a dropped
  // socket). The floor holds until it succeeds, so the reader is told
  // rather than left clicking a control that appears to do nothing.
  let readFailed = false;
  let disposed = false;

  // ---- the merged set ------------------------------------------------

  function allEvents(state) {
    const live = state.events || [];
    const seen = new Set(live.map((e) => e.id));
    return live.concat(older.filter((e) => !seen.has(e.id))).sort(newestFirst);
  }

  function matchesFilter(ev) {
    if (failedOnly && !ev.failed) return false;
    if (cats.size && !cats.has(categoryOf(ev))) return false;
    if (actors.size && !actors.has(actorOf(ev))) return false;
    return true;
  }

  // ---- branches ------------------------------------------------------

  function branches() {
    // A selection covering every category the engine assigns is not a
    // filter. Collapsing it keeps the fan-out at one read instead of
    // ten, and the client predicate agrees because every event has one
    // of these categories.
    const everyCat =
      cats.size === CATEGORY_KEYS.length && CATEGORY_KEYS.every((k) => cats.has(k));
    const cs = cats.size && !everyCat ? [...cats].sort() : [null];
    const as = actors.size ? [...actors].sort() : [null];
    const out = [];
    for (const category of cs) {
      for (const actor of as) {
        out.push({ key: JSON.stringify([category, actor]), category, actor });
      }
    }
    return out;
  }

  function inBranch(branch, ev) {
    if (branch.category && categoryOf(ev) !== branch.category) return false;
    if (branch.actor && actorOf(ev) !== branch.actor) return false;
    return true;
  }

  /**
   * How deep the view is complete, and which branches are holding it up.
   *
   * `merged` is newest-first, so the last matching row is the oldest one
   * held for a branch. The live ring covers EVERY branch down to its own
   * oldest row (it is an unfiltered tail), so a branch that has never
   * been read is covered to exactly there — which is why the first read
   * of every branch starts from the live tail and nothing is hidden
   * before one happens.
   */
  function coverage(state, merged) {
    const live = state.events || [];
    let liveOldest = null;
    for (const ev of live) liveOldest = deeperOf(liveOldest, ev);

    const open = [];
    for (const branch of branches()) {
      if (ended.has(branch.key)) continue;
      let oldest = null;
      for (let i = merged.length - 1; i >= 0; i--) {
        if (inBranch(branch, merged[i])) {
          oldest = merged[i];
          break;
        }
      }
      open.push({ ...branch, depth: deeperOf(liveOldest, oldest) });
    }

    if (!open.length) return { open, floor: null, complete: true };
    let floor = open[0].depth;
    for (const branch of open) floor = shallowestOf(floor, branch.depth);
    return { open, floor, complete: false };
  }

  // A row is shown when the view can honestly claim to hold everything
  // beside it. Live rows always pass: the floor is never newer than the
  // live tail, because every branch is covered at least that far.
  function inWindow(ev, cover) {
    if (cover.complete) return true;
    if (!cover.floor) return false;
    return newestFirst(ev, cover.floor) <= 0;
  }

  // ---- reading history -----------------------------------------------

  /**
   * Fetch the page beneath the floor.
   *
   * Only the branches AT the floor are read: a branch already deeper
   * cannot move it, and every row such a read returned would stay hidden
   * behind the branch that is actually holding things up.
   */
  async function loadOlder(state) {
    if (loading || noStore) return;
    if (branches().length > MAX_BRANCHES) return;
    const merged = allEvents(state);
    const cover = coverage(state, merged);
    if (cover.complete) return;
    const due = cover.open.filter((branch) => sameDepth(branch.depth, cover.floor));
    if (!due.length) return;

    loading = true;
    readFailed = false;
    refresh();
    // Settled, not all: one branch failing must not discard the pages
    // that came back, and the floor already refuses to show rows the
    // failed branch has not covered.
    const results = await Promise.allSettled(
      due.map((branch) => {
        const request = { limit: HISTORY_PAGE };
        if (branch.depth) {
          request.before = branch.depth.timestamp;
          request.before_id = branch.depth.id;
        }
        // Filters are pushed into the query rather than applied to what
        // comes back: filtering a paged list client-side silently
        // excludes, because a 100-row page holding 2 matches reads as
        // "only 2 exist".
        if (branch.category) request.category = branch.category;
        if (branch.actor) request.actor = branch.actor;
        return query("events", request);
      }),
    );

    const known = new Set(merged.map((e) => e.id));
    const fresh = [];
    let landed = false;
    results.forEach((result, i) => {
      if (result.status === "rejected") {
        const code = result.reason && result.reason.message;
        if (code === "no_event_store") noStore = true;
        else readFailed = true;
        return;
      }
      landed = true;
      const rows = Array.isArray(result.value) ? result.value : [];
      // A short page is the end of THIS branch's history. Not a zero-row
      // page: that would take one wasted round trip to discover, every
      // time.
      if (rows.length < HISTORY_PAGE) ended.add(due[i].key);
      for (const row of rows) {
        if (!row || !row.id || known.has(row.id)) continue;
        known.add(row.id);
        fresh.push(row);
      }
    });
    older = older.concat(fresh);
    if (landed) shown += PAGE;
    loading = false;
    if (!disposed) refresh();
  }

  // ---- URL -----------------------------------------------------------

  function syncUrl() {
    replaceParams("activity", {
      actor: joinParam(actors),
      cat: joinParam(cats),
      failed: failedOnly ? "1" : "",
      sort: sortAsc ? "asc" : "",
    });
  }

  // ---- filters -------------------------------------------------------

  function filterBar(state) {
    // THE CHOICE, stated once: these counts describe the retained LIVE
    // FEED and say so on the row. They cannot describe the filtered
    // result — that reaches into the event store, whose size no query
    // here can total — and a number that silently means something other
    // than the list under it is the defect this label exists to close.
    // The live ring is the honest basis precisely because it is
    // unfiltered: counting the loaded set instead would report "Task
    // 412, Notify 12" purely because Task is the branch being paged.
    const live = state.events || [];
    const counts = {};
    const actorCounts = {};
    let failures = 0;
    for (const ev of live) {
      const cat = categoryOf(ev);
      counts[cat] = (counts[cat] || 0) + 1;
      const who = actorOf(ev);
      if (who && who !== "system") actorCounts[who] = (actorCounts[who] || 0) + 1;
      if (ev.failed) failures++;
    }

    // An active filter always gets a pill, even at zero matches in the
    // loaded window: a filter the reader cannot see is an empty list
    // with no way back. Both filters are reachable from a URL, so either
    // can be set before a single matching event has loaded.
    const catPills = [
      ...EVENT_CATEGORIES.filter((c) => counts[c.key] || cats.has(c.key)),
      // A category the URL named that this build does not know. It still
      // filters, so it still needs a way out.
      ...[...cats]
        .filter((key) => !CATEGORY_KEYS.includes(key))
        .sort()
        .map((key) => ({ key, label: key })),
    ]
      .map((c) => pill(c.label, counts[c.key] || 0, cats.has(c.key), {
        action: "cat",
        data: `data-cat="${escAttr(c.key)}"`,
        cls: catClass(c.key),
        key: `cat:${c.key}`,
        dot: true,
      }))
      .join("");

    for (const who of actors) if (!(who in actorCounts)) actorCounts[who] = 0;
    const actorPills = Object.keys(actorCounts)
      .sort()
      .map((who) =>
        pill(who, actorCounts[who], actors.has(who), {
          action: "actor",
          data: `data-actor="${escAttr(who)}"`,
          key: `actor:${who}`,
        }),
      )
      .join("");

    const scope = `
      <span class="act-scope" data-k="scope"
            title="These counts describe the live feed this page retains — a bounded ring of the most recent events. The list below reaches further back through the event store, so a filtered result will not match them.">counts: last ${esc(String(live.length))} events</span>`;

    // "All" clears EVERY filter, including the actor the seat page seeds.
    // Scoping it to the row it sits in left it lit while an actor filter
    // was still cutting the list down — a control that says the list is
    // unfiltered while it is not, on the one screen an operator opens to
    // find out what happened.
    const clear = `
      <span class="pill ${!cats.size && !actors.size && !failedOnly ? "active" : ""}"
            data-k="all" data-action="cat-all" role="button" tabindex="0"
            title="Clear every filter">All</span>`;

    const failed = failures
      ? pill("Failures", failures, failedOnly, {
          action: "failed",
          cls: "failed",
          key: "failed",
          iconId: "alert",
        })
      : "";

    const sort = `
      <span class="pill sort" data-k="sort" data-action="sort" role="button"
            tabindex="0">${icon("chevron", "sm")} ${sortAsc ? "Oldest first" : "Newest first"}</span>`;

    return `
      <div class="filters act-filters" data-k="filters">
        ${scope}${clear}${failed}${catPills}${sort}
      </div>
      ${actorPills ? `<div class="filters act-filters" data-k="actors">${actorPills}</div>` : ""}`;
  }

  function pill(label, count, on, { action, data = "", cls = "", key, dot = false, iconId = "" }) {
    return `
      <span class="pill ${cls} ${on ? "active" : ""}" data-k="${escAttr(key)}"
            data-action="${escAttr(action)}" ${data} role="button" tabindex="0"
            aria-pressed="${on ? "true" : "false"}">
        ${dot ? '<i class="dot" style="background:currentColor"></i>' : ""}${iconId ? icon(iconId, "sm") : ""}${esc(label)}
        <span class="ct">${esc(String(count))}</span>
      </span>`;
  }

  // ---- traces --------------------------------------------------------

  function groupTraces(evs) {
    const groups = new Map();
    for (const ev of evs) {
      const key = ev.trace_id || "_solo_" + ev.id;
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(ev);
    }
    const arr = [...groups.entries()].map(([key, list]) => {
      // Through the shared comparator: raw ISO strings are not safely
      // comparable across the naive and aware encodings the API emits,
      // so a trace's own events could order wrongly among themselves.
      list.sort((a, b) => -newestFirst(a, b));
      const root = list.find((e) => !e.parent_span_id) || list[0];
      return { key, list, root, last: list[list.length - 1] };
    });
    arr.sort((a, b) =>
      sortAsc ? -newestFirst(a.last, b.last) : newestFirst(a.last, b.last),
    );
    return arr;
  }

  function renderTrace(g) {
    const open = expanded.has(g.key);
    const solo = g.list.length === 1;
    const r = g.root;
    // An inspect link with no store behind it can only dead-end: the
    // detail page reads the stored payload.
    const inspectable = !noStore;
    const inspectRoot = solo && inspectable && isInspectable(r);

    if (solo) {
      return `
        <div class="trace" data-k="t:${escAttr(g.key)}" data-trace="${escAttr(g.key)}">
          <div class="trace-head ${inspectRoot ? "expandable" : ""} ${r.failed ? "is-failed" : ""}"
               ${inspectRoot ? `data-action="open" data-id="${escAttr(r.id)}" role="link" tabindex="0"` : ""}>
            <span class="act-nub"></span>
            <span class="node-dot ${catClass(r.category)}"></span>
            <span class="trace-summary">${eventSummary(r)}</span>
            <div class="trace-meta">
              ${inspectRoot ? `<span class="inspect ${catClass(r.category)}">inspect →</span>` : ""}
              <span class="evt-cat ${catClass(r.category)}">${esc(categoryOf(r))}</span>
              <span class="row-ts" data-ts="${escAttr(r.timestamp)}">${esc(relTime(r.timestamp))}</span>
            </div>
          </div>
        </div>`;
    }

    const kids = open
      ? g.list.map((c) => traceNode(c, { inspectable, keyPrefix: "c" })).join("")
      : "";

    return `
      <div class="trace ${open ? "open" : ""}" data-k="t:${escAttr(g.key)}" data-trace="${escAttr(g.key)}">
        <div class="trace-head expandable ${g.list.some((e) => e.failed) ? "is-failed" : ""}"
             data-action="toggle" data-key="${escAttr(g.key)}" role="button"
             tabindex="0" aria-expanded="${open ? "true" : "false"}">
          ${icon("chevron", "chevron")}
          <span class="node-dot ${catClass(r.category)}"></span>
          <span class="trace-summary">${eventSummary(r)}</span>
          <div class="trace-meta">
            <span class="trace-count">${esc(String(g.list.length))} events · ${esc(fmtDuration(g.list[0].timestamp, g.last.timestamp))}</span>
            ${
              g.root.trace_id
                ? `<span class="trace-id trace-link" data-action="open-trace"
                     data-trace="${escAttr(g.root.trace_id)}" role="link"
                     tabindex="0">${esc(shortId(g.key))} →</span>`
                : `<span class="trace-id">${esc(shortId(g.key))}</span>`
            }
            <span class="row-ts" data-ts="${escAttr(g.last.timestamp)}">${esc(relTime(g.last.timestamp))}</span>
          </div>
        </div>
        <div class="trace-kids">${kids}</div>
      </div>`;
  }

  // ---- the pager -----------------------------------------------------
  // Three distinct jobs that must not look alike: revealing rows already
  // in memory is instant, reading the store is a network trip that can
  // fail or run out, and saying why nothing more is coming is neither.

  function note(text, cls = "is-quiet") {
    return `<div class="show-more act-pager ${cls}" data-k="pager">${esc(text)}</div>`;
  }

  function pager(hiddenLocally, held, cover) {
    const fanOut = branches().length;
    if (hiddenLocally > 0) {
      const n = Math.min(hiddenLocally, PAGE);
      return `<div class="show-more act-pager" data-action="more" data-k="pager"
                   role="button" tabindex="0">Show ${esc(String(n))} ${
                     sortAsc ? "newer" : "older"
                   } · ${esc(String(hiddenLocally))} remaining</div>`;
    }
    if (noStore) {
      return note(
        "Live feed only — no event store is configured, so nothing older is retained.",
      );
    }
    if (fanOut > MAX_BRANCHES) {
      return note(
        `This filter needs ${fanOut} separate reads of the event store — more than one click should make. Narrow it to read further back; the live feed above is filtered either way.`,
      );
    }
    if (cover.complete) return note("That is the beginning of the retained history.");
    if (loading) return note("Reading history…", "is-reading");
    const heldNote = held
      ? `<div class="act-held" data-k="held">${esc(String(held))} older ${
          held === 1 ? "row is" : "rows are"
        } loaded and hidden until every part of this filter has been read that far back.</div>`
      : "";
    const failNote = readFailed
      ? `<div class="act-held" data-k="readfail">Part of that read did not come back — try again.</div>`
      : "";
    return `<div class="show-more act-pager" data-action="load-older" data-k="pager"
                 role="button" tabindex="0">Load ${esc(String(HISTORY_PAGE))} more from history</div>${heldNote}${failNote}`;
  }

  let unsubscribe = null;
  let wasDisconnected = false;

  return {
    slices: ["events", "health"],

    mount() {
      // A reconnect can land on a different API process, which may have
      // an event store where the last one did not. Re-asking on
      // reconnect is the only way a cached "no store" answer stops
      // being permanent.
      unsubscribe = store.subscribe(["health"], (state) => {
        if (state.connected && wasDisconnected) {
          noStore = false;
          readFailed = false;
          ended.clear();
        }
        wasDisconnected = !state.connected;
      });
    },

    render(state) {
      const merged = allEvents(state);
      const cover = coverage(state, merged);
      let held = 0;
      const evs = [];
      for (const ev of merged) {
        if (!matchesFilter(ev)) continue;
        if (inWindow(ev, cover)) evs.push(ev);
        else held++;
      }
      const groups = groupTraces(evs);
      const page = groups.slice(0, shown);
      const more = groups.length - page.length;
      const filtered = cats.size || actors.size || failedOnly;
      // The pager belongs under an EMPTY list too, and only there does it
      // matter: a filter with no match in the live ring — a seat that has
      // been quiet for an hour, arrived at from its own page — used to
      // render an empty state with no way to look further back, which is
      // exactly the reader who most needs one. It is withheld only while
      // the socket is down, where the body is a skeleton and a control
      // that cannot be answered would be noise.
      const body = groups.length
        ? `<div class="list" data-k="list">${page.map(renderTrace).join("")}</div>` +
          pager(more, held, cover)
        : `<div data-k="list">${emptyOrPending(
            state,
            () => skeletonRows(6),
            "activity",
            "No events",
            filtered
              ? "No events match the selected filters"
              : "Activity will appear here as agents work",
          )}</div>` + (state.connected ? pager(0, held, cover) : "");
      return `
        ${sectionHead("activity", "Traces", groups.length)}
        ${filterBar(state)}
        ${body}`;
    },

    onAction(action, t) {
      if (action === "toggle") {
        const k = t.dataset.key;
        if (expanded.has(k)) expanded.delete(k);
        else expanded.add(k);
      } else if (action === "open") {
        navigate("/events/" + encodeURIComponent(t.dataset.id));
        return;
      } else if (action === "open-trace") {
        navigate("/traces/" + encodeURIComponent(t.dataset.trace));
        return;
      } else if (action === "cat") {
        const c = t.dataset.cat;
        if (cats.has(c)) cats.delete(c);
        else cats.add(c);
      } else if (action === "cat-all") {
        cats.clear();
        actors.clear();
        failedOnly = false;
      } else if (action === "failed") {
        failedOnly = !failedOnly;
      } else if (action === "actor") {
        const a = t.dataset.actor;
        if (actors.has(a)) actors.delete(a);
        else actors.add(a);
      } else if (action === "sort") {
        sortAsc = !sortAsc;
      } else if (action === "more") {
        shown += PAGE;
      } else if (action === "load-older") {
        // Guarded in the view, not by the socket: a shared in-flight cap
        // would make this control feel dead behind an unrelated read.
        loadOlder(store.state);
        return;
      } else {
        return;
      }
      // A filter change invalidates the history already fetched: it was
      // fetched under the previous branches, and the next page's cursor
      // has to start from the top again.
      if (["cat", "cat-all", "failed", "actor"].includes(action)) {
        older = [];
        ended.clear();
        readFailed = false;
        shown = PAGE;
      }
      if (["cat", "cat-all", "failed", "actor", "sort"].includes(action)) syncUrl();
      refresh();
    },

    destroy() {
      disposed = true;
      if (unsubscribe) unsubscribe();
    },
  };
}
