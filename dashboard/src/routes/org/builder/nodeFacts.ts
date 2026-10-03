/**
 * What the node editor and the builder's dialogs read about one node, beyond
 * its own authored fields.
 *
 * READ, NEVER DERIVED, WHERE THE ENGINE DERIVES. A seat's primary manager
 * and the seats a lead manages automatically are the ENGINE's answers, and
 * they come from the one derivation there is: the org push's, of the SAVED
 * chart (`chartModel.ts`). Nothing derives a draft — the chart has no dry run
 * — so those answers are read only while the draft's chart is still the saved
 * one ([savedDerivation]), and a screen says they are not derived yet rather
 * than guess. What the chart's rows STATE is read off the draft: where a seat
 * sits, who a unit declares as its lead, and the lead and channel a unit
 * inherits by the one cascade `chartModel.effectiveLeads` restates.
 *
 * A few small rules ARE restated, each because a screen has to say something
 * the engine does not report, and each marked at its definition: the order
 * the company's providers are tried in and the one an unpinned seat runs on
 * (`config.Providers.ProviderOrder`, `agent/phase.Registry.Chain`), which
 * integration block counts as connected, a Mattermost bot's default username
 * (`mattermost.BotUsername`), what makes a value a whole reference
 * (`envref.Whole`), the unit `mcp_env` a seat receives (`org.inheritMCPEnv`)
 * and the servers a provisioning vendor enrols a seat by, and what a
 * placement block means (`config.RolePlacement`). None of them decides
 * anything the engine validates.
 *
 * NAMES, NEVER VALUES. Every credential-adjacent helper here returns the
 * names a seat's runtime half uses (a tool server, a variable, a `${NAME}`
 * reference), because no screen renders a credential and a reference is the
 * name of a sealed entry rather than one.
 */

import type {
  AgentRow,
  CompanyDocument,
  Derived,
  DerivedSeat,
  SandboxEntry,
} from "~/protocol/index.ts";
import { activityOf } from "~/lib/seats.ts";
import { keyOfHandle } from "./chartModel.ts";
import { placeDerivation, type PlacedDerivation } from "./model/document.ts";
import { COMPANY_KEY, isMintedKey, type NodeKey } from "./model/keys.ts";
import {
  allUnits,
  locate,
  sameChart,
  type Draft,
  type DraftSeat,
  type DraftUnit,
  type SeatData,
  type UnitData,
} from "./model/draft.ts";
import { getPath, isRecord } from "./model/json.ts";
import type { BuilderState } from "./model/reducer.ts";

// ---------------------------------------------------------------------------
// The engine's derivation of the saved chart
// ---------------------------------------------------------------------------

/** The saved chart's derivation, whole and placed on the base's nodes. */
export interface SavedDerivation extends PlacedDerivation {
  readonly derived: Derived;
}

/**
 * The engine's derivation of the saved chart, while the draft's chart is
 * still the saved one: see the module doc. `undefined` otherwise, and while
 * the engine has not described the saved chart.
 */
export function savedDerivation(
  state: Pick<BuilderState, "draft" | "baseDraft" | "base">,
): SavedDerivation | undefined {
  const derived = state.base.derived;
  if (!derived || !sameChart(state.draft, state.baseDraft)) return undefined;
  return { derived, ...placeDerivation(state.baseDraft, derived) };
}

/** The derivation of a seat, while [savedDerivation] answers. */
export function derivedSeatOf(state: BuilderState, key: NodeKey): DerivedSeat | undefined {
  return key === COMPANY_KEY ? undefined : savedDerivation(state)?.seatByKey.get(key);
}

/**
 * The name to show for a handle the engine derived: the seat's name as the
 * draft now has it, else the name the derivation carries, else the handle.
 */
export function nameOfHandle(state: BuilderState, handle: string): string {
  const key = keyOfHandle(state, handle);
  const found = key === undefined ? undefined : locate(state.draft, key);
  if (found?.kind === "seat") return found.node.data.name || found.node.data.handle;
  const reported = state.base.derived?.seats?.find((s) => s.handle === handle);
  return reported?.name ?? handle;
}

/**
 * The unit a seat is a direct member of: the one the draft holds it in, which
 * is where its chart row places it. `undefined` at the top level.
 */
export function homeUnitOf(draft: Draft, key: NodeKey): DraftUnit | undefined {
  const found = locate(draft, key);
  if (found?.kind !== "seat" || found.parent === COMPANY_KEY) return undefined;
  const home = locate(draft, found.parent);
  return home?.kind === "unit" ? home.node : undefined;
}

/**
 * The handle the RUNNING seat answers to: the one the saved chart holds it
 * under, which a new handle in this draft has not changed yet. `undefined`
 * for a seat this draft created, which nothing runs.
 */
export function savedHandleOf(
  state: Pick<BuilderState, "baseDraft">,
  key: NodeKey,
): string | undefined {
  const saved = locate(state.baseDraft, key);
  return saved?.kind === "seat" ? saved.node.data.handle : undefined;
}

/** The units whose DECLARED lead is this seat's handle, depth-first. */
export function unitsLedBy(draft: Draft, handle: string): DraftUnit[] {
  return [...allUnits(draft)].map(({ unit }) => unit).filter((unit) => unit.data.lead === handle);
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
// Placement
// ---------------------------------------------------------------------------

/**
 * Which nodes may run a seat, as the configuration document says it: a node
 * it is pinned to, and labels a node must carry, every one of them
 * (`config.RolePlacement`). A block that names neither constrains nothing.
 */
export function placementSummary(placement: Readonly<Record<string, unknown>>): string {
  const node = typeof placement.node === "string" ? placement.node.trim() : "";
  const labels = isRecord(placement.labels)
    ? Object.entries(placement.labels).map(([key, value]) => `${key}=${String(value)}`)
    : [];
  if (node !== "" && labels.length > 0) {
    return `Pinned to node ${node}, which must be labelled ${labels.join(", ")}`;
  }
  if (node !== "") return `Pinned to node ${node}`;
  if (labels.length > 0) return `Nodes labelled ${labels.join(", ")}`;
  return "Any node that runs seats";
}

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

/**
 * A unit's or seat's tool credentials, as server names and the variable names
 * under each: its runtime half's `mcp_env`. Empty where the reader was not
 * shown that half, which a caller that must tell the two apart asks the base
 * about (`BaseCompany.runtimeVisible`).
 */
export function toolCredentialNames(
  data: SeatData | UnitData,
): { server: string; variables: string[] }[] {
  const env = getPath(data, ["runtime", "mcp_env"]);
  if (!isRecord(env)) return [];
  return Object.entries(env).map(([server, vars]) => ({
    server,
    variables: isRecord(vars) ? Object.keys(vars) : [],
  }));
}

/**
 * The tool servers a seat holds credentials for, by name: its own `mcp_env`
 * and its home unit's, which the engine layers under every direct member
 * (`org.inheritMCPEnv`).
 */
export function toolServersOf(seat: SeatData, home: DraftUnit | undefined): Set<string> {
  return new Set(
    [...toolCredentialNames(seat), ...(home ? toolCredentialNames(home.data) : [])].map(
      (entry) => entry.server,
    ),
  );
}

/**
 * The `mcp_env` servers each provisioning vendor reads a seat's own account
 * from, as the engine names them: `gitlab.SeatEnv`, `datadog.SeatEnv`, and
 * Atlassian's shared server with its two product servers
 * (`atlassian.ProductAny.servers`). A seat is enrolled with a vendor by
 * holding credentials for one of its servers, and a seat that holds none
 * has no account there, whatever the company has connected.
 */
const VENDOR_SERVERS = {
  gitlab: ["gitlab"],
  datadog: ["datadog"],
  atlassian: ["atlassian", "jira", "confluence"],
} as const;

/**
 * What exists at the vendors for a seat, by name, never by value: its own
 * GitHub App, Slack app and Mattermost bot (its runtime half's `github`,
 * `slack` and `mattermost`), and the GitLab, Datadog and Atlassian accounts it
 * is enrolled for. Each stays when the seat is deleted or stops being an
 * agent, until somebody decommissions it. All of it is in the runtime half, so
 * a reader who was not shown that half is told nothing here, and the dialog
 * says so (`dialogParts.StaysUntilDecommissioned`).
 *
 * NOTHING FOR A SEAT THIS DRAFT CREATED: it was never saved, so no engine
 * made anything for it anywhere. And an account is named only where the seat
 * is enrolled for it (see [VENDOR_SERVERS]), because the provisioners create
 * accounts for enrolled agent seats only; naming one for every seat of a
 * company that connected the tool sends somebody to decommission an account
 * that does not exist.
 */
export function vendorIdentities(state: BuilderState, seat: DraftSeat): string[] {
  if (isMintedKey(seat.key)) return [];
  const company = state.draft.company;
  const servers = toolServersOf(seat.data, homeUnitOf(state.draft, seat.key));
  const enrolled = (vendor: keyof typeof VENDOR_SERVERS) =>
    VENDOR_SERVERS[vendor].some((server) => servers.has(server));
  const out: string[] = [];
  const github = getPath(seat.data, ["runtime", "github"]);
  if (isRecord(github) && typeof github.app_slug === "string" && github.app_slug !== "") {
    out.push(`the GitHub App ${github.app_slug}`);
  }
  if (isRecord(getPath(seat.data, ["runtime", "slack"]))) out.push("its Slack app");
  if (isRecord(getPath(seat.data, ["runtime", "mattermost"]))) out.push("its Mattermost bot");
  if (hasGitLabProvisioning(company) && enrolled("gitlab")) {
    out.push("its GitLab service account");
  }
  if (
    isRecord(getPath(company, ["integrations", "datadog", "provisioning"])) &&
    enrolled("datadog")
  ) {
    out.push("its Datadog service account");
  }
  if (isConnected(company, "atlassian") && enrolled("atlassian")) out.push("its Atlassian account");
  return out;
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
  const state = activityOf(agent);
  return state === "working" || state === "needs";
}

/** The sentence every dialog says about a seat with work in flight. */
export function workingNote(name: string): string {
  return `${name} is working now. Its current turn continues on the previous configuration until the engine applies this change.`;
}
