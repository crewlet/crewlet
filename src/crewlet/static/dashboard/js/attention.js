// What needs a person, derived from what the engine already reports.
//
// The dashboard knew every one of these conditions and kept each in a
// different room: a sandbox parked on a question was a badge on the
// overview, a seat that stopped was a card among the healthy ones, a
// budget refusing charges was a bar on a seat page, and an engine with no
// active configuration was a line in the health popover. An operator
// asking the only question they actually arrive with — "is anything
// waiting on me?" — had to visit four screens and remember what each one
// meant.
//
// This derives one list. It invents nothing: every item is a state the
// server already projects, and each one names the room it belongs to so
// the answer is one click from the question.
//
// Client-side for now. The fleet-sourced conditions (a role no node
// runs, a seat whose teardown could not be proven) cannot be reached
// from the pushed slices, so they are absent rather than guessed at —
// see docs/reference/dashboard-design.md for the note on moving this
// derivation into the projection.

import { parseUTC } from "./format.js";

// Two levels, matching the rule the whole surface follows: red means
// something broke, amber means something needs attention and nothing
// broke. A budget doing exactly what it was configured to do is not a
// failure, however much it is blocking; a turn that died is.
export const SEVERITY = { broke: "broke", needs: "needs" };

const SEVERITY_RANK = { broke: 0, needs: 1 };

/**
 * Every open obligation, most urgent first.
 *
 * Returns `{ items, stale }`. `stale` is the honesty half: losing the
 * socket clears the health slice and freezes every other one, so an
 * empty list on a disconnected page means "cannot see", not "nothing to
 * do". A caller that renders the count without reading `stale` will
 * eventually tell somebody there is nothing waiting for them during an
 * outage.
 */
export function buildAttention(state, now = Date.now()) {
  const items = [];
  const stale = !state.connected;

  for (const run of state.sandboxes || []) {
    if (run.status !== "awaiting_input") continue;
    const seat = run.role || run.agent_handle || "";
    items.push({
      id: `sandbox:${run.turn_id}`,
      severity: SEVERITY.needs,
      seat,
      // The question itself is the headline. A row that said "sandbox
      // needs input" would make the reader open it to learn whether they
      // can answer in five seconds or need an hour.
      title: run.question || "A sandbox run is waiting on an answer",
      detail: sandboxDetail(run),
      at: run.started_at || "",
      zone: "now",
      route: "/work",
      action: "Answer",
    });
  }

  for (const agent of state.agents || []) {
    const seat = agent.role || agent.name || "";
    const error = agent.last_error;
    // A seat can be AFK with the full error record already gone — it does
    // not survive an API restart — so the state alone has to be enough to
    // keep a stopped seat on the list. Keying only on `last_error` hid
    // every broken seat at exactly the moment an operator went looking.
    if (!error && agent.state !== "afk") continue;
    items.push({
      id: `seat:${seat}`,
      severity: SEVERITY.broke,
      seat,
      title: stoppedTitle(agent, error),
      detail: error && error.message ? error.message : "",
      at: (error && error.at) || "",
      zone: "now",
      route: `/seats/${encodeURIComponent(agent.handle || seat)}`,
      action: "Open seat",
    });
  }

  // The engine records WHEN a cap turned a charge away, and that is the
  // only honest signal of exhaustion: `TokenBudget.consume` refuses a
  // charge that would exceed the cap and increments nothing, so a seat
  // charged in 3k rounds against a 100k cap stalls near 99k and never
  // reaches its own maximum. A test on `used >= max` shows a permanently
  // blocked seat at 97% and calls it healthy.
  const org = (state.budget || {}).org;
  if (org && org.refused_at) {
    items.push({
      id: "budget:org",
      severity: SEVERITY.needs,
      seat: "",
      title: "The org token budget is refusing charges",
      detail: budgetDetail(org),
      at: org.refused_at,
      zone: "operations",
      route: "/spend",
      action: "Budgets",
    });
  }
  for (const agent of state.agents || []) {
    const budget = agent.budget;
    if (!budget || !budget.refused_at) continue;
    const seat = agent.role || agent.name || "";
    items.push({
      id: `budget:${seat}`,
      severity: SEVERITY.needs,
      seat,
      title: `${seat} has spent its token budget`,
      detail: budgetDetail(budget),
      at: budget.refused_at,
      zone: "operations",
      route: "/spend",
      action: "Budgets",
    });
  }

  // Read three-valued, deliberately. The health slice is cleared on
  // disconnect, so `configured` arrives undefined at exactly the moment
  // the reader most needs the truth — and `!configured` would then
  // announce an unconfigured engine to a page that cannot see one.
  if (state.health && state.health.configured === false) {
    items.push({
      id: "config:none",
      severity: SEVERITY.needs,
      seat: "",
      title: "No company configuration is active",
      detail:
        "The engine is running and every inbound webhook is being turned away. Import or activate a revision to start the company.",
      at: "",
      zone: "operations",
      route: "/config",
      action: "Configuration",
    });
  }

  items.sort((a, b) => {
    const rank = SEVERITY_RANK[a.severity] - SEVERITY_RANK[b.severity];
    if (rank) return rank;
    // Oldest obligation first. A question that has been waiting four days
    // outranks one asked a minute ago, which is the opposite of a feed's
    // ordering and the right one for a list of debts.
    return age(b.at, now) - age(a.at, now);
  });

  return { items, stale };
}

function age(at, now) {
  const d = parseUTC(at);
  return d ? now - d.getTime() : 0;
}

function sandboxDetail(run) {
  const parts = [];
  if (run.coding_agent) parts.push(run.coding_agent);
  // `audience` is who the question was addressed to, never where it was
  // sent: the engine always posts back to the conversation the task came
  // from. Rendering it as a destination would send somebody to the wrong
  // place looking for a question that is not there.
  if (run.audience) parts.push(`asked ${run.audience}`);
  return parts.join(" · ");
}

function stoppedTitle(agent, error) {
  const seat = agent.role || agent.name || "a seat";
  const kind = (error && error.kind) || agent.afk_reason || "";
  if (!kind) return `${seat} stopped`;
  const phase = error && error.phase ? ` in ${error.phase}` : "";
  return `${seat} stopped — ${String(kind).replace(/_/g, " ")}${phase}`;
}

function budgetDetail(budget) {
  const used = Number(budget.used || 0);
  const max = Number(budget.max || 0);
  if (!max) return "The cap turned a charge away.";
  return `${used.toLocaleString()} of ${max.toLocaleString()} tokens used this engine run.`;
}

/**
 * Per-zone counts for the sidebar, and the total for the tab title.
 *
 * Counts are only meaningful when the page can see the engine; a caller
 * gets `null` while the socket is down so it can render "unknown"
 * rather than a confident zero.
 */
export function attentionCounts({ items, stale }) {
  if (stale) return null;
  const counts = { total: items.length, broke: 0 };
  for (const item of items) {
    counts[item.zone] = (counts[item.zone] || 0) + 1;
    if (item.severity === SEVERITY.broke) counts.broke++;
  }
  return counts;
}
