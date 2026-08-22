// Fleet view: every node, what it runs, what it holds, and how far
// behind it is.
//
// `/health` answers about the node that served it, which behind a load
// balancer means a refresh tells a different story. This reads the lease
// table instead — one shared answer, from whichever node you happen to
// reach.
//
// It was also, for a long time, the one view outside the shell's
// architecture: it took an HTTP client from a context field nothing
// populated, and it painted with `innerHTML` and returned no
// `render(state)` at all — so the shell's own `patch(root,
// active.render(...))` threw on mount, in every build. Both halves are
// gone. Reads go over the socket like every other view's, and this
// returns markup the shell patches, so a lease moving re-keys one row
// instead of rebuilding two tables under the reader's cursor.

import { esc, escAttr, relTime } from "../format.js";
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

// A node re-stamps its apply row every reconcile tick (15 s). One that
// has not for four ticks is not lagging, it has stopped reporting, and
// its row is a fact about the past rather than the present.
const REPORT_SILENT_SECONDS = 60;

const UNMANNED_COST = {
  ingress:
    "No webhook reaches this company and the dashboard is unreachable from anywhere else.",
  seats: "Every trigger queues up unread.",
  workers:
    "Nothing fires on a schedule, no sandbox run is collected, and the retention sweeps do not run.",
};

export function createFleetView({ query, refresh }) {
  let data = null;
  // When `data` was actually read, and whether the last read failed.
  //
  // The footer used to stamp `new Date()` at RENDER time, so a view
  // rendering month-old leases said "Refreshed just now" — and a failed
  // poll was swallowed silently, so the one moment this view is most
  // likely to be looked at (nodes dying, API struggling) is the moment
  // it most confidently lied. Same defect the health surface documents
  // at length: a page that cannot see the engine must not report on the
  // engine's behalf.
  let fetchedAt = null;
  let stale = false;
  let timer = null;
  let disposed = false;

  async function load() {
    try {
      data = await query("fleet");
      fetchedAt = new Date().toISOString();
      stale = false;
    } catch {
      // Keep what we have — the last fleet is the only thing the
      // operator still has — but stop claiming it is current. Every
      // failure lands here: a socket that is down, a lease store that
      // cannot be read, a query that timed out.
      stale = true;
    }
    if (!disposed) refresh();
  }

  function roleBadges(roles) {
    if (!roles || !roles.length) return '<span class="badge">none</span>';
    return roles
      .map(
        (r) =>
          `<span class="badge" style="background:${
            ROLE_HUE[r] || "var(--bg-card-2)"
          }">${esc(r)}</span>`,
      )
      .join(" ");
  }

  function labelChips(labels) {
    const keys = Object.keys(labels || {});
    if (!keys.length) return '<span class="zero">—</span>';
    return keys
      .sort()
      .map((k) => `<span class="code-cron">${esc(k)}=${esc(labels[k])}</span>`)
      .join(" ");
  }

  // Where this node is against where the fleet is heading.
  //
  // An epoch on its own is a number with nothing to compare it to. Lag
  // alone is also not a fault — every successful rollout produces it —
  // so this reports the distance and leaves the reading to whoever is
  // looking, except where the node has stopped reporting at all.
  function configCell(node, target) {
    const status = node.config_status || "";
    const epoch = Number(node.config_epoch || 0);
    const behind = target && epoch ? target - epoch : 0;
    const color =
      status === "degraded"
        ? "var(--red-ink)"
        : status === "error"
          ? "var(--amber-ink)"
          : status === "ok"
            ? "var(--green-ink)"
            : "var(--text-muted)";

    const silentFor = reportAge(node);
    const parts = [];
    parts.push(
      `<span style="color:${color}"${
        node.config_error ? ` title="${escAttr(node.config_error)}"` : ""
      }>${esc(status || "unknown")}</span>`,
    );
    parts.push(
      `<span class="mono">${epoch ? `epoch ${epoch}` : "—"}${
        behind > 0 ? ` · ${behind} behind` : ""
      }</span>`,
    );
    if (silentFor !== null && silentFor > REPORT_SILENT_SECONDS) {
      parts.push(
        `<span class="caution-ink">stopped reporting ${esc(relTime(node.config_reported_at))}</span>`,
      );
    }
    return `<div class="fleet-config">${parts.join("")}</div>`;
  }

  function reportAge(node) {
    if (!node.config_reported_at) return null;
    const at = Date.parse(node.config_reported_at);
    if (Number.isNaN(at)) return null;
    return (Date.now() - at) / 1000;
  }

  function ttlCell(seconds) {
    const left = Math.round(seconds || 0);
    const color = left < STALE_SECONDS ? "var(--amber-ink)" : "var(--text-muted)";
    return `<span style="color:${color}">${left}s</span>`;
  }

  // The states worth interrupting for: a job nobody in the fleet is
  // doing, and a seat nobody is allowed to run. Both are absences —
  // nothing errors, every node looks healthy — so they go at the top.
  function alerts() {
    const out = [];
    for (const role of data.unmanned_roles || []) {
      out.push(
        `<div class="card fleet-alert is-bad" data-k="unmanned:${escAttr(role)}">
           <b>No node runs <code>${esc(role)}</code>.</b>
           ${esc(UNMANNED_COST[role] || "")}
         </div>`,
      );
    }
    for (const seat of data.unplaceable || []) {
      out.push(
        `<div class="card fleet-alert is-warn" data-k="unplaceable:${escAttr(seat.handle)}">
           <b>${esc(seat.handle)}</b> is not being served — no live node
           matches its placement (<code>${esc(seat.placement)}</code>).
         </div>`,
      );
    }
    if (stale) {
      out.push(
        `<div class="card fleet-alert is-warn" data-k="stale">
           <b>This view is not live.</b> The last read of the lease table
           failed, so everything below is the fleet as it was, not as it
           is.
         </div>`,
      );
    }
    if (data.degraded) {
      out.push(
        `<div class="card fleet-alert is-warn" data-k="degraded">${esc(data.degraded)}</div>`,
      );
    }
    return out.join("");
  }

  // What the footer says about the data's age, and whether to say it at
  // all. Read from when the fetch LANDED, never from render time.
  function freshness() {
    if (!fetchedAt) return "";
    const when = esc(relTime(fetchedAt));
    return stale
      ? `<b class="caution-ink">Could not refresh</b> — showing the fleet as of ${when}.`
      : `Refreshed ${when}.`;
  }

  return {
    // No store slice: leases move on their own, with no event to push.
    // The view re-renders when its own poll lands.
    slices: [],

    mount() {
      load();
      // A lease TTL is 45 s and a heartbeat 15 s; polling on the
      // heartbeat means a handover is visible within one tick of
      // happening.
      timer = setInterval(load, 15000);
    },

    destroy() {
      disposed = true;
      if (timer) clearInterval(timer);
      timer = null;
    },

    render() {
      if (!data) {
        // A skeleton means "loading". If the first read already failed
        // there is nothing still loading, and a spinner that never
        // resolves is the most patient lie a page can tell.
        return stale
          ? empty(
              "cpu",
              "Could not read the fleet",
              "The fleet query failed. The lease table is the only source for this view, so there is nothing to fall back to — retrying every 15s.",
            )
          : skeletonRows(4);
      }

      const nodes = data.nodes || [];
      const seats = data.seats || [];
      const duties = data.duties || [];
      const target = Number(data.target_epoch || 0);

      if (!nodes.length) {
        return `${alerts()}${empty(
          "cpu",
          "No nodes registered",
          "Node presence lives in the lease table. Without a database configured, leases are process-local and there is no fleet to show.",
        )}`;
      }

      const nodeRows = nodes
        .map(
          (n) => `
        <tr data-k="node:${escAttr(n.id)}">
          <td><b>${esc(n.id)}</b>${
            n.id === data.this_node
              ? ' <span class="badge info">this node</span>'
              : ""
          }</td>
          <td>${roleBadges(n.roles)}</td>
          <td>${labelChips(n.labels)}</td>
          <td class="num">${esc(String(n.seats))}</td>
          <td>${configCell(n, target)}</td>
          <td>${ttlCell(n.expires_in)}</td>
        </tr>`,
        )
        .join("");

      const seatRows = seats
        .map(
          (s) => `
        <tr data-k="seat:${escAttr(s.handle)}" class="clickable"
            data-action="seat" data-seat="${escAttr(s.handle)}">
          <td>${esc(s.handle)}</td>
          <td>${esc(s.node)}</td>
          <td class="mono num">${esc(String(s.epoch))}</td>
          <td>${ttlCell(s.expires_in)}</td>
        </tr>`,
        )
        .join("");

      const dutyRows = duties
        .map(
          (d) => `
        <tr data-k="duty:${escAttr(d.duty)}">
          <td>${esc(d.duty)}</td>
          <td>${esc(d.node)}</td>
          <td>${ttlCell(d.expires_in)}</td>
        </tr>`,
        )
        .join("");

      return `
        ${alerts()}
        ${sectionHead("cpu", "Nodes", nodes.length)}
        ${
          target
            ? `<div class="fleet-target">The fleet is converging on <b>epoch ${esc(String(target))}</b>. Lag alone is not a fault — every successful rollout produces it.</div>`
            : ""
        }
        <div class="list tbl-wrap"><table class="tbl">
          <thead><tr>
            <th>Node</th><th>Roles</th><th>Labels</th><th class="num">Seats</th>
            <th>Config</th><th>Lease</th>
          </tr></thead>
          <tbody>${nodeRows}</tbody>
        </table></div>

        ${sectionHead("users", "Seat ownership", seats.length)}
        ${
          seats.length
            ? `<div class="list tbl-wrap"><table class="tbl">
                 <thead><tr><th>Seat</th><th>Node</th><th class="num">Epoch</th><th>Lease</th></tr></thead>
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
        <p class="fleet-foot">
          Read from the lease table, so it is the same answer from every
          node. In-flight turn counts are per process and stay on
          <code>/health</code>. ${freshness()}
        </p>`;
    },

    // One poll, awaited. The interval cannot be awaited and a test that
    // slept for it would be a 15-second test; this is the same `load`
    // the timer calls, exposed so the refresh paths are assertable.
    __loadForTest: load,
  };
}
