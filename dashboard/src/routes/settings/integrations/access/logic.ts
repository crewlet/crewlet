/**
 * The Agents tab's readings, apart from anything drawn: what a seat's row
 * says (its status word, what it holds at the vendor, its tool, its last
 * sync), which actions it can take now, whether a draft differs from what is
 * saved, and whether a save raises a seat to an administration level (D10).
 *
 * Pure functions of the engine's answer, so each reading is held by a unit
 * test rather than by a screen.
 */

import type {
  AccessDesired,
  AccessLane,
  AccessObserved,
  AccessSeatLane,
  GitHubAccess,
} from "~/contract/access.ts";
import { plural, relTime } from "~/lib/format.ts";
import { GITHUB_ACCESS_NAMES, laneWords } from "./words.ts";

type Tone = "success" | "neutral" | "warning" | "danger" | "info";

export type Permissions = Record<string, GitHubAccess>;

const sameScope = (a: string[] | undefined, b: string[] | undefined) =>
  [...(a ?? [])].sort().join("\n") === [...(b ?? [])].sort().join("\n");

const order = (a: GitHubAccess | undefined) => (a === "write" ? 2 : a === "read" ? 1 : 0);

export function samePermissions(
  lane: AccessLane,
  a: Permissions | undefined,
  b: Permissions | undefined,
): boolean {
  return (lane.permissions ?? []).every((p) => (a?.[p.key] ?? "none") === (b?.[p.key] ?? "none"));
}

/** Whether two desired states ask for the same thing. */
export function sameDesired(lane: AccessLane, a: AccessDesired, b: AccessDesired): boolean {
  return (
    a.enabled === b.enabled &&
    (a.level ?? "") === (b.level ?? "") &&
    samePermissions(lane, a.permissions, b.permissions) &&
    sameScope(a.scope, b.scope)
  );
}

/** The vendor's exact name for a level. */
export function levelName(lane: AccessLane, id: string | undefined): string {
  return lane.levels.find((l) => l.id === id)?.name ?? id ?? "None";
}

/** "Contents: Read and write, Issues: Read-only", for the permissions held, in GitHub's words. */
export function permissionWords(lane: AccessLane, p: Permissions | undefined): string {
  const held = (lane.permissions ?? []).filter((x) => p?.[x.key]);
  if (held.length === 0) return "No permissions";
  return held.map((x) => `${x.name}: ${GITHUB_ACCESS_NAMES[p?.[x.key] ?? "none"]}`).join(", ");
}

/** "5 permissions", for a table cell. */
export function permissionCount(lane: AccessLane, p: Permissions | undefined): string {
  const n = (lane.permissions ?? []).filter((x) => p?.[x.key]).length;
  return n === 0 ? "No permissions" : plural(n, "permission");
}

/** What the vendor holds for a seat, in the vendor's terms. */
export function accessWords(lane: AccessLane, o: AccessObserved): string {
  if (lane.permissions) return permissionWords(lane, o.permissions);
  if (lane.scopes) return plural(lane.scopes.length, "bot token scope");
  return levelName(lane, o.level);
}

/** A seat's scope as a sentence fragment: "gophrase/backend and gophrase/web", or the lane's empty words. */
export function scopeWords(lane: AccessLane, scope: string[] | undefined): string {
  const words = laneWords(lane.key).scope;
  if (!scope || scope.length === 0) return words?.empty ?? "None";
  if (scope.length === 1) return scope[0]!;
  return `${scope.slice(0, -1).join(", ")} and ${scope[scope.length - 1]}`;
}

export interface Status {
  label: string;
  tone: Tone;
  outline?: boolean;
  busy?: boolean;
}

/** One lane's word, from what is saved, what is drafted and what the last sync found. */
export function statusOf(
  lane: AccessLane,
  saved: AccessSeatLane,
  drafted: boolean,
  syncing: boolean,
): Status {
  if (drafted) return { label: "Not saved", tone: "info", outline: true };
  const pending = saved.plan.length > 0;
  if (syncing && pending) return { label: "Syncing", tone: "info", busy: true };
  if (saved.sync?.outcome === "failed") return { label: "Refused", tone: "danger" };
  if (saved.observed.changed_by_hand_at && pending)
    return { label: `Changed at ${lane.name}`, tone: "warning" };
  if (saved.sync?.outcome === "waiting" || saved.plan.some((s) => s.by === "you"))
    return { label: "Needs you", tone: "warning" };
  if (pending) return { label: "Next sync", tone: "info" };
  if (saved.desired.enabled) return { label: "In sync", tone: "success" };
  if (saved.observed.exists) return { label: "Off, account kept", tone: "neutral" };
  return { label: "Off", tone: "neutral", outline: true };
}

/** Whether a lane's status needs a person's attention. */
export function needsAttention(status: Status): boolean {
  return status.tone === "danger" || status.tone === "warning";
}

/** The tool, as one value: whether it works, and what it works through. */
export function toolWords(lane: AccessLane, saved: AccessSeatLane): string {
  const through = laneWords(lane.key).tool;
  switch (saved.tool) {
    case "live":
      return `Live, ${through}`;
    case "builtin":
      return through;
    case "waiting":
      return `Waiting for the credential, ${through}`;
    case "no_server":
      return `Not set up: no ${lane.tool_server ?? lane.key} tool server is declared`;
    case "off":
      return "None while off";
  }
}

/** The last sync, as one value. */
export function syncValue(saved: AccessSeatLane, now: number): string {
  if (!saved.sync) return "Never";
  const when = relTime(saved.sync.at, now);
  if (saved.sync.outcome === "failed") return `Refused ${when}`;
  if (saved.sync.outcome === "waiting") return `Waiting on a person since ${when}`;
  return when;
}

export type SeatAction = "rotate" | "remove";

/** The actions a seat can take now. One that is not available is not drawn at all. */
export function availableActions(
  lane: AccessLane,
  saved: AccessSeatLane,
  held: boolean,
  syncing: boolean,
): SeatAction[] {
  if (held || syncing) return [];
  const out: SeatAction[] = [];
  if (lane.rotatable && saved.observed.credential) out.push("rotate");
  if (!saved.desired.enabled && saved.observed.exists) out.push("remove");
  return out;
}

/** The one sentence a seat that is off reads as. */
export function offSentence(lane: AccessLane, saved: AccessSeatLane): string {
  return saved.observed.exists
    ? "Off. Its account is disabled and kept, so turning it on again restores it."
    : `Off. ${laneWords(lane.key).turnOn}`;
}

/**
 * Whether a change raises a seat to an administration level, which a save asks
 * a fresh step-up for (D10): an `admin` level it did not hold, or Read and
 * write on a GitHub permission marked `admin_write` that it did not have.
 */
export function raisesToAdmin(lane: AccessLane, from: AccessDesired, to: AccessDesired): boolean {
  if (!to.enabled) return false;
  const level = lane.levels.find((l) => l.id === to.level);
  if (level?.admin && (to.level !== from.level || !from.enabled)) return true;
  return (lane.permissions ?? []).some(
    (p) =>
      p.admin_write &&
      to.permissions?.[p.key] === "write" &&
      (!from.enabled || from.permissions?.[p.key] !== "write"),
  );
}

/** Whether `to` holds any permission wider than `from`, GitHub's order. */
export function widensPermissions(
  lane: AccessLane,
  from: Permissions | undefined,
  to: Permissions | undefined,
): boolean {
  return (lane.permissions ?? []).some((p) => order(to?.[p.key]) > order(from?.[p.key]));
}
