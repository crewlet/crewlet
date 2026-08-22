// The Org room: one company, looked at four ways.
//
// This was four screens — Company, People, Org chart, Agents — each
// showing a different projection of ONE payload (`/org`, plus the live
// agent rows) and each reachable only from the nav. Answering "who is
// this person, who do they report to, and is their agent running?" meant
// three navigations and a mental join, because no screen carried more
// than a third of the answer.
//
// So it is one room with three LENSES over the same seat list:
//
//   chart      the reporting structure, drawn — who answers to whom
//   directory  every seat as a person: where they sit, how to reach them
//   charter    who the company says it is — mission, vision, policies
//   seats      the runtime: one card per agent seat, ordered by urgency
//
// The lens is a query parameter, not view state, so a lens is a link: the
// old `#/company`, `#/people` and `#/agents` routes redirect here naming
// the one they used to be (see router.js).
//
// A pure store read — the org tree arrives on the snapshot and the live
// rows come from the projection, so this room never fetches anything.

import { esc, escAttr, fmtNum, trunc } from "../format.js";
import { icon } from "../icons.js";
import { flattenSeats, flattenUnits, managerOf } from "../org.js";
import { pushParams, replaceParams } from "../router.js";
import {
  CONTACT_FIELDS,
  avatarFor,
  effectiveAgentState,
  integrationMeta,
  stateBadgeClass,
  stateLabel,
  seatTone,
} from "../state.js";
import {
  empty,
  emptyOrPending,
  sectionHead,
  skeletonCards,
  skeletonRows,
  statStrip,
} from "../ui.js";

// The lenses, in the order the segmented control draws them: the shape of
// the company first, then its people, then what it says it is, then what
// the engine is actually running.
const LENSES = [
  { key: "chart", label: "Chart", icon: "building" },
  { key: "directory", label: "Directory", icon: "users" },
  { key: "charter", label: "Charter", icon: "book" },
];
const LENS_KEYS = new Set(LENSES.map((l) => l.key));
const DEFAULT_LENS = "chart";

// The directory's filter. `all` is the default and is left out of the URL
// rather than written as `kind=all`.
const KINDS = [
  ["all", "Everyone"],
  ["agent", "Agents"],
  ["human", "Humans"],
];
const KIND_KEYS = new Set(KINDS.map(([key]) => key));

// Seat order on the `seats` lens: what needs attention, then what is
// happening, then everything else.
const STATE_RANK = {
  afk: 0,
  working: 1,
  awaiting_sandbox: 1,
  idle: 2,
  offline: 3,
  terminated: 4,
};

function rank(agent) {
  if (!agent) return STATE_RANK.offline;
  if (agent.last_error) return -1;
  const r = STATE_RANK[agent.state];
  return r === undefined ? STATE_RANK.offline : r;
}

/**
 * The reporting tree, as `{ roots, kids }`.
 *
 * `managerOf` owns the rule — an explicit `manages[]` entry, else the
 * effective lead of the seat's unit — and stays the only place it is
 * written down. A seat whose manager is not itself a seat (a lead naming
 * somebody who was removed from the config) is a root: it is still in the
 * company, and dropping it would make the chart quietly incomplete.
 */
export function buildReporting(seats) {
  const known = new Set(seats.map((s) => s.name));
  const kids = new Map();
  const roots = [];
  for (const seat of seats) {
    const boss = managerOf(seats, seat.name);
    if (boss && boss !== seat.name && known.has(boss)) {
      const list = kids.get(boss);
      if (list) list.push(seat);
      else kids.set(boss, [seat]);
    } else {
      roots.push(seat);
    }
  }
  return { roots, kids };
}

export function createOrgRoomView({ params = {}, navigate, refresh }) {
  let lens = LENS_KEYS.has(params.lens) ? params.lens : DEFAULT_LENS;
  let kind = KIND_KEYS.has(params.kind) ? params.kind : "all";

  // The lens and the directory's filter live in the URL, so any view of
  // this room can be handed to somebody else as a link — and the two are
  // not the same kind of move. A lens is a screen: four of them, each
  // answering a different question about the company, and Back after
  // three of them should walk out through them rather than leave the
  // room. The directory's agent/human filter is a filter.
  function syncUrl(section = false) {
    const params = { lens, kind: kind === "all" ? "" : kind };
    if (section) pushParams("org", params);
    else replaceParams("org", params);
  }

  // ---- the lens bar ----

  // `data-k` is both the patcher's key for the tab and the lens key
  // `onAction` reads back off the clicked element — the set is fixed, so
  // the key identifies a tab across every render.
  function lensBar(counts) {
    const tab = (l) => {
      const on = lens === l.key;
      const count = counts[l.key];
      return `
        <span class="${on ? "org-lens-tab on" : "org-lens-tab"}" role="tab"
              data-action="org-lens" data-k="${escAttr(l.key)}"
              tabindex="0" aria-selected="${on ? "true" : "false"}">
          ${icon(l.icon, "sm")}<span class="org-lens-label">${esc(l.label)}</span>
          ${count == null ? "" : `<span class="org-lens-count">${esc(String(count))}</span>`}
        </span>`;
    };
    return `<div class="org-lens" role="tablist" aria-label="Org view">${LENSES.map(tab).join("")}</div>`;
  }

  // ---- lens: chart ----

  // One node. The name carries the seat's identity hue, the dot carries
  // its live state — and a human seat gets neither a run state nor a
  // link into the runtime, because the engine does not run it.
  function chartNode(seat, byRole, sandboxes) {
    const human = seat.kind === "human";
    const agent = human ? null : byRole.get(seat.name);
    const runState = human
      ? "human"
      : agent
        ? effectiveAgentState(agent, sandboxes)
        : "offline";
    const nav = human
      ? ""
      : ` data-action="seat" data-seat="${escAttr(seat.handle)}" role="link" tabindex="0"`;
    return `
      <div class="org-node ${human ? "is-human" : "clickable"}"${nav}
           title="${escAttr(seat.goal || seat.name)}">
        <span class="org-node-top">
          <i class="org-node-dot" data-state="${escAttr(runState)}"></i>
          <span class="org-node-name">${esc(seat.name)}</span>
        </span>
        <span class="org-node-handle mono">@${esc(seat.handle)}</span>
        <span class="org-node-foot">${
          human
            ? '<span class="badge purple">human</span>'
            : `<span class="badge ${stateBadgeClass(runState)}"><i class="dot"></i>${esc(stateLabel(runState))}</span>`
        }</span>
      </div>`;
  }

  function chartLens(state, seats, byRole) {
    const { roots, kids } = buildReporting(seats);
    // Every seat is drawn exactly once. The set is also the cycle guard:
    // `manages` is free text in the config, so A can name B while B names
    // A, which leaves both parented and neither reachable from a root.
    const drawn = new Set();
    const branch = (seat) => {
      drawn.add(seat.name);
      const reports = (kids.get(seat.name) || []).filter((c) => !drawn.has(c.name));
      return `
        <div class="org-branch" data-k="node:${escAttr(seat.name)}">
          ${chartNode(seat, byRole, state.sandboxes)}
          ${reports.length ? `<div class="org-kids">${reports.map(branch).join("")}</div>` : ""}
        </div>`;
    };
    const trees = roots.map(branch);
    // Whatever a cycle stranded is planted as its own root, which breaks
    // the loop at its first member. A chart whose one job is to show
    // every seat must not silently lose two of them to a typo.
    for (const seat of seats) if (!drawn.has(seat.name)) trees.push(branch(seat));
    return `
      ${sectionHead("building", "Reporting structure", seats.length, null)}
      <div class="org-chart">
        <div class="org-tree">${trees.join("")}</div>
      </div>
      <div class="org-chart-note">
        A line means one seat reports to another — an explicit
        <code>manages</code> entry, or the lead of the unit the seat sits in.
      </div>`;
  }

  // ---- lens: directory ----

  // Where a seat is reachable. Each chip is keyed by the contact field it
  // came from — the field set is fixed and ordered by CONTACT_FIELDS, so
  // the key identifies a chip across renders even when a seat gains one.
  function contactChips(seat) {
    const out = [];
    for (const [field, key] of CONTACT_FIELDS) {
      const value = seat.contact && seat.contact[field];
      if (!value) continue;
      const meta = integrationMeta(key);
      out.push(
        `<span class="int-badge sm" data-k="c:${escAttr(field)}" style="--int-color:${meta.color}" title="${escAttr(field)}">${icon(
          meta.icon,
          "sm",
        )}${esc(value)}</span>`,
      );
    }
    if (seat.email) out.push(`<span class="chip" data-k="c:email">${esc(seat.email)}</span>`);
    return out.join("");
  }

  // One directory row, keyed by the seat's own name: that is the identity
  // the engine addresses a seat by (it is what `agents[].role` matches),
  // so the row keeps its node as the filter narrows or the org is edited.
  function directoryRow(seat, seats, byRole, sandboxes) {
    const human = seat.kind === "human";
    const agent = human ? null : byRole.get(seat.name);
    const runState = human
      ? "human"
      : agent
        ? effectiveAgentState(agent, sandboxes)
        : "offline";
    // Through the shared helpers, not a local ternary chain over the raw
    // state string — that is how `awaiting_sandbox` once reached a screen
    // verbatim, underscore and all.
    const badge = human
      ? '<span class="badge purple">human</span>'
      : `<span class="badge ${stateBadgeClass(runState)}"><i class="dot"></i>${esc(stateLabel(runState))}</span>`;
    const nav = human
      ? 'class="row"'
      : `class="row clickable" data-action="seat" data-seat="${escAttr(seat.handle)}" role="link" tabindex="0"`;
    const manager = managerOf(seats, seat.name);
    const where = [seat.unitPath.join(" › "), manager ? `reports to ${manager}` : ""]
      .filter(Boolean)
      .join(" · ");

    return `
      <div ${nav} data-k="seat:${escAttr(seat.name)}">
        ${avatarFor(seat.name, human ? "quiet" : seatTone(agent, sandboxes))}
        <div class="row-body">
          <div class="row-title">${esc(seat.name)}
            <span class="org-handle mono">@${esc(seat.handle)}</span></div>
          <div class="row-sub">${esc(where || "root level")}</div>
          ${seat.goal ? `<div class="row-sub">${esc(trunc(seat.goal, 96))}</div>` : ""}
          <div class="org-contacts">${contactChips(seat)}</div>
        </div>
        ${seat.availability ? `<span class="badge text">${esc(trunc(seat.availability, 28))}</span>` : ""}
        ${badge}
      </div>`;
  }

  function directoryLens(state, seats, byRole) {
    const counts = {
      all: seats.length,
      agent: seats.filter((s) => s.kind === "agent").length,
      human: seats.filter((s) => s.kind === "human").length,
    };
    const pills = KINDS.map(
      ([key, label]) =>
        `<span class="${kind === key ? "pill active" : "pill"}" data-action="org-kind" data-k="${key}" role="button" tabindex="0">${label} <span class="ct">${counts[key]}</span></span>`,
    ).join("");
    const shown = seats.filter((s) => kind === "all" || s.kind === kind);
    const body = shown.length
      ? `<div class="list">${shown
          .map((s) => directoryRow(s, seats, byRole, state.sandboxes))
          .join("")}</div>`
      : // The org IS configured — this list is empty because the filter
        // asked for a kind of seat this company does not have.
        empty(
          "users",
          kind === "human" ? "No human seats" : "No agent seats",
          kind === "human"
            ? "Add a role with `kind: human` to give a teammate a seat in the chart."
            : "Every seat in this company is a human seat.",
        );
    return `
      ${sectionHead("users", "People", shown.length, null)}
      <div class="filters">${pills}</div>
      ${body}`;
  }

  // ---- lens: charter ----

  function charterLens(state, seats, units) {
    const org = state.org || {};
    const humans = seats.filter((s) => s.kind === "human").length;
    const agents = seats.length - humans;
    const running = (state.agents || []).filter((a) => a.state !== "offline").length;
    const policies = org.policies || [];

    return `
      <div class="card org-charter" data-k="charter">
        <div class="eyebrow">Company</div>
        <h2 class="org-charter-name">${esc(org.name || "Unnamed company")}</h2>
        ${org.mission ? `<p class="org-charter-line">${esc(org.mission)}</p>` : ""}
        ${org.vision ? `<p class="org-charter-line is-quiet">${esc(org.vision)}</p>` : ""}
      </div>

      ${statStrip([
        {
          label: "Seats",
          value: fmtNum(seats.length),
          foot: esc(`${agents} agent · ${humans} human`),
        },
        {
          label: "Units",
          value: fmtNum(units.length),
          foot: esc(
            units.length ? `${units.filter((u) => !u.depth).length} at the top` : "flat org",
          ),
        },
        {
          label: "Spawned",
          value: fmtNum(running),
          foot: "agents the engine is running",
        },
        {
          label: "Policies",
          value: fmtNum(policies.length),
          foot: policies.length ? "org-wide" : "none set",
        },
      ])}

      ${
        policies.length
          ? sectionHead("check", "Policies", policies.length, null) +
            // A policy has no id — its own text is its identity.
            `<div class="list" data-k="policies">${policies
              .map(
                (p) => `
              <div class="row" data-k="policy:${escAttr(p)}">
                <span class="row-icon">${icon("check", "sm")}</span>
                <div class="row-body"><div class="row-title">${esc(p)}</div></div>
              </div>`,
              )
              .join("")}</div>`
          : ""
      }

      ${
        units.length
          ? sectionHead("folder", "Units", units.length, {
              action: "go",
              label: "See the chart",
              route: "/org?lens=chart",
            }) +
            // Unit names are unique across the tree, so the name keys the
            // row even as a unit changes depth under a live revision.
            `<div class="list" data-k="units">${units
              .map(
                (u) => `
              <div class="row" data-k="unit:${escAttr(u.name)}">
                <span class="row-icon" style="margin-left:calc(${u.depth} * var(--space-4))">${icon("folder", "sm")}</span>
                <div class="row-body">
                  <div class="row-title">${esc(u.name)} <span class="org-unit-type">${esc(u.type)}</span></div>
                  ${u.purpose ? `<div class="row-sub">${esc(u.purpose)}</div>` : ""}
                </div>
                ${u.lead ? `<span class="badge">lead · ${esc(u.lead)}</span>` : ""}
                <span class="row-ts">${fmtNum(u.seats)} seat${u.seats === 1 ? "" : "s"}</span>
              </div>`,
              )
              .join("")}</div>`
          : ""
      }`;
  }

  // ---- lens: seats ----


  return {
    slices: ["org", "agents", "sandboxes", "events", "tokens", "budget", "health"],

    render(state) {
      const org = state.org || {};
      if (!org.name && !(org.units || []).length && !(org.roles || []).length) {
        // An empty tree means three different things — nothing received
        // yet, no configuration at all, or a configuration with no org —
        // and `emptyOrPending` is where that is decided once. The lens
        // bar is deliberately absent: four ways of looking at nothing is
        // four empty screens.
        return emptyOrPending(
          state,
          () => skeletonCards(4),
          "building",
          "No company configured",
          "Import a company config with `crewlet config import`.",
        );
      }

      const seats = flattenSeats(org);
      const units = flattenUnits(org);
      // Role name → live agent row, first match wins. Hoisted so a large
      // org does not rescan the agent list once per seat on every emit.
      const byRole = new Map();
      for (const a of state.agents || []) {
        const role = a.role || a.name;
        if (role && !byRole.has(role)) byRole.set(role, a);
      }

      const bar = lensBar({
        directory: seats.length,
        seats: seats.filter((s) => s.kind === "agent").length,
        charter: units.length || null,
      });

      if (!seats.length && lens !== "charter") {
        // A configured company with no seats at all: the charter still
        // has something to say, the other three lenses do not.
        return (
          bar +
          emptyOrPending(state, () => skeletonRows(6), "users", "No seats configured")
        );
      }

      let body;
      if (lens === "directory") body = directoryLens(state, seats, byRole);
      else if (lens === "charter") body = charterLens(state, seats, units);
      else body = chartLens(state, seats, byRole);
      return bar + body;
    },

    // Back and Forward land here rather than remounting the room, so
    // the lens and filter are re-read from the URL. Idempotent: the
    // action handler has usually set them already, and the push it made
    // is what brought us back through.
    setParams(next = {}) {
      lens = LENS_KEYS.has(next.lens) ? next.lens : DEFAULT_LENS;
      kind = KIND_KEYS.has(next.kind) ? next.kind : "all";
      refresh();
    },

    onAction(action, target) {
      let section = false;
      if (action === "org-lens") {
        const next = target.dataset.k;
        if (!LENS_KEYS.has(next) || next === lens) return;
        lens = next;
        section = true;
      } else if (action === "org-kind") {
        const next = target.dataset.k;
        if (!KIND_KEYS.has(next) || next === kind) return;
        kind = next;
      } else if (action === "agent") {
        // The shared seat card still addresses a seat by its runtime id.
        // The shell routes `seat` (a handle) and forwards everything else
        // here, so this room resolves the id form rather than leaving its
        // own cards inert — the seat route takes either.
        navigate("/seats/" + encodeURIComponent(target.dataset.id || ""));
        return;
      } else {
        return;
      }
      syncUrl(section);
      refresh();
    },
  };
}
