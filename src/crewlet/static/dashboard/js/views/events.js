// Activity view: trace-grouped event log, over the live feed AND the
// stored history beneath it.
//
// The live feed is a bounded ring — a few hundred events — which a busy
// org fills in minutes. Everything older lives in the event store, and
// the `events` query has been able to read it since the query channel
// was built; nothing asked. So the view "paged" purely within what it
// already held, and hitting the end read as the beginning of time.

import {
  esc,
  escAttr,
  newestFirst,
  relTime,
  fmtDuration,
  shortId,
} from "../format.js";
import { icon } from "../icons.js";
import { catClass, EVENT_CATEGORIES } from "../state.js";
import { empty, eventSummary, skeletonRows } from "../ui.js";
import { isInspectable } from "../traceNodes.js";

// Traces rendered before the pager. The projection retains several
// hundred events and the feed grows as they stream, so rendering all of
// them puts a wall of identical rows on screen and a few hundred nodes
// into every patch. This is roughly three screens — enough to scroll
// through what just happened without paging.
const PAGE = 60;

// Rows fetched per trip to the store. Larger than PAGE so one network
// read usually yields more than one screenful, and well under the
// server's MAX_EVENT_LIMIT.
const HISTORY_PAGE = 100;

export function createEventsView({ navigate, refresh, query, store }) {
  const expanded = new Set();
  const cats = new Set();
  const agents = new Set();
  // Failures cut across every category — a dead phase is `system`, a
  // dead task is `task` — so this is its own toggle rather than another
  // category pill. It is the one filter an operator reaches for under
  // pressure, so it sits first and stays first.
  let failedOnly = false;
  let sortAsc = false;
  let shown = PAGE;

  // Stored history fetched beneath the live feed, oldest-growing.
  let older = [];
  let loadingOlder = false;
  let exhausted = false;
  // Set when the server says it has no event store. Cached rather than
  // re-asked, and cleared on reconnect: a reconnect may land on a
  // different process.
  let noStore = false;
  let disposed = false;

  // The merged, ordered, deduped list every other function reads.
  //
  // The live feed wins on a conflict: it carries `payload` and `topic`,
  // which the store's list rows do not. Ordering is `(instant, id)`
  // descending for both halves, so a row's position never depends on
  // which source it came from and nothing can jump when a page lands.
  function allEvents(state) {
    const live = state.events || [];
    const seen = new Set(live.map((e) => e.id));
    const merged = live.concat(older.filter((e) => !seen.has(e.id)));
    return merged.sort(newestFirst);
  }

  function filtered(state) {
    let evs = allEvents(state);
    if (failedOnly) evs = evs.filter((e) => e.failed);
    if (cats.size) evs = evs.filter((e) => cats.has(e.category || "system"));
    if (agents.size)
      evs = evs.filter((e) => agents.has(e.actor || e.source || "system"));
    return evs;
  }

  // Fetch the page beneath everything currently held.
  //
  // The cursor is recomputed from the MERGED set every time rather than
  // kept as a running variable. The live ring evicts as it grows, so a
  // row can leave the live window while still in `older`, and a row
  // never paged can leave both — deriving the cursor from `older` alone
  // would silently open a hole no error would report.
  async function loadOlder(state) {
    if (loadingOlder || exhausted || noStore) return;
    const merged = allEvents(state);
    const oldest = merged[merged.length - 1];
    loadingOlder = true;
    refresh();
    try {
      const params = { limit: HISTORY_PAGE };
      if (oldest) {
        params.before = oldest.timestamp;
        params.before_id = oldest.id;
      }
      // Filters are pushed into the query rather than applied to what
      // comes back: filtering a paged list client-side silently
      // excludes, because a 100-row page holding 2 matches reads as
      // "only 2 exist".
      if (cats.size === 1) params.category = [...cats][0];
      if (agents.size === 1) params.actor = [...agents][0];
      const rows = await query("events", params);
      const known = new Set(merged.map((e) => e.id));
      const fresh = (rows || []).filter((e) => e && e.id && !known.has(e.id));
      older = older.concat(fresh);
      // A short page is the end of the history. Not a zero-row page:
      // that would take one wasted round trip to discover, every time.
      if (!rows || rows.length < HISTORY_PAGE) exhausted = true;
      shown += PAGE;
    } catch (err) {
      if (err.message === "no_event_store") noStore = true;
      // Anything else (a timeout, a dropped socket) leaves the control
      // enabled so the reader can try again — the query layer already
      // re-sends across a reconnect.
    } finally {
      loadingOlder = false;
      if (!disposed) refresh();
    }
  }

  function groupTraces(evs) {
    const groups = new Map();
    for (const e of evs) {
      const key = e.trace_id || "_solo_" + e.id;
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(e);
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

  function filterBar(state) {
    const counts = {};
    for (const e of state.events || [])
      counts[e.category || "system"] = (counts[e.category || "system"] || 0) + 1;
    const actorCounts = {};
    for (const e of state.events || []) {
      const a = e.actor || e.source || "system";
      if (a !== "system") actorCounts[a] = (actorCounts[a] || 0) + 1;
    }

    const catPills = EVENT_CATEGORIES.filter((c) => counts[c.key])
      .map(
        (c) => `
        <span class="pill ${catClass(c.key)} ${cats.has(c.key) ? "active" : ""}" data-action="cat" data-cat="${c.key}">
          <i class="dot" style="background:currentColor"></i>${esc(c.label)} <span class="ct">${counts[c.key]}</span>
        </span>`,
      )
      .join("");

    const agentPills = Object.keys(actorCounts)
      .sort()
      .map(
        (a) => `
        <span class="pill ${agents.has(a) ? "active" : ""}" data-action="actor" data-actor="${esc(a)}">
          ${esc(a)} <span class="ct">${actorCounts[a]}</span>
        </span>`,
      )
      .join("");

    const failures = (state.events || []).filter((e) => e.failed).length;

    return `
      <div class="filters">
        <span class="pill ${cats.size === 0 && !failedOnly ? "active" : ""}" data-action="cat-all">All</span>
        ${
          failures
            ? `<span class="pill failed ${failedOnly ? "active" : ""}" data-action="failed">
                 ${icon("alert", "sm")}Failures <span class="ct">${failures}</span>
               </span>`
            : ""
        }
        ${catPills}
        <span class="pill sort" data-action="sort">${icon("chevron", "sm")} ${sortAsc ? "Oldest first" : "Newest first"}</span>
      </div>
      ${agentPills ? `<div class="filters">${agentPills}</div>` : ""}`;
  }

  function renderTrace(g) {
    const open = expanded.has(g.key);
    const solo = g.list.length === 1;
    const r = g.root;
    const inspectRoot = solo && isInspectable(r);

    if (solo) {
      return `
        <div class="trace" data-k="t:${esc(g.key)}" data-trace="${esc(g.key)}">
          <div class="trace-head ${inspectRoot ? "expandable" : ""} ${r.failed ? "is-failed" : ""}" ${inspectRoot ? `data-action="open" data-id="${esc(r.id)}"` : ""}>
            <span style="width:14px"></span>
            <span class="node-dot ${catClass(r.category)}"></span>
            <span class="trace-summary">${eventSummary(r)}</span>
            <div class="trace-meta">
              ${inspectRoot ? `<span class="inspect ${catClass(r.category)}">inspect →</span>` : ""}
              <span class="evt-cat ${catClass(r.category)}">${esc(r.category || "system")}</span>
              <span class="row-ts" data-ts="${esc(r.timestamp)}">${esc(relTime(r.timestamp))}</span>
            </div>
          </div>
        </div>`;
    }

    const kids = g.list
      .map((c) => {
        const insp = isInspectable(c);
        const skipped = c.type === "notification_skipped";
        return `
        <div class="trace-child ${insp ? "clickable" : ""} ${skipped ? "skipped" : ""} ${c.failed ? "is-failed" : ""}" data-k="c:${esc(c.id)}"
             ${insp ? `data-action="open" data-id="${esc(c.id)}"` : ""}>
          <span class="node-dot ${catClass(c.category)}" style="width:7px;height:7px"></span>
          <span class="trace-summary">${eventSummary(c)}</span>
          <div class="trace-meta">
            ${insp ? `<span class="inspect ${catClass(c.category)}">inspect →</span>` : ""}
            <span class="evt-cat ${catClass(c.category)}">${esc(c.type)}</span>
            <span class="row-ts" data-ts="${esc(c.timestamp)}">${esc(relTime(c.timestamp))}</span>
          </div>
        </div>`;
      })
      .join("");

    return `
      <div class="trace ${open ? "open" : ""}" data-k="t:${esc(g.key)}" data-trace="${esc(g.key)}">
        <div class="trace-head expandable ${g.list.some((e) => e.failed) ? "is-failed" : ""}" data-action="toggle" data-key="${esc(g.key)}">
          ${icon("chevron", "chevron")}
          <span class="node-dot ${catClass(r.category)}"></span>
          <span class="trace-summary">${eventSummary(r)}</span>
          <div class="trace-meta">
            <span class="trace-count">${g.list.length} events · ${esc(fmtDuration(g.list[0].timestamp, g.last.timestamp))}</span>
            ${
              g.root.trace_id
                ? `<span class="trace-id trace-link" data-action="open-trace"
                     data-trace="${escAttr(g.root.trace_id)}">${esc(shortId(g.key))} →</span>`
                : `<span class="trace-id">${esc(shortId(g.key))}</span>`
            }
            <span class="row-ts" data-ts="${esc(g.last.timestamp)}">${esc(relTime(g.last.timestamp))}</span>
          </div>
        </div>
        <div class="trace-kids">${open ? kids : ""}</div>
      </div>`;
  }

  // The pager has two distinct jobs and they must not look alike: one
  // reveals rows already in memory and is instant, the other is a
  // network read into the event store and can fail or run out.
  function pager(hiddenLocally) {
    if (hiddenLocally > 0) {
      const n = Math.min(hiddenLocally, PAGE);
      return `<div class="show-more" data-action="more" data-k="pager">Show ${n} older · ${hiddenLocally} remaining</div>`;
    }
    if (noStore) {
      return `<div class="show-more is-note" data-k="pager">Live feed only — no event store is configured, so nothing older is retained.</div>`;
    }
    if (exhausted) {
      return `<div class="show-more is-note" data-k="pager">That is the beginning of the retained history.</div>`;
    }
    if (loadingOlder) {
      return `<div class="show-more is-loading" data-k="pager">Reading history…</div>`;
    }
    return `<div class="show-more" data-action="load-older" data-k="pager">Load ${HISTORY_PAGE} more from history</div>`;
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
          exhausted = false;
        }
        wasDisconnected = !state.connected;
      });
    },

    render(state) {
      if (!state.connected && !(state.events || []).length) return skeletonRows(6);
      const evs = filtered(state);
      const groups = groupTraces(evs);
      const page = groups.slice(0, shown);
      const more = groups.length - page.length;
      return `
        <div class="sec"><span class="sec-title">${icon("inbox", "sm")} Traces
          <span class="sec-count">${evs.length}</span></span></div>
        ${filterBar(state)}
        ${
          groups.length
            ? '<div class="list">' + page.map(renderTrace).join("") + "</div>" + pager(more)
            : empty(
                "inbox",
                "No events",
                cats.size || agents.size || failedOnly
                  ? "No events match the selected filters"
                  : "Activity will appear here as agents work",
              )
        }`;
    },

    onAction(action, t) {
      if (action === "toggle") {
        const k = t.dataset.key;
        if (expanded.has(k)) expanded.delete(k);
        else expanded.add(k);
      } else if (action === "open") {
        navigate("/events/" + encodeURIComponent(t.dataset.id));
        return;
      } else if (action === "cat") {
        const c = t.dataset.cat;
        if (cats.has(c)) cats.delete(c);
        else cats.add(c);
      } else if (action === "cat-all") {
        cats.clear();
        failedOnly = false;
      } else if (action === "failed") {
        failedOnly = !failedOnly;
      } else if (action === "actor") {
        const a = t.dataset.actor;
        if (agents.has(a)) agents.delete(a);
        else agents.add(a);
      } else if (action === "sort") {
        sortAsc = !sortAsc;
      } else if (action === "more") {
        shown += PAGE;
      } else if (action === "load-older") {
        // Guarded in the view, not by the socket's in-flight semaphore:
        // that cap is shared with every other query the page makes, so
        // leaning on it would make the control feel dead behind an
        // unrelated read.
        loadOlder(store.state);
        return;
      } else if (action === "open-trace") {
        navigate("/traces/" + encodeURIComponent(t.dataset.trace));
        return;
      } else {
        return;
      }
      // A filter change invalidates the history already fetched: it was
      // fetched under the previous filter, and the next page's cursor
      // has to start from the top again.
      if (["cat", "cat-all", "failed", "actor"].includes(action)) {
        older = [];
        exhausted = false;
        shown = PAGE;
      }
      refresh();
    },

    destroy() {
      disposed = true;
      if (unsubscribe) unsubscribe();
    },
  };
}
