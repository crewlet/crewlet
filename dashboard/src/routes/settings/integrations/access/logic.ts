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
  AccessBoxLine,
  AccessDesired,
  AccessLaneKey,
  AccessObserved,
  AccessOptions,
  AccessSeatLane,
  AccessStep,
  GitHubAccess,
} from "~/contract/access.ts";
import { plural, relTime } from "~/lib/format.ts";
import { andList } from "~/lib/words.ts";
import { integrationDocHref } from "../docs.ts";
import { GITHUB_ACCESS_NAMES, laneWords, type LaneWords } from "./words.ts";

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

/** What a person does at the vendor where only a person can make a seat's account: the Set up sheet's three openings. */
export type HandoffStep = "create" | "mint" | "rerole";

/**
 * One seat's handoff, as the Set up sheet draws it, read off the sync's wait
 * and the lane's own levels: nothing here is a guess at the vendor.
 */
export interface Handoff {
  step: HandoffStep;
  /** The vendor's page where the person acts, where the engine names it. */
  url: string | undefined;
  /** The engine's sentence for the wait; empty for a rotate, which waits on nothing yet. */
  said: string;
  /** The account to make, or the one the seat already has, by name, where the engine names it. */
  account: string | undefined;
  /**
   * The name to give what the person makes at the vendor, at the seat's
   * level: the engine's `hand_in`, else the account's own name on a lane
   * where the person makes an account. Never one this screen composes.
   */
  name: string | undefined;
  /** The role to give it, in the vendor's word, with the vendor's own line on what it allows. */
  role: { name: string; note: string | undefined } | undefined;
  /** The level chosen here, by its identifier, whose scopes a person ticks where the vendor has them. */
  level: string | undefined;
  /** The role the vendor shows now, where it is one of the lane's levels. */
  current: string | undefined;
  /** A token was sealed for the seat before (revoked at the vendor, or being rotated), so this is a new one. */
  renew: boolean;
  /** Opened to rotate a token the vendor still accepts: the sheet starts at the paste. */
  rotating: boolean;
}

const HANDOFF_STEPS: ReadonlySet<string> = new Set<HandoffStep>(["create", "mint", "rerole"]);

/**
 * What the engine says of a seat's account at the vendor: its name, the name
 * to give what the person makes, the role chosen, and the role it holds.
 *
 * THE ACCOUNT NAMES WHAT TO MAKE ONLY WHERE THE PERSON MAKES AN ACCOUNT (an
 * Expo robot). Where the token is the agent's whole identity, the observed
 * account is whose token it is ("Miles on gophrase"), never a name to type.
 */
function seatFacts(lane: Lane, saved: AccessSeatLane) {
  const level = (id: string | undefined) => lane.levels.find((l) => l.id !== "" && l.id === id);
  const chosen = level(saved.desired.level);
  const account = saved.observed.account || undefined;
  const makesAccount = laneWords(lane.key).handoff?.account !== undefined;
  return {
    account,
    name: saved.hand_in?.name || (makesAccount ? account : undefined),
    role: chosen ? { name: chosen.name, note: chosen.note } : undefined,
    level: chosen?.id,
    current: level(saved.observed.level)?.name,
  };
}

/**
 * The handoff a seat is waiting on, or undefined: only on a lane whose
 * account a person makes (its words say so), only while the sync waits on a
 * person, and only for a step the sheet opens on. Deleting a robot or an old
 * token at the vendor is a wait of its own, drawn on the row with its page.
 */
export function handoffOf(lane: Lane, saved: AccessSeatLane): Handoff | undefined {
  if (!laneWords(lane.key).handoff) return undefined;
  const wait = saved.sync?.outcome === "waiting" ? saved.sync.error : undefined;
  const url = wait?.action_url || lane.action_url;
  if (!wait || wait.actor !== "you" || !url || !HANDOFF_STEPS.has(wait.step)) {
    return undefined;
  }
  return {
    step: wait.step as HandoffStep,
    url,
    said: wait.said,
    ...seatFacts(lane, saved),
    renew: saved.observed.credential !== undefined || saved.observed.credential_gone === true,
    rotating: false,
  };
}

/**
 * The handoff a rotate opens on a lane whose tokens a person makes: a new
 * token for the account the seat already has, pasted here. The engine takes a
 * paste for a seat whose token still works as the rotate, so the seat action
 * is never called. The vendor's page is the lane's own, since a rotate waits
 * on nothing yet. Undefined on every other lane, which rotates by itself.
 */
export function rotateHandoffOf(lane: Lane, saved: AccessSeatLane): Handoff | undefined {
  if (!laneWords(lane.key).handoff) return undefined;
  return {
    step: "mint",
    url: lane.action_url,
    said: "",
    ...seatFacts(lane, saved),
    renew: true,
    rotating: true,
  };
}

/**
 * How to tick one section of the vendor's scope picker for a level's writes:
 * the whole section, the whole section less the scopes the level does not
 * hold, or only the writes it needs, whichever asks for fewer clicks.
 */
export interface ScopeSection {
  /** The resource, as the picker's search finds it: `droplet`. */
  resource: string;
  /** The writes to tick one by one; empty where the whole section is ticked. */
  tick: string[];
  /** Where the whole section is ticked, what to untick in it again. */
  untick: string[];
}

/** What a person ticks in the vendor's scope picker for one level, read off the engine's lists. */
export interface ScopeGuide {
  /** Reads the bulk select ticks that a token is refused for holding at this level. */
  refused: string[];
  /** Reads the bulk select ticks that agents do not need. */
  unneeded: string[];
  /** The sections holding the level's writes, in the picker's alphabetical order. */
  sections: ScopeSection[];
}

const resourceOf = (scope: string) => scope.slice(0, scope.indexOf(":"));
const isRead = (scope: string) => scope.endsWith(":read");

/**
 * The scope picker's steps for a seat at `levelId`, or undefined where the
 * level names no scopes. A person bulk-selects every read, unticks the reads
 * the level must not hold, then ticks the level's writes section by section.
 *
 * DERIVED, NEVER LISTED HERE. The levels' `grants`, the lane's `refused` and
 * its `unneeded` together cover every scope the vendor offers (the engine
 * holds that), so a section's full contents, and so what selecting all of it
 * also ticks, is known without a copy of the vendor's catalogue. A scope a
 * higher level grants is refused at this one, as the engine's paste refuses it.
 */
export function scopeGuide(lane: Lane, levelId: string | undefined): ScopeGuide | undefined {
  const at = lane.levels.findIndex((l) => l.id === levelId);
  const grants = new Set(lane.levels[at]?.grants ?? []);
  if (grants.size === 0) return undefined;
  const higher = lane.levels.slice(at + 1).flatMap((l) => l.grants ?? []);
  const refused = new Set([...(lane.refused ?? []), ...higher].filter((s) => !grants.has(s)));
  const unneeded = new Set((lane.unneeded ?? []).filter((s) => !grants.has(s)));
  const offered = new Set([...lane.levels.flatMap((l) => l.grants ?? []), ...refused, ...unneeded]);
  const writes = [...grants].filter((s) => !isRead(s));
  const sections = [...new Set(writes.map(resourceOf))].sort().map((resource): ScopeSection => {
    const tick = writes.filter((s) => resourceOf(s) === resource).sort();
    const untick = [...offered].filter((s) => resourceOf(s) === resource && !grants.has(s)).sort();
    return untick.length < tick.length
      ? { resource, tick: [], untick }
      : { resource, tick, untick: [] };
  });
  return {
    refused: [...refused].filter(isRead).sort(),
    unneeded: [...unneeded].filter(isRead).sort(),
    sections,
  };
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

/**
 * The level a lane of sets stores for the ids chosen: sorted and
 * space-separated, and holding the level the engine says every other one
 * needs (`required_level`, Confluence's View) whenever it holds any.
 */
export function joinLevels(lane: Lane, ids: string[]): string {
  const set = new Set(ids.filter((id) => id !== ""));
  if (lane.required_level && set.size > 0) set.add(lane.required_level);
  return [...set].sort().join(" ");
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

/** What a draft changes on one lane, in a word or two, and the tone its tag takes. */
export interface ChangeSummary {
  label: string;
  tone: "success" | "danger" | "info";
}

/**
 * The change a draft makes on one lane, said in the head of its card in the
 * review: on, off, or the one part that moved ("Projects changed"). Two parts
 * at once are "Access changed", and the rows under the word say which.
 */
export function changeSummary(lane: Lane, from: AccessDesired, to: AccessDesired): ChangeSummary {
  if (!from.enabled && to.enabled) return { label: "Turned on", tone: "success" };
  if (from.enabled && !to.enabled) return { label: "Turned off", tone: "danger" };
  const parts = [
    !sameLevel(lane, from.level, to.level) && "Level",
    !samePermissions(lane, from.permissions, to.permissions) && "Permissions",
    scopeApplies(lane, to) &&
      !sameScope(from.scope, to.scope) &&
      (laneWords(lane.key).scope?.noun ?? "Scope"),
  ].filter((part) => part !== false);
  return { label: parts.length === 1 ? `${parts[0]} changed` : "Access changed", tone: "info" };
}

const TAKING_OPS: ReadonlySet<AccessStep["op"]> = new Set(["disable", "revoke", "remove"]);
const TAKING_VERBS: ReadonlySet<AccessStep["verb"]> = new Set(["disable", "revoke", "remove"]);

/**
 * Whether a planned step takes an agent's access away: a disable, revoke or
 * delete done to turn it off, revoke its credential or remove its account.
 * The revoke a mint or a level change ends on retires the key it replaced,
 * and the agent keeps working, so it is not one.
 */
export function takesAccessAway(step: AccessStep): boolean {
  return TAKING_OPS.has(step.op) && TAKING_VERBS.has(step.verb);
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
 * Whether a seat's scope is offered: only on a lane that narrows by scope,
 * never on one managed outside Crewlet, and not with product access only,
 * unless a list is already held there (which the document refuses), so it
 * can be cleared.
 */
export function scopeApplies(lane: Lane, want: AccessDesired): boolean {
  if (!lane.scoped || managedOutside(lane)) return false;
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
 * Why a seat's draft cannot be saved, or null, as the engine would refuse it:
 * a set without the level the engine requires of it (Confluence's View);
 * product access only together with a project or space list, which needs a
 * role; or, where the vendor has no empty level, a seat turned on with no
 * role or permission chosen.
 */
export function refusedLevel(lane: Lane, want: AccessDesired): string | null {
  if (!want.enabled) return null;
  const ids = levelIds(want.level);
  const required = lane.multiple ? lane.required_level : undefined;
  if (required && ids.length > 0 && !ids.includes(required))
    return `${lane.name} grants nothing else without ${levelName(lane, required)}. Add it to the set.`;
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
 * The scope a seat takes as it is turned on, or undefined: on a lane where an
 * empty scope reaches nothing, the one entry a list read just now holds, so a
 * person need not pick the only one there is.
 */
export function onlyScope(lane: Lane, want: AccessDesired): string[] | undefined {
  if (!lane.scope_required || !lane.scope_live || want.enabled) return undefined;
  if ((want.scope ?? []).length > 0 || lane.scope?.length !== 1) return undefined;
  return [...lane.scope];
}

/**
 * Why a seat's scope cannot be saved, or null, in the engine's words for the
 * same refusal: on a lane where an empty scope reaches nothing
 * (`scope_required`), the agent `who` turned on with none.
 */
export function refusedScope(lane: Lane, want: AccessDesired, who: string): string | null {
  if (!lane.scope_required || !want.enabled || managedOutside(lane)) return null;
  if ((want.scope ?? []).length > 0) return null;
  const singular = laneWords(lane.key).scope?.singular ?? "entry";
  return `Choose at least one ${singular} for ${who}: an agent in no ${singular} can reach nothing.`;
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

/** How a person reads one scope entry: the vendor's name for it, else the identifier stored. */
export function scopeLabel(lane: Lane, id: string): string {
  return lane.scope_names?.[id] ?? id;
}

/** A seat's scope as a sentence fragment: "gophrase/backend and gophrase/web", or the lane's empty words. */
export function scopeWords(lane: Lane, scope: string[] | undefined): string {
  if (!scope || scope.length === 0) return laneWords(lane.key).scope?.empty ?? "None";
  return andList(scope.map((id) => scopeLabel(lane, id)));
}

/** One entry a scope picker offers: the vendor's name, with the identifier beneath where they differ. */
export interface ScopeChoice {
  value: string;
  label: string;
  description?: string;
}

/**
 * What a seat's scope picker offers: every entry the connection reaches, then
 * any the seat holds that it no longer reaches (a project deleted at the
 * vendor), so a person can clear it.
 */
export function scopeChoices(lane: Lane, held: string[] | undefined): ScopeChoice[] {
  const ids = [...(lane.scope ?? [])];
  for (const id of held ?? []) if (!ids.includes(id)) ids.push(id);
  return ids.map((id) => {
    const label = scopeLabel(lane, id);
    return label === id ? { value: id, label } : { value: id, label, description: id };
  });
}

/** What a scope picker says in place of itself, and where a person makes an entry to offer. */
export interface ScopeUnavailable {
  words: string;
  link?: { label: string; href: string };
}

/**
 * What a scope picker says where it has nothing to offer, or null where it
 * offers something or takes typed entries (`scope_open`): the vendor's list
 * could not be read, or it was read and holds nothing, with the lane's docs
 * section on making one where it names one.
 */
export function scopeUnavailable(lane: Lane, held: string[] | undefined): ScopeUnavailable | null {
  if (lane.scope_open || scopeChoices(lane, held).length > 0) return null;
  const words = laneWords(lane.key).scope;
  const noun = (words?.noun ?? "Scope").toLowerCase();
  if (!lane.scope_live) return { words: `Crewlet could not read your ${noun}` };
  const none = { words: words?.none ?? `No ${noun} to choose from` };
  const link = words?.noneLink;
  return link
    ? { ...none, link: { label: link.label, href: integrationDocHref(link.doc, link.anchor) } }
    : none;
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

/** The credentials the lane's tool, a CLI in the seat's box, reads there, as the engine sends them. */
export function boxTool(saved: AccessSeatLane): AccessBoxLine[] {
  return (saved.box?.env ?? []).filter((line) => line.tool);
}

/**
 * Everything else Crewlet puts in a seat's box itself, its variables by name
 * and then its files by path, or null where it puts nothing else.
 */
export function boxSettings(saved: AccessSeatLane): string | null {
  const names = [
    ...(saved.box?.env ?? []).filter((l) => !l.tool).map((l) => l.name),
    ...(saved.box?.files ?? []).map((f) => f.path),
  ];
  return names.length > 0 ? andList(names) : null;
}

/**
 * What the details say after a seat's credential variable: who provides it,
 * that Crewlet issues it at use for the lane's period (never sealed, so no
 * time is a fleet fact), or when it was minted.
 */
export function credentialNote(
  lane: Lane,
  o: AccessObserved,
  now: number,
  words: LaneWords = laneWords(lane.key),
): string {
  if (providedExternally(lane, o)) return ", provided externally";
  if (o.credential?.source === "issued")
    return words.issued ? `, issued for ${words.issued} at a time, at use` : ", issued at use";
  return o.credential?.minted_at ? `, minted ${relTime(o.credential.minted_at, now)}` : "";
}

/** The details' "Set by Crewlet" value: the settings, and the lane's note on them, or null. */
export function boxSettingsFact(
  lane: Lane,
  saved: AccessSeatLane,
  words: LaneWords = laneWords(lane.key),
): { settings: string; note?: string } | null {
  const settings = boxSettings(saved);
  if (!settings) return null;
  return words.boxNote ? { settings, note: words.boxNote } : { settings };
}

/** The tool, as one value: whether it works, and what it works through. */
export function toolWords(lane: Lane, saved: AccessSeatLane): string {
  const names = andList(boxTool(saved).map((line) => line.name));
  const through = names ? `${laneWords(lane.key).tool}, as ${names}` : laneWords(lane.key).tool;
  const server = lane.tool_server ? `${lane.tool_server} tool server` : "tool server";
  switch (saved.tool) {
    case "live":
      return `Live, ${through}`;
    case "builtin":
      return through;
    case "waiting":
      // THE ENGINE'S REASON WHERE IT SENDS ONE (a vendor still applying a new
      // grant), since a wait the person did not cause should say who it is on.
      return saved.tool_reason
        ? `Waiting: ${saved.tool_reason}, ${through}`
        : `Waiting for the credential, ${through}`;
    case "no_server":
      return `Not set up: no ${server} is declared`;
    case "server_off":
      return `Not running: the ${server} is turned off`;
    case "off":
      return "None while off";
  }
}

/**
 * What agents with an account and no tool lack, and how to give them one, said
 * once above the table after "... nothing to use it with: ". Crewlet sets a box
 * tool's credentials itself, so what is missing is always a tool server.
 */
export function noToolRemedy(lane: Lane): string {
  const where = "under Settings › MCP & tools as one instance per agent";
  return lane.tool_server
    ? `the company declares no ${lane.tool_server} tool server. Add it ${where}, and each agent's own credential is sent with it.`
    : `the company declares no tool server for it. Add one ${where}, and each agent's own credential is sent with it.`;
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

/**
 * What turning an off agent on does, said once per distinct sentence over the
 * off agents it is true of ("2 agents are off. Turning one on creates ..."),
 * where every off row said it again in its details.
 */
export function offNote(
  rows: readonly { lane: Lane; saved: AccessSeatLane; seat: { departed?: boolean | undefined } }[],
): string[] {
  const counts = new Map<string, number>();
  for (const { lane, saved, seat } of rows) {
    if (saved.desired.enabled || seat.departed) continue;
    const said = offSentence(lane, saved).replace(/^Off\. /, "");
    counts.set(said, (counts.get(said) ?? 0) + 1);
  }
  return [...counts].map(([said, n]) =>
    n === 1
      ? `One agent is off. ${said}`
      : `${n} agents are off. ${said.replace(/^Turning it on/, "Turning one on")}`,
  );
}

const STATUS_ORDER: Record<string, number> = {
  danger: 0,
  warning: 1,
  info: 2,
  success: 3,
  neutral: 4,
};

/** A status's place in a sort: what needs a person first, then what is moving, then what is at rest. */
export function statusRank(status: Status): number {
  return STATUS_ORDER[status.tone] ?? 5;
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
 * scope is literal (Mattermost: no channels) or refused (`scope_required`),
 * or null.
 */
export function emptyScopeHint(lane: Lane, want: AccessDesired): string | null {
  if (managedOutside(lane) || !want.enabled || (want.scope ?? []).length > 0) return null;
  const words = laneWords(lane.key).scope;
  if (words?.emptyOn) return words.emptyOn;
  if (!lane.scope_required) return null;
  const noun = (words?.noun ?? "scope").toLowerCase();
  return `Not in any ${words?.singular ?? "entry"} yet, so it can reach nothing. Choose its ${noun}.`;
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

/**
 * What one agent will review once a draft is saved, as the review says it:
 * how many of its open items were raised under its access now, "at least" where
 * the count is a floor, and that it reviews them after the save. The agent is
 * named as the tab names it.
 */
export function reviewWords(open: number, agent: string, atLeast = false): string {
  const one = open === 1 && !atLeast;
  const items = one ? "1 open item" : `${open.toLocaleString()} open items`;
  const were = one ? "was" : "were";
  return `${atLeast ? "At least " : ""}${items} from ${agent} ${were} raised under its current access; it will review them after you save.`;
}
