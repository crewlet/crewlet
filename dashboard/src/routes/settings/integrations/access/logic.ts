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

/** Whether the engine leaves a lane's access to the company, outside Crewlet. */
export function managedOutside(lane: Lane): boolean {
  return lane.managed === "external";
}

/** Whether a seat's credential was provided from outside Crewlet, which Crewlet never rotates. */
export function providedExternally(lane: Lane, o: AccessObserved): boolean {
  return o.credential?.source === "external" || managedOutside(lane);
}

/** What the access cell and the details say for a lane managed outside Crewlet. */
export const MANAGED_OUTSIDE = "Managed outside Crewlet";

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

/** The ids a level holds: one, or for a lane of sets, each chosen one. */
export function levelIds(level: string | undefined): string[] {
  return (level ?? "").split(" ").filter((id) => id !== "");
}

/** The level a lane of sets stores for the ids chosen: sorted and space-separated. */
export function joinLevels(ids: string[]): string {
  return [...new Set(ids.filter((id) => id !== ""))].sort().join(" ");
}

const sameLevel = (lane: Lane, a: string | undefined, b: string | undefined) =>
  lane.multiple
    ? [...levelIds(a)].sort().join(" ") === [...levelIds(b)].sort().join(" ")
    : (a ?? "") === (b ?? "");

/** Whether two desired states ask for the same thing. */
export function sameDesired(lane: Lane, a: AccessDesired, b: AccessDesired): boolean {
  return (
    a.enabled === b.enabled &&
    sameLevel(lane, a.level, b.level) &&
    samePermissions(lane, a.permissions, b.permissions) &&
    sameScope(a.scope, b.scope)
  );
}

/**
 * The vendor's exact name for a level; for a lane of sets, the names chosen
 * in the vendor's order ("View, Add pages"), or "No permissions".
 */
export function levelName(lane: Lane, id: string | undefined): string {
  const only = productOnly(lane, id);
  if (only) return only;
  if (lane.multiple) {
    const ids = new Set(levelIds(id));
    if (ids.size === 0) return "No permissions";
    const known = lane.levels.filter((l) => ids.has(l.id));
    const unknown = [...ids].filter((x) => !lane.levels.some((l) => l.id === x));
    return [...known.map((l) => l.name), ...unknown].join(", ");
  }
  return lane.levels.find((l) => l.id === id)?.name ?? id ?? "None";
}

/** The empty level, where the vendor offers one: the product with no role (Jira, Confluence). */
export function emptyLevel(lane: Lane) {
  return lane.levels.find((l) => l.id === "");
}

/**
 * The empty level's name ("Product access only") where a seat holds it, or
 * null. On a lane of sets it is the empty set.
 */
export function productOnly(lane: Lane, id: string | undefined): string | null {
  const empty = emptyLevel(lane);
  return empty && levelIds(id).length === 0 ? empty.name : null;
}

/**
 * Whether a seat's scope is offered: never on a lane managed outside Crewlet,
 * and not with product access only, unless a list is already held there
 * (which the document refuses), so it can be cleared.
 */
export function scopeApplies(lane: Lane, want: AccessDesired): boolean {
  if (lane.scope === undefined || managedOutside(lane)) return false;
  return productOnly(lane, want.level) === null || (want.scope ?? []).length > 0;
}

/** A set of levels for a table cell: two names in the vendor's order, then a count. */
export function levelSummary(lane: Lane, level: string | undefined): string {
  const names = levelName(lane, level).split(", ");
  return names.length <= 2
    ? names.join(", ")
    : `${names.slice(0, 2).join(", ")} and ${names.length - 2} more`;
}

/**
 * Why a seat's draft cannot be saved, or null: product access only together
 * with a project or space list, which needs a role; or, where the vendor has
 * no empty level, a seat turned on with no role or permission chosen.
 */
export function refusedLevel(lane: Lane, want: AccessDesired): string | null {
  if (!want.enabled) return null;
  const noun = laneWords(lane.key).scope?.noun.toLowerCase() ?? "scope";
  if (productOnly(lane, want.level) && (want.scope ?? []).length > 0)
    return `Product access only on ${lane.name} takes no ${noun}. Choose a role, or clear its ${noun}.`;
  if (levelIds(want.level).length === 0 && lane.levels.length > 0 && !emptyLevel(lane))
    return lane.multiple
      ? `Choose at least one ${lane.name} permission for every agent that is on.`
      : `Choose a ${lane.name} role for every agent that is on.`;
  return null;
}

/**
 * The empty level's note under a seat on a lane of sets that holds it, where
 * the multi-select has no menu row to carry it, or null.
 */
export function productOnlyNote(lane: Lane, want: AccessDesired): string | null {
  if (!lane.multiple || !want.enabled || !productOnly(lane, want.level)) return null;
  return emptyLevel(lane)?.note ?? null;
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
  if (managedOutside(lane)) return MANAGED_OUTSIDE;
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
  if (lane.rotatable && o.credential && !providedExternally(lane, o)) out.push("rotate");
  if (!saved.desired.enabled && o.exists) out.push("remove");
  return out;
}

/** The one sentence a seat that is off reads as. */
export function offSentence(lane: Lane, saved: AccessSeatLane, departed = false): string {
  if (departed)
    return saved.observed.exists
      ? "This agent left the company. Its account is kept, disabled, until you remove it."
      : "This agent left the company and has no account here.";
  if (managedOutside(lane))
    return "Off. Turning it on gives it its tool with the credential provided outside Crewlet.";
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
  return hint && !managedOutside(lane) && want.enabled && (want.scope ?? []).length === 0
    ? hint
    : null;
}

/**
 * Why a reader may not act on agents' access here, or undefined. A document
 * the operator writes holds only what writes a revision, which the tab hides
 * on its own (`readOnly`), so its actions at the vendor stay with an admin; a
 * document managed any other way holds everything.
 */
export function accessHold(
  managed: string | null,
  admin: boolean,
  operator: string | undefined,
): string | undefined {
  const notAdmin = admin ? undefined : "Admins change an agent's access.";
  return operator ? notAdmin : (managed ?? notAdmin);
}
