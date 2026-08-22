// Mission Control: what needs me, what the engine is doing, what is stuck.
//
// The page this replaces was mostly other rooms' work rendered again,
// badly. It carried a 40px "3 of 9 seats working" numeral that the Agents
// room now answers better; a per-seat pulse grid costing ~280px and
// scaling with headcount to deliver two integers the sentence above it
// already printed in words — drawn TWICE, once in the hero and again as a
// spark inside every seat card; a phase bar that belongs to Spend, where
// the window is selectable; an in-flight board whose three row types each
// had a better-sourced twin in Work or Agents (one of them, the sandbox
// card, had "go read this properly in the other room" as its only
// affordance); the full seat grid Agents owns outright; and twelve feed
// rows that are a truncated Activity with none of Activity's machinery.
//
// Worse than useless, half of it was untrustworthy: `store.setConnected`
// clears only the `health` slice, so `agents`, `events`, `tokens`,
// `budget` and `sandboxes` stay frozen — and this page happily printed
// them at full confidence on a dead socket.
//
// What is left is what no other room owns, and what changes:
//
//   1. what needs a person, and how long the oldest has waited
//   2. the ENGINE's own state — in-flight turns, posture, event store,
//      draining. Most of this reached no pixel outside a popover you had
//      to know to click.
//   3. what is STUCK: turns that stopped producing rounds, and seats
//      whose teardown could not be proven — runtime.py calls the second
//      "the one to alert on" and it reached no screen at all.
//   4. did anything break, and how far back can this page even see
//   5. money, in the two spans that are honest
//
// The rule every band keeps: a number whose precondition is absent
// renders as an em dash, never as zero. A frozen projection is not a
// quiet company.

import { esc, escAttr, fmtCompact, fmtNum, relTime } from "../format.js";
import { icon } from "../icons.js";
import { buildPulse, MIN_LIT, PULSE_MINUTES } from "../pulse.js";
import { buildAttention } from "../attention.js";
import { flattenSeats } from "../org.js";
import { staleness, STALE_MS, STALLED_MS } from "../state.js";
import {
  attentionList,
  empty,
  sectionHead,
  skeletonCards,
  statStrip,
} from "../ui.js";

// A seat whose teardown has gone unproven this long is not a system
// retrying — it is a seat no node in the fleet is running while this one
// still holds its lease. Under this, a failed teardown that succeeds on
// the next heartbeat is ordinary.
const STRANDED_SECONDS = 60;

export function createMissionView({ store, navigate, refresh }) {
  // ---- 1. what needs a person -----------------------------------------

  function attention(state) {
    const queue = buildAttention(state);
    // The age of the head of the queue, which is the figure that says
    // whether anybody has looked. `at` is an empty string on the
    // config-missing item, and an unfiltered max would rank that ageless
    // entry as the newest thing in the list.
    const stamps = queue.items
      .map((item) => (item.at ? Date.parse(item.at) : NaN))
      .filter((ms) => !Number.isNaN(ms));
    const oldest = stamps.length ? Math.min(...stamps) : null;
    const age =
      queue.stale || oldest === null
        ? null
        : relTime(new Date(oldest).toISOString());
    // The panel is drawn even when it is empty, and that is deliberate:
    // "nothing is waiting on you" is an answer, and a panel that
    // disappears when there is nothing to do teaches the reader to check
    // whether it is there rather than to read it. The rows are bare —
    // separators, no surface of their own — so the panel is also the only
    // thing holding them off the page ground.
    return `
      <section class="panel att-panel" data-k="attention">
        ${sectionHead(
          "inbox",
          "Needs you",
          queue.stale ? null : queue.items.length,
          null,
        )}
        ${age ? `<p class="ms-age">Oldest has been waiting ${esc(age)}.</p>` : ""}
        ${attentionList(queue)}
      </section>`;
  }

  // ---- 2. the engine's own state ---------------------------------------
  // Every clause is three-valued. `health` is the one slice the store
  // wipes on disconnect, which is exactly what makes it safe to read
  // here: absent means "cannot see", and absent renders as a dash.

  function clause(label, value, tone = "") {
    return `<div class="ms-clause${tone ? " tone-" + tone : ""}" data-k="c:${escAttr(label)}">
        <span class="ms-clause-label">${esc(label)}</span>
        <span class="ms-clause-value">${value}</span>
      </div>`;
  }

  const DASH = '<span class="ms-none">—</span>';

  function engineLine(state, pulse) {
    const h = state.health || {};
    const seen = state.connected !== false && h.status !== undefined;

    // Engine state. `wait` is ordinary rollout propagation and must never
    // be painted as trouble — every successful rollout produces it.
    // Draining is orderly, so amber; only shed and stuck are faults.
    let engine = DASH;
    let tone = "";
    if (seen) {
      if (h.configured === false) {
        engine = "no active configuration";
        tone = "bad";
      } else if (h.shutting_down === true) {
        engine = "draining";
        tone = "warn";
      } else if (h.posture && !["serve", "wait"].includes(h.posture)) {
        engine = esc(String(h.posture));
        tone = "bad";
      } else if (h.engine === false) {
        engine = "API only, no engine here";
      } else {
        engine = "serving";
        tone = "ok";
      }
    }

    // In-flight is per PROCESS. Behind a load balancer a refresh answers
    // about a different one, so it is labelled with the node that
    // answered rather than presented as a company-wide figure.
    const inFlight =
      seen && h.engine === true && typeof h.in_flight === "number"
        ? `${esc(String(h.in_flight))}${h.node ? ` <span class="ms-on">on ${esc(h.node)}</span>` : ""}`
        : DASH;

    const working =
      state.connected === false
        ? DASH
        : `${esc(String(pulse.working))} <span class="ms-on">of ${esc(String(pulse.rows.length))}</span>`;

    // An engine with no event store has not failed — it simply keeps no
    // record. This is also the clause that stops the pulse below being
    // read as "nothing happened".
    let store_ = DASH;
    let storeTone = "";
    if (seen && h.event_store) {
      store_ = esc(String(h.event_store));
      if (h.event_store === "none") storeTone = "warn";
    }

    return `
      ${sectionHead("cpu", "Engine", null, null)}
      <div class="card ms-line" data-k="engine">
        ${clause("State", engine, tone)}
        ${clause("Turns in flight", inFlight)}
        ${clause("Seats working", working)}
        ${clause("Event store", store_, storeTone)}
      </div>`;
  }

  // ---- 3. what is stuck -------------------------------------------------

  function stuck(state) {
    const rows = [];

    // (a) Turns that stopped producing rounds. Applied per row on the old
    //     board and never aggregated, so "is anything hung?" meant
    //     scrolling and reading ages.
    //
    //     Deliberately empty while disconnected: `updated_at` ages
    //     against the browser clock whether or not the projection is
    //     moving, so a three-minute outage manufactures stalled turns out
    //     of perfectly healthy ones.
    if (state.connected !== false) {
      for (const agent of state.agents || []) {
        const call = agent.live_call;
        if (!call || !call.updated_at) continue;
        const how = staleness(call.updated_at);
        if (!how) continue;
        rows.push({
          key: `turn:${agent.role}`,
          tone: how === "stalled" ? "bad" : "warn",
          who: agent.role || agent.name,
          handle: agent.handle || agent.role,
          what:
            how === "stalled"
              ? `no round in ${Math.round(STALLED_MS / 60000)} minutes — this is not coming back on its own`
              : `no round in ${Math.round(STALE_MS / 60000)} minutes`,
          when: relTime(call.updated_at),
        });
      }
    }

    // (b) Seats whose teardown could not be proven. This node holds a
    //     lease no peer can take while possibly still consuming the seat.
    //     Alerted on the DURATION, not the list: a teardown that fails
    //     once and succeeds on the next heartbeat is a working system.
    //
    //     `health.seats` is omitted entirely on a node that does no
    //     placement — absent means "not applicable", never "holds none".
    const ages = ((state.health || {}).seats || {}).unproven_seconds || {};
    for (const [handle, seconds] of Object.entries(ages)) {
      if (!(Number(seconds) > STRANDED_SECONDS)) continue;
      rows.push({
        key: `seat:${handle}`,
        tone: "warn",
        who: handle,
        handle,
        what:
          "teardown never confirmed — this node still holds the lease, so no peer can take the seat",
        when: `${Math.round(Number(seconds))}s`,
      });
    }

    if (!rows.length) return "";
    return `
      ${sectionHead("alert", "Stuck", rows.length, null)}
      <div class="list ms-stuck">
        ${rows
          .map(
            (r) => `
          <div class="ms-stuck-row tone-${r.tone} clickable" data-k="${escAttr(r.key)}"
               data-action="seat" data-seat="${escAttr(r.handle)}">
            <span class="ms-stuck-who">${esc(r.who)}</span>
            <span class="ms-stuck-what">${esc(r.what)}</span>
            <span class="ms-stuck-when">${esc(r.when)}</span>
          </div>`,
          )
          .join("")}
      </div>`;
  }

  // ---- 4. did anything break, and how far back can this page see -------

  function pulseFor(state) {
    // The org tree is walked in ONE place, `org.js`. This room had grown
    // its own copy of the walk, which is how lead inheritance and
    // `mcp_env` inheritance end up resolved twice and differently.
    return buildPulse({
      seats: flattenSeats(state.org || {}).filter((s) => s.kind === "agent"),
      agents: state.agents || [],
      events: state.events || [],
      tokens: state.tokens,
      sandboxes: state.sandboxes || [],
    });
  }

  function record(state, pulse) {
    if (state.connected === false) {
      return `
        ${sectionHead("activity", "Recent record", null, null)}
        <p class="ms-note">The socket is down, so the feed below stopped where
        it stopped. It is not a quiet hour.</p>`;
    }
    const cells = pulse.cells || [];
    const scale = Math.max(1, pulse.cellMax || 1);
    const track = cells
      .map((c, i) => {
        const cls = c.unknown
          ? "is-unknown"
          : c.failed
            ? "is-failed"
            : c.n
              ? "is-on"
              : "";
        // One floor for "a minute in which something happened", shared
        // with every other renderer of these cells.
        const level = c.n
          ? MIN_LIT + (1 - MIN_LIT) * Math.min(1, c.n / scale)
          : 0;
        return `<i class="ms-cell ${cls}" data-k="p${i}" style="--level:${level.toFixed(2)}"></i>`;
      })
      .join("");

    // `failures / total` is the ONE ratio legal on this page: one
    // bucketing pass, one window. It must never be divided into the 24h
    // token rollup.
    const line = pulse.total
      ? `${fmtNum(pulse.total)} events in the last ${esc(String(pulse.covered))} minutes${
          pulse.failures
            ? ` · <b class="ms-broke">${esc(String(pulse.failures))} failed</b>`
            : " · none failed"
        }`
      : `Nothing in the last ${esc(String(pulse.covered))} minutes`;

    return `
      ${sectionHead("activity", "Recent record", null, {
        action: "go",
        label: "Activity",
        route: "/activity",
      })}
      <div class="card ms-record" data-k="record">
        <div class="ms-track">${track}</div>
        <div class="ms-axis" data-k="axis">
          <span>−${esc(String(PULSE_MINUTES))}m</span>
          <span>−${esc(String(Math.round(PULSE_MINUTES / 2)))}m</span>
          <span>now</span>
        </div>
        <div class="ms-record-foot">
          <span>${line}</span>
          ${
            pulse.blindTo
              ? `<span class="ms-blind">${icon("alert", "sm")}Past the retention edge there is no record — the pale cells are not quiet, they are unknown.</span>`
              : ""
          }
        </div>
      </div>`;
  }

  // ---- 5. money, in the two spans that are honest -----------------------

  function money(state) {
    const totals = (state.tokens && state.tokens.totals) || null;
    const budget = (state.budget && state.budget.org) || null;
    const frozen = state.connected === false;

    const tiles = [];

    tiles.push({
      label: "Spend · last 24h",
      value: frozen || !totals ? DASH : fmtCompact(totals.total_tokens || 0),
      foot:
        frozen && state.tokens && state.tokens.aggregated_through
          ? `as of ${esc(relTime(state.tokens.aggregated_through))}`
          : totals
            ? `${fmtCompact(totals.input_tokens || 0)} in · ${fmtCompact(totals.output_tokens || 0)} out · ${fmtNum(totals.calls || 0)} calls`
            : "no rollup",
    });

    // A cap with no meter is not a cap at zero: the tile is dropped
    // rather than drawn empty. Meter and cap share one span, which is
    // the only reason this percentage is allowed to exist.
    if (budget && budget.max) {
      const pct = Math.min(100, Math.round(((budget.used || 0) / budget.max) * 100));
      tiles.push({
        label: "Org budget · this run",
        value: frozen ? DASH : `${pct}%`,
        foot: budget.refused_at
          ? "refusing charges"
          : `${fmtCompact(budget.used || 0)} of ${fmtCompact(budget.max)}`,
        // A cap that is refusing has STOPPED work, which is the reserved
        // status hue; three quarters spent is the caution one.
        tone: budget.refused_at ? "bad" : pct >= 75 ? "warn" : "",
      });
    }

    // Seats a cap is actively blocking. `budget === null` means no meter
    // — no per-seat cap configured, or no engine reporting — and that is
    // not a seat at zero, so it is not counted either way.
    const refusing = (state.agents || []).filter(
      (a) => a.budget && a.budget.refused_at,
    );
    if (refusing.length) {
      tiles.push({
        label: "Seats refusing charges",
        value: esc(String(refusing.length)),
        // Seat names come from config, and `foot` is inserted as markup.
        foot: esc(refusing.map((a) => a.role || a.name).join(", ")),
        tone: "bad",
      });
    }

    return `${sectionHead("zap", "Cost", null, {
      action: "go",
      label: "Spend & Budgets",
      route: "/spend",
    })}${statStrip(tiles)}`;
  }

  return {
    slices: ["agents", "events", "sandboxes", "tokens", "budget", "health", "org"],

    render(state) {
      // A first paint with nothing yet is a skeleton. A page that HAS
      // seen the engine and then lost it is not — it keeps what it has
      // and says, band by band, which parts it can no longer vouch for.
      if (state.connected === false && !(state.agents || []).length) {
        return skeletonCards(3);
      }
      if (
        !(state.agents || []).length &&
        !(state.events || []).length &&
        state.health &&
        state.health.configured === false
      ) {
        return (
          attention(state) +
          empty(
            "building",
            "No company configured",
            "The engine is running and has no company to run. Every inbound webhook is being discarded.",
          )
        );
      }

      const pulse = pulseFor(state);
      return `
        ${attention(state)}
        ${engineLine(state, pulse)}
        ${stuck(state)}
        ${record(state, pulse)}
        ${money(state)}`;
    },
  };
}
