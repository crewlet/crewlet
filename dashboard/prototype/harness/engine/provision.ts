/**
 * What the engine's provisioning passes write for one agent, on the four
 * tools whose agent accounts the engine creates itself: Datadog, Atlassian,
 * GitLab and Mattermost (each package's `PlanFor` and seat reconcile).
 *
 * TWO WRITERS, as in the engine. A person opts an agent in by naming a
 * `${VAR}` at the agent's credential address, which no pass can do for it;
 * the pass then creates the account and seals what the tool issued under
 * that name, writing as the reconcile loop. The variables are named the way
 * the engine's documentation names them (`GITLAB_TOKEN_SWE`,
 * `MATTERMOST_TOKEN_ENGINEER`, `CTO_JIRA_TOKEN`), and a variable the agent
 * already names is the one sealed.
 *
 * Who the agent is at the tool is derived from its handle on three of them
 * (the setup forms read it the same way); Atlassian assigns the address, so
 * its pass records it beside the token, and the demo keeps that one value to
 * show it (`DemoCompany.recorded`).
 */

import type { CompanyDocument, ConfigRole } from "~/protocol/types.ts";
import { documentOf, type DemoCompany } from "./company.ts";
import { agentRoles } from "./derive.ts";
import { fixtureId } from "./ids.ts";
import { blockOf, passRuns } from "./loop.ts";
import { varSuffix, wholeRef } from "./refs.ts";
import { APP_NAMES, MCP_CREDENTIALS } from "./vendors.ts";

/** The tools whose agent accounts a pass creates. */
export const PROVISIONED = ["datadog", "atlassian", "gitlab", "mattermost"] as const;
export type ProvisionedKind = (typeof PROVISIONED)[number];

/**
 * Who a pass writes as (`engine.reconcileOperator`): the engine itself, with
 * no credential behind it.
 */
export const RECONCILE_LOOP = { name: "reconcile loop", kind: "system", operator_id: "" } as const;
export const PROVISION_SOURCE = "provision";

/** What one pass writes for one agent. */
export interface Provisioning {
  /** The document with the agent's variables named, or null where it already names them. */
  doc: CompanyDocument | null;
  /** The variables the pass seals, each with the value the demo may show for it, if any. */
  sealed: { name: string; shown?: string }[];
}

type McpEnv = Record<string, Record<string, string>>;
type Role = ConfigRole & { mcp_env?: McpEnv };

/** Where an Atlassian seat's token and address may sit (`atlassian.Product.servers`, `tokenKeys`, `emailKeys`). */
const ATLASSIAN_SERVERS = ["atlassian", "jira", "confluence"];
const ATLASSIAN_TOKEN_KEYS = [
  "JIRA_API_TOKEN",
  "CONFLUENCE_API_TOKEN",
  "ATLASSIAN_API_TOKEN",
  "JIRA_PERSONAL_TOKEN",
  "CONFLUENCE_PERSONAL_TOKEN",
  "JIRA_TOKEN",
  "CONFLUENCE_TOKEN",
  "Authorization",
];
const ATLASSIAN_EMAIL_KEYS = [
  "JIRA_USERNAME",
  "JIRA_EMAIL",
  "ATLASSIAN_EMAIL",
  "CONFLUENCE_USERNAME",
  "CONFLUENCE_EMAIL",
];

/** The first slot of `servers` holding one of `keys`, raw (`atlassian.seatCredential`, `seatEmail`). */
function slotIn(env: McpEnv | undefined, servers: string[], keys: string[]) {
  for (const server of servers) {
    const block = env?.[server];
    if (!block || Object.keys(block).length === 0) continue;
    for (const key of keys) {
      const value = (block[key] ?? "").trim();
      if (value) return { server, key, value };
    }
  }
  return null;
}

/**
 * The variable a slot names, or a refusal naming the slot: the engine's
 * plan leaves alone a seat whose credential is a value rather than a whole
 * `${VAR}`, since there is nowhere to write what it mints (`provision.SoleVar`).
 */
function soleVar(value: string, where: string, handle: string, kind: ProvisionedKind): string {
  const name = wholeRef(value);
  if (!name) {
    throw new Error(
      `${handle}'s ${where} holds a value rather than a whole \${VAR} reference, so the ${APP_NAMES[kind]} pass has nowhere to write what it mints and leaves this agent to be managed by hand.`,
    );
  }
  return name;
}

/**
 * What the `kind` pass writes for the agent `handle`: its credential
 * variable named where the agent has none, and what the pass seals. `email`
 * is the address Atlassian assigns, which a scenario may name; by default it
 * has Atlassian's service-account shape.
 *
 * Throws, naming why, where the pass would not provision the agent: the tool
 * is not connected, the handle is no agent's, or the agent's credential is a
 * value rather than a variable.
 */
export function provisioning(
  c: DemoCompany,
  kind: ProvisionedKind,
  handle: string,
  options: { email?: string } = {},
): Provisioning {
  const current = documentOf(c);
  if (!blockOf(current, kind) || !passRuns(current, kind)) {
    throw new Error(
      `${APP_NAMES[kind]} is not connected, so no pass runs for it: connect it first.`,
    );
  }
  const doc = structuredClone(current);
  const agent = agentRoles(doc).find((r) => r.handle === handle);
  if (!agent)
    throw new Error(`There is no agent "${handle}"; a person has an account of their own.`);
  const role = agent.role as Role;
  const suffix = varSuffix(handle);
  let changed = false;
  /** Name `variable` at `env[server][key]`. */
  const name = (server: string, key: string, variable: string) => {
    const env = (role.mcp_env ??= {});
    (env[server] ??= {})[key] = `\${${variable}}`;
    changed = true;
    return variable;
  };
  const sealed: Provisioning["sealed"] = [];
  switch (kind) {
    case "datadog":
    case "gitlab": {
      const { env, keys } = MCP_CREDENTIALS[kind]!;
      const slot = slotIn(role.mcp_env, [env], keys);
      // GitLab reads a bearer header's token as the credential (`gitlab.stripScheme`).
      const raw =
        kind === "gitlab" ? (slot?.value.replace(/^Bearer /, "").trim() ?? "") : slot?.value;
      const token = slot
        ? soleVar(raw ?? "", `mcp_env.${env}.${slot.key}`, handle, kind)
        : name(env, keys[0]!, `${keys[0]!}_${suffix}`);
      sealed.push({ name: token });
      break;
    }
    case "mattermost": {
      const integrations = ((role as { integrations?: Record<string, unknown> }).integrations ??=
        {}) as Record<string, unknown>;
      const bot = (integrations.mattermost ??= {}) as Record<string, unknown>;
      const stored = typeof bot.bot_token === "string" ? bot.bot_token.trim() : "";
      if (stored) {
        sealed.push({ name: soleVar(stored, "integrations.mattermost.bot_token", handle, kind) });
      } else {
        bot.bot_token = `\${MATTERMOST_TOKEN_${suffix}}`;
        changed = true;
        sealed.push({ name: `MATTERMOST_TOKEN_${suffix}` });
      }
      break;
    }
    case "atlassian": {
      const tokenSlot = slotIn(role.mcp_env, ATLASSIAN_SERVERS, ATLASSIAN_TOKEN_KEYS);
      const token = tokenSlot
        ? soleVar(tokenSlot.value, `mcp_env.${tokenSlot.server}.${tokenSlot.key}`, handle, kind)
        : name("atlassian", "JIRA_API_TOKEN", `${suffix}_JIRA_TOKEN`);
      // THE ADDRESS GOES BESIDE THE TOKEN: Atlassian authenticates its
      // products as base64(address:token), and assigns the address itself.
      const emailSlot = slotIn(role.mcp_env, ATLASSIAN_SERVERS, ATLASSIAN_EMAIL_KEYS);
      const email =
        options.email ??
        `${handle}-${fixtureId(`atlassian-account-${handle}`).replace(/-/g, "").slice(0, 6)}@serviceaccount.atlassian.com`;
      const address = emailSlot
        ? wholeRef(emailSlot.value)
        : name(tokenSlot?.server ?? "atlassian", "JIRA_USERNAME", `${suffix}_JIRA_USER`);
      sealed.push({ name: token });
      // A literal address is the operator's own and the pass leaves it be.
      if (address) sealed.push({ name: address, shown: email });
      break;
    }
  }
  return { doc: changed ? doc : null, sealed };
}
