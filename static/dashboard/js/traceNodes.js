// One event, rendered as a node.
//
// Three surfaces show the same thing — Activity's trace tree, the trace
// view, and the agent page's event history — and they showed it three
// slightly different ways because the markup lived inline in the one
// view that had it first. A node's dot, its category chip, whether it
// is inspectable and whether it is marked failed are properties of the
// event, not of the screen it happens to be on.

import { esc, escAttr, relTime, shortId } from "./format.js";
import { catClass } from "./state.js";
import { eventSummary } from "./ui.js";

// Event types whose stored payload is worth opening on its own screen.
// Everything else has already said all it has to say in its summary, and
// an "inspect" affordance that leads to a bare JSON dump is a promise
// the detail page cannot keep.
const A2A_TYPES = new Set([
  "a2a_channel_opened",
  "a2a_message_sent",
  "a2a_message_delivered",
  "a2a_channel_closed",
]);

export function isInspectable(ev) {
  return (
    ev.type === "agent_turn_completed" ||
    ev.type === "agent_phase_completed" ||
    A2A_TYPES.has(ev.type) ||
    ev.category === "webhook" ||
    ev.category === "notification"
  );
}

/**
 * One event row inside a trace or a history list.
 *
 * `depth` indents a node under its parent span. `inspectable` is
 * resolved by the caller rather than here, because a surface with no
 * event store behind it must not offer a link that can only dead-end.
 */
export function traceNode(ev, { depth = 0, inspectable = true, keyPrefix = "n" } = {}) {
  const skipped = ev.type === "notification_skipped";
  const canOpen = inspectable && isInspectable(ev);
  const cls = [
    "trace-child",
    canOpen ? "clickable" : "",
    skipped ? "skipped" : "",
    ev.failed ? "is-failed" : "",
  ]
    .filter(Boolean)
    .join(" ");
  return `
    <div class="${cls}" data-k="${escAttr(keyPrefix)}:${escAttr(ev.id)}"
         ${depth ? `style="--depth:${depth}"` : ""}
         ${canOpen ? `data-action="open" data-id="${escAttr(ev.id)}"` : ""}>
      <span class="node-dot ${catClass(ev.category)}" style="width:7px;height:7px"></span>
      <span class="trace-summary">${eventSummary(ev)}</span>
      <div class="trace-meta">
        ${canOpen ? `<span class="inspect ${catClass(ev.category)}">inspect →</span>` : ""}
        <span class="evt-cat ${catClass(ev.category)}">${esc(ev.type)}</span>
        <span class="row-ts" data-ts="${escAttr(ev.timestamp)}">${esc(relTime(ev.timestamp))}</span>
      </div>
    </div>`;
}

/**
 * Arrange a trace's events into parent/child order by span.
 *
 * Returns `[{ event, depth }]` in render order. Rows arrive oldest-first
 * and that order is preserved among siblings, so the sequence still
 * reads causally; the indent only shows what nested under what.
 *
 * A span whose parent is not in the list is rendered at the root rather
 * than dropped — a trace can be truncated, or a parent can have been
 * written by a process whose events did not survive.
 */
export function arrangeSpans(events) {
  const bySpan = new Map();
  for (const ev of events) {
    if (ev.span_id && !bySpan.has(ev.span_id)) bySpan.set(ev.span_id, ev);
  }
  const children = new Map();
  const roots = [];
  for (const ev of events) {
    const parent = ev.parent_span_id;
    if (parent && bySpan.has(parent) && bySpan.get(parent) !== ev) {
      if (!children.has(parent)) children.set(parent, []);
      children.get(parent).push(ev);
    } else {
      roots.push(ev);
    }
  }
  const out = [];
  // Iterative rather than recursive: a pathological trace (a long
  // delegation chain) would otherwise be able to blow the stack on a
  // page that is only trying to draw it.
  const stack = roots
    .slice()
    .reverse()
    .map((ev) => ({ ev, depth: 0 }));
  const seen = new Set();
  while (stack.length) {
    const { ev, depth } = stack.pop();
    if (seen.has(ev)) continue;
    seen.add(ev);
    out.push({ event: ev, depth });
    const kids = children.get(ev.span_id) || [];
    for (let i = kids.length - 1; i >= 0; i--) {
      stack.push({ ev: kids[i], depth: depth + 1 });
    }
  }
  // Anything unreachable from a root (a cycle in the span graph, which
  // should be impossible and would otherwise silently vanish).
  for (const ev of events) {
    if (!seen.has(ev)) out.push({ event: ev, depth: 0 });
  }
  return out;
}

/** A short, stable label for a trace. */
export function traceLabel(traceId) {
  return shortId(traceId, 12);
}
