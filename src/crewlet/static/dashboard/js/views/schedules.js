// Schedules view: configured role/unit schedules + recent dispatch ledger.
//
// The configured list rides in on the snapshot, so the table paints on the
// first frame with no request of its own. The dispatch ledger is the only
// half that needs asking for — it is a database read the snapshot
// deliberately does not carry — so it is queried on mount and re-queried
// whenever a schedule actually fires.

import { esc, escAttr, fmtDateTime, untilTime } from "../format.js";
import { empty, sectionHead, skeletonRows } from "../ui.js";

// Text steps: these are set on the outcome label, not on a mark.
const OUTCOME_COLOR = {
  fired: "var(--green-ink)",
  skipped_catchup: "var(--amber-ink)",
};

// How long to wait before asking for the ledger again after a failed
// query. The server swallows its own ledger read errors and answers with
// an empty list, so a rejection here means the socket is down — which is
// exactly what socket.js polls through at FALLBACK_MS. Matching it keeps
// the history on screen a beat after a reconnect, without a dashboard
// left open against a stopped engine doing anything but idling.
const RETRY_MS = 5_000;

// Placeholder rows standing in for the ledger while it loads. The tail is
// short by design (the section is recent activity, not an audit log), so
// three rows is enough to hold the space without faking a full table.
const RUNS_SKELETON_ROWS = 3;

export function createSchedulesView({ query, refresh }) {
  // Dispatch history lives here rather than in the store: no other view
  // reads it, and it is the one thing on this page the snapshot omits.
  let runs = null;
  let inFlight = false;
  let reloadQueued = false;
  let retryTimer = 0;
  let disposed = false;

  function scopeBadge(s) {
    const cls = s.scope_type === "unit" ? "purple" : "info";
    return `<span class="badge ${cls}">${esc(s.scope_type)}: ${esc(s.scope_id)}</span>`;
  }

  function targetLabel(s) {
    if (s.scope_type === "role") return "self";
    const runners = (s.runners || []).join(", ");
    return `${esc(s.target)}${runners ? ` → ${esc(runners)}` : ""}`;
  }

  function nextRun(iso) {
    if (!iso) return "—";
    return `${fmtDateTime(iso)} · <span style="color:var(--text-muted)">${untilTime(iso)}</span>`;
  }

  // A schedule is identified by where it is declared plus its name — the
  // triple the engine itself keys the dispatch ledger by.
  function scheduleKey(s) {
    return `${s.scope_type}|${s.scope_id}|${s.name}`;
  }

  // A ledger row carries no id, so its identity is the dispatch it
  // records: one schedule, one scheduled slot. `trace_id` is that
  // identity when the run produced a trace; the slot itself is the
  // fallback for a row that never got one (a skipped catch-up).
  function runKey(r) {
    return (
      r.trace_id ||
      `${r.scope_id}|${r.schedule_name}|${r.scheduled_at || r.fired_at}`
    );
  }

  // ---- data loading ----
  async function loadRuns() {
    if (disposed) return;
    // A burst of fires must not open a query per event; the last one
    // still gets its refresh because the tail re-runs once.
    if (inFlight) {
      reloadQueued = true;
      return;
    }
    inFlight = true;
    clearTimeout(retryTimer);
    retryTimer = 0;
    try {
      const data = await query("schedules", {});
      runs = data.recent_runs || [];
      if (!disposed) refresh();
    } catch {
      // Offline or timed out. The section keeps its skeleton — the load
      // genuinely is still in progress — and asks again shortly. A
      // rejection arriving after the reader has navigated away arms no
      // retry: that poll would outlive the view it was reading for.
      if (!disposed) retryTimer = setTimeout(loadRuns, RETRY_MS);
    } finally {
      inFlight = false;
      if (reloadQueued && !disposed) {
        reloadQueued = false;
        loadRuns();
      }
    }
  }

  return {
    slices: ["schedules", "health"],

    mount() {
      loadRuns();
    },

    render(state) {
      const schedules = state.schedules;
      // `null` until the snapshot lands. An org that configures no
      // schedules at all still arrives as an empty array, so "not here
      // yet" and "genuinely none" are distinguishable without consulting
      // `state.connected`.
      if (!schedules) return skeletonRows(5);
      if (!schedules.length) {
        return empty(
          "clock",
          "No schedules configured",
          "Add a schedules: block to a role or unit in your company config to run recurring work (standups, audits, nightly jobs).",
        );
      }

      const schedRows = schedules
        .map(
          (s) => `
      <tr data-k="sch:${escAttr(scheduleKey(s))}" style="${s.enabled ? "" : "opacity:.55"}">
        <td>${scopeBadge(s)}</td>
        <td>${esc(s.name)}${s.enabled ? "" : ' <span class="badge pending">disabled</span>'}</td>
        <td><span class="code-cron">${esc(s.cron)}</span></td>
        <td>${esc(s.timezone || "UTC")}</td>
        <td>${targetLabel(s)}</td>
        <td>${s.enabled ? nextRun(s.next_run) : "—"}</td>
      </tr>`,
        )
        .join("");

      const runRows = (runs || [])
        .map((r) => {
          const color = OUTCOME_COLOR[r.outcome] || "var(--text-muted)";
          return `
        <tr data-k="run:${escAttr(runKey(r))}">
          <td class="mono" style="font-size:11px">${esc(fmtDateTime(r.fired_at || r.scheduled_at))}</td>
          <td><span style="color:${color}">● ${esc(r.outcome)}</span></td>
          <td>${esc(r.scope_id)} / ${esc(r.schedule_name)}</td>
          <td>${esc(r.target_handle || "—")}</td>
        </tr>`;
        })
        .join("");

      // The ledger's count is unknown until it lands, and `sectionHead`
      // drops the pill for a null count rather than claiming zero.
      const runsSection =
        runs === null
          ? skeletonRows(RUNS_SKELETON_ROWS)
          : runs.length
            ? `<div class="list tbl-wrap"><table class="tbl">
              <thead><tr><th>When</th><th>Outcome</th><th>Scope / Schedule</th><th>Target</th></tr></thead>
              <tbody>${runRows}</tbody></table></div>`
            : empty("clock", "No runs yet");

      return `
      ${sectionHead("clock", "Configured Schedules", schedules.length)}
      <div class="list tbl-wrap"><table class="tbl">
        <thead><tr><th>Scope</th><th>Name</th><th>Cron</th><th>TZ</th><th>Target</th><th>Next run</th></tr></thead>
        <tbody>${schedRows}</tbody>
      </table></div>
      ${sectionHead("activity", "Recent Runs", runs === null ? null : runs.length)}
      ${runsSection}`;
    },

    // The ledger is the only thing a fire changes that is not already
    // pushed, so exactly one event type re-reads it.
    onEvent(ev) {
      if (ev.type === "scheduled_task_fired") loadRuns();
    },

    destroy() {
      // An in-flight query cannot be cancelled, so the flag is what stops
      // its reply from refreshing, retrying, or re-queueing against a page
      // nobody is looking at any more.
      disposed = true;
      clearTimeout(retryTimer);
      retryTimer = 0;
      reloadQueued = false;
    },
  };
}
