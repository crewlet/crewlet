/**
 * The setup surface, `/setup/integrations`, answered as the engine's
 * `setupapi` answers it: every tool's form DERIVED from the company document
 * on each read, and the writes a person makes from it.
 *
 * THE FORMS ARE THE ENGINE'S OWN. Every requirement's definition (its label,
 * help, kind, choices, where it is written) comes from a listing captured from
 * the engine for the demo company with nothing connected (`fixtures/engine`).
 * What each one holds, which agents each tool lists and how far each agent
 * has got are read from the document and the secret store on every request,
 * by ports of the engine's own rules, named after the function each ports. So
 * a write made anywhere (this surface, `PATCH /config`, a scenario) moves
 * every form as it moves in the engine.
 *
 * ONE ATLASSIAN DEPLOYMENT IS MODELLED: Cloud, the default and the one the
 * capture is of. A Data Center document's Jira and Confluence forms differ,
 * and a listing built over one says so in the console.
 */

import type {
  CompanyDocument,
  ConfigRole,
  SetupListing,
  SetupRequirement,
  SetupRun,
  SetupSeatState,
  SetupToolState,
} from "~/protocol/types.ts";
import { documentOf, type DemoCompany } from "./company.ts";
import { agentRoles } from "./derive.ts";
import type { DemoEngine } from "./engine.ts";
import { verdictOf } from "./findings.ts";
import { uuidV7 } from "./ids.ts";
import {
  askTeardown,
  blockOf,
  forgetScript,
  NOT_CONFIGURED,
  PUBLIC_BASE_KEY,
  publicBase,
  recordEndpoint,
  runPass,
} from "./loop.ts";
import { actorOf } from "./principal.ts";
import { deref, resolution, resolves, seal, wholeRef } from "./refs.ts";
import { LOG, notModelled } from "./refusal.ts";
import type { RestReply, RestRequest } from "./rest.ts";
import { appendRevision, reloadRevision } from "./revisions.ts";
import {
  APP_NAMES,
  atlassianCredential,
  atlassianSeatEmail,
  datadogAccess,
  datadogEmail,
  githubAppPage,
  githubTier,
  githubWebBase,
  gitlabUsername,
  isKind,
  KINDS,
  MCP_CREDENTIALS,
  mattermostBot,
  mcpCredential,
  PASS_KINDS,
  slackManageURL,
  slackManifest,
  type AtlassianProduct,
  type Kind,
} from "./vendors.ts";

/** A seat as the engine answers it, with the secrets it holds (`SeatState.Secrets`, which the dashboard does not read). */
type Seat = SetupSeatState & { secrets?: string[] };
type Tool = Omit<SetupToolState, "seats"> & { seats?: Seat[] };
type Listing = Omit<SetupListing, "tools"> & { tools: Tool[]; revision_id: string };
type Doc = Record<string, unknown>;

/** What a requirement holds, which the engine never sends (`Requirement.Stored`). */
const STORED = new WeakMap<SetupRequirement, string>();
const storedOf = (r: SetupRequirement) => STORED.get(r) ?? "";

// ---- reading the document -------------------------------------------------------

function getPath(root: unknown, path: string): unknown {
  let at: unknown = root;
  for (const part of path.split(".")) {
    if (!at || typeof at !== "object" || Array.isArray(at)) return undefined;
    at = (at as Doc)[part];
  }
  return at;
}

/** Write `value` at a dotted path under `root`, making each object on the way. */
function setPath(root: Doc, path: string, value: unknown): void {
  const parts = path.split(".");
  let at = root;
  for (const part of parts.slice(0, -1)) {
    const next = at[part];
    at = (
      typeof next === "object" && next !== null && !Array.isArray(next) ? next : (at[part] = {})
    ) as Doc;
  }
  at[parts[parts.length - 1]!] = value;
}

/**
 * One requirement as the document holds it now. The rule that reads it is
 * the vendor's own, and the capture says which: a credential, or a field the
 * capture reports unresolved rather than absent, is HELD and resolves through
 * the secret store (`setup.Held`); a toggle reads on or off (`setup.Toggle`);
 * anything else is PLAIN, present and resolved when it has a value
 * (`setup.Plain`). The Atlassian deployment reads its default (`DeploymentOrDefault`).
 */
function fill(c: DemoCompany, def: SetupRequirement, root: unknown, kind: Kind): SetupRequirement {
  const req: SetupRequirement = structuredClone(def);
  delete req.value;
  delete req.resolved_value;
  const raw = getPath(root, def.config_path);
  let stored: string;
  if (def.kind === "toggle") {
    const on = raw === true || raw === "true";
    stored = on ? "true" : "false";
    req.present = on;
    req.resolved = on;
  } else {
    stored =
      typeof raw === "string"
        ? raw
        : typeof raw === "number" || typeof raw === "boolean"
          ? String(raw)
          : "";
    if (kind === "atlassian" && def.field === "deployment") {
      stored = stored.trim().toLowerCase() === "data_center" ? "data_center" : "cloud";
    }
    if (def.kind === "secret" || def.resolved === null) {
      Object.assign(req, resolution(c, stored));
    } else {
      req.present = stored.trim() !== "";
      req.resolved = req.present;
    }
  }
  if (def.kind !== "secret") {
    if (stored) req.value = stored;
  } else if (wholeRef(stored)) {
    req.value = stored.trim();
  }
  STORED.set(req, stored);
  return req;
}

/** Whether a requirement is asked for, given the values beside it (`Requirement.Needed`). */
function needed(r: SetupRequirement, among: SetupRequirement[]): boolean {
  if (!r.required_when) return r.required;
  const gate = among.find((o) => o.field === r.required_when!.field);
  return gate ? storedOf(gate).trim() === r.required_when.equals : false;
}

/** The requirements still owed (`setup.Outstanding`). */
function outstanding(reqs: SetupRequirement[]): SetupRequirement[] {
  return reqs.filter((r) => needed(r, reqs) && !(r.present && r.resolved !== false));
}

// ---- the rosters ------------------------------------------------------------

/** The agents' seats every roster lists, with each one's role. */
function rolesOf(doc: CompanyDocument): { handle: string; role: ConfigRole }[] {
  return agentRoles(doc);
}

type RoleDoc = ConfigRole & {
  integrations?: Doc;
  mcp_env?: Record<string, Record<string, string>>;
};

/** The app a running engine's Slack transport learned for a seat, by the id Slack would give it. */
function slackApp(c: DemoCompany, handle: string, token: SetupRequirement | undefined): string {
  if (!token?.present || token.resolved === false) return "";
  let hash = 0x811c9dc5;
  for (const ch of `${documentOf(c).name ?? ""}/${handle}`) {
    hash = Math.imul(hash ^ ch.charCodeAt(0), 0x01000193) >>> 0;
  }
  return `A0${hash.toString(36).toUpperCase().padStart(7, "0")}${handle.length % 10}`;
}

/** Every agent's own Slack app (`setupapi.slackSeats`). */
function slackSeats(c: DemoCompany, catalogue: Tool): Seat[] {
  const doc = documentOf(c);
  const base = publicBase(c);
  const template = catalogue.seats?.[0]?.requirements ?? [];
  return rolesOf(doc).map(({ handle, role }) => {
    const reqs = template.map((def) => fill(c, { ...def, seat: handle }, role, "slack"));
    const seat: Seat = {
      handle,
      name: role.name,
      requirements: reqs,
      present: reqs.length > 0 && reqs[0]!.present,
      satisfied: outstanding(reqs).length === 0,
      inbound_path: `/webhooks/slack/${handle}`,
    };
    const app = slackApp(c, handle, reqs[0]);
    if (app) {
      seat.detail = `App ${app}`;
      seat.manage_url = slackManageURL(app);
    } else {
      seat.detail = "integrations.slack.bot_token";
    }
    if (!base) {
      seat.manifest_note = `no manifest yet: set \`${PUBLIC_BASE_KEY}\` in this node's Tier A file to the address this agent's app delivers to, and restart the node`;
    } else {
      const built = slackManifest(role.name, handle, base);
      if ("manifest" in built) seat.manifest = built.manifest;
      else seat.manifest_note = built.refused;
      seat.public_url = base + seat.inbound_path;
    }
    return seat;
  });
}

/** The `${VAR}`s an agent's GitHub app has sealed values in (`setupapi.githubSeatSecrets`). */
function githubSeatSecrets(app: Doc | undefined): string[] {
  if (!app) return [];
  const names = [app.private_key, app.webhook_secret]
    .map((ref) => (typeof ref === "string" ? wholeRef(ref) : null))
    .filter((n): n is string => Boolean(n));
  return [...new Set(names)].sort();
}

/** Every agent's own GitHub app, and which act is outstanding (`setupapi.githubSeats`). */
function githubSeats(c: DemoCompany): Seat[] {
  const doc = documentOf(c);
  const block = blockOf(doc, "github");
  const webBase = githubWebBase(block ? (block.url as string | undefined) : undefined);
  const org = String(getPath(block, "provisioning.org") ?? "").trim();
  return rolesOf(doc).map(({ handle, role }) => {
    const app = getPath(role, "integrations.github") as Doc | undefined;
    const tier = githubTier(app ? (app.tier as string | undefined) : "");
    const seat: Seat = {
      handle,
      name: role.name,
      tier: tier.tier,
      tier_label: tier.label,
      tier_hint: tier.hint,
      requirements: [],
      present: false,
      satisfied: false,
    };
    if (app) seat.enrolled = true;
    const secrets = githubSeatSecrets(app);
    if (secrets.length > 0) seat.secrets = secrets;
    const slug = String(app?.app_slug ?? "").trim();
    if (app && slug) seat.manage_url = githubAppPage(webBase, org, slug, "advanced");
    if (!app || !Number(app.app_id)) {
      seat.step = "create_app";
      seat.detail = "no app of its own yet, so this agent acts as nobody on GitHub";
    } else if (!Number(app.installation_id)) {
      seat.present = true;
      seat.step = "install_app";
      if (slug) seat.action_url = githubAppPage(webBase, org, slug, "installations");
      seat.detail =
        "the app exists and nothing has installed it, so it sees no repository and mints no usable token";
    } else {
      seat.present = true;
      const key = resolution(c, String(app.private_key ?? ""));
      if (!key.present) {
        seat.detail =
          "the app is installed with no private key recorded, so nothing can be minted for it: GitHub returns that key once, so the app has to be created again";
      } else if (key.resolved === false) {
        seat.detail = `${String(app.private_key)} did not resolve, so no token can be minted for this seat and it reaches GitHub as nobody`;
      } else {
        seat.satisfied = true;
        const repos = Array.isArray(app.repos) ? (app.repos as string[]) : [];
        seat.detail = slug + (repos.length > 0 ? ` in ${repos.join(", ")}` : "");
        if (!seat.detail) {
          seat.detail =
            repos.length > 0 ? repos.join(", ") : "Every repository the installation covers";
        }
      }
    }
    return seat;
  });
}

/** Where a tool keeps each agent's own credential, and who the agent is there (`setupapi.seatCredentialAt`). */
interface CredentialAt {
  find(role: RoleDoc): { stored: string; where: string };
  address: string;
  vars?(role: RoleDoc): string[];
  identity?(role: RoleDoc, handle: string): string;
}

function atlassianAt(c: DemoCompany, product: AtlassianProduct): CredentialAt {
  return {
    find: (role) => {
      const cred = atlassianCredential(product, role.mcp_env);
      return cred.where
        ? { stored: cred.token, where: `mcp_env.${cred.where}` }
        : { stored: "", where: "" };
    },
    address: "mcp_env.atlassian",
    vars: (role) => [
      atlassianCredential(product, role.mcp_env).token,
      atlassianSeatEmail(role.mcp_env),
    ],
    // RECORDED, NOT DERIVED: Atlassian assigns the address, and the pass
    // records it under the variable the seat names (`atlassianIdentity`).
    identity: (role) => deref(c, atlassianSeatEmail(role.mcp_env)),
  };
}

/** The roster for a tool whose agents each hold a credential (`setupapi.credentialSeats`). */
function credentialSeats(c: DemoCompany, kind: Kind): Seat[] {
  const doc = documentOf(c);
  const block = blockOf(doc, kind);
  const provisions = ["datadog", "atlassian", "gitlab", "mattermost"].includes(kind);
  const app = APP_NAMES[kind];
  let at: CredentialAt;
  switch (kind) {
    case "mattermost":
      at = {
        find: (role) => {
          const value = String(getPath(role, "integrations.mattermost.bot_token") ?? "").trim();
          return value
            ? { stored: value, where: "integrations.mattermost.bot_token" }
            : { stored: "", where: "" };
        },
        address: "integrations.mattermost.bot_token",
        identity: (_role, handle) =>
          block
            ? `@${mattermostBot(getPath(block, "provisioning.username_prefix") as string | undefined, handle)}`
            : "",
      };
      break;
    case "datadog":
    case "gitlab": {
      const { env, keys } = MCP_CREDENTIALS[kind]!;
      at = {
        find: (role) => mcpCredential(role.mcp_env, [env], keys),
        address: `mcp_env.${env}`,
        identity: (_role, handle) => {
          if (!block) return "";
          return kind === "datadog"
            ? datadogEmail(
                getPath(block, "provisioning.email_domain") as string | undefined,
                handle,
              )
            : gitlabUsername(
                getPath(block, "provisioning.username_prefix") as string | undefined,
                handle,
              );
        },
      };
      break;
    }
    default:
      at = atlassianAt(c, kind === "jira" ? "jira" : kind === "confluence" ? "confluence" : "any");
  }
  const access =
    kind === "datadog"
      ? datadogAccess(getPath(block, "provisioning.role") as string | undefined)
      : null;
  return rolesOf(doc).map(({ handle, role: plain }) => {
    const role = plain as RoleDoc;
    const seat: Seat = {
      handle,
      name: role.name,
      requirements: [],
      present: false,
      satisfied: false,
    };
    if (access) {
      seat.tier = access.tier;
      seat.tier_label = access.label;
      seat.tier_hint = access.hint;
    }
    const { stored, where } = at.find(role);
    seat.present = stored !== "";
    const raw = at.vars ? at.vars(role) : [stored];
    const secrets = [
      ...new Set(raw.map((v) => wholeRef(v)).filter((n): n is string => Boolean(n))),
    ].sort();
    if (secrets.length > 0) seat.secrets = secrets;
    if (!block) {
      seat.detail = `waiting for ${app} to be connected: this company declares no ${app} integration, so nothing reads what this agent holds for it`;
    } else if (!stored) {
      seat.detail = provisions
        ? `not on ${app} yet: name a \${VAR} under this seat's ${at.address} and the next sync creates the account and seals it there`
        : `no ${app} credential yet, and ${app} issues none on request: add one to this seat's ${at.address}`;
    } else if (resolution(c, stored).resolved === false) {
      seat.detail = provisions
        ? `waiting for the next sync to create the account and seal ${where}`
        : `${where} did not resolve, so this agent authenticates with nothing`;
    } else {
      seat.satisfied = true;
      seat.detail = at.identity?.(role, handle) || where;
    }
    return seat;
  });
}

// ---- one tool's form ------------------------------------------------------------

/** The capture's own definition of a tool. */
function catalogueOf(c: DemoCompany, kind: Kind): Tool {
  const tool = c.integrations.catalogue.tools.find((t) => t.key === kind);
  if (!tool) throw new Error(`The setup catalogue holds no definition for ${kind}.`);
  return tool;
}

/** The captured roster's handles, which a handle field's own choices are told apart from. */
function capturedHandles(c: DemoCompany): Set<string> {
  return new Set(
    c.integrations.catalogue.tools.flatMap((t) => (t.seats ?? []).map((s) => s.handle)),
  );
}

/** A kind's form as the document stands (`Service.state`). */
function toolState(c: DemoCompany, kind: Kind): Tool {
  const doc = documentOf(c);
  const def = catalogueOf(c, kind);
  const block = blockOf(doc, kind);
  if ((kind === "jira" || kind === "confluence") && atlassianDataCenter(doc)) {
    console.warn(
      LOG,
      `The ${APP_NAMES[kind]} form for an Atlassian Data Center deployment is not modelled in this prototype; the Cloud form is shown.`,
    );
  }
  const reqs = def.requirements.map((r) => fill(c, r, doc, kind));
  const roster = rolesOf(doc);
  const captured = capturedHandles(c);
  for (const req of reqs) {
    if (req.kind !== "handle") continue;
    // THE ROSTER, after the field's own choices (`seatChoices`).
    req.choices = [
      ...(req.choices ?? []).filter((choice) => !captured.has(choice.value)),
      ...roster.map(({ handle, role }) => ({
        value: handle,
        label:
          role.name.trim() && role.name.trim() !== handle
            ? `${role.name.trim()} (${handle})`
            : handle,
      })),
    ];
  }
  const seats =
    kind === "slack"
      ? slackSeats(c, def)
      : kind === "github"
        ? githubSeats(c)
        : credentialSeats(c, kind);
  let configured: boolean;
  let enabled: boolean;
  switch (kind) {
    case "datadog":
    case "github":
    case "gitlab":
    case "mattermost":
      configured = Boolean(block);
      enabled = Boolean(block) && block?.enabled === true;
      break;
    case "slack":
      configured = enabled = seats.some((s) => s.requirements[0]?.present);
      break;
    default:
      configured = enabled = Boolean(block);
  }
  let satisfied = outstanding(reqs).length === 0;
  const formComplete = satisfied && seats.every((s) => outstanding(s.requirements).length === 0);
  const seatsRequired = kind === "slack" || kind === "github";
  if (seatsRequired) {
    if (seats.length > 0 && !seats.some((s) => s.satisfied)) satisfied = false;
    if (seats.some((s) => (s.present || s.enrolled) && !s.satisfied)) satisfied = false;
  }
  const tool: Tool = {
    key: kind,
    configured,
    enabled,
    requirements: reqs,
    satisfied,
    form_complete: formComplete,
    can_provision: PASS_KINDS.includes(kind),
  };
  if (def.summary) tool.summary = def.summary;
  if (seats.length > 0) tool.seats = seats;
  if (def.inbound_path) tool.inbound_path = def.inbound_path;
  if (seatsRequired) tool.seats_required = true;
  if (def.manage_path) tool.manage_path = def.manage_path;
  const base = publicBase(c);
  if (base && tool.inbound_path) tool.public_url = base + tool.inbound_path;
  return withLoopFindings(c, kind, tool);
}

function atlassianDataCenter(doc: CompanyDocument): boolean {
  return (
    String(blockOf(doc, "atlassian")?.deployment ?? "")
      .trim()
      .toLowerCase() === "data_center"
  );
}

/**
 * The loop's per-agent findings folded into the roster (`setupapi.withLoopFindings`):
 * the first finding naming an agent unsettles its row and gives it the
 * finding's sentence, unless the finding's own verdict is ready.
 */
function withLoopFindings(c: DemoCompany, kind: Kind, tool: Tool): Tool {
  const faults = new Map<string, { detail?: string }>();
  for (const f of c.integrations.states[kind]?.findings ?? []) {
    if (!f.subject || verdictOf(f.kind).phase === "ready" || faults.has(f.subject)) continue;
    faults.set(f.subject, f);
  }
  if (faults.size === 0 || !tool.seats) return tool;
  tool.seats = tool.seats.map((seat) => {
    const f = faults.get(seat.handle);
    return f ? { ...seat, satisfied: false, detail: f.detail ?? "" } : seat;
  });
  return tool;
}

/** The whole setup listing, as `GET /setup/integrations` answers it. */
export function listingOf(c: DemoCompany): Listing {
  return {
    tools: KINDS.map((kind) => toolState(c, kind)),
    revision_id: c.config.revisions[c.config.revisions.length - 1]?.meta.revision_id ?? "",
    external_url: { value: publicBase(c), config_path: PUBLIC_BASE_KEY },
  };
}

// ---- writes ---------------------------------------------------------------------

const fail = (status: number, error: string, fields: Record<string, unknown> = {}): RestReply => ({
  status,
  body: { error, ...fields },
});

const unknownKind = () => fail(404, "unknown_kind", { hint: `one of ${KINDS.join(", ")}` });

/** What `POST /setup/integrations/{kind}/inputs` takes. */
export interface Submission {
  values?: Record<string, string>;
  generate?: string[];
  /** The agent whose own block this writes, for a tool with one per agent. */
  seat?: string;
  summary?: string;
  if_match?: string;
}

/** The name a sealed credential is kept under (`setup.SecretNameFor`). */
function secretNameFor(
  kind: string,
  req: Pick<SetupRequirement, "secret_name" | "field" | "seat">,
): string {
  if (req.secret_name) return req.secret_name;
  return [kind, req.field, ...(req.seat ? [req.seat] : [])]
    .join("_")
    .trim()
    .toUpperCase()
    .replace(/[^A-Z0-9_]/g, "_");
}

/**
 * Apply one submission as the engine's setup surface does
 * (`Service.inputs`, `setup.Writer.Write`): each value written where its
 * requirement says, a credential sealed and pointed at, and a new revision,
 * or a reload when only sealed values changed. Answers what the surface
 * answers. A scenario connects a tool through this same function.
 */
export function submit(c: DemoCompany, key: string, body: Submission, at: string): RestReply {
  if (!isKind(key)) return unknownKind();
  const kind = key;
  const state = toolState(c, kind);
  const unknown = Object.keys(body).filter(
    (k) => !["if_match", "summary", "seat", "values", "generate"].includes(k),
  );
  if (unknown.length > 0) {
    return fail(400, "invalid_body", {
      detail: `json: unknown field ${JSON.stringify(unknown[0])}`,
    });
  }
  let against = state.requirements;
  if (body.seat) {
    const seat = (state.seats ?? []).find((s) => s.handle === body.seat);
    if (!seat) {
      return fail(404, "invalid_input", {
        detail: `no seat called ${body.seat} takes per-seat setup for ${kind}`,
      });
    }
    against = seat.requirements;
  }
  const values: Record<string, string> = { ...(body.values ?? {}) };
  const minted = new Set<string>();
  for (const field of body.generate ?? []) {
    const req = against.find((r) => r.field === field);
    if (!req)
      return fail(400, "invalid_input", {
        detail: `no field ${JSON.stringify(field)} to generate`,
      });
    if (!req.mintable) {
      return fail(400, "invalid_input", {
        detail: `${field} is not something this engine can generate; it comes from the third-party app`,
      });
    }
    if (minted.has(field))
      return fail(400, "invalid_input", {
        detail: `${field} is named twice in generate; name it once`,
      });
    if (field in values) {
      return fail(400, "invalid_input", {
        detail: `${field} was both supplied and asked to be generated; send one or the other`,
      });
    }
    minted.add(field);
    values[field] = uuidV7().replace(/-/g, "").toUpperCase();
  }
  if (Object.keys(values).length === 0) {
    return fail(400, "invalid_input", { detail: "the submission carried no values" });
  }
  for (const r of against) {
    const when = r.required_when;
    if (!when) continue;
    const after = (field: string) => {
      if (field in values) return values[field]!.trim();
      const other = against.find((o) => o.field === field);
      return other ? storedOf(other).trim() : "";
    };
    if (after(when.field) !== when.equals) continue;
    if (after(r.field) !== "" || (r.present && r.resolved !== false)) continue;
    return fail(400, "invalid_input", {
      detail: `${r.field} is required by the answer this submission gives for ${when.field}, and it is unset`,
      hint: "the answer this submission gives needs that field; send it, or choose the answer that does not",
    });
  }
  for (const [field, value] of Object.entries(values)) {
    const r = against.find((o) => o.field === field);
    if (value.trim() === "" && r && (r.required || r.kind === "secret")) {
      return fail(400, "invalid_input", {
        detail: `${field} is a credential the submission clears; omit the field to leave it alone`,
      });
    }
  }
  const active = c.config.revisions[c.config.revisions.length - 1]?.meta.revision_id ?? "";
  if (body.if_match && body.if_match !== active) {
    return fail(409, "revision_advanced", {
      your_base: body.if_match,
      current_revision_id: active,
      hint: "the configuration changed since you read it; re-read the setup state and submit again",
    });
  }
  const doc = structuredClone(documentOf(c)) as CompanyDocument;
  const root = (body.seat ? rolesOf(doc).find((r) => r.handle === body.seat)!.role : doc) as Doc;
  const written: string[] = [];
  let changed = false;
  for (const [field, raw] of Object.entries(values)) {
    const value = raw.trim();
    const r = against.find((o) => o.field === field);
    // An unknown field is a plain error in the engine's writer, which its
    // surface answers as an internal error.
    if (!r) return fail(500, "internal_error");
    if (["url", "id", "email"].includes(r.kind) && /\s/.test(value))
      return fail(500, "internal_error");
    if (r.kind !== "secret") {
      setPath(
        root,
        r.config_path,
        r.kind === "toggle" ? ["true", "yes", "on", "1"].includes(value.toLowerCase()) : value,
      );
      changed = true;
      continue;
    }
    const ref = wholeRef(value);
    if (ref) {
      setPath(root, r.config_path, `\${${ref}}`);
      changed = true;
      continue;
    }
    const current = storedOf(r).trim();
    let name: string;
    if (!current) {
      name = secretNameFor(kind, r);
      setPath(root, r.config_path, `\${${name}}`);
      changed = true;
    } else if (wholeRef(current)) {
      name = wholeRef(current)!;
    } else {
      return fail(409, "literal_in_config", {
        path: r.config_path,
        detail:
          "this field holds a value rather than a ${VAR} reference, so there is no variable to write the credential into",
        hint: "replace it with a whole ${VAR} reference, or clear it and submit again",
      });
    }
    seal(c, name, at, actorOf(c), "setup");
    written.push(name);
  }
  // THE TOOL IS ASKED AFRESH: the write's revision runs this surface's pass,
  // which no longer hears what a scenario scripted it to answer.
  forgetScript(c, kind);
  const summary = `connect ${kind}${body.summary?.trim() ? `: ${body.summary.trim().split(/\s+/).join(" ")}` : ""}`;
  let revision: string;
  let reloaded = false;
  if (changed) {
    revision = appendRevision(c, doc, summary, { at, source: "setup" });
  } else {
    revision = reloadRevision(c, at);
    reloaded = true;
  }
  recordEndpoint(c, kind);
  return {
    status: 201,
    body: {
      revision_id: revision,
      epoch: c.config.epoch,
      wrote_secrets: written,
      reloaded,
      state: toolState(c, kind),
    },
  };
}

/** The secrets a disconnect leaves behind, which it names rather than deletes (`setupapi.orphanedSecrets`). */
function orphanedSecrets(c: DemoCompany, kind: Kind, state: Tool): string[] {
  const out: string[] = [];
  const add = (reqs: SetupRequirement[]) => {
    for (const req of reqs) {
      if (req.kind !== "secret" || !req.present) continue;
      const current = storedOf(req).trim();
      if (!current) out.push(secretNameFor(kind, req));
      else if (wholeRef(current)) out.push(wholeRef(current)!);
    }
  };
  add(state.requirements);
  for (const seat of state.seats ?? []) {
    add(seat.requirements);
    out.push(...(seat.secrets ?? []));
  }
  // A credential Jira and Confluence share stays in use by the one staying (`stillUsedBySibling`).
  const siblings: Partial<Record<Kind, Kind[]>> = {
    jira: ["confluence"],
    confluence: ["jira"],
    atlassian: ["jira", "confluence"],
  };
  const keep = new Set<string>();
  const doc = documentOf(c);
  for (const sibling of siblings[kind] ?? []) {
    if (!blockOf(doc, sibling) || c.integrations.states[sibling]?.disconnecting) continue;
    for (const seat of toolState(c, sibling).seats ?? [])
      for (const name of seat.secrets ?? []) keep.add(name);
  }
  return [...new Set(out)].filter((n) => !keep.has(n)).sort();
}

/**
 * Finish a disconnect as the loop's teardown pass does (`Disconnector`): the
 * company block dropped and, where the agents' own accounts go too, what
 * each tool removes of them.
 */
function finishTeardown(c: DemoCompany, kind: Kind, removeSeats: boolean, at: string): void {
  const doc = structuredClone(documentOf(c)) as CompanyDocument;
  const integrations = doc.integrations as Doc | undefined;
  if (integrations) delete integrations[kind];
  if (removeSeats) {
    const forget = new Set<string>();
    for (const { role } of rolesOf(doc)) {
      const own = (role as RoleDoc).integrations;
      if (kind === "slack" && own?.slack) {
        for (const ref of [getPath(own, "slack.bot_token"), getPath(own, "slack.signing_secret")]) {
          const name = typeof ref === "string" ? wholeRef(ref) : null;
          if (name) forget.add(name);
        }
        delete own.slack;
      }
      if (kind === "github" && own?.github && Number(getPath(own, "github.installation_id"))) {
        (own.github as Doc).installation_id = 0;
      }
      if (kind === "mattermost" && own?.mattermost) {
        const name = wholeRef(String(getPath(own, "mattermost.bot_token") ?? ""));
        if (name) forget.add(name);
      }
      if (kind === "gitlab" || kind === "datadog") {
        const { env, keys } = MCP_CREDENTIALS[kind]!;
        const name = wholeRef(mcpCredential((role as RoleDoc).mcp_env, [env], keys).stored);
        if (name) forget.add(name);
      }
    }
    c.secrets = c.secrets.filter((s) => !forget.has(s.name));
  }
  forgetScript(c, kind);
  appendRevision(c, doc, `disconnect ${kind}`, { at, source: "setup" });
  delete c.integrations.states[kind];
}

/** Disconnect a surface (`Service.disconnect`): asked of the loop, or forced at once. */
export function disconnect(
  c: DemoCompany,
  key: string,
  body: Record<string, unknown>,
  at: string,
): RestReply {
  if (!isKind(key)) return unknownKind();
  const kind = key;
  const state = toolState(c, kind);
  if (!state.configured) return { status: 200, body: { key: kind, removed: false } };
  const unknown = Object.keys(body).filter((k) => !["remove_seats", "force"].includes(k));
  if (unknown.length > 0) {
    return fail(400, "invalid_body", {
      detail: `json: unknown field ${JSON.stringify(unknown[0])}`,
    });
  }
  const removeSeats = body.remove_seats === true;
  const orphaned = orphanedSecrets(c, kind, state);
  if (body.force !== true) {
    askTeardown(c, kind, removeSeats);
    // The demo's loop runs the teardown it was just asked for at once.
    finishTeardown(c, kind, removeSeats, at);
    return {
      status: 202,
      body: {
        key: kind,
        removed: false,
        disconnecting: true,
        remove_seats: removeSeats,
        detail:
          "the engine is removing what this integration holds at the third-party app; the block is dropped when that finishes",
        orphaned_secrets: orphaned,
      },
    };
  }
  const doc = structuredClone(documentOf(c)) as CompanyDocument;
  const integrations = doc.integrations as Doc | undefined;
  if (integrations) delete integrations[kind];
  forgetScript(c, kind);
  const revision = appendRevision(c, doc, `disconnect ${kind}`, { at, source: "setup" });
  delete c.integrations.states[kind];
  return {
    status: 200,
    body: {
      key: kind,
      removed: true,
      revision_id: revision,
      epoch: c.config.epoch,
      orphaned_secrets: orphaned,
    },
  };
}

/** The setup passes a node remembers per kind (`setupapi.MaxRunsListed`). */
const MAX_RUNS = 10;

/**
 * Run a surface's setup pass now (`Service.runPass`): read-only for a check,
 * held to the form and the public address for a provision. The pass's report
 * is what the loop records.
 */
export function runSetupPass(
  c: DemoCompany,
  key: string,
  readOnly: boolean,
  at: string,
): RestReply {
  // ASKED FIRST, as the engine asks it: a kind this build knows nothing of
  // runs no pass either.
  if (!isKind(key) || !PASS_KINDS.includes(key)) {
    return fail(409, "not_provisionable", {
      detail: `this build runs no provisioning pass for ${key}`,
      hint: "its setup is the values on this surface; anything at the third-party app is done there or with the crewlet command line",
    });
  }
  const kind = key;
  if (!readOnly) {
    const missing = outstanding(toolState(c, kind).requirements).map((r) => r.field);
    if (missing.length > 0) {
      return fail(409, "requirements_outstanding", {
        fields: missing,
        hint: "supply these first; a pass writes at the third-party app and must not run against a half-configured integration",
      });
    }
    if (!publicBase(c)) {
      return fail(409, "no_external_url", {
        message:
          "No external address is set. Set `api.external_url` in this node's config file and restart.",
        config_path: PUBLIC_BASE_KEY,
        hint: `set \`${PUBLIC_BASE_KEY}\` in this node's Tier A file to the HTTPS address a vendor reaches this deployment on, and restart it; without one the pass can register no webhook`,
      });
    }
  }
  const ended = new Date(Date.parse(at) + 1_000).toISOString();
  const run: SetupRun = {
    run_id: uuidV7(),
    key: kind,
    state: "done",
    started_at: at,
    ended_at: ended,
  };
  const outcome = runPass(c, kind, ended);
  (c.integrations.runs[kind] ??= []).unshift(run);
  c.integrations.runs[kind] = c.integrations.runs[kind]!.slice(0, MAX_RUNS);
  if (outcome === NOT_CONFIGURED) {
    run.state = "failed";
    run.error = NOT_CONFIGURED;
    return {
      status: 502,
      body: {
        error: "vendor_refused",
        run,
        hint: "the third-party app refused this pass; nothing it had already done is undone, and re-running is safe",
      },
    };
  }
  const state = c.integrations.states[kind]!;
  if (state.findings.length > 0) run.findings = structuredClone(state.findings);
  const report: Record<string, string> = { phase: state.phase };
  if (state.actor) report.actor = state.actor;
  if (state.detail) report.detail = state.detail;
  if (state.action_url) report.action_url = state.action_url;
  run.report = report as unknown as SetupRun["report"];
  return { status: 200, body: run };
}

/**
 * PROPOSED, NOT AN ENGINE ROUTE: check one typed credential at the tool
 * without storing it. A design proposal asks for it; the engine serves
 * nothing at `POST /setup/integrations/{kind}/verify`. Its answers follow a
 * rule a walk can drive both ways: a whole `${NAME}` works when the secret
 * store holds NAME, and a literal is refused when it says "bad" or is
 * shorter than eight characters.
 */
export function verify(c: DemoCompany, key: string, body: Record<string, unknown>): RestReply {
  if (!isKind(key)) return unknownKind();
  const kind = key;
  const state = toolState(c, kind);
  const field = typeof body.field === "string" ? body.field : "";
  const value = typeof body.value === "string" ? body.value.trim() : "";
  let against = state.requirements;
  if (typeof body.seat === "string" && body.seat) {
    const seat = (state.seats ?? []).find((s) => s.handle === body.seat);
    if (!seat) {
      return fail(404, "invalid_input", {
        detail: `no seat called ${body.seat} takes per-seat setup for ${kind}`,
      });
    }
    against = seat.requirements;
  }
  const req = against.find((r) => r.field === field);
  if (!req)
    return fail(400, "invalid_input", {
      detail: `no field ${JSON.stringify(field)} to check on ${kind}`,
    });
  const app = APP_NAMES[kind];
  const ref = wholeRef(value);
  if (ref) {
    return resolves(c, ref)
      ? { status: 200, body: { ok: true, detail: "Works." } }
      : { status: 200, body: { ok: false, detail: `There is no secret ${ref}.` } };
  }
  const label = /^[A-Z]{2,}\b/.test(req.label)
    ? req.label
    : req.label.charAt(0).toLowerCase() + req.label.slice(1);
  if (/bad/i.test(value) || value.length < 8) {
    return { status: 200, body: { ok: false, detail: `${app} refused this ${label}.` } };
  }
  return { status: 200, body: { ok: true, detail: `Works. ${app} accepted it.` } };
}

/** Answer one `/setup/integrations` request. */
export function handleSetup(engine: DemoEngine, request: RestRequest): RestReply {
  const c = engine.company;
  const at = engine.now().toISOString();
  const [key, sub, id] = request.path.split("/").filter(Boolean).slice(2).map(decodeURIComponent);
  const body = (): Record<string, unknown> | null => {
    try {
      const parsed: unknown = JSON.parse(request.body || "{}");
      return parsed && typeof parsed === "object" && !Array.isArray(parsed)
        ? (parsed as Record<string, unknown>)
        : null;
    } catch {
      return null;
    }
  };
  const invalidBody = () => fail(400, "invalid_body", { detail: "the body is not a JSON object" });
  /** A write's answer, with what an apply pushes when it moved the company. */
  const applied = (reply: RestReply): RestReply => {
    if (reply.status < 400) engine.pushConfiguration();
    return reply;
  };
  if (!key) {
    return request.method === "GET"
      ? { status: 200, body: listingOf(c) }
      : fail(405, "method_not_allowed");
  }
  if (!sub) {
    if (request.method === "GET") {
      if (!isKind(key)) {
        return fail(404, "unknown_kind", {
          detail: `this build serves no setup for ${key}`,
          hint: `one of ${KINDS.join(", ")}`,
        });
      }
      return { status: 200, body: toolState(c, key) };
    }
    if (request.method === "DELETE") {
      const parsed = body();
      return parsed ? applied(disconnect(c, key, parsed, at)) : invalidBody();
    }
  }
  if (sub === "inputs" && request.method === "POST") {
    const parsed = body();
    return parsed ? applied(submit(c, key, parsed as Submission, at)) : invalidBody();
  }
  if ((sub === "check" || sub === "provision") && request.method === "POST") {
    return applied(runSetupPass(c, key, sub === "check", at));
  }
  if (sub === "verify" && request.method === "POST") {
    const parsed = body();
    return parsed ? verify(c, key, parsed) : invalidBody();
  }
  if (sub === "runs" && request.method === "GET") {
    if (!isKind(key))
      return fail(404, "run_not_found", { hint: "this build knows no integration by that name" });
    const runs = c.integrations.runs[key] ?? [];
    if (id) {
      const run = runs.find((r) => r.run_id === id);
      return run
        ? { status: 200, body: run }
        : fail(404, "run_not_found", {
            hint: "a run is remembered by the node that executed it, and only for the last few passes",
          });
    }
    return { status: 200, body: { runs, scope: "this node" } };
  }
  if (key === "github" && sub === "app" && request.method === "POST") {
    console.warn(LOG, "Creating a GitHub app sends the browser to GitHub; refused.");
    return fail(409, "not_modelled", { detail: notModelled("Creating an app at GitHub") });
  }
  return { status: 404, body: { error: "no_route" } };
}
