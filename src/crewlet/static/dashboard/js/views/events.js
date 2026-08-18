// Activity view: trace-grouped event log with category/agent filters.

import { esc, relTime, fmtDuration, shortId } from "../format.js";
import { icon } from "../icons.js";
import { catClass, EVENT_CATEGORIES } from "../state.js";
import { empty, eventSummary, skeletonRows } from "../ui.js";

const A2A_TYPES = new Set([
  "a2a_channel_opened",
  "a2a_message_sent",
  "a2a_message_delivered",
  "a2a_channel_closed",
]);

function isInspectable(e) {
  return (
    e.type === "agent_turn_completed" ||
    e.type === "agent_phase_completed" ||
    A2A_TYPES.has(e.type) ||
    e.category === "webhook" ||
    e.category === "notification"
  );
}

// Traces rendered before the "show older" control. The projection
// retains several hundred events and the feed grows as they stream, so
// rendering all of them puts a wall of identical rows on screen and a
// few hundred nodes into every patch. This is roughly three screens —
// enough to scroll through what just happened without paging, and the
// rest is one click away.
const PAGE = 60;

export function createEventsView({ navigate, refresh }) {
  const expanded = new Set();
  const cats = new Set();
  const agents = new Set();
  let sortAsc = false;
  let shown = PAGE;

  function filtered(state) {
    let evs = state.events || [];
    if (cats.size) evs = evs.filter((e) => cats.has(e.category || "system"));
    if (agents.size)
      evs = evs.filter((e) => agents.has(e.actor || e.source || "system"));
    return evs;
  }

  function groupTraces(evs) {
    const groups = new Map();
    for (const e of evs) {
      const key = e.trace_id || "_solo_" + e.id;
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(e);
    }
    const arr = [...groups.entries()].map(([key, list]) => {
      list.sort((a, b) => (a.timestamp < b.timestamp ? -1 : 1));
      const root = list.find((e) => !e.parent_span_id) || list[0];
      return { key, list, root, last: list[list.length - 1].timestamp };
    });
    arr.sort((a, b) => (sortAsc ? (a.last < b.last ? -1 : 1) : a.last < b.last ? 1 : -1));
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

    return `
      <div class="filters">
        <span class="pill ${cats.size === 0 ? "active" : ""}" data-action="cat-all">All</span>
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
          <div class="trace-head ${inspectRoot ? "expandable" : ""}" ${inspectRoot ? `data-action="open" data-id="${esc(r.id)}"` : ""}>
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
        <div class="trace-child ${insp ? "clickable" : ""} ${skipped ? "skipped" : ""}" data-k="c:${esc(c.id)}"
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
        <div class="trace-head expandable" data-action="toggle" data-key="${esc(g.key)}">
          ${icon("chevron", "chevron")}
          <span class="node-dot ${catClass(r.category)}"></span>
          <span class="trace-summary">${eventSummary(r)}</span>
          <div class="trace-meta">
            <span class="trace-count">${g.list.length} events · ${esc(fmtDuration(g.list[0].timestamp, g.last))}</span>
            <span class="trace-id">${esc(shortId(g.key))}</span>
            <span class="row-ts" data-ts="${esc(g.last)}">${esc(relTime(g.last))}</span>
          </div>
        </div>
        <div class="trace-kids">${open ? kids : ""}</div>
      </div>`;
  }

  return {
    slices: ["events", "health"],

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
            ? '<div class="list">' +
              page.map(renderTrace).join("") +
              "</div>" +
              (more
                ? `<div class="show-more" data-action="more">Show ${more > PAGE ? PAGE : more} older · ${more} remaining</div>`
                : "")
            : empty(
                "inbox",
                "No events",
                cats.size || agents.size
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
      } else if (action === "actor") {
        const a = t.dataset.actor;
        if (agents.has(a)) agents.delete(a);
        else agents.add(a);
      } else if (action === "sort") {
        sortAsc = !sortAsc;
      } else if (action === "more") {
        shown += PAGE;
      } else {
        return;
      }
      refresh();
    },
  };
}
