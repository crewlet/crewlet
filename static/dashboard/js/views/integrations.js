// Integrations: how work reaches this company, and what happened to it.
//
// The dashboard branded an event once it had already been accepted and
// routed, which meant every failure mode an operator actually hits was
// invisible. Rejected deliveries are never written to the event store —
// verification runs before the row is logged, which is correct — so a
// mis-pasted signing secret left no trace anywhere but the provider's
// own delivery UI, and the engine looked idle.
//
// Two rules this room keeps, because they are the difference between a
// useful panel and a confident one:
//
//   Silence is not health. An idle Slack and a 401-ing Slack are
//   indistinguishable in the event store, so no traffic is reported as
//   "no traffic seen" — never as "down", and never as "healthy".
//
//   A missing secret IS a finding. A webhook route with no secret
//   configured answers 503 to every delivery, which the sender sees as
//   a retry and the operator sees as nothing at all.

import { esc, escAttr, fmtNum, relTime } from "../format.js";
import { icon } from "../icons.js";
import { integrationMeta } from "../state.js";
import { empty, emptyOrPending, sectionHead, skeletonCards } from "../ui.js";

const POLL_MS = 60_000;

export function createIntegrationsView({ query, refresh }) {
  let data = null;
  let failed = false;
  let timer = null;
  let disposed = false;

  async function load() {
    try {
      data = await query("integrations");
      failed = false;
    } catch {
      failed = true;
    }
    if (!disposed) refresh();
  }

  // The routing funnel as one bar: what arrived, and what the notification
  // service did with it. Every segment prints its own count, because a
  // proportion with no absolute figure cannot tell "40% skipped of ten"
  // from "40% skipped of ten thousand".
  function funnel(row) {
    const routed = row.routed || 0;
    const skipped = row.skipped || 0;
    const coalesced = row.coalesced || 0;
    const total = routed + skipped + coalesced;
    if (!total) return "";
    const seg = (n, hue, label) =>
      n
        ? `<span class="fn-seg" style="flex:${n};background:color-mix(in srgb, var(--${hue}) 55%, transparent)"
                 title="${esc(label)}: ${esc(fmtNum(n))}"></span>`
        : "";
    return `
      <div class="fn-bar">
        ${seg(routed, "green", "routed")}
        ${seg(skipped, "amber", "skipped")}
        ${seg(coalesced, "cyan", "coalesced")}
      </div>
      <div class="fn-legend">
        ${routed ? `<span><i style="background:var(--green)"></i>${esc(fmtNum(routed))} routed</span>` : ""}
        ${skipped ? `<span><i style="background:var(--amber)"></i>${esc(fmtNum(skipped))} skipped</span>` : ""}
        ${coalesced ? `<span><i style="background:var(--cyan)"></i>${esc(fmtNum(coalesced))} coalesced</span>` : ""}
      </div>`;
  }

  // What the traffic figures are allowed to claim.
  function trafficLine(row, known, windowHours) {
    if (!known) {
      return `<span class="int-quiet">no event store — traffic cannot be counted</span>`;
    }
    if (row.last_at) {
      return `<span class="int-live">${esc(fmtNum(row.inbound))} inbound in ${windowHours}h · last ${esc(relTime(row.last_at))}</span>`;
    }
    return `<span class="int-quiet">no traffic seen in ${windowHours}h — which is not the same as broken</span>`;
  }

  function secretRow(row) {
    if (row.secret_present === null || row.secret_present === undefined) {
      // A surface with no shared secret is not a surface missing one.
      return `<div class="int-kv"><span>Verification</span><span>the seat's own token</span></div>`;
    }
    return `<div class="int-kv"><span>Signing secret</span>${
      row.secret_present
        ? `<span>configured</span>`
        : `<span class="warn-ink">missing — every delivery is being turned away with a 503</span>`
    }</div>`;
  }

  function card(row, known, windowHours) {
    const meta = integrationMeta(row.key);
    return `
      <div class="int-card ${row.configured ? "" : "is-off"}" data-k="int:${escAttr(row.key)}">
        <div class="int-head">
          <span class="int-badge" style="--int-color:${meta.color}">
            ${icon(meta.icon, "sm")}${esc(meta.label)}
          </span>
          ${
            row.configured
              ? row.enabled
                ? ""
                : `<span class="badge">disabled</span>`
              : `<span class="badge">not configured</span>`
          }
          <span style="flex:1"></span>
          ${row.configured ? trafficLine(row, known, windowHours) : ""}
        </div>
        ${row.configured ? funnel(row) : ""}
        ${
          row.configured
            ? `<div class="int-body">
                 <div class="int-kv"><span>Inbound</span><span class="mono">${
                   row.inbound_kind === "websocket"
                     ? "one websocket per seat — no public URL needed"
                     : esc(row.inbound_path)
                 }</span></div>
                 ${secretRow(row)}
                 ${row.url ? `<div class="int-kv"><span>Host</span><span class="mono">${esc(row.url)}</span></div>` : ""}
                 ${row.workspace ? `<div class="int-kv"><span>Workspace</span><span class="mono">${esc(row.workspace)}</span></div>` : ""}
                 <div class="int-kv"><span>Seats</span><span>${
                   row.seats.length
                     ? row.seats
                         .map(
                           (h) =>
                             `<span class="chip clickable" data-action="seat" data-seat="${escAttr(h)}">${esc(h)}</span>`,
                         )
                         .join(" ")
                     : `<span class="zero">none carry credentials for this surface</span>`
                 }</span></div>
               </div>`
            : `<div class="int-body int-off">Nothing in the active configuration wires this surface up.</div>`
        }
      </div>`;
  }

  return {
    slices: ["health", "connected", "org"],

    mount() {
      load();
      timer = setInterval(load, POLL_MS);
    },

    destroy() {
      disposed = true;
      if (timer) clearInterval(timer);
      timer = null;
    },

    render(state) {
      if (data === null) {
        return failed
          ? empty(
              "globe",
              "Could not read integrations",
              "The query failed — retrying every minute.",
            )
          : skeletonCards(3);
      }
      const rows = data.integrations || [];
      const configured = rows.filter((r) => r.configured);
      const rest = rows.filter((r) => !r.configured);
      if (!configured.length) {
        return emptyOrPending(
          state,
          skeletonCards(3),
          "globe",
          "No integrations configured",
          "Agents reach the outside world through chat, a tracker, a knowledge base and a code host. Wire one up in the company configuration.",
        );
      }
      return `
        ${sectionHead("globe", "Connected", configured.length)}
        <div class="int-grid">${configured
          .map((r) => card(r, data.traffic_known, data.window_hours))
          .join("")}</div>
        ${
          rest.length
            ? sectionHead("plug", "Available", rest.length) +
              `<div class="int-grid">${rest
                .map((r) => card(r, data.traffic_known, data.window_hours))
                .join("")}</div>`
            : ""
        }`;
    },

    __loadForTest: load,
  };
}
