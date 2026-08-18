// Reusable render helpers shared across views.

import { esc, escAttr, fmtNum, fmtTime } from "./format.js";
import { icon } from "./icons.js";
import {
  PHASE_ORDER,
  phaseColor,
  phaseInk,
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
  let out = '<div class="seat-grid">';
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

/**
 * The empty state a view should show, chosen from what the client knows.
 *
 * Three cases hide behind one empty list, and every list view had
 * collapsed them into two:
 *
 *   - the socket is down, so nothing can be concluded → `skeleton`
 *   - the engine has no active company configuration, so this list is
 *     empty because NOTHING is configured and nothing will run
 *   - the engine is configured and this particular list is empty
 *
 * Only the third deserves "No agents configured". The second was
 * rendering as the third, which is the whole complaint: an unconfigured
 * engine looked exactly like a correctly-configured idle one, on every
 * screen at once, and the one fact that explained all of it — `configured`
 * — had been on the wire the entire time and reached no pixel.
 *
 * `skeleton` is a THUNK, not markup: the views that pass
 * `skeletonCards(6)` would otherwise build it on every render whether or
 * not it is the branch taken.
 */
export function emptyOrPending(state, skeleton, iconId, title, sub = "") {
  if (!state.connected) return skeleton();
  if (state.health && state.health.configured === false) {
    return empty(
      "database",
      "No company configuration is active",
      "Nothing is running. Import one with `crewlet config import`.",
    );
  }
  return empty(iconId, title, sub);
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
  const cls = ev.failed ? "is-failed" : catClass(ev.category);
  return `${badge}<span class="${cls}" style="color:inherit">${esc(ev.summary || ev.type)}</span>`;
}

// One line of the engine activity feed: clock time, the actor in its own
// identity hue, the surface the work landed on, then what happened. The
// integration chip is only rendered when the event actually names one —
// engine-internal events get their category instead, never a guessed brand.
export function activityRow(ev, { agents } = {}) {
  const actor = ev.actor || "";
  const agent = (agents || []).find((a) => (a.role || a.name) === actor);
  // The projection stamps `failed` on every feed row, live and hydrated
  // (live_state._light_event). Without reading it, a turn that died read
  // exactly like one that succeeded — same grey row, same prose — and
  // scanning a feed for the thing that broke meant reading every line.
  const cls = "act-row" + (ev.failed ? " is-failed" : "") + (agent ? " clickable" : "");
  // The row keys itself. It used to be wrapped in a keyed <div> by its one
  // caller, which put a node between the row and its list — so `:last-child`
  // matched every row (each was the only child of its own wrapper) and the
  // separator between rows never rendered.
  const rowKey = `data-k="e:${escAttr(ev.id)}"`;
  const nav = agent
    ? `class="${cls}" ${rowKey} data-action="agent" data-id="${escAttr(agent.id)}"`
    : `class="${cls}" ${rowKey}`;
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
      <span class="act-text">${ev.failed ? icon("alert", "sm") : ""}${esc(ev.summary || ev.type)}</span>
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
// The turn rail
// ---------------------------------------------------------------------
// A turn is the unit of work this engine does, and until now the
// dashboard never drew one: a running agent got a phase name in a pill,
// which says where it is but not where it has been or what is left. The
// rail is the turn as an object — the phases in order, the ones already
// spent filled in their own hue, the current one lit and breathing, the
// rest hollow — with a packet travelling the segment feeding the live
// phase so motion on the page always means work is actually happening.

// The canonical turn. Onboarding is deliberately not in it: it runs at
// most once per agent, before the first Plan, so carrying it as a
// permanently-dead first node would mislead on every other turn. A turn
// sitting in onboarding (or in any phase off this path — a judge, a
// sub-agent) renders as its own single-node rail instead.
const RAIL_PHASES = ["plan", "execute", "review"];

/**
 * The phase rail for one in-flight turn.
 *
 * `phase` is the phase running now; anything before it on the canonical
 * path is drawn as spent, anything after as pending.
 */
export function turnRail(phase) {
  const current = String(phase || "");
  const at = RAIL_PHASES.indexOf(current);
  const steps = at === -1 && current ? [current] : RAIL_PHASES;
  const live = at === -1 ? 0 : at;

  let out = '<div class="rail">';
  steps.forEach((name, i) => {
    if (i) {
      // The segment entering the live step is the one that animates —
      // the others are plain, spent or pending.
      const cls = i <= live ? (i === live ? "rail-link is-live" : "rail-link is-done") : "rail-link";
      out += `<span class="${cls}"></span>`;
    }
    const state = i < live ? "is-done" : i === live ? "is-live" : "";
    out += `<span class="rail-step ${state}" style="--n:${phaseColor(name)};--ink:${phaseInk(name)}"><i></i>${esc(name)}</span>`;
  });
  return out + "</div>";
}

// ---------------------------------------------------------------------
// The stat strip
// ---------------------------------------------------------------------
// Four figures on one hairline-divided rail rather than four cards.
// Cards give a number a whole panel each, which is a lot of screen for
// four integers and pushes everything that moves below the fold; the
// strip fits inside the lead panel and leaves the room to the thing
// worth looking at.

/**
 * `items` are `{label, value, foot, tone}` — `tone` names a hue family
 * (`red`, `green`, …) for a figure whose *value* carries a warning.
 */
export function statStrip(items) {
  const cells = items
    .filter(Boolean)
    .map(
      (it) => `
      <div class="strip-cell" data-k="strip:${escAttr(it.label)}">
        <div class="stat-label">${esc(it.label)}</div>
        <div class="strip-value num${it.tone ? " tone" : ""}"${
          it.tone ? ` style="color:var(--${it.tone}-ink)"` : ""
        }>${it.value}</div>
        <div class="strip-foot">${it.foot || ""}</div>
      </div>`,
    )
    .join("");
  return `<div class="strip">${cells}</div>`;
}
