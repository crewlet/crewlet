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

// ---------------------------------------------------------------------
// Sparklines
// ---------------------------------------------------------------------
// Every widget on the overview showed one number and no history, so a
// dashboard that was busy looked exactly like one that had been idle for
// an hour. These draw a trend from data the client already has — the
// activity feed's timestamps, the spend rollup's per-turn rows — so they
// cost no request and no server work.

// Bucket timestamped items into `buckets` equal slices ending now, and
// return the per-bucket totals. `value` maps an item to its contribution
// and defaults to counting items.
//
// Deliberately not named `valueOf`: destructuring that name off an
// options object yields `Object.prototype.valueOf` rather than
// `undefined` when the caller omits it, so the "no mapper given" branch
// never runs and every call throws.
export function bucketSeries(items, { minutes = 60, buckets = 24, value } = {}) {
  const now = Date.now();
  const span = minutes * 60_000;
  const width = span / buckets;
  const series = new Array(buckets).fill(0);
  for (const item of items || []) {
    const at = Date.parse(item.timestamp || item.ended_at || "");
    if (!at) continue;
    const age = now - at;
    if (age < 0 || age > span) continue;
    const idx = Math.min(buckets - 1, Math.floor((span - age) / width));
    series[idx] += value ? value(item) : 1;
  }
  return series;
}

// An area sparkline with a dot on the last point. Drawn as inline SVG in
// `currentColor` so one function serves every hue, and given an explicit
// height so a widget's layout never shifts when the data arrives.
export function sparkline(series, { height = 28, fill = true } = {}) {
  const points = series || [];
  if (points.length < 2) return `<div class="spark" style="height:${height}px"></div>`;
  const max = Math.max(...points, 1);
  const stepX = 100 / (points.length - 1);
  const y = (v) => (1 - v / max) * (height - 4) + 2;
  const line = points.map((v, i) => `${(i * stepX).toFixed(2)},${y(v).toFixed(2)}`);
  const last = points[points.length - 1];
  return `
    <svg class="spark" viewBox="0 0 100 ${height}" preserveAspectRatio="none" aria-hidden="true">
      ${fill ? `<polygon class="spark-area" points="0,${height} ${line.join(" ")} 100,${height}"></polygon>` : ""}
      <polyline class="spark-line" points="${line.join(" ")}"></polyline>
      <circle class="spark-dot" cx="100" cy="${y(last).toFixed(2)}" r="2"></circle>
    </svg>`;
}

// A stat widget: label, headline number, a supporting line, and a trend.
export function statWidget({ hue, iconId, label, value, foot, series, action }) {
  const clickable = action ? ` clickable" data-action="${escAttr(action)}` : "";
  return `
    <div class="widget ${hue}${clickable}" data-k="w:${escAttr(label)}">
      <div class="widget-head">${icon(iconId, "sm")} ${esc(label)}</div>
      <div class="widget-big">${esc(String(value))}</div>
      <div class="widget-foot">${foot || ""}</div>
      ${series ? sparkline(series) : ""}
    </div>`;
}
