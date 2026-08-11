// Schedules view: configured role/unit schedules + recent dispatch ledger.

import { esc, fmtDateTime, relTime, untilTime } from "../format.js";
import { icon } from "../icons.js";
import { empty, sectionHead, skeletonRows } from "../ui.js";

// Text steps: these are set on the outcome label, not on a mark.
const OUTCOME_COLOR = {
  fired: "var(--green-ink)",
  skipped_catchup: "var(--amber-ink)",
};

export function createSchedulesView({ store, api, navigate }) {
  let root;

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

  function render() {
    const data = store.state.schedules;
    if (!data) {
      root.innerHTML = skeletonRows(5);
      return;
    }
    const schedules = data.schedules || [];
    const runs = data.recent_runs || [];
    if (!schedules.length) {
      root.innerHTML = empty(
        "clock",
        "No schedules configured",
        "Add a schedules: block to a role or unit in your company config to run recurring work (standups, audits, nightly jobs).",
      );
      return;
    }

    const schedRows = schedules
      .map(
        (s) => `
      <tr style="${s.enabled ? "" : "opacity:.55"}">
        <td>${scopeBadge(s)}</td>
        <td>${esc(s.name)}${s.enabled ? "" : ' <span class="badge pending">disabled</span>'}</td>
        <td><span class="code-cron">${esc(s.cron)}</span></td>
        <td>${esc(s.timezone || "UTC")}</td>
        <td>${targetLabel(s)}</td>
        <td>${s.enabled ? nextRun(s.next_run) : "—"}</td>
      </tr>`,
      )
      .join("");

    const runRows = runs
      .map((r) => {
        const color = OUTCOME_COLOR[r.outcome] || "var(--text-muted)";
        return `
        <tr>
          <td class="mono" style="font-size:11px">${esc(fmtDateTime(r.fired_at || r.scheduled_at))}</td>
          <td><span style="color:${color}">● ${esc(r.outcome)}</span></td>
          <td>${esc(r.scope_id)} / ${esc(r.schedule_name)}</td>
          <td>${esc(r.target_handle || "—")}</td>
        </tr>`;
      })
      .join("");

    root.innerHTML = `
      ${sectionHead("clock", "Configured Schedules", schedules.length)}
      <div class="list tbl-wrap"><table class="tbl">
        <thead><tr><th>Scope</th><th>Name</th><th>Cron</th><th>TZ</th><th>Target</th><th>Next run</th></tr></thead>
        <tbody>${schedRows}</tbody>
      </table></div>
      ${sectionHead("activity", "Recent Runs", runs.length)}
      ${
        runs.length
          ? `<div class="list tbl-wrap"><table class="tbl">
              <thead><tr><th>When</th><th>Outcome</th><th>Scope / Schedule</th><th>Target</th></tr></thead>
              <tbody>${runRows}</tbody></table></div>`
          : empty("clock", "No runs yet")
      }`;
  }

  async function load() {
    const d = await api.schedules();
    if (d && !d._error) store.setSchedules(d);
    if (root && root.isConnected) render();
  }

  return {
    mount(el) {
      root = el;
      render();
      load();
    },
    onEvent(ev) {
      if (ev.type === "scheduled_task_fired") load();
    },
  };
}
