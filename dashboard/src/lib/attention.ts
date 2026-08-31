/**
 * What needs a person.
 *
 * Every one of these conditions was already known to the dashboard and each
 * lived in a different screen: a sandbox run parked on a question was a badge
 * on the board (and a run whose box had been reclaimed appeared NOWHERE at
 * all), a seat the engine stopped was a card among the healthy ones, a budget
 * refusing charges was a bar on one seat's page, and an engine with no active
 * configuration — dropping every inbound webhook — was a line in a popover.
 *
 * They are one list because they are one question, and it is the question an
 * operator opens this page with. Ordered by what it costs to ignore, and every
 * item carries where to go.
 */

import type { AgentRow, EngineHealth, OrgBudget, SandboxEntry } from "~/protocol/index.ts";
import type { IconName } from "~/ui/Icon.tsx";
import { runState, staleness, type Seat } from "./seats.ts";

export type Severity = "critical" | "caution" | "info";

export interface Attention {
  id: string;
  severity: Severity;
  icon: IconName;
  /** What happened, in the fewest words that are still true. */
  title: string;
  /** What it costs to leave it, or what to do about it. */
  detail: string;
  /** Where the answer is. */
  path?: string[];
  query?: Record<string, string>;
  /** The instant this became true, for ordering within a severity. */
  at?: string;
  who?: string;
}

export interface AttentionInput {
  agents: AgentRow[];
  sandboxes: SandboxEntry[];
  budget: OrgBudget;
  engine: EngineHealth | null;
  seats: Seat[];
  connected: boolean;
  authRejected: boolean;
  now: number;
}

const ORDER: Record<Severity, number> = { critical: 0, caution: 1, info: 2 };

export function attentionQueue(input: AttentionInput): Attention[] {
  const out: Attention[] = [];
  const { agents, sandboxes, budget, engine, connected, authRejected, now } = input;

  // --- the engine itself ---------------------------------------------------
  if (authRejected) {
    out.push({
      id: "auth",
      severity: "critical",
      icon: "key",
      title: "The engine refused this browser's token",
      detail:
        "Reads and writes are both blocked. Set a token matching one of the api.auth.tokens entries.",
    });
  } else if (!connected) {
    out.push({
      id: "offline",
      severity: "critical",
      icon: "power",
      title: "No connection to the engine",
      detail:
        "The page is showing the last state it received and polling a REST snapshot until the socket returns.",
    });
  }

  // An engine with no active company revision looks exactly like a healthy
  // idle one, and drops every inbound webhook. This is the whole reason the
  // engine carries a `configured` flag.
  if (engine && engine.configured === false) {
    out.push({
      id: "unconfigured",
      severity: "critical",
      icon: "sliders",
      title: "No company configuration is active",
      detail:
        "The engine is running with nothing to run: no seats are spawned and every inbound webhook is dropped. Import a company revision.",
      path: ["config"],
    });
  }
  if (engine?.posture && ["shed", "stuck", "isolated"].includes(engine.posture)) {
    out.push({
      id: `posture-${engine.posture}`,
      severity: "critical",
      icon: "server",
      title: `This node's control-plane posture is "${engine.posture}"`,
      detail:
        engine.posture === "shed"
          ? "It has released its seats because it could not reach the configuration it is supposed to run."
          : "It cannot converge on the fleet's active configuration.",
      path: ["fleet"],
    });
  }
  if (engine?.shutting_down) {
    out.push({
      id: "draining",
      severity: "caution",
      icon: "power",
      title: "This node is draining",
      detail: `${engine.in_flight ?? 0} turn(s) still in flight. Seats are released as each finishes.`,
      path: ["fleet"],
    });
  }

  // --- budgets -------------------------------------------------------------
  const org = budget?.org;
  if (org?.refused_at) {
    out.push({
      id: "org-budget",
      severity: "critical",
      icon: "coin",
      title: "The company token budget is refusing charges",
      detail: `Turns are being declined at the budget gate. Last refusal ${org.refused_at}.`,
      path: ["spend"],
      at: org.refused_at,
    });
  } else if (org && org.max > 0 && org.used / org.max >= 0.9) {
    out.push({
      id: "org-budget-near",
      severity: "caution",
      icon: "coin",
      title: "The company token budget is nearly spent",
      detail: `${Math.round((org.used / org.max) * 100)}% of the process-lifetime meter is used.`,
      path: ["spend"],
    });
  }

  // --- sandboxes -----------------------------------------------------------
  for (const box of sandboxes) {
    if (box.status !== "awaiting_input") continue;
    out.push({
      id: `sandbox-${box.turn_id}`,
      severity: "caution",
      icon: "help",
      title: `${box.role || box.agent_handle} is waiting on an answer`,
      detail: box.question || "A coding run paused on a clarification and cannot continue.",
      path: ["runs"],
      query: { run: box.turn_id },
      at: box.started_at,
      who: box.agent_handle,
    });
  }

  // --- seats ---------------------------------------------------------------
  for (const agent of agents) {
    const state = runState(agent, sandboxes);
    if (agent.last_error) {
      out.push({
        id: `error-${agent.role}`,
        severity: "critical",
        icon: "alert",
        title: `${agent.role} stopped: ${agent.last_error.kind || "error"}`,
        detail: agent.last_error.message || "The seat stopped and has not done work since.",
        path: ["seats", String(agent.handle ?? agent.id)],
        at: agent.last_error.at,
        who: String(agent.handle ?? agent.role),
      });
      continue;
    }
    if (state === "afk") {
      out.push({
        id: `afk-${agent.role}`,
        severity: "caution",
        icon: "pause",
        title: `${agent.role} is AFK`,
        detail: agent.afk_reason
          ? `The engine paused it: ${agent.afk_reason}.`
          : "The engine paused this seat.",
        path: ["seats", String(agent.handle ?? agent.id)],
        who: String(agent.handle ?? agent.role),
      });
      continue;
    }
    // A live call that has not moved is the condition a spinning row hides.
    const call = agent.live_call;
    if (call?.in_progress) {
      const how = staleness(call.updated_at, now);
      if (how) {
        out.push({
          id: `stale-${agent.role}-${call.turn_id}`,
          severity: how === "stalled" ? "critical" : "caution",
          icon: "clock",
          title:
            how === "stalled"
              ? `${agent.role} has been on one round for over 10 minutes`
              : `${agent.role} has been on one round for over 2 minutes`,
          detail: `${call.phase} · round ${call.round_num >= 0 ? call.round_num : "?"} — no update since ${call.updated_at}.`,
          path: ["seats", String(agent.handle ?? agent.id)],
          query: { tab: "model" },
          at: call.updated_at,
          who: String(agent.handle ?? agent.role),
        });
      }
    }
    const meter = agent.budget;
    if (meter?.refused_at) {
      out.push({
        id: `seat-budget-${agent.role}`,
        severity: "caution",
        icon: "coin",
        title: `${agent.role}'s token budget is refusing charges`,
        detail: "This seat's turns are being declined at the budget gate.",
        path: ["seats", String(agent.handle ?? agent.id)],
        query: { tab: "cost" },
        at: meter.refused_at,
      });
    }
  }

  return out.sort((a, b) => {
    const d = ORDER[a.severity] - ORDER[b.severity];
    if (d !== 0) return d;
    // Newest first inside a severity: the thing that just broke is the thing
    // being looked for.
    const at = a.at ? Date.parse(a.at) : 0;
    const bt = b.at ? Date.parse(b.at) : 0;
    if (at !== bt) return bt - at;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}
