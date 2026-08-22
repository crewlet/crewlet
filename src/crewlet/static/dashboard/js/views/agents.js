// Agents — who is working, who is not, and why.
//
// This was the product's most-asked question and its worst-buried answer.
// It lived as one lens of the Org room (Company → Org → Seats), which
// files it under how the company is *arranged* rather than what it is
// *doing* — and the LLM calls behind it were another two clicks down, in
// a tab of a seat page you could only reach from that lens. Meanwhile the
// most prominent list of agents in the product, the pulse strip on
// Mission Control, rendered every row with a pointer cursor and did
// nothing when clicked.
//
// So this is a room in the Now zone, not a lens in Company. Org keeps the
// structure — the chart, the directory, the charter. This owns the live
// state, and it is the only place the seats list lives; two screens
// answering "who is working" would eventually disagree.
//
// GROUPED BY WHAT IT ASKS OF THE READER, not by the state enum. A seat
// parked on a question and a seat that fell over are both "not working",
// and only one of them is broken — the enum cannot express that, so the
// grouping does. Order is urgency: anything wanting a person first,
// anything running second, everything quiet last.
//
// The colour rule the design system sets holds here: red means something
// broke, amber means something needs attention, and neither ever carries
// the meaning alone — every row says its state in words as well.

import { esc, escAttr, relTime, fmtNum } from "../format.js";
import { icon } from "../icons.js";
import { empty, emptyOrPending, sectionHead, skeletonCards } from "../ui.js";
import { flattenSeats } from "../org.js";
import {
  effectiveAgentState,
  statusLine,
  phaseInk,
  phaseSuffix,
  avatarFor,
  staleness,
  seatTone,
} from "../state.js";

// The four questions a seat can be putting to the reader, in the order
// they deserve attention. `tone` is the hue family; `hue` is null where
// the group is deliberately uncoloured — quiet is not a status.
const GROUPS = [
  {
    key: "needs",
    title: "Needs a person",
    tone: "needs",
    blurb:
      "Parked on a question, or stopped. Nothing here moves again on its own.",
  },
  {
    key: "working",
    title: "Working",
    tone: "working",
    blurb: "Mid-turn right now.",
  },
  {
    key: "idle",
    title: "Idle",
    tone: "idle",
    blurb: "Running, with an empty inbox. Waiting for something to arrive.",
  },
  {
    key: "off",
    title: "Not running",
    tone: "off",
    blurb:
      "No process is serving this seat. On a fleet that means no node has claimed it.",
  },
];

// The room's groups, in the avatar's tone vocabulary. `broken` is decided
// per row rather than per group, because "needs a person" holds both a
// question (amber) and a failure (red).
const TONE = { needs: "needs", working: "working", idle: "quiet", off: "quiet" };

/**
 * Which group a seat belongs in, and whether it is broken.
 *
 * Broken is separate from the group on purpose: a stopped seat and a seat
 * waiting for an answer both need a person, but only one of them is a
 * failure, and the design system reserves red for failure.
 */
export function classify(seat, { agent, sandbox, sandboxes } = {}) {
  if (!agent) return { group: "off", broken: false };
  const state = effectiveAgentState(agent, sandboxes);
  if (sandbox && sandbox.status === "awaiting_input") {
    return { group: "needs", broken: false, reason: "question" };
  }
  if (agent.last_error) return { group: "needs", broken: true, reason: "error" };
  if (state === "afk") return { group: "needs", broken: true, reason: "stopped" };
  if (state === "working" || state === "awaiting_sandbox") {
    return { group: "working", broken: false };
  }
  if (state === "idle") return { group: "idle", broken: false };
  return { group: "off", broken: false };
}

export function createAgentsView({ store, navigate, refresh, params = {} }) {
  // A group can be focused from the summary strip, which is a FILTER —
  // it replaces rather than pushes, because four ticked chips are one
  // screen.
  let only = GROUPS.some((g) => g.key === params.group) ? params.group : "";

  function seatsOf(state) {
    const org = state.org || {};
    const configured = flattenSeats(org).filter((s) => s.kind === "agent");
    if (configured.length) return configured;
    // Before /org lands there is still a live agent list, and a room that
    // shows nothing until the org arrives is a room that looks broken
    // during the second it takes.
    return (state.agents || []).map((a) => ({
      name: a.role || a.name,
      handle: a.handle || "",
      kind: "agent",
      unitPath: [],
      integrations: [],
    }));
  }

  function bucket(state) {
    const seats = seatsOf(state);
    const byRole = new Map((state.agents || []).map((a) => [a.role || a.name, a]));
    const sandboxByRole = new Map((state.sandboxes || []).map((s) => [s.role, s]));
    const out = { needs: [], working: [], idle: [], off: [] };
    for (const seat of seats) {
      const agent = byRole.get(seat.name);
      const sandbox = sandboxByRole.get(seat.name);
      const verdict = classify(seat, {
        agent,
        sandbox,
        sandboxes: state.sandboxes,
      });
      out[verdict.group].push({ seat, agent, sandbox, ...verdict });
    }
    // Inside a group, oldest first: a question asked on Friday outranks
    // one asked a minute ago, and a seat that has been stopped longer is
    // the one nobody has looked at.
    for (const key of Object.keys(out)) {
      out[key].sort((a, b) => stamp(a) - stamp(b));
    }
    return out;
  }

  function stamp(row) {
    const at = row.sandbox?.updated_at || row.agent?.updated_at || "";
    const ms = at ? Date.parse(at) : NaN;
    return Number.isNaN(ms) ? Infinity : ms;
  }

  // ---- the summary strip -----------------------------------------------
  // Counts, and a way into each group. Never drawn from a projection the
  // page cannot currently see: a confident "0 stopped" on a dead socket
  // is the one number an operator must not be given.

  function strip(groups, connected) {
    const chips = GROUPS.map((g) => {
      const n = groups[g.key].length;
      const on = only === g.key;
      const broken = groups[g.key].some((r) => r.broken);
      return `<button class="ag-chip tone-${g.tone}${on ? " active" : ""}${
        broken ? " is-broken" : ""
      }" data-action="ag-group" data-group="${escAttr(g.key)}"
              aria-pressed="${on ? "true" : "false"}">
          <span class="ag-chip-n">${connected ? esc(String(n)) : "–"}</span>
          <span class="ag-chip-label">${esc(g.title)}</span>
        </button>`;
    }).join("");
    return `<div class="ag-strip" data-k="strip">${chips}${
      only
        ? `<button class="ag-clear" data-action="ag-group" data-group="">Show all</button>`
        : ""
    }</div>`;
  }

  // ---- one seat --------------------------------------------------------

  function row(entry) {
    const { seat, agent, sandbox, broken, reason } = entry;
    const handle = seat.handle || seat.name;
    const live = agent ? { ...agent, ...seat } : seat;
    const status = statusLine(live, { sandbox });
    const where = seat.unitPath?.length ? seat.unitPath.join(" › ") : "";
    const stale = agent && agent.updated_at ? staleness(agent.updated_at) : "";

    // A working seat shows what it is working ON: the phase, which
    // iteration, and the model burning the tokens. Those three are the
    // difference between "busy" and "stuck on round 9 of a loop".
    const phase = agent?.current_phase
      ? `<span class="ag-phase" style="color:${phaseInk(agent.current_phase)}">
           ${esc(agent.current_phase)}${esc(phaseSuffix(agent) || "")}
         </span>`
      : "";
    const model = agent?.live_call?.model
      ? `<span class="ag-model mono">${esc(agent.live_call.model)}</span>`
      : "";
    const tokens = agent?.total_tokens
      ? `<span class="ag-tokens num">${esc(fmtNum(agent.total_tokens))}</span>`
      : "";

    // THE WHOLE ROW OPENS THE SEAT. It used to be a plain div with the
    // name as the only target — a row you could point anywhere in and
    // click nothing, on the room whose entire job is getting you to a
    // seat. That also retires the "Open seat" button: a secondary button
    // repeating what the row already does is a second affordance for one
    // action, and it made the row's own click look like it did something
    // else. The chevron says the row goes somewhere; "LLM calls" is the
    // one place it goes that ISN'T the seat's front page, so it stays a
    // button. `closest("[data-action]")` resolves innermost-first, so
    // that button wins inside the row without any stopPropagation.
    return `
      <div class="ag-row clickable${broken ? " is-broken" : ""}${
        reason === "question" ? " is-waiting" : ""
      }" data-k="ag:${escAttr(handle)}"
           data-action="seat" data-seat="${escAttr(handle)}"
           role="link" tabindex="0">
        ${avatarFor(seat.name, broken ? "broken" : TONE[entry.group] || "quiet")}
        <div class="ag-id">
          <span class="ag-name">${esc(seat.name)}</span>
          <span class="ag-handle mono">@${esc(handle)}</span>
          ${where ? `<span class="ag-where">${esc(where)}</span>` : ""}
        </div>
        <div class="ag-what">
          <span class="ag-status">${esc(status)}</span>
          <span class="ag-meta">${phase}${model}${tokens}${
            stale ? `<span class="ag-stale">${esc(stale)}</span>` : ""
          }</span>
        </div>
        <div class="ag-when">${
          stamp(entry) === Infinity ? "" : esc(relTime(new Date(stamp(entry)).toISOString()))
        }</div>
        <div class="ag-do">
          <button class="btn sm" data-action="seat-calls" data-seat="${escAttr(handle)}"
                  title="${escAttr(`Every LLM call ${seat.name} has made`)}">
            ${icon("activity", "sm")}LLM calls
          </button>
          <span class="ag-go">${icon("chevron", "sm")}</span>
        </div>
      </div>`;
  }

  function group(g, rows) {
    if (!rows.length) return "";
    return `
      <section class="ag-group tone-${g.tone}" data-k="g:${escAttr(g.key)}">
        ${sectionHead("users", g.title, rows.length, null)}
        <p class="ag-blurb">${esc(g.blurb)}</p>
        <div class="ag-list list">${rows.map(row).join("")}</div>
      </section>`;
  }

  return {
    slices: ["org", "agents", "sandboxes", "health"],

    setParams(next = {}) {
      only = GROUPS.some((g) => g.key === next.group) ? next.group : "";
      refresh();
    },

    render(state) {
      const seats = seatsOf(state);
      if (!seats.length) {
        return emptyOrPending(
          state,
          () => skeletonCards(4),
          "users",
          "No agent seats",
          "Every seat in this company is a human seat, or no company is configured yet.",
        );
      }

      const groups = bucket(state);
      const connected = state.connected !== false;
      const shown = only ? [GROUPS.find((g) => g.key === only)] : GROUPS;

      const body = shown.map((g) => group(g, groups[g.key])).join("");
      return `
        ${strip(groups, connected)}
        ${
          connected
            ? ""
            : `<div class="card ag-alert" data-k="stale"><b>This page is not live.</b>
                 The socket is down, so every state below is the last one seen
                 rather than the one now.</div>`
        }
        ${
          body ||
          empty(
            "users",
            only ? "Nothing in that group" : "No seats to show",
            only ? "Clear the filter to see the rest." : "",
          )
        }`;
    },

    onAction(action, target) {
      if (action === "ag-group") {
        const next = target.dataset.group || "";
        only = GROUPS.some((g) => g.key === next) ? next : "";
        // A filter, so it replaces: Back means "leave this screen", not
        // "untick one chip".
        const search = only ? `?group=${encodeURIComponent(only)}` : "";
        history.replaceState(history.state, "", `#/agents${search}`);
        refresh();
      } else if (action === "seat-calls") {
        // Straight to the turns, which is what somebody clicking an agent
        // almost always wants and had to find behind a tab.
        navigate(
          `/seats/${encodeURIComponent(target.dataset.seat || "")}?tab=now`,
        );
      }
      // `seat` is handled by the shell.
    },

    __classifyForTest: classify,
  };
}
