// Fleet view: every node, what it runs, and what it holds.
//
// /health answers about the node that served it, which behind a load
// balancer means a refresh tells a different story. This reads the lease
// table instead — one shared answer, from whichever node you happen to
// reach.

import { esc, relTime } from "../format.js";
import { empty, sectionHead, skeletonRows } from "../ui.js";

// The categorical hues, from the design system's mark steps. Not new
// classes: `.badge` ships info/purple/warn only, and a role chip is a
// mark, not a status.
const ROLE_HUE = {
  ingress: "var(--blue)",
  seats: "var(--green)",
  workers: "var(--purple)",
};

// A lease TTL is 45 s and a heartbeat is 15 s, so anything under one
// heartbeat of runway is a node whose renew is late — worth showing, not
// worth alarming about on its own.
const STALE_SECONDS = 15;

export function createFleetView({ store, api }) {
  let root;
  let data = null;

  function roleBadges(roles) {
    if (!roles || !roles.length) return '<span class="badge">none</span>';
    return roles
      .map(
        (r) =>
          `<span class="badge" style="background:${
            ROLE_HUE[r] || "var(--surface-2)"
          }">${esc(r)}</span>`,
      )
      .join(" ");
  }

  function labelChips(labels) {
    const keys = Object.keys(labels || {});
    if (!keys.length) return '<span style="color:var(--text-muted)">—</span>';
    return keys
      .sort()
      .map((k) => `<span class="code-cron">${esc(k)}=${esc(labels[k])}</span>`)
      .join(" ");
  }

  function configCell(node) {
    const status = node.config_status || "";
    const color =
      status === "ok"
        ? "var(--green-ink)"
        : status === "degraded"
          ? "var(--red)"
          : status === "error"
            ? "var(--amber-ink)"
            : "var(--text-muted)";
    const epoch = node.config_epoch ? `epoch ${node.config_epoch}` : "—";
    const label = status ? `${status} · ${epoch}` : epoch;
    const title = node.config_error ? ` title="${esc(node.config_error)}"` : "";
    return `<span style="color:${color}"${title}>${esc(label)}</span>`;
  }

  function ttlCell(seconds) {
    const left = Math.round(seconds || 0);
    const color = left < STALE_SECONDS ? "var(--amber-ink)" : "var(--text-muted)";
    return `<span style="color:${color}">${left}s</span>`;
  }

  // The two states worth interrupting for: a job nobody in the fleet is
  // doing, and a seat nobody is allowed to run. Both are absences —
  // nothing errors, every node looks healthy — so they go at the top.
  function alerts() {
    const out = [];
    for (const role of data.unmanned_roles || []) {
      out.push(
        `<div class="card" style="border-left:3px solid var(--red)">
           <b>No node runs <code>${esc(role)}</code>.</b>
           ${esc(UNMANNED_COST[role] || "")}
         </div>`,
      );
    }
    for (const seat of data.unplaceable || []) {
      out.push(
        `<div class="card" style="border-left:3px solid var(--amber-ink)">
           <b>${esc(seat.handle)}</b> is not being served — no live node
           matches its placement (<code>${esc(seat.placement)}</code>).
         </div>`,
      );
    }
    if (data.degraded) {
      out.push(
        `<div class="card" style="border-left:3px solid var(--amber-ink)">
           ${esc(data.degraded)}
         </div>`,
      );
    }
    return out.join("");
  }

  const UNMANNED_COST = {
    ingress: "No webhook reaches this company and the dashboard is unreachable from anywhere else.",
    seats: "Every trigger queues up unread.",
    workers:
      "Nothing fires on a schedule, no sandbox run is collected, and the retention sweeps do not run.",
  };

  function render() {
    if (!data) {
      root.innerHTML = skeletonRows(4);
      return;
    }
    const nodes = data.nodes || [];
    const seats = data.seats || [];
    const duties = data.duties || [];

    if (!nodes.length) {
      root.innerHTML = `${alerts()}${empty(
        "cpu",
        "No nodes registered",
        "Node presence lives in the lease table. Without a database configured, leases are process-local and there is no fleet to show.",
      )}`;
      return;
    }

    const nodeRows = nodes
      .map(
        (n) => `
      <tr>
        <td><b>${esc(n.id)}</b>${
          n.id === data.this_node
            ? ' <span class="badge info">this node</span>'
            : ""
        }</td>
        <td>${roleBadges(n.roles)}</td>
        <td>${labelChips(n.labels)}</td>
        <td>${n.seats}</td>
        <td>${configCell(n)}</td>
        <td>${ttlCell(n.expires_in)}</td>
      </tr>`,
      )
      .join("");

    const seatRows = seats
      .map(
        (s) => `
      <tr>
        <td>${esc(s.handle)}</td>
        <td>${esc(s.node)}</td>
        <td class="mono" style="font-size:11px">${s.epoch}</td>
        <td>${ttlCell(s.expires_in)}</td>
      </tr>`,
      )
      .join("");

    const dutyRows = duties
      .map(
        (d) => `
      <tr>
        <td>${esc(d.duty)}</td>
        <td>${esc(d.node)}</td>
        <td>${ttlCell(d.expires_in)}</td>
      </tr>`,
      )
      .join("");

    root.innerHTML = `
      ${alerts()}
      ${sectionHead("cpu", "Nodes", nodes.length)}
      <div class="list tbl-wrap"><table class="tbl">
        <thead><tr>
          <th>Node</th><th>Roles</th><th>Labels</th><th>Seats</th>
          <th>Config</th><th>Lease</th>
        </tr></thead>
        <tbody>${nodeRows}</tbody>
      </table></div>

      ${sectionHead("users", "Seat ownership", seats.length)}
      ${
        seats.length
          ? `<div class="list tbl-wrap"><table class="tbl">
               <thead><tr><th>Seat</th><th>Node</th><th>Epoch</th><th>Lease</th></tr></thead>
               <tbody>${seatRows}</tbody></table></div>`
          : empty("users", "No seats claimed")
      }

      ${sectionHead("clock", "Singleton duties", duties.length)}
      ${
        duties.length
          ? `<div class="list tbl-wrap"><table class="tbl">
               <thead><tr><th>Duty</th><th>Node</th><th>Lease</th></tr></thead>
               <tbody>${dutyRows}</tbody></table></div>`
          : empty(
              "clock",
              "No duties held",
              "Duties are claimed per tick, so an empty table between ticks is normal — a persistently empty one is not.",
            )
      }
      <p style="color:var(--text-muted);font-size:12px;margin-top:12px">
        Read from the lease table, so it is the same answer from every
        node. In-flight turn counts are per process and stay on
        <code>/health</code>. Refreshed ${esc(relTime(new Date().toISOString()))}.
      </p>`;
  }

  async function load() {
    const d = await api.fleet();
    if (d && !d._error) data = d;
    if (root && root.isConnected) render();
  }

  let timer = null;

  return {
    mount(el) {
      root = el;
      render();
      load();
      // Leases move on their own — a node dies, a seat is taken over —
      // with no event to listen for, so this view polls. A lease TTL is
      // 45 s and a heartbeat 15 s; polling on the heartbeat means a
      // handover is visible within one tick of happening.
      timer = setInterval(load, 15000);
    },
    destroy() {
      if (timer) clearInterval(timer);
      timer = null;
    },
  };
}
