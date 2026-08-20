// What the engine knows about itself, rendered.
//
// The entire health surface used to be one 7px dot with three colours.
// Everything below has been on the wire the whole time and reached no
// screen: whether a company configuration is even active, whether the
// event store is durable or will evaporate on restart, whether the
// activity feed was seeded from history, how many envelopes this tab has
// silently dropped.
//
// It lives in a popover on the dot rather than behind a nav entry. A
// health screen would be backed by real data, so it does not literally
// break the "no placeholder screens" rule — but it breaks its purpose.
// Health has to be seen when you were NOT looking for it; a screen you
// navigate to is one you open after you already suspect a problem, and
// it would sit green and empty in the nav every other day of the year.
//
// Two conditions escalate out of the popover into always-on chrome,
// because they must never wait for a click: an unconfigured engine, and
// a drain.

import { esc, fmtNum, relTime } from "./format.js";

// Status → the dot's class. A map rather than a ternary chain: the
// chain's final `else` sent every unrecognised status to green, so a
// state added on the server would have arrived on screen as "healthy".
const DOT_CLASS = {
  ok: "ok",
  unconfigured: "warn",
  shutting_down: "drain",
};

export function dotClass(health, connected) {
  if (!connected) return "down";
  return DOT_CLASS[health && health.status] || "warn";
}

const STORE_NOTE = {
  durable: "history survives a restart",
  memory: "in memory only — history will not survive a restart",
  none: "no event store — history and token totals are unavailable",
};

/**
 * Failure-adjacent telemetry in the retained feed.
 *
 * `provider_fallback` is the early warning the chain recovered from, so
 * it is deliberately NOT counted as a failure in the feed's red — that
 * would fire the sidebar's alert count on turns that succeeded. Counted
 * here instead, where "a provider is degrading" is the actual question.
 *
 * The window is stated as a row count, never as a duration: the feed is
 * a bounded ring, and a busy org fills it in minutes.
 */
export function providerTrouble(events) {
  const rows = events || [];
  let fallbacks = 0;
  let unavailable = 0;
  for (const ev of rows) {
    if (ev.type === "provider_fallback") fallbacks++;
    else if (ev.type === "llm_unavailable") unavailable++;
  }
  return { fallbacks, unavailable, retained: rows.length };
}

/**
 * One label/value/note line.
 *
 * `tone` is `caution-ink` (amber) or `warn-ink` (red), and the two are
 * not interchangeable: red is reserved across this dashboard for
 * something that FAILED — a failed phase, an exhausted budget, a trace
 * with errors in it. An engine with no event store has not failed, and
 * painting it the same colour as a crashed turn trains the reader to
 * ignore the colour that matters.
 */
function row(label, value, note = "", tone = "") {
  return `
    <div class="hp-row" data-k="hp:${esc(label)}">
      <span class="hp-label">${esc(label)}</span>
      <span class="hp-value${tone ? " " + tone : ""}">${esc(value)}</span>
      ${note ? `<span class="hp-note">${esc(note)}</span>` : ""}
    </div>`;
}

/**
 * A boolean the engine reported, or the fact that it reported nothing.
 *
 * Three-valued on purpose. Dropping the socket CLEARS the health slice
 * to `{status: "unknown"}` — correctly, because a five-second-old tick
 * is not evidence about now — so every one of these fields arrives as
 * `undefined` the moment the connection goes. Read with a plain
 * `=== false ? bad : good`, that renders "Configuration: active" on a
 * page that cannot see the engine at all: the exact
 * unknown-reads-as-healthy failure the dot's lookup table exists to
 * prevent, in the surface built to prevent it.
 */
function tri(value, whenTrue, whenFalse) {
  if (value === true) return whenTrue;
  if (value === false) return whenFalse;
  return "—";
}

/**
 * The popover's markup. Pure: takes what it renders and touches no DOM.
 *
 * `stream` is the reply to the `stream` query — per-socket facts that
 * cannot ride the shared health tick — or `null` while it is in flight.
 */
export function renderHealthPopover(health, stream, events, connected) {
  const h = health || {};
  const status = connected ? h.status || "unknown" : "disconnected";
  const trouble = providerTrouble(events);
  // Said once, on the rows it invalidates, rather than as a paragraph.
  const unknown = connected ? "" : "not known while disconnected";

  let engineRows;
  if (h.engine === true) {
    engineRows =
      row("In flight", fmtNum(h.in_flight || 0)) +
      (h.engine_started_at
        ? row("Engine up", relTime(h.engine_started_at).replace(" ago", ""))
        : "");
  } else if (h.engine === false) {
    // Never a zero here. "Nothing is running" and "this process has no
    // engine to ask" are different answers, and the standalone API can
    // only give the second.
    engineRows = row("In flight", "—", "no engine in this API process");
  } else {
    engineRows = row("In flight", "—", unknown);
  }

  const socketRows = stream
    ? row(
        "Dropped",
        fmtNum(stream.dropped || 0),
        stream.dropped
          ? `on this connection, over ${relTime(stream.connected_at).replace(" ago", "")}`
          : "on this connection",
        stream.dropped ? "caution-ink" : "",
      ) +
      row("Queued", `${fmtNum(stream.queued || 0)} / ${fmtNum(stream.capacity || 0)}`) +
      row("Tabs connected", fmtNum(stream.clients || 0))
    : row("This connection", "…");

  return `
    <div class="health-pop" data-k="health-pop">
      <div class="hp-head">
        <span class="live-dot ${dotClass(h, connected)}"></span>
        <span class="hp-status">${esc(status)}</span>
        <span style="flex:1"></span>
        <span class="hp-version mono">${esc(h.version || "")}</span>
      </div>

      <div class="eyebrow hp-section">Engine</div>
      ${engineRows}
      ${row(
        "Configuration",
        tri(h.configured, "active", "none active"),
        h.configured === false ? "inbound webhooks are dropped" : unknown,
        h.configured === false ? "caution-ink" : "",
      )}

      <div class="eyebrow hp-section">Wiring</div>
      ${row("Queue", h.queue || "—", h.queue ? "" : unknown)}
      ${row(
        "Event store",
        h.event_store || "—",
        STORE_NOTE[h.event_store] || unknown,
        h.event_store === "none" ? "caution-ink" : "",
      )}
      ${row(
        "Activity feed",
        tri(h.feed_hydrated, "seeded from history", "not seeded"),
        h.feed_hydrated === false ? "history could not be read at startup" : unknown,
        // Red, not amber: this one is a store read that ERRORED, not a
        // deployment shape the operator chose.
        h.feed_hydrated === false ? "warn-ink" : "",
      )}

      <div class="eyebrow hp-section">This connection</div>
      ${socketRows}
      ${
        stream && stream.dropped
          ? `<div class="hp-action" data-action="reconnect">Reconnect for a fresh snapshot</div>`
          : ""
      }

      <div class="eyebrow hp-section">Providers</div>
      ${
        trouble.fallbacks || trouble.unavailable
          ? row(
              "LLM trouble",
              `${fmtNum(trouble.fallbacks)} fallback${trouble.fallbacks === 1 ? "" : "s"}, ${fmtNum(trouble.unavailable)} exhausted`,
              `in the last ${fmtNum(trouble.retained)} retained events`,
              "caution-ink",
            ) + `<div class="hp-action" data-action="view-events">Open Activity →</div>`
          : row("LLM trouble", "none", `in the last ${fmtNum(trouble.retained)} retained events`)
      }
    </div>`;
}

/** The always-on banner text, or `""` when there is nothing to say. */
export function bannerFor(health, connected, events, authRejected) {
  // Checked before `connected`, and it has to be: a refused handshake
  // leaves the socket down, so the outage text is literally true and
  // completely misleading. "Retrying" describes a wait that ends by
  // itself; this one ends only when somebody supplies a token.
  // Plain prose, not markdown: this is set with `textContent`, so
  // backticks would render as backticks.
  if (authRejected) {
    return "The engine refused this browser's API token — reads are guarded on this deployment. Set a token matching one of its api.auth.tokens entries.";
  }
  if (!connected) {
    const last = (events || [])[0];
    return last
      ? `Not connected to the engine — showing the last state received, from ${relTime(last.timestamp)}.`
      : "Not connected to the engine — retrying.";
  }
  if (health && health.configured === false) {
    // Literally true of the webhook routes: with no active revision they
    // accept and discard. An operator staring at empty screens deserves
    // to be told that, not to infer it.
    return "No company configuration is active — inbound webhooks are being dropped. Import one with `crewlet config import`.";
  }
  return "";
}

/** Which tone the banner takes: an unconfigured engine is not an outage. */
export function bannerTone(health, connected, authRejected) {
  if (authRejected) return "warn";
  if (!connected) return "down";
  return health && health.configured === false ? "warn" : "";
}
