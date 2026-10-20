/**
 * One state per tool card, from the surfaces it is made of
 * (`integration.Rollup`), so the room reads what the engine's room reads.
 *
 * Each present surface is judged on its own (`judge`) and the worst verdict
 * speaks for the tool: a teardown first, then the most serious state, then
 * the phase furthest from ready.
 */

import type { IntegrationTool, IntegrationToolState } from "~/contract/integrations.ts";
import { sentenceOf } from "./findings.ts";
import type { DemoSurfaceState } from "./loop.ts";
import { outcomeOf } from "./loop.ts";

/** What the roll-up knows about one present surface (`integration.SurfaceStatus`). */
export interface SurfaceStatus {
  key: string;
  enabled: boolean;
  state: DemoSurfaceState | null;
  converges: boolean;
  secretUsable: boolean | null;
  routes: boolean | null;
  endpointCurrent: boolean | null;
}

/** The phases, in the order the roll-up ranks them (`integration.Phases`). */
const PHASES = [
  "disconnecting",
  "unconfigured",
  "awaiting_admin",
  "provisioning",
  "activating",
  "degraded",
  "ready",
];

/** A phase in a reader's words (`Phase.Label`). */
export function phaseLabel(phase: string): string {
  switch (phase) {
    case "ready":
      return "Connected";
    case "degraded":
      return "Action required";
    case "awaiting_admin":
      return "Action needed";
    case "provisioning":
      return "Setting up agents";
    case "activating":
      return "Waiting for the provider";
    case "disconnecting":
      return "Disconnecting";
    case "unconfigured":
      return "Failed";
    default:
      return phase.replace(/_/g, " ");
  }
}

/** The report a surface's record stands for (`State.Reported`): a disconnect asked for reads as one under way. */
export function reported(state: DemoSurfaceState): {
  phase: string;
  actor?: string;
  detail?: string;
  action_url?: string;
} {
  if (state.disconnecting && state.phase !== "disconnecting") {
    return { phase: "disconnecting", actor: "engine", detail: "this integration is being removed" };
  }
  return {
    phase: state.phase,
    actor: state.actor,
    detail: state.detail,
    action_url: state.action_url,
  };
}

const RANK: Record<IntegrationToolState, number> = {
  attention: 3,
  not_connected: 2,
  connected: 1,
  not_in_use: 0,
};

interface Verdict {
  state: IntegrationToolState;
  label: string;
  reason: string;
  surface: string;
  phase: string;
  tearing: boolean;
}

/** What makes a ready surface unable to receive anything (`integration.ingressFault`). */
function ingressFault(s: SurfaceStatus, name: string): string {
  if (s.secretUsable === false) {
    return `${name}'s delivery secret did not resolve on this node, so every delivery is refused`;
  }
  if (s.routes === false) {
    return `${name} deliveries are verified and stored, and no parser turns them into work for a seat`;
  }
  if (s.endpointCurrent === false) {
    return `${name} is registered at an address this deployment no longer answers on, so its deliveries go nowhere`;
  }
  return "";
}

/** One surface's verdict (`integration.judge`). */
function judge(s: SurfaceStatus, name: string): Verdict {
  const v: Verdict = {
    state: "not_connected",
    label: "",
    reason: "",
    surface: s.key,
    phase: "",
    tearing: false,
  };
  const state = s.state;
  if (state && (state.disconnecting || state.phase === "disconnecting")) {
    const r = reported(state);
    return {
      ...v,
      label: phaseLabel(r.phase),
      reason: r.detail ?? "",
      phase: r.phase,
      tearing: true,
    };
  }
  if (state && reported(state).phase !== "") {
    const r = reported(state);
    v.phase = r.phase;
    v.label = phaseLabel(r.phase);
    v.reason = r.detail ?? "";
    const outcome = outcomeOf(r.phase, r.actor);
    v.state =
      outcome === "settled" ? "connected" : outcome === "blocked" ? "attention" : "not_connected";
    if (r.phase === "ready") {
      const fault = ingressFault(s, name);
      if (fault) return { ...v, state: "attention", label: "Action needed", reason: fault };
    }
    if (v.state !== "attention") {
      const expiring = state.findings.find((f) => f.kind === "credential_expiring");
      if (expiring) {
        v.state = "attention";
        v.label = "Credential expiring";
        v.reason = expiring.detail || sentenceOf(expiring.kind, expiring.subject);
      }
    }
    return v;
  }
  if (!s.enabled) {
    return {
      ...v,
      state: "not_in_use",
      label: "Paused",
      reason: `${name} is switched off in the company configuration`,
    };
  }
  if (!s.converges) {
    const fault = ingressFault(s, name);
    return fault
      ? { ...v, state: "attention", label: "Action needed", reason: fault }
      : { ...v, state: "connected", label: phaseLabel("ready") };
  }
  return {
    ...v,
    state: "not_connected",
    label: "Connecting",
    reason: `${name} is configured and the reconcile loop has not reported on it yet`,
  };
}

/** How far a phase is from ready, smaller being further (`integration.distance`). */
function distance(phase: string): number {
  const at = PHASES.indexOf(phase || "ready");
  return at >= 0 ? at * 2 : PHASES.length * 2 - 3;
}

function worse(a: Verdict, b: Verdict): boolean {
  if (a.tearing !== b.tearing) return a.tearing;
  if (RANK[a.state] !== RANK[b.state]) return RANK[a.state] > RANK[b.state];
  return distance(a.phase) < distance(b.phase);
}

/** One tool's card from its present surfaces (`integration.Rollup`). */
export function rollup(
  tool: { key: string; surfaces: { key: string; name: string }[] },
  present: SurfaceStatus[],
): IntegrationTool {
  const out: IntegrationTool = {
    key: tool.key,
    surfaces: tool.surfaces.map((s) => s.key),
    state: "not_in_use",
    label: "Not in use",
    reason: "",
  };
  const order = new Map(tool.surfaces.map((s, i) => [s.key, i]));
  const names = new Map(tool.surfaces.map((s) => [s.key, s.name]));
  const verdicts = present
    .map((s) => judge(s, names.get(s.key) || s.key))
    .sort((a, b) => (order.get(a.surface) ?? 0) - (order.get(b.surface) ?? 0));
  if (verdicts.length === 0) return out;
  let best = verdicts[0]!;
  for (const v of verdicts.slice(1)) if (worse(v, best)) best = v;
  out.state = best.state;
  out.label = best.label;
  out.reason = best.reason;
  out.surface = best.surface;
  return out;
}
