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

  // What the traffic figures are allowed to claim.
  //
  // `since` rather than a fixed window: the count is over the most recent
  // page of deliveries, not the last N hours, so naming a window would be a
  // number the server never measured.
  // What BECAME of the deliveries, beside how many arrived.
  //
  // "128 inbound" alone cannot tell a working integration from one whose
  // every delivery reaches nobody. The two outcome counts are three-valued —
  // null means this node could not read its event log — so an absent one is
  // omitted rather than rendered as a zero the operator would act on. Both
  // zero renders nothing either: a line saying "0 dropped, 0 merged" on every
  // healthy integration is noise that trains a reader to skip the whole line.
  function outcomes(row) {
    const parts = [];
    if (typeof row.skipped === "number" && row.skipped > 0) {
      parts.push(`${esc(fmtNum(row.skipped))} dropped`);
    }
    if (typeof row.coalesced === "number" && row.coalesced > 0) {
      parts.push(`${esc(fmtNum(row.coalesced))} merged`);
    }
    return parts.length ? ` (${parts.join(", ")})` : "";
  }

  function trafficLine(row, known, since) {
    if (!known) {
      return `<span class="int-quiet">no event store — traffic cannot be counted</span>`;
    }
    if (row.last_at) {
      const span = since ? ` since ${esc(relTime(since))}` : "";
      return `<span class="int-live">${esc(fmtNum(row.inbound))} inbound${span}${outcomes(row)} · last ${esc(relTime(row.last_at))}</span>`;
    }
    return `<span class="int-quiet">nothing has arrived — which is not the same as broken</span>`;
  }

  // Whether a delivery from this surface can WAKE A SEAT.
  //
  // Verifying and storing a delivery is the first half; a parser turning it
  // into a notification is the second, and a vendor can have one without the
  // other. Those render identically everywhere else — configured, secret
  // present, deliveries arriving — so this is the only place an operator can
  // see that a surface is ingesting into a void.
  //
  // Null means the server could not say (a standalone API has no engine to
  // ask), which must not render as "no".
  function routingRow(row) {
    if (row.routes === null || row.routes === undefined) {
      return `<div class="int-kv"><span>Routing</span><span class="zero">not visible from this process</span></div>`;
    }
    return `<div class="int-kv"><span>Routing</span>${
      row.routes
        ? `<span>wakes the seats it concerns</span>`
        : `<span class="warn-ink">ingest only — deliveries are verified and stored, and wake nobody</span>`
    }</div>`;
  }

  // Whether a delivery from this surface can be VERIFIED.
  //
  // Two facts, and the gap between them is the failure worth showing. A
  // secret lives in the config as a ${VAR}: `secret_present` says an
  // operator wrote one down, `secret_usable` says this process resolved it
  // to something a route can check a signature with. An unset variable is
  // present-and-unusable — and from everywhere else it looks fine: the
  // config shows a secret, the vendor's settings page shows a healthy hook,
  // and every delivery is refused with nothing naming the variable.
  //
  // `secret_usable` null means the server could not say (a standalone API
  // has no engine to ask), which must not render as "no".
  function secretRow(row) {
    if (row.secret_present === null || row.secret_present === undefined) {
      // A surface with no shared secret is not a surface missing one.
      return `<div class="int-kv"><span>Verification</span><span>the seat's own token</span></div>`;
    }
    if (!row.secret_present) {
      return `<div class="int-kv"><span>Signing secret</span><span class="warn-ink">missing — every delivery is being turned away with a 503</span></div>`;
    }
    if (row.secret_usable === false) {
      return `<div class="int-kv"><span>Signing secret</span><span class="warn-ink">configured, but it did not resolve to a usable key — every delivery is being turned away with a 503</span></div>`;
    }
    return `<div class="int-kv"><span>Signing secret</span>${
      row.secret_usable
        ? `<span>configured and resolved</span>`
        : `<span>configured<span class="zero"> — whether it resolved is not visible from this process</span></span>`
    }</div>`;
  }

  function card(row, known, since) {
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
          ${row.configured ? trafficLine(row, known, since) : ""}
        </div>
        ${
          row.configured
            ? `<div class="int-body">
                 <div class="int-kv"><span>Inbound</span><span class="mono">${
                   row.inbound_kind === "websocket"
                     ? "one websocket per seat — no public URL needed"
                     : esc(row.inbound_path)
                 }</span></div>
                 ${secretRow(row)}
                 ${routingRow(row)}
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
          .map((r) => card(r, data.traffic_known, data.traffic_since))
          .join("")}</div>
        ${
          rest.length
            ? sectionHead("plug", "Available", rest.length) +
              `<div class="int-grid">${rest
                .map((r) => card(r, data.traffic_known, data.traffic_since))
                .join("")}</div>`
            : ""
        }`;
    },

    __loadForTest: load,
  };
}
