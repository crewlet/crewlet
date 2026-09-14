/**
 * What the node editor and the builder's dialogs read about one node, beyond
 * its own authored fields.
 *
 * READ, NEVER DERIVED. A seat's handle, its manager, the units it leads by
 * inheritance and its effective home are the ENGINE's answers, and they come
 * from the derivation the last check returned, placed on nodes through the
 * path index of the document that check sent (the same pairing
 * `model/problems.ts` uses). A node the last check has not described (one
 * added since) has no answer here, and the screens say so rather than guess.
 *
 * Two small rules ARE restated, each because a screen has to say something
 * the engine does not report and each marked at its definition: which
 * provider an unpinned seat runs on (`agent/phase.Registry.Chain`), and which
 * integration block counts as connected. Neither decides anything the engine
 * validates.
 *
 * NAMES, NEVER VALUES. Every credential-adjacent helper here returns the
 * names a document uses (a tool server, a variable, a `${NAME}` reference),
 * because no screen renders a credential and a reference is the name of a
 * sealed entry rather than one.
 */

import type {
  AgentRow,
  CompanyDocument,
  ConfigRole,
  ConfigUnit,
  DerivedSeat,
  DerivedUnit,
  SandboxEntry,
} from "~/protocol/index.ts";
import { runState } from "~/lib/seats.ts";
import { COMPANY_KEY, handleOfKey, type NodeKey } from "./model/keys.ts";
import { allSeats, allUnits, locate, type Draft, type DraftUnit } from "./model/draft.ts";
import { getPath, isRecord } from "./model/json.ts";
import type { BuilderState } from "./model/reducer.ts";

// ---------------------------------------------------------------------------
// The engine's derivation, placed on nodes
// ---------------------------------------------------------------------------

/** The last check's derivation of a seat, when that check described it. */
export function derivedSeatOf(state: BuilderState, key: NodeKey): DerivedSeat | undefined {
  const { sent, derived } = state.check;
  const path = sent?.index.pathOf.get(key);
  if (path === undefined || key === COMPANY_KEY) return undefined;
  return (derived?.seats ?? []).find((seat) => seat.path === path);
}

/** The last check's derivation of a unit, when that check described it. */
export function derivedUnitOf(state: BuilderState, key: NodeKey): DerivedUnit | undefined {
  const { sent, derived } = state.check;
  const path = sent?.index.pathOf.get(key);
  if (path === undefined || key === COMPANY_KEY) return undefined;
  return (derived?.units ?? []).find((unit) => unit.path === path);
}

/**
 * A seat's handle: the one its key carries (a seat of the base), the one it
 * declares, or the one the last check derived. `undefined` for a seat nobody
 * has named a handle for yet.
 */
export function handleOf(state: BuilderState, key: NodeKey): string | undefined {
  const found = locate(state.draft, key);
  if (found?.kind !== "seat") return undefined;
  const declared = found.node.data.handle;
  return (
    handleOfKey(key) ??
    (typeof declared === "string" && declared !== "" ? declared : undefined) ??
    derivedSeatOf(state, key)?.handle
  );
}

/** The key of the seat the last check gave this handle, when it still stands in the draft. */
export function keyOfHandle(state: BuilderState, handle: string): NodeKey | undefined {
  const seat = (state.check.derived?.seats ?? []).find((s) => s.handle === handle);
  const key = seat?.path === undefined ? undefined : state.check.sent?.index.byPath.get(seat.path);
  return key !== undefined && locate(state.draft, key)?.kind === "seat" ? key : undefined;
}

/**
 * The name to show for a handle the engine reported: the seat's name as the
 * draft now has it, else the name the check reported, else the handle.
 */
export function nameOfHandle(state: BuilderState, handle: string): string {
  const key = keyOfHandle(state, handle);
  const found = key === undefined ? undefined : locate(state.draft, key);
  if (found?.kind === "seat") return found.node.data.name;
  return (state.check.derived?.seats ?? []).find((s) => s.handle === handle)?.name ?? handle;
}

/** The units whose DECLARED lead names this seat, in the document's order. */
export function unitsLedBy(draft: Draft, seatName: string): DraftUnit[] {
  return [...allUnits(draft)].map(({ unit }) => unit).filter((unit) => unit.data.lead === seatName);
}

// ---------------------------------------------------------------------------
// Integrations
// ---------------------------------------------------------------------------

/** The tools a seat or unit field depends on, as the company document names their blocks. */
export type Tool =
  "github" | "slack" | "mattermost" | "gitlab" | "jira" | "confluence" | "datadog" | "atlassian";

/** How each tool is named on screen. */
export const TOOL_NAMES: Readonly<Record<Tool, string>> = {
  github: "GitHub",
  slack: "Slack",
  mattermost: "Mattermost",
  gitlab: "GitLab",
  jira: "Jira",
  confluence: "Confluence",
  datadog: "Datadog",
  atlassian: "Atlassian",
};

/**
 * Whether the company has connected a tool: its `integrations.<tool>` block
 * is present.
 *
 * PRESENCE, NOT `enabled`, which is the reading the builder's operations use
 * too (`model/operations.ts` refuses a GitLab access level or a Datadog
 * fallback without the block and never creates one). A block with `enabled`
 * off is still the company's settings for that tool, and a seat's field under
 * it is what applies when the tool is switched on. A field for a tool with no
 * block at all would read as a working setting and do nothing, so the editor
 * says the tool is not connected instead of offering it.
 */
export function isConnected(company: CompanyDocument, tool: Tool): boolean {
  return isRecord(getPath(company, ["integrations", tool]));
}

/** Whether GitLab provisioning is connected: the block per-seat access levels live in. */
export function hasGitLabProvisioning(company: CompanyDocument): boolean {
  return isRecord(getPath(company, ["integrations", "gitlab", "provisioning"]));
}

/** The company-wide GitLab access level a seat without its own override receives. */
export function defaultGitLabAccessLevel(company: CompanyDocument): string {
  const level = getPath(company, ["integrations", "gitlab", "provisioning", "access_level"]);
  return typeof level === "string" && level !== "" ? level : "developer";
}

/** The seat's own GitLab access level override, when it has one. */
export function gitLabAccessLevel(company: CompanyDocument, handle: string | undefined): string {
  if (handle === undefined) return "";
  const level = getPath(company, [
    "integrations",
    "gitlab",
    "provisioning",
    "access_levels",
    handle,
  ]);
  return typeof level === "string" ? level : "";
}

/**
 * The username the Mattermost provisioner gives a seat's bot when the seat
 * names none: the provisioning prefix and the handle, lowercased.
 *
 * RESTATES `mattermost.BotUsername`, and the lowercasing is the load-bearing
 * half: Mattermost usernames are lowercase, so a bot for a mixed-case handle
 * is created as one name and looked up as another, which reads as a missing
 * bot and makes a second one. A screen offering the handle as the default
 * would name an account that never exists.
 */
export function mattermostBotUsername(company: CompanyDocument, handle: string): string {
  const prefix = getPath(company, [
    "integrations",
    "mattermost",
    "provisioning",
    "username_prefix",
  ]);
  return `${typeof prefix === "string" ? prefix.trim() : ""}${handle}`.toLowerCase();
}

/** The handle of the Datadog fallback seat, when Datadog names one. */
export function datadogFallback(company: CompanyDocument): string | undefined {
  const handle = getPath(company, ["integrations", "datadog", "route_to"]);
  return typeof handle === "string" && handle !== "" ? handle : undefined;
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

/**
 * The keys of `providers.llm`, in the order seats resolve them.
 *
 * RESTATES `config.Providers.ProviderOrder`: the declared `llm_order` first,
 * keeping only keys the map has and each once, then every other key sorted.
 * The model picker offers the keys in the order the engine would fall back
 * through them, which is the order an operator reads a chain in.
 */
export function providerOrder(company: CompanyDocument): string[] {
  const llm = getPath(company, ["providers", "llm"]);
  if (!isRecord(llm)) return [];
  const keys = Object.keys(llm);
  const declared = getPath(company, ["providers", "llm_order"]);
  const out: string[] = [];
  for (const key of Array.isArray(declared) ? declared : []) {
    if (typeof key === "string" && keys.includes(key) && !out.includes(key)) out.push(key);
  }
  const rest = keys.filter((key) => !out.includes(key)).sort();
  return [...out, ...rest];
}

/**
 * The provider a seat that names no model runs on, or `undefined` when the
 * company has none.
 *
 * RESTATES the last two levels of `agent/phase.Registry.Chain`: a provider
 * keyed `default`, else the first provider in the company's order.
 */
export function unpinnedProvider(company: CompanyDocument): string | undefined {
  const order = providerOrder(company);
  return order.includes("default") ? "default" : order[0];
}

/** The per-phase model fields a seat may carry beside `llm`, as the document names them. */
export const PHASE_MODEL_FIELDS = [
  "llm_review",
  "llm_subagent",
  "llm_auxiliary",
  "llm_judge",
  "llm_sandbox",
] as const;

// ---------------------------------------------------------------------------
// Names a document uses for credentials
// ---------------------------------------------------------------------------

/** A value that is exactly one `${NAME}` reference, as `envref.Whole` reads one. */
const WHOLE_REFERENCE = /^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$/;

/**
 * Whether a value is exactly one `${NAME}` reference.
 *
 * WHICH IS WHAT MAKES A CREDENTIAL PROVISIONABLE. A provisioner mints into the
 * secret store and points the document at it, so it acts only where the
 * document already names an entry (`provision.SoleVar`); anything else is a
 * value somebody wrote by hand, which it reports and leaves alone. The
 * configuration surface sends a literal as its mask, so a screen cannot tell
 * the two apart by looking at the value, only by its shape.
 */
export function isWholeReference(value: unknown): boolean {
  return typeof value === "string" && WHOLE_REFERENCE.test(value.trim());
}

/**
 * Every secret store entry a value refers to by a whole `${NAME}`, sorted and
 * each once. Masked literals and partial references are not names and are
 * never returned: they are credentials, or parts of one.
 */
export function referenceNames(value: unknown): string[] {
  const out = new Set<string>();
  const walk = (v: unknown) => {
    if (typeof v === "string") {
      const match = WHOLE_REFERENCE.exec(v.trim());
      if (match) out.add(match[1]!);
    } else if (Array.isArray(v)) v.forEach(walk);
    else if (isRecord(v)) Object.values(v).forEach(walk);
  };
  walk(value);
  return [...out].sort();
}

/** A unit's or seat's tool credentials, as server names and the variable names under each. */
export function toolCredentialNames(
  data: ConfigRole | ConfigUnit,
): { server: string; variables: string[] }[] {
  const env = data.mcp_env;
  if (!isRecord(env)) return [];
  return Object.entries(env).map(([server, vars]) => ({
    server,
    variables: isRecord(vars) ? Object.keys(vars) : [],
  }));
}

// ---------------------------------------------------------------------------
// Live state
// ---------------------------------------------------------------------------

/**
 * Whether the seat with this handle has work in flight: a turn running, or a
 * detached coding run it is waiting on.
 */
export function isWorking(
  handle: string | undefined,
  agents: readonly AgentRow[],
  sandboxes: readonly SandboxEntry[],
): boolean {
  if (handle === undefined) return false;
  if (sandboxes.some((run) => run.agent_handle === handle)) return true;
  const agent = agents.find((row) => row.handle === handle);
  if (!agent) return false;
  const state = runState(agent, [...sandboxes]);
  return state === "working" || state === "awaiting_sandbox";
}

/** The sentence every dialog says about a seat with work in flight. */
export function workingNote(name: string): string {
  return `${name} is working now. Its current turn continues on the previous configuration until the engine applies this change.`;
}

/** Every seat in the draft, by key, with its name and kind, in the document's order. */
export function seatChoices(draft: Draft): { key: NodeKey; name: string; human: boolean }[] {
  return [...allSeats(draft)].map(({ seat }) => ({
    key: seat.key,
    name: seat.data.name,
    human: seat.data.kind === "human",
  }));
}
