// The company pulse — one row per seat, one cell per minute of the last
// hour, lit by what that seat actually did.
//
// This is the overview's lead device, and it exists because the dashboard
// could not answer the first question anyone asks it: *is anything
// happening, and did something break?*  It had a count of agents, a count
// of tokens and a scrolling log, so an org that had been flat out for
// twenty minutes looked exactly like one that had been asleep all day.
//
// Everything here is derived from data the page already holds — the
// projection's event feed and its spend rollup — so the panel costs no
// request and no server work.  A lit cell is a real event; a red cell is a
// real failure.  Nothing is drawn to fill space.
//
// The visual language is the site's masked dot field, with the same rule
// applied literally rather than decoratively: a dot is lit because a seat
// was working in that minute.

import { esc, escAttr, fmtCompact, fmtNum } from "./format.js";
import { effectiveAgentState } from "./state.js";
import { MAX_EVENTS } from "./store.js";

// The window and its resolution. One cell per minute over an hour: the
// window is long enough to show a burst of work receding into quiet, and
// per-minute cells keep the axis honest — a cell means one minute, not
// "about a minute and a half", so hovering one gives a real clock time.
export const PULSE_MINUTES = 60;
export const PULSE_BUCKETS = 60;

// Floor under a lit cell's opacity. A single event in a minute has to be
// clearly visible next to an empty cell, or the quiet seats vanish and
// the panel only ever shows the busiest one.
const MIN_LIT = 0.3;

/**
 * Roll the event feed up into per-seat, per-minute activity.
 *
 * `seats` are flattened org seats (see org.js); `agents` are the live
 * projection rows; `tokens` is the spend rollup. Returns a plain data
 * object — `pulseGrid` renders it, and the split keeps the bucketing
 * testable without a DOM.
 */
export function buildPulse(
  { seats, agents, events, tokens, sandboxes },
  { minutes = PULSE_MINUTES, buckets = PULSE_BUCKETS, now = Date.now() } = {},
) {
  const span = minutes * 60_000;
  const width = span / buckets;
  const since = now - span;

  // Seats come from the org chart when there is one so the pulse lists
  // every configured seat — including one that has done nothing, which is
  // itself worth seeing — and falls back to the live agent list before
  // /org has landed.
  const live = new Map((agents || []).map((a) => [a.role || a.name, a]));
  const roster = (seats && seats.length ? seats : (agents || []).map((a) => ({
    name: a.role || a.name,
    kind: "agent",
  }))).filter((s) => s.kind !== "human");

  const spend = new Map(
    ((tokens && tokens.by_agent) || []).map((row) => [row.role, row]),
  );

  const rows = roster.map((seat) => {
    const agent = live.get(seat.name) || null;
    const money = spend.get(seat.name) || null;
    return {
      name: seat.name,
      agentId: agent ? agent.id : "",
      // The same derivation the seat card uses: an agent with a detached
      // sandbox run in flight is busy, even though its kick-off turn
      // already completed. Reading the raw state here put a green idle
      // dot on the pulse row beside a card saying "awaiting sandbox".
      state: agent ? effectiveAgentState(agent, sandboxes) : "offline",
      phase: agent && agent.state === "working" ? agent.current_phase || "" : "",
      cells: Array.from({ length: buckets }, () => ({
        n: 0,
        failed: false,
        unknown: false,
      })),
      events: 0,
      failures: 0,
      tokens: money ? money.total_tokens || 0 : agent ? agent.total_tokens || 0 : 0,
      calls: money ? money.calls || 0 : 0,
      lastAt: 0,
    };
  });
  const byName = new Map(rows.map((row) => [row.name, row]));

  // How far back the feed can actually speak for. The buffer holds a
  // bounded number of events, and a busy org fills it in minutes — so on
  // such an org the older half of the window has no record, which is NOT
  // the same as no activity. Drawing it as idle would make a company that
  // has been flat out all hour look like one that woke up five minutes
  // ago. Only a FULL buffer implies truncation: a short feed on a quiet
  // org genuinely covers the whole window.
  const feed = events || [];
  let oldest = Infinity;
  const truncated = feed.length >= MAX_EVENTS;

  let max = 0;
  let total = 0;
  let failures = 0;
  for (const ev of feed) {
    // Tracked before the roster check: the retention edge is a property
    // of the FEED, and engine-authored events with no seat still mark it.
    const at = Date.parse(withZone(ev.timestamp));
    if (at && at < oldest) oldest = at;
    const row = byName.get(ev.actor);
    if (!row || !at) continue;
    const age = now - at;
    if (age > span) continue;
    // Indexed off the event's AGE rather than its offset from the window
    // start, so the cell an event lands in is exactly the one the axis
    // and the tooltip call it: cell `buckets - 1` is this minute, and an
    // event `m` whole minutes old sits `m` cells to its left. Deriving
    // it from the window start instead puts an event that is exactly ten
    // minutes old in the nine-minute cell, and every label is then a
    // minute out. A timestamp slightly in the future (clock skew between
    // the engine and the browser) clamps to now rather than being
    // dropped — an event that happened is better placed approximately
    // than not shown.
    const idx = Math.min(
      buckets - 1,
      Math.max(0, buckets - 1 - Math.floor(age / width)),
    );
    const cell = row.cells[idx];
    cell.n += 1;
    row.events += 1;
    total += 1;
    if (ev.failed) {
      cell.failed = true;
      row.failures += 1;
      failures += 1;
    }
    if (at > row.lastAt) row.lastAt = at;
    if (cell.n > max) max = cell.n;
  }

  // The first cell the record covers. Everything left of it is unknown.
  const blindTo =
    truncated && oldest > since
      ? Math.min(buckets - 1, Math.max(0, buckets - 1 - Math.floor((now - oldest) / width)))
      : 0;
  for (const row of rows) {
    for (let i = 0; i < blindTo; i++) row.cells[i].unknown = true;
  }

  return {
    minutes,
    buckets,
    since,
    now,
    width,
    rows,
    // Minutes the grid can actually speak for, which is what the panel
    // must claim — never the nominal window.
    covered: Math.max(1, Math.round(((buckets - blindTo) * width) / 60_000)),
    blindTo,
    max: Math.max(max, 1),
    total,
    failures,
    working: rows.filter(
      (r) => r.state === "working" || r.state === "awaiting_sandbox",
    ).length,
  };
}

// The API emits naive-UTC and aware-UTC interchangeably; `format.parseUTC`
// handles that for display, and this is its parse-only twin so the hot
// bucketing loop does not allocate a Date per event just to reject it.
function withZone(ts) {
  const s = String(ts || "");
  if (!s) return "";
  return /[zZ]|[+-]\d\d:?\d\d$/.test(s) ? s : s + "Z";
}

// ---------------------------------------------------------------------
// Render
// ---------------------------------------------------------------------

function cellMarkup(cell, pulse, row, index, isNow) {
  const minutesAgo = pulse.buckets - 1 - index;
  const when = minutesAgo === 0 ? "this minute" : `${minutesAgo}m ago`;
  if (cell.failed) {
    return `<i class="pc is-failed" title="${escAttr(`${row.name} · ${when} · ${cell.n} event${cell.n === 1 ? "" : "s"}, failed`)}"></i>`;
  }
  if (cell.unknown) {
    return `<i class="pc is-unknown" title="${escAttr(`${when} · outside the retained feed`)}"></i>`;
  }
  if (!cell.n) {
    return `<i class="pc" title="${escAttr(`${row.name} · ${when} · idle`)}"></i>`;
  }
  const lit = MIN_LIT + (1 - MIN_LIT) * Math.min(1, cell.n / pulse.max);
  const cls = isNow && row.state === "working" ? "pc is-lit is-now" : "pc is-lit";
  return `<i class="${cls}" style="--lit:${lit.toFixed(2)}" title="${escAttr(
    `${row.name} · ${when} · ${cell.n} event${cell.n === 1 ? "" : "s"}`,
  )}"></i>`;
}

function rowMarkup(row, pulse) {
  const nav = row.agentId
    ? ` clickable" data-action="agent" data-id="${escAttr(row.agentId)}`
    : "";
  const cells = row.cells
    .map((cell, i) => cellMarkup(cell, pulse, row, i, i === pulse.buckets - 1))
    .join("");
  const foot = row.failures
    ? `<span class="pulse-fail">${fmtNum(row.failures)} failed</span>`
    : row.events
      ? `<span class="pulse-count">${fmtCompact(row.tokens)}</span>`
      : `<span class="pulse-count is-zero">—</span>`;
  return `
    <div class="pulse-row${nav}" data-k="pulse:${escAttr(row.name)}"
         style="--seat:${row.failed ? "var(--red)" : "var(--border-strong)"}">
      <span class="pulse-name">
        <i class="dot ${esc(row.state)}"></i>${esc(row.name)}
      </span>
      <span class="pulse-track">${cells}</span>
      <span class="pulse-foot">${foot}</span>
    </div>`;
}

/**
 * The same track, shrunk to fit inside a seat card.
 *
 * Deliberately the same device rather than a second chart type: a reader
 * who has learned the hero grid already knows how to read the strip on a
 * card, and the two cannot disagree because they are built from one
 * bucketing pass.
 */
export function pulseSpark(row, max) {
  if (!row) return "";
  const cells = row.cells
    .map((cell) => {
      if (cell.failed) return '<i class="pc is-failed"></i>';
      if (!cell.n) return '<i class="pc"></i>';
      const lit = MIN_LIT + (1 - MIN_LIT) * Math.min(1, cell.n / Math.max(max, 1));
      return `<i class="pc is-lit" style="--lit:${lit.toFixed(2)}"></i>`;
    })
    .join("");
  return `<span class="pulse-track is-mini"
    title="${escAttr(`${row.name} · ${row.events} event${row.events === 1 ? "" : "s"} in the last hour`)}">${cells}</span>`;
}

/** The grid itself, plus its axis. */
export function pulseGrid(pulse) {
  if (!pulse.rows.length) return "";
  return `
    <div class="pulse">
      ${pulse.rows.map((row) => rowMarkup(row, pulse)).join("")}
      <div class="pulse-axis" data-k="pulse:axis">
        <span class="pulse-name"></span>
        <span class="pulse-track">
          <span>−${pulse.minutes}m</span>
          <span>−${Math.round(pulse.minutes / 2)}m</span>
          <span>now</span>
        </span>
        <span class="pulse-foot"></span>
      </div>
    </div>`;
}
