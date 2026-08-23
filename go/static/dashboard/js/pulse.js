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

import { effectiveAgentState } from "./state.js";
import { MAX_EVENTS } from "./store.js";

// The window and its resolution. One cell per minute over an hour: the
// window is long enough to show a burst of work receding into quiet, and
// per-minute cells keep the axis honest — a cell means one minute, not
// "about a minute and a half", so hovering one gives a real clock time.
export const PULSE_MINUTES = 60;
export const PULSE_BUCKETS = 60;

// Floor under a lit cell's opacity. A single event in a minute has to be
// clearly visible next to an empty cell, or the quiet minutes vanish and
// the track only ever shows the busiest one. Exported because the cell
// brightness is computed at the render site: two renderers each picking
// their own floor is two different claims about what "one event" looks
// like, and the difference only shows up on a quiet hour, which is
// exactly when it misleads.
export const MIN_LIT = 0.3;

/**
 * Roll the event feed up into per-seat, per-minute activity.
 *
 * `seats` are flattened org seats (see org.js); `agents` are the live
 * projection rows; `tokens` is the spend rollup. Returns a plain data
 * object — the caller renders it — and the split keeps the bucketing
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
  // The company-wide track: one cell per bucket, counting EVERY event in
  // the window rather than only the ones a seat claims. Summing the rows
  // instead would undercount by exactly the engine-authored events that
  // match no `actor` — the same events `total` already counts, so the
  // track and the sentence beside it would disagree.
  const cells = Array.from({ length: buckets }, () => ({
    n: 0,
    failed: false,
    unknown: false,
  }));
  let cellMax = 0;
  for (const ev of feed) {
    // Tracked before the roster check: the retention edge is a property
    // of the FEED, and engine-authored events with no seat still mark it.
    const at = Date.parse(withZone(ev.timestamp));
    if (at && at < oldest) oldest = at;
    if (at) {
      const trackAge = now - at;
      if (trackAge <= span) {
        const ti = Math.min(
          buckets - 1,
          Math.max(0, buckets - 1 - Math.floor(trackAge / width)),
        );
        cells[ti].n += 1;
        if (ev.failed) cells[ti].failed = true;
        if (cells[ti].n > cellMax) cellMax = cells[ti].n;
      }
    }
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
  for (let i = 0; i < blindTo; i++) cells[i].unknown = true;

  return {
    minutes,
    buckets,
    since,
    now,
    width,
    rows,
    // The whole company as one row, and its own maximum: a track scaled
    // by the busiest SEAT-cell would read as flat whenever one minute of
    // company-wide traffic exceeds any single seat's.
    cells,
    cellMax,
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
