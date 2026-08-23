// One trace, end to end.
//
// A turn's story is spread across a dozen events — the notification that
// woke the agent, each phase, the tools, the sub-agents, the task
// completion — and until now the only way to read it was to scroll the
// Activity feed and recognise them by eye. Every one of those events
// already carries the trace id that binds them, and the server has
// answered `trace` queries since the query channel was built; nothing
// asked.

import { esc, escAttr, fmtDateTime, fmtDuration, oldestFirst } from "../format.js";
import { icon } from "../icons.js";
import { empty, sectionHead, skeletonRows } from "../ui.js";
import { arrangeSpans, traceLabel, traceNode } from "../traceNodes.js";

// The server caps a trace at this many rows, keeping the oldest — the
// root is what explains a trace. Seeing exactly this many means the tail
// was cut, and the view has to say so rather than let a reader believe
// the turn simply stopped.
const MAX_TRACE_ROWS = 500;

export function createTraceView({ query, navigate, refresh, params, store }) {
  const traceId = params.trace_id || "";
  let rows = null;
  let loading = true;
  let loadError = "";
  let disposed = false;

  async function load() {
    try {
      rows = await query("trace", { trace_id: traceId });
    } catch (err) {
      loadError = err.message;
    }
    loading = false;
    if (!disposed) refresh();
  }

  // A trace opened while its turn is still running keeps growing. The
  // live feed carries the same events, so the view listens rather than
  // polling — and dedupes by id, because a row can arrive here and also
  // be in the page the query already answered.
  function onEvent(ev) {
    if (!rows || !ev || ev.trace_id !== traceId) return;
    if (rows.some((r) => r.id === ev.id)) return;
    rows = [...rows, ev].sort(oldestFirst);
    refresh();
  }

  function header() {
    const failures = rows.filter((r) => r.failed).length;
    const span = rows.length
      ? fmtDuration(rows[0].timestamp, rows[rows.length - 1].timestamp)
      : "";
    return `
      <div class="card trace-head-card" data-k="head">
        <div class="trace-head-row">
          <span class="eyebrow">Trace</span>
          <span class="mono trace-id-full">${esc(traceLabel(traceId))}</span>
          <span style="flex:1"></span>
          <span class="row-ts">${esc(fmtDateTime(rows[0] && rows[0].timestamp))}</span>
        </div>
        <div class="trace-head-facts">
          <span>${rows.length} event${rows.length === 1 ? "" : "s"}</span>
          ${span ? `<span>${esc(span)}</span>` : ""}
          ${
            failures
              ? `<span class="warn-ink">${failures} failed</span>`
              : '<span class="ok-ink">no failures</span>'
          }
        </div>
        ${
          rows.length >= MAX_TRACE_ROWS
            ? `<div class="trace-truncated">Showing the first ${MAX_TRACE_ROWS} events of this trace — the tail is not displayed.</div>`
            : ""
        }
      </div>`;
  }

  return {
    slices: [],

    mount() {
      load();
    },

    onEvent,

    render() {
      const back = `<div class="back-link" data-action="back">${icon("chevron", "sm")} Back to activity</div>`;
      if (loading) {
        return back + skeletonRows(6);
      }
      if (loadError) {
        return (
          back +
          empty(
            "inbox",
            loadError === "no_event_store"
              ? "No event store configured"
              : "Could not load this trace",
            loadError === "no_event_store"
              ? "Traces are read from the event store; this deployment has none, so only the live feed is available."
              : "",
          )
        );
      }
      if (!rows || !rows.length) {
        return (
          back +
          empty(
            "inbox",
            "Trace not found",
            "It may have aged out of the event store's retention window.",
          )
        );
      }
      const arranged = arrangeSpans(rows);
      return `
        ${back}
        ${header()}
        ${sectionHead("activity", "Events", rows.length, null)}
        <div class="list trace-list">
          ${arranged
            .map(({ event, depth }) =>
              traceNode(event, { depth, keyPrefix: "tn" }),
            )
            .join("")}
        </div>`;
    },

    onAction(action, target) {
      if (action === "back") {
        navigate("/events");
        return;
      }
      if (action === "open") {
        navigate("/events/" + encodeURIComponent(target.dataset.id));
      }
    },

    destroy() {
      disposed = true;
    },
  };
}
