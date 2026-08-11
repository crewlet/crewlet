// Reusable render helpers shared across views.

import { esc, escAttr, fmtNum, fmtTime } from "./format.js";
import { icon } from "./icons.js";
import {
  PHASE_ORDER,
  phaseColor,
  catClass,
  integrationFromSource,
  integrationBadge,
  integrationMeta,
  roleInk,
} from "./state.js";

export function skeletonRows(n = 5) {
  let out = '<div class="list">';
  for (let i = 0; i < n; i++) {
    out += '<div class="row"><div class="skel skel-line" style="flex:1"></div></div>';
  }
  return out + "</div>";
}

export function skeletonCards(n = 3) {
  let out = '<div class="widgets">';
  for (let i = 0; i < n; i++) out += '<div class="skel skel-card"></div>';
  return out + "</div>";
}

export function empty(iconId, title, sub = "") {
  return `
    <div class="empty">
      ${icon(iconId, "lg")}
      <div>${esc(title)}</div>
      ${sub ? `<div class="empty-sub">${esc(sub)}</div>` : ""}
    </div>`;
}

export function badge(text, cls = "") {
  return `<span class="badge ${cls}">${esc(text)}</span>`;
}

export function dotBadge(text, cls = "") {
  return `<span class="badge ${cls}"><i class="dot"></i>${esc(text)}</span>`;
}

// Stacked token bar by phase + a legend with percentages.
export function phaseBar(phases, total) {
  const list = (phases || []).filter((p) => p.total_tokens > 0);
  if (!list.length || !total) {
    return '<div class="empty-sub">No phase activity yet</div>';
  }
  const ordered = [...list].sort(
    (a, b) => (PHASE_ORDER[a.phase] || 50) - (PHASE_ORDER[b.phase] || 50),
  );
  let bar = '<div class="phase-bar">';
  for (const p of ordered) {
    const pct = (p.total_tokens / total) * 100;
    bar += `<i style="width:${pct.toFixed(2)}%;background:${phaseColor(p.phase)}" title="${esc(p.phase)}: ${fmtNum(p.total_tokens)}"></i>`;
  }
  bar += "</div>";
  let legend = '<div class="phase-legend">';
  for (const p of ordered) {
    const pct = ((p.total_tokens / total) * 100).toFixed(0);
    legend += `<span><i style="background:${phaseColor(p.phase)}"></i>${esc(p.phase)} <span class="pct">${fmtNum(p.total_tokens)} · ${pct}%</span></span>`;
  }
  legend += "</div>";
  return bar + legend;
}

// A colour-tinted event summary line. Notification rows lead with a
// branded integration badge (Slack / Jira / …) derived from the event
// source so the originating integration is identifiable at a glance.
export function eventSummary(ev) {
  let badge = "";
  if (ev.category === "notification") {
    const key = integrationFromSource(ev.source);
    if (key) badge = integrationBadge(key);
  }
  return `${badge}<span class="${catClass(ev.category)}" style="color:inherit">${esc(ev.summary || ev.type)}</span>`;
}

// One line of the engine activity feed: clock time, the actor in its own
// identity hue, the surface the work landed on, then what happened. The
// integration chip is only rendered when the event actually names one —
// engine-internal events get their category instead, never a guessed brand.
export function activityRow(ev, { agents } = {}) {
  const actor = ev.actor || "";
  const agent = (agents || []).find((a) => (a.role || a.name) === actor);
  const nav = agent ? `class="act-row clickable" data-action="agent" data-id="${escAttr(agent.id)}"` : 'class="act-row"';
  // `source` is the only integration signal here: the projection's event
  // buffer keeps a payload-free copy (live_state._record_event), so a
  // payload lookup would never fire on this surface.
  const key = integrationFromSource(ev.source);
  const surface = key
    ? `<span class="int-badge sm" style="--int-color:${integrationMeta(key).color}">${icon(
        integrationMeta(key).icon,
        "sm",
      )}${esc(integrationMeta(key).label)}</span>`
    : `<span class="evt-cat ${catClass(ev.category)}">${esc(ev.category || "system")}</span>`;
  return `
    <div ${nav}>
      <span class="act-time">${esc(fmtTime(ev.timestamp))}</span>
      <span class="act-actor" ${actor ? `style="color:${roleInk(actor)}"` : ""}>${esc(actor || "engine")}</span>
      <span class="act-surface">${surface}</span>
      <span class="act-text">${esc(ev.summary || ev.type)}</span>
    </div>`;
}

export function sectionHead(iconId, title, count, link) {
  return `
    <div class="sec">
      <span class="sec-title">${iconId ? icon(iconId, "sm") : ""}${esc(title)}
        ${count != null ? `<span class="sec-count">${esc(String(count))}</span>` : ""}
      </span>
      ${link ? `<span class="sec-link" data-action="${esc(link.action)}">${esc(link.label)} →</span>` : ""}
    </div>`;
}
