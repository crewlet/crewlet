/**
 * The Agents tab's readings, apart from anything drawn: what a seat's row
 * says (its status word, what it holds at the vendor, its tool, its last
 * sync), which actions it can take now, and whether a draft differs from what
 * is saved. Whether a save raises a seat to administration access (D10) is
 * the engine's to say, on the plan (`step_up`).
 *
 * Pure functions of the engine's answer, so each reading is held by a unit
 * test rather than by a screen.
 */

import type {
  AccessAnswer,
  AccessDesired,
  AccessLaneKey,
  AccessObserved,
  AccessOptions,
  AccessSeatLane,
  GitHubAccess,
} from "~/contract/access.ts";
import { plural, relTime } from "~/lib/format.ts";
import { GITHUB_ACCESS_NAMES, laneWords } from "./words.ts";

type Tone = "success" | "neutral" | "warning" | "danger" | "info";

export type Permissions = Record<string, GitHubAccess>;

/**
 * One lane as the tab draws it: what the engine offers on it, keyed and named
 * in the reader's words, with its levels always a list.
 */
export interface Lane extends AccessOptions {
  key: AccessLaneKey;
  name: string;
  levels: NonNullable<AccessOptions["levels"]>;
}

/** The lanes an answer offers, in the engine's order. */
export function lanesOf(answer: AccessAnswer): Lane[] {
  return answer.options.map((o) => ({
    ...o,
    key: o.lane,
    name: laneWords(o.lane).name,
    levels: o.levels ?? [],
  }));
}

/** Who sealed a credential provided from outside Crewlet, which Crewlet never rotates. */
const EXTERNAL = "external";

/** Whether a seat's credential was provided from outside Crewlet. */
export function providedExternally(o: AccessObserved): boolean {
  return o.credential?.by === EXTERNAL;
}

/** The menu heading over a lane's levels: where the list was read from. */
export function levelsHeading(lane: Lane): string {
  return lane.live ? `Read from ${lane.name} now` : `${lane.name}'s own roles`;
}

const sameScope = (a: string[] | undefined, b: string[] | undefined) =>
  [...(a ?? [])].sort().join("\n") === [...(b ?? [])].sort().join("\n");

const order = (a: GitHubAccess | undefined) => (a === "write" ? 2 : a === "read" ? 1 : 0);

export function samePermissions(
  lane: Lane,
  a: Permissions | undefined,
  b: Permissions | undefined,
): boolean {
  return (lane.permissions ?? []).every((p) => (a?.[p.key] ?? "none") === (b?.[p.key] ?? "none"));
}

/** Whether two desired states ask for the same thing. */
export function sameDesired(lane: Lane, a: AccessDesired, b: AccessDesired): boolean {
  return (
    a.enabled === b.enabled &&
    (a.level ?? "") === (b.level ?? "") &&
    samePermissions(lane, a.permissions, b.permissions) &&
    sameScope(a.scope, b.scope)
  );
}

/** The vendor's exact name for a level. */
export function levelName(lane: Lane, id: string | undefined): string {
  return lane.levels.find((l) => l.id === id)?.name ?? id ?? "None";
}

/** "Contents: Read and write, Issues: Read-only", for the permissions held, in GitHub's words. */
export function permissionWords(lane: Lane, p: Permissions | undefined): string {
  const held = (lane.permissions ?? []).filter((x) => p?.[x.key]);
  if (held.length === 0) return "No permissions";
  return held.map((x) => `${x.name}: ${GITHUB_ACCESS_NAMES[p?.[x.key] ?? "none"]}`).join(", ");
}

/** "5 permissions", for a table cell. */
export function permissionCount(lane: Lane, p: Permissions | undefined): string {
  const n = (lane.permissions ?? []).filter((x) => p?.[x.key]).length;
  return n === 0 ? "No permissions" : plural(n, "permission");
}

/** What the vendor holds for a seat, in the vendor's terms. */
export function accessWords(lane: Lane, o: AccessObserved): string {
  if (lane.permissions) return permissionWords(lane, o.permissions);
  return laneWords(lane.key).fixed ?? levelName(lane, o.level);
}

/** A seat's scope as a sentence fragment: "gophrase/backend and gophrase/web", or the lane's empty words. */
export function scopeWords(lane: Lane, scope: string[] | undefined): string {
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
  lane: Lane,
  saved: AccessSeatLane,
  drafted: boolean,
  syncing: boolean,
  departed = false,
): Status {
  if (departed) return { label: "Agent left", tone: "warning" };
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
export function toolWords(lane: Lane, saved: AccessSeatLane): string {
  const through = laneWords(lane.key).tool;
  switch (saved.tool) {
    case "live":
      return `Live, ${through}`;
    case "builtin":
      return through;
    case "waiting":
      return `Waiting for the credential, ${through}`;
    case "no_server":
      return `Not set up: no ${laneWords(lane.key).server} tool server is declared`;
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
  lane: Lane,
  saved: AccessSeatLane,
  held: boolean,
  syncing: boolean,
): SeatAction[] {
  if (held || syncing) return [];
  const out: SeatAction[] = [];
  const o = saved.observed;
  if (laneWords(lane.key).rotatable && o.credential && !providedExternally(o)) out.push("rotate");
  if (!saved.desired.enabled && o.exists) out.push("remove");
  return out;
}

/** The one sentence a seat that is off reads as. */
export function offSentence(lane: Lane, saved: AccessSeatLane): string {
  return saved.observed.exists
    ? "Off. Its account is disabled and kept, so turning it on again restores it."
    : `Off. ${laneWords(lane.key).turnOn}`;
}

/** Whether `to` holds any permission wider than `from`, GitHub's order. */
export function widensPermissions(
  lane: Lane,
  from: Permissions | undefined,
  to: Permissions | undefined,
): boolean {
  return (lane.permissions ?? []).some((p) => order(to?.[p.key]) > order(from?.[p.key]));
}

/**
 * The quiet hint under a seat that is on with an empty scope, where an empty
 * scope is literal (Mattermost: no channels), or null.
 */
export function emptyScopeHint(lane: Lane, want: AccessDesired): string | null {
  const hint = laneWords(lane.key).scope?.emptyOn;
  return hint && want.enabled && (want.scope ?? []).length === 0 ? hint : null;
}
