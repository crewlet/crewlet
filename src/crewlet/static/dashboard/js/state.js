// Agent-state, phase, and colour logic shared across views.

import { parseUTC } from "./format.js";
import { icon } from "./icons.js";

// Phase ordering + colours (CSS custom-property names).
export const PHASE_ORDER = {
  onboarding: 0,
  plan: 1,
  execute: 2,
  review: 3,
  auxiliary: 4,
  subagent: 5,
  judge: 6,
  turn: 99,
};

// Phase → hue family. The assignment is not arbitrary: it is the one that
// keeps every pair the UI renders side by side separable, on both themes,
// for normal and red-green colour vision — the phase bar stacks these in
// PHASE_ORDER, so neighbours in that order are what has to hold apart. See
// the categorical-hue note at the top of styles/tokens.css for the measured
// separations. Each family ships a mark step (fills, bars, dots) and an ink
// step (text) so a phase label always clears contrast.
const PHASE_HUE = {
  onboarding: "green",
  plan: "blue",
  execute: "amber",
  review: "purple",
  auxiliary: "orange",
  subagent: "orange",
  judge: "cyan",
};

export const PHASE_COLOR = Object.fromEntries(
  Object.entries(PHASE_HUE).map(([phase, hue]) => [phase, `var(--${hue})`]),
);

// The mark step — bar segments, legend swatches, row accents.
export function phaseColor(phase) {
  return PHASE_COLOR[phase] || "var(--text-muted)";
}

// The text step — phase pills and any label set in the phase's colour.
export function phaseInk(phase) {
  const hue = PHASE_HUE[phase];
  return hue ? `var(--${hue}-ink)` : "var(--text-muted)";
}

// Identity hues for agent avatars. CSS custom properties rather than hex, so
// the tile re-steps with the theme instead of being frozen at one theme's
// values.
const PALETTE = [
  "var(--blue)",
  "var(--green)",
  "var(--amber)",
  "var(--pink)",
  "var(--cyan)",
  "var(--purple)",
  "var(--orange)",
  "var(--brown)",
];

function hash(name) {
  let h = 0;
  for (const c of String(name || "")) h = c.charCodeAt(0) + ((h << 5) - h);
  return Math.abs(h);
}

export function roleColor(name) {
  return PALETTE[hash(name) % PALETTE.length];
}

// The same identity hue as a text step, for a name set in the seat's colour.
export function roleInk(name) {
  return roleColor(name).replace(/\)$/, "-ink)");
}

// Map a role name to a representative icon.
export function roleIcon(name) {
  const n = String(name || "").toLowerCase();
  if (/ceo|chief|founder|president|owner/.test(n)) return "crown";
  if (/cto|vp|head|director|lead|manager/.test(n)) return "crown";
  if (/eng|dev|architect|programmer|coder|sre|ops/.test(n)) return "code";
  if (/design|ux|ui|brand/.test(n)) return "diamond";
  if (/market|growth|sales|comm|globe/.test(n)) return "globe";
  if (/product|pm|analyst|research/.test(n)) return "clipboard";
  return "user";
}

export function stateBadgeClass(state) {
  switch (state) {
    case "working":
    case "awaiting_sandbox":
      return "live";
    case "idle":
      return "done";
    case "afk":
      return "afk";
    case "failed":
      return "failed";
    default:
      return "pending";
  }
}

// An agent with an in-flight detached sandbox run is still busy ("awaiting
// sandbox"), even though its kick-off turn already emitted TaskCompleted
// (which the projection reads as idle). Derive the displayed state from the
// live active-sandboxes set at render time so it's correct on both the
// initial snapshot and live SandboxRun* events.
export function effectiveAgentState(agent, sandboxes) {
  const role = agent.role || agent.name;
  if (role && (sandboxes || []).some((s) => s.role === role)) {
    return "awaiting_sandbox";
  }
  return agent.state || "offline";
}

export function stateLabel(state) {
  // "sandbox", not "awaiting sandbox": the badge shares a seat card's
  // top row with the seat's name, and the longer phrase pushed the name
  // into an ellipsis on every card carrying it. The status line
  // underneath says what it is waiting for, and which coding agent.
  return state === "awaiting_sandbox" ? "sandbox" : state || "offline";
}

// A human, slightly-wry quip explaining WHY an agent is AFK, keyed on the
// engine-detected failure cause.  The generic fallback is the safety net.
export function afkQuip(reason) {
  const quips = {
    llm_unavailable: "🧠 brain unplugged — the LLM provider is unreachable",
    stall: "😴 stalled — no forward progress, gave up the turn",
    max_iter: "🔁 hit the round cap before finishing",
    unhandled_exception: "💥 tripped over an unhandled error mid-turn",
    budget_exhausted: "💸 out of token budget for now",
    depth_cap: "🪜 delegation went too deep and stopped",
    scheduled_timeout: "⏰ scheduled turn ran past its time cap",
  };
  return quips[reason] || "☕ stepped out for coffee (engine-detected pause)";
}

// What this seat is doing, in a sentence — the line under a name on an
// agent card. Derived from live state only: an agent with no in-flight work
// says so rather than being given something plausible to be doing.
const PHASE_DOING = {
  onboarding: "reading the team's onboarding docs",
  plan: "planning the task",
  execute: "carrying out the plan",
  review: "reviewing its own work",
};

export function statusLine(seat, { sandbox = null, human = false } = {}) {
  if (human) return seat.availability || "human teammate — not run by the engine";
  if (sandbox) {
    return sandbox.status === "awaiting_input"
      ? "waiting on an answer to keep coding"
      : `writing code in a sandbox (${sandbox.coding_agent || "coding agent"})`;
  }
  const state = seat.state || "offline";
  if (state === "afk") return afkQuip(seat.afk_reason);
  if (state === "working") {
    return PHASE_DOING[seat.current_phase] || "working on a task";
  }
  if (state === "terminated") return "terminated";
  if (state === "offline") return "not running";
  return "idle — nothing in the inbox";
}

// ---------------------------------------------------------------------
// Staleness
// ---------------------------------------------------------------------
// A live row's pips animate, which sells motion — so a turn that has been
// on round 3 for eleven minutes looked exactly like one that started two
// seconds ago, and "is anything stuck?" was unanswerable on a page whose
// whole job is to answer it.

// A round that has not produced an update in this long is suspicious: it
// is either a genuinely long tool call (a sandbox launch, a slow MCP
// server) or a hang. Two minutes clears every builtin tool and the
// engine's own LLM call latency with room to spare.
export const STALE_MS = 120_000;
// And this long means it is not coming back on its own — long past any
// provider timeout, so the row says so in red and stops animating.
export const STALLED_MS = 600_000;

/**
 * How stale a live thing is: `""`, `"stale"`, or `"stalled"`.
 *
 * `at` is an ISO timestamp of the last sign of life.
 */
export function staleness(at, now = Date.now()) {
  const d = parseUTC(at);
  if (!d) return "";
  const age = now - d.getTime();
  if (age >= STALLED_MS) return "stalled";
  if (age >= STALE_MS) return "stale";
  return "";
}

// Integration surfaces a seat can act on, read from the MCP servers wired to
// it (its own `mcp_env` plus every ancestor unit's, which it inherits).
export function seatIntegrations(mcpEnvChain) {
  const seen = [];
  for (const env of mcpEnvChain) {
    for (const server of Object.keys(env || {})) {
      const key = String(server).toLowerCase();
      if (!seen.includes(key)) seen.push(key);
    }
  }
  return seen;
}

// A human seat's `contact` fields, and the integration each one identifies
// them on. Human seats hold no MCP credentials — they are reachable, not
// runnable — so this is what the same chip row shows for them.
export const CONTACT_FIELDS = [
  ["slack_user_id", "slack"],
  ["mattermost_user_id", "mattermost"],
  ["atlassian_account_id", "jira"],
  ["github_login", "github"],
  ["gitlab_username", "gitlab"],
  ["plane_user_id", "plane"],
];

export function contactIntegrations(contact) {
  return CONTACT_FIELDS.filter(([field]) => contact && contact[field]).map(([, key]) => key);
}

// " · plan #2" suffix on a working badge.
export function phaseSuffix(a) {
  if (a.state !== "working" || !a.current_phase) return "";
  const iter = a.current_iteration ? ` #${a.current_iteration}` : "";
  return ` · ${a.current_phase}${iter}`;
}

export function avatarFor(role) {
  return `<span class="avatar" style="--avatar-color:${roleColor(role)}">${icon(roleIcon(role))}</span>`;
}

// Event categories → label + accent class (matches the server-side map).
export const EVENT_CATEGORIES = [
  { key: "lifecycle", label: "Lifecycle" },
  { key: "task", label: "Task" },
  { key: "communication", label: "Comms" },
  { key: "decision", label: "Decision" },
  { key: "knowledge", label: "Knowledge" },
  { key: "learning", label: "Learning" },
  { key: "a2a", label: "A2A" },
  { key: "notification", label: "Notify" },
  { key: "webhook", label: "Webhook" },
  { key: "system", label: "System" },
];

export function catClass(category) {
  return "cat-" + (category || "system");
}

// External integrations a notification can originate from. Each maps to a
// display label, sprite icon id, and accent colour so the dashboard
// identifies the source at a glance instead of a generic "notification".
export const INTEGRATIONS = {
  slack: { label: "Slack", icon: "message", color: "var(--purple-ink)" },
  mattermost: { label: "Mattermost", icon: "hash", color: "var(--brown-ink)" },
  jira: { label: "Jira", icon: "clipboard", color: "var(--blue-ink)" },
  github: { label: "GitHub", icon: "git", color: "var(--text)" },
  gitlab: { label: "GitLab", icon: "git", color: "var(--orange-ink)" },
  confluence: { label: "Confluence", icon: "book", color: "var(--cyan-ink)" },
  plane: { label: "Plane", icon: "clipboard", color: "var(--green-ink)" },
  email: { label: "Email", icon: "inbox", color: "var(--amber-ink)" },
};

// Resolve an integration key (e.g. "slack") to its display metadata,
// title-casing the key with a generic icon as the fallback for any custom
// source an extension registered.
export function integrationMeta(key) {
  const k = String(key || "").toLowerCase();
  if (INTEGRATIONS[k]) return INTEGRATIONS[k];
  return {
    label: k ? k.charAt(0).toUpperCase() + k.slice(1) : "External",
    icon: "inbox",
    color: "var(--pink-ink)",
  };
}

// Notification events stamp their source as "notification_service.<key>"
// (e.g. "notification_service.slack"). Return the bare integration key,
// or "" for any non-notification source.
export function integrationFromSource(source) {
  const prefix = "notification_service.";
  const s = String(source || "");
  return s.startsWith(prefix) ? s.slice(prefix.length) : "";
}

// A compact branded badge (icon + label) identifying an integration.
export function integrationBadge(key, cls = "") {
  const m = integrationMeta(key);
  return `<span class="int-badge ${cls}" style="--int-color:${m.color}">${icon(
    m.icon,
    "sm",
  )}${m.label}</span>`;
}
