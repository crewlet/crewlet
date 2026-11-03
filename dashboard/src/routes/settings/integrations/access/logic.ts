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
  AccessBoxFile,
  AccessBoxLine,
  AccessDesired,
  AccessLaneKey,
  AccessObserved,
  AccessOptions,
  AccessSeatLane,
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
  /** The role to give it, in the vendor's word, with the vendor's own line on what it allows. */
  role: { name: string; note: string | undefined } | undefined;
  /** The role the vendor shows now, where it is one of the lane's levels. */
  current: string | undefined;
  /** A token was sealed for the seat before (revoked at the vendor, or being rotated), so this is a new one. */
  renew: boolean;
  /** Opened to rotate a token the vendor still accepts: the sheet starts at the paste. */
  rotating: boolean;
}

const HANDOFF_STEPS: ReadonlySet<string> = new Set<HandoffStep>(["create", "mint", "rerole"]);

/** What the engine says of a seat's account at the vendor: its name, the role chosen, and the role it holds. */
function seatFacts(lane: Lane, saved: AccessSeatLane) {
  const level = (id: string | undefined) => lane.levels.find((l) => l.id !== "" && l.id === id);
  const chosen = level(saved.desired.level);
  return {
    account: saved.observed.account || undefined,
    role: chosen ? { name: chosen.name, note: chosen.note } : undefined,
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

/** The lines a person adds to a seat's box for its CLI, as the engine sends them. */
export function boxLines(saved: AccessSeatLane): AccessBoxLine[] {
  return (saved.box?.env ?? []).filter((line) => line.by === "you");
}

/**
 * The settings Crewlet puts in a seat's box itself, its variables by name and
 * then its files by path, or null where it puts none.
 */
export function boxSettings(saved: AccessSeatLane): string | null {
  const names = [
    ...(saved.box?.env ?? []).filter((l) => l.by === "crewlet").map((l) => l.name),
    ...crewletFiles(saved).map((f) => f.path),
  ];
  return names.length > 0 ? andList(names) : null;
}

/** The files Crewlet writes into a seat's box itself, as the engine sends them. */
function crewletFiles(saved: AccessSeatLane): AccessBoxFile[] {
  return (saved.box?.files ?? []).filter((f) => f.by === "crewlet");
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
  const names = andList(boxLines(saved).map((line) => line.name));
  const through = names ? `${laneWords(lane.key).tool}, as ${names}` : laneWords(lane.key).tool;
  const server = lane.tool_server ? `${lane.tool_server} tool server` : "tool server";
  // A LANE WITH BOTH ROUTES is live through either, so a seat with neither is
  // told both, the server first.
  const andBox = names ? `, and its sandbox env does not set ${names}` : "";
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
      return names && !lane.tool_server
        ? `Not set up: its sandbox env does not set ${names}`
        : `Not set up: no ${server} is declared${andBox}`;
    case "server_off":
      return `Not running: the ${server} is turned off${andBox}`;
    case "off":
      return "None while off";
  }
}

/**
 * What agents with an account and no tool lack, and how to give them one, said
 * once above the table after "... nothing to use it with: ". A box tool's
 * remedy is each agent's own lines, exactly as the engine sends them; a lane
 * with a tool server too offers the server first.
 */
export function noToolRemedy(
  lane: Lane,
  seats: readonly { name: string; saved: AccessSeatLane }[],
): string {
  const server = lane.tool_server;
  const where = "under Settings › MCP & tools as one instance per agent";
  const boxed = seats.filter((s) => boxLines(s.saved).length > 0);
  if (boxed.length === 0)
    return server
      ? `the company declares no ${server} tool server. Add it ${where}, and each agent's own credential is sent with it.`
      : `the company declares no tool server for it. Add one ${where}, and each agent's own credential is sent with it.`;
  const one = boxed.length === 1 ? boxed[0]! : null;
  const lines = (s: (typeof boxed)[number]) =>
    andList(boxLines(s.saved).map(({ name, value }) => `${name}: "${value}"`));
  const names = andList([...new Set(boxed.flatMap((s) => boxLines(s.saved).map((l) => l.name)))]);
  const unset = `${one ? "its" : "their"} sandbox env does not set ${names}`;
  const lineup = one
    ? `${lines(one)} to ${one.name}'s sandbox.env in the company document`
    : `each agent's own lines to its sandbox.env in the company document: ${boxed
        .map((s) => `${s.name}, ${lines(s)}`)
        .join("; ")}`;
  const signsIn = one ? "its tool signs in as the agent" : "each tool signs in as its agent";
  return server
    ? `the company declares no ${server} tool server, and ${unset}. Add the ${server} tool server ${where}, or add ${lineup}. Either way ${signsIn}.`
    : `${unset}. Add ${lineup}. Then ${signsIn}.`;
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
