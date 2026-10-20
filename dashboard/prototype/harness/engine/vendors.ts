/**
 * What each third-party tool is, as the engine's integration packages say
 * it: which surfaces the reconcile loop runs a pass for, where deliveries
 * arrive, how an agent's identity at the tool is spelled, the Slack app
 * manifest and the GitHub app pages.
 *
 * PORTS, each named after the function it ports, so a reader can hold one
 * against the other. Nothing here reads the demo company: these are the
 * vendor rules the setup surface, the reconcile loop and the integrations
 * answer apply to it.
 */

import type { ReconcileFinding } from "~/contract/integrations.ts";
import { count } from "./findings.ts";
import { goQuote } from "./refusal.ts";

/** Every surface the setup surface serves, in the engine's order (`integration.Kinds`). */
export const KINDS = [
  "slack",
  "mattermost",
  "jira",
  "confluence",
  "github",
  "gitlab",
  "datadog",
  "atlassian",
] as const;
export type Kind = (typeof KINDS)[number];

/** What each tool is called in a sentence. */
export const APP_NAMES: Record<Kind, string> = {
  slack: "Slack",
  mattermost: "Mattermost",
  jira: "Jira",
  confluence: "Confluence",
  github: "GitHub",
  gitlab: "GitLab",
  datadog: "Datadog",
  atlassian: "Atlassian",
};

export function isKind(value: string): value is Kind {
  return (KINDS as readonly string[]).includes(value);
}

/**
 * The surfaces the reconcile loop runs a setup pass for (`engine.setupPasses`):
 * what `can_provision` says, what `Converges` answers, and what a check runs.
 * Slack has none: its apps are made from a manifest by a person.
 */
export const PASS_KINDS: readonly Kind[] = [
  "atlassian",
  "datadog",
  "jira",
  "confluence",
  "gitlab",
  "mattermost",
  "github",
];

/**
 * Who registers a surface's delivery address at the tool (`Kind.Ingress`):
 * the engine's own pass, the operator (a Slack app's request URL), or
 * nobody, since nothing is delivered.
 */
export function ingressOf(kind: string): "engine" | "operator" | "none" {
  if (["github", "gitlab", "jira", "confluence", "datadog"].includes(kind)) return "engine";
  if (kind === "slack") return "operator";
  return "none";
}

/** Where a surface's company-wide deliveries arrive, as the setup surface reports it (`setupapi.inboundPath`). */
export function setupInboundPath(kind: string): string {
  return ["datadog", "github", "jira", "gitlab", "confluence"].includes(kind)
    ? `/webhooks/${kind}`
    : "";
}

/** Where a surface's deliveries arrive, as the integrations answer reports it (`queries.inboundPath`). */
export function rowInboundPath(kind: string): string {
  switch (kind) {
    case "mattermost":
      return "";
    case "slack":
      return "/webhooks/slack/{handle}";
    case "github":
      return "/webhooks/github/{handle}";
    case "forge":
      return "/webhooks/forge";
    default:
      return `/webhooks/${kind}`;
  }
}

/** The delivery sources a surface's events arrive as (`queries.deliversAs`). */
export function deliversAs(kind: string): string[] {
  if (kind === "forge") return ["jira", "confluence"];
  if (kind === "atlassian") return [];
  return [kind];
}

/** The surfaces each tool card rolls up, with the name each is called by (`integration.Tools`). */
export const TOOLS: { key: string; surfaces: { key: string; name: string }[] }[] = [
  { key: "slack", surfaces: [{ key: "slack", name: "Slack" }] },
  { key: "mattermost", surfaces: [{ key: "mattermost", name: "Mattermost" }] },
  {
    key: "atlassian",
    surfaces: [
      { key: "atlassian", name: "Organization" },
      { key: "confluence", name: "Confluence" },
      { key: "jira", name: "Jira" },
      { key: "forge", name: "Forge relay" },
    ],
  },
  { key: "github", surfaces: [{ key: "github", name: "GitHub" }] },
  { key: "gitlab", surfaces: [{ key: "gitlab", name: "GitLab" }] },
  { key: "datadog", surfaces: [{ key: "datadog", name: "Datadog" }] },
];

// ---- Slack ---------------------------------------------------------------------

/** The scopes and events every agent's Slack app asks for (`slack.BotScopes`, `slack.BotEvents`). */
const SLACK_BOT_SCOPES = [
  "app_mentions:read",
  "channels:history",
  "channels:read",
  "chat:write",
  "files:read",
  "groups:history",
  "groups:read",
  "im:history",
  "im:read",
  "im:write",
  "mpim:history",
  "mpim:read",
  "reactions:write",
  "usergroups:read",
  "usergroups:write",
  "users:read",
];
const SLACK_BOT_EVENTS = [
  "app_mention",
  "message.channels",
  "message.groups",
  "message.im",
  "message.mpim",
];
const SLACK_APP_NAME_MAX = 35;
const SLACK_DESCRIPTION_MAX = 140;

/**
 * One agent's Slack app manifest, rendered as the engine renders it
 * (`slack.ManifestJSON`): Go's indented encoding, which sorts keys and
 * escapes `<`, `>` and `&`. Answers the refusal instead where Slack would
 * refuse the app.
 */
export function slackManifest(
  roleName: string,
  handle: string,
  base: string,
): { manifest: string } | { refused: string } {
  const description = `Crewlet agent @${handle}`;
  const runes = (s: string) => [...s].length;
  if (runes(roleName) > SLACK_APP_NAME_MAX) {
    return {
      refused: `slack: role name ${goQuote(roleName)} is ${runes(roleName)} characters and Slack caps an app name at ${SLACK_APP_NAME_MAX}: shorten the role's \`name\` in the company config`,
    };
  }
  if (runes(description) > SLACK_DESCRIPTION_MAX) {
    return {
      refused: `slack: handle ${goQuote(handle)} makes a ${runes(description)}-character app description and Slack caps it at ${SLACK_DESCRIPTION_MAX}: shorten the seat's handle`,
    };
  }
  const root = base.trim().replace(/\/+$/, "");
  return {
    manifest: goIndent({
      display_information: { name: roleName, description },
      features: { bot_user: { display_name: handle, always_online: true } },
      oauth_config: {
        redirect_urls: [`${root}/webhooks/slack-oauth`],
        scopes: { bot: SLACK_BOT_SCOPES },
      },
      settings: {
        event_subscriptions: {
          request_url: `${root}/webhooks/slack/${handle}`,
          bot_events: SLACK_BOT_EVENTS,
        },
        org_deploy_enabled: false,
        socket_mode_enabled: false,
        token_rotation_enabled: false,
      },
    }),
  };
}

/** Where an agent's Slack app is managed (`slack.ManageURL`). */
export function slackManageURL(appID: string): string {
  return appID ? `https://api.slack.com/apps/${encodeURIComponent(appID)}` : "";
}

/** `json.MarshalIndent(value, "", "  ")`: keys sorted, HTML-sensitive characters escaped. */
function goIndent(value: unknown): string {
  const sorted = (v: unknown): unknown =>
    Array.isArray(v)
      ? v.map(sorted)
      : v && typeof v === "object"
        ? Object.fromEntries(
            Object.keys(v)
              .sort()
              .map((k) => [k, sorted((v as Record<string, unknown>)[k])]),
          )
        : v;
  return JSON.stringify(sorted(value), null, 2)
    .replace(/</g, "\\u003c")
    .replace(/>/g, "\\u003e")
    .replace(/&/g, "\\u0026")
    .replace(/\u2028/g, "\\u2028")
    .replace(/\u2029/g, "\\u2029");
}

// ---- GitHub --------------------------------------------------------------------

/** An agent's code-access tier (`github.ParseTier`, `Tier.Label`, `Tier.Hint`). */
export function githubTier(raw: string | undefined): { tier: string; label: string; hint: string } {
  const value = (raw ?? "").replace(/-/g, "_").trim().toLowerCase();
  const tier = ["read_only", "review", "full_access"].includes(value) ? value : "read_only";
  const labels: Record<string, [string, string]> = {
    read_only: ["Read-only", "Reads code and issues, changes nothing."],
    review: ["Review", "Reads code and writes about it: issues, comments, reviews."],
    full_access: [
      "Full access",
      "Branches, commits, pull requests, issues and checks. No administration.",
    ],
  };
  const [label, hint] = labels[tier]!;
  return { tier, label, hint };
}

/** The GitHub web address a company's apps live under (`GitHub.WebURL`). */
export function githubWebBase(url: string | undefined): string {
  const base = (url ?? "").trim().replace(/\/+$/, "");
  return base ? base.replace(/\/api\/v3$/, "") : "https://github.com";
}

/** One of an app's settings pages (`github.appSettingsPage`): `installations` or `advanced`. */
export function githubAppPage(
  webBase: string,
  org: string,
  slug: string,
  page: "installations" | "advanced",
): string {
  const name = slug.trim();
  if (!name) return "";
  let base = webBase.trim().replace(/\/+$/, "") || "https://github.com";
  if (org.trim()) base += `/organizations/${encodeURIComponent(org.trim())}`;
  return `${base}/settings/apps/${encodeURIComponent(name)}/${page}`;
}

/** One agent's own app as GitHub's pass reads it (`github.SeatApp`). */
export interface GitHubSeatApp {
  handle: string;
  /** Zero until a person creates the app. */
  appId: number;
  slug: string;
  /** Zero until a person installs it. */
  installationId: number;
  /** Whether the app's private key resolves on this node. */
  keyResolves: boolean;
}

/**
 * The one finding for the agents with no GitHub App at all, or null when
 * every agent has one (`github.SeatsWithNoApp`). The engine's remedy joins
 * its two clauses with an em dash, which a colon replaces here.
 */
export function githubSeatsWithNoApp(handles: readonly string[]): ReconcileFinding | null {
  if (handles.length === 0) return null;
  const detail =
    handles.length > 1
      ? `${count(handles.length, "agent")} have no GitHub App of their own, so nothing they do on GitHub is theirs`
      : `${handles[0]} has no GitHub App of its own, so nothing it does on GitHub is its own`;
  return {
    kind: "approval_required",
    subject: "agents without an app",
    detail,
    remedy:
      "Create one per agent: GitHub offers no API for it, so a person clicking is the only thing that can.",
    subjects: [...handles],
  };
}

/**
 * What GitHub's pass finds about each agent's own app, in handle order
 * (`github.ReconcileSeatApps`), against a GitHub that answers every read:
 * an app not created yet, a private key that does not resolve, an app
 * nothing has installed. An installed app whose key resolves is ready; the
 * demo's GitHub never suspends an installation or grants other than the
 * tier asks.
 */
export function githubSeatAppFindings(
  seats: readonly GitHubSeatApp[],
  webBase: string,
  org: string,
): ReconcileFinding[] {
  const out: ReconcileFinding[] = [];
  const ordered = [...seats].sort((a, b) =>
    a.handle < b.handle ? -1 : a.handle > b.handle ? 1 : 0,
  );
  for (const seat of ordered) {
    const h = seat.handle;
    if (!seat.appId) {
      out.push({
        kind: "approval_required",
        subject: h,
        detail: `${h} has no GitHub App of its own, so nothing it does on GitHub is its own: create one from Settings › Integrations`,
      });
    } else if (!seat.keyResolves) {
      out.push({
        kind: "credential_missing",
        subject: h,
        detail: `the private key for ${h}'s app did not resolve, and GitHub issues it once: delete the app at GitHub and create it again`,
      });
    } else if (!seat.installationId) {
      out.push({
        kind: "approval_required",
        subject: h,
        action_url: githubAppPage(webBase, org, seat.slug, "installations"),
        detail: `${h}'s app exists and nothing has installed it, so it sees no repository: install it on ${org.trim() || "your organization"}`,
      });
    }
  }
  return out;
}

// ---- the agents' own accounts --------------------------------------------------

/** An agent's Mattermost bot (`mattermost.BotUsername`). */
export function mattermostBot(prefix: string | undefined, handle: string): string {
  return `${(prefix ?? "").trim()}${handle}`.toLowerCase();
}

/** An agent's GitLab service account (`gitlab.Username`). */
export function gitlabUsername(prefix: string | undefined, handle: string): string {
  return `${(prefix ?? "").trim() || "crewlet"}-${handle}`;
}

/** An agent's Datadog account (`datadog.AccountEmail`). */
export function datadogEmail(domain: string | undefined, handle: string): string {
  return `crewlet-${handle}@${(domain ?? "").trim() || "agents.crewlet.invalid"}`;
}

/** The access the agents' Datadog role grants (`datadog.TierOf`, `RoleLabel`, `RoleHint`). */
export function datadogAccess(role: string | undefined): {
  tier: string;
  label: string;
  hint: string;
} {
  const name = (role ?? "").trim() || "Datadog Read Only Role";
  switch (name) {
    case "Datadog Read Only Role":
      return {
        tier: "read_only",
        label: "Read-only",
        hint: "Reads monitors, dashboards and logs, changes nothing.",
      };
    case "Datadog Standard Role":
      return {
        tier: "review",
        label: "Standard",
        hint: "Reads everything and writes monitors, dashboards and notebooks.",
      };
    case "Datadog Admin Role":
      return {
        tier: "full_access",
        label: "Admin",
        hint: "Everything a teammate could do, including billing and users.",
      };
    default:
      return {
        tier: "",
        label: name,
        hint: "A role this organization defined, so what it grants is set at Datadog.",
      };
  }
}

/** The keys an agent's own Datadog and GitLab credential is read from in its `mcp_env` (`CredentialKeys`). */
export const MCP_CREDENTIALS: Record<string, { env: string; keys: string[] }> = {
  datadog: { env: "datadog", keys: ["DD_APP_KEY", "DATADOG_APP_KEY", "app_key"] },
  gitlab: {
    env: "gitlab",
    keys: ["GITLAB_TOKEN", "GITLAB_PERSONAL_ACCESS_TOKEN", "Private-Token", "Authorization"],
  },
};

type McpEnv = Record<string, Record<string, string>> | undefined;

/** The first credential an agent holds under `env`, and where (`setupapi.seatCredential`). */
export function mcpCredential(
  env: McpEnv,
  envs: string[],
  keys: string[],
): { stored: string; where: string } {
  for (const name of envs) {
    const block = env?.[name];
    if (!block || Object.keys(block).length === 0) continue;
    for (const key of keys) {
      const value = (block[key] ?? "").trim();
      if (value) return { stored: value, where: `mcp_env.${name}.${key}` };
    }
  }
  return { stored: "", where: "" };
}

/** Which Atlassian product an agent's credential is read for. */
export type AtlassianProduct = "jira" | "confluence" | "any";

/** An agent's Atlassian token and where it was found (`atlassian.CredentialAt`). */
export function atlassianCredential(
  product: AtlassianProduct,
  env: McpEnv,
): { token: string; where: string } {
  const servers =
    product === "jira"
      ? ["jira", "atlassian"]
      : product === "confluence"
        ? ["confluence", "atlassian"]
        : ["atlassian", "jira", "confluence"];
  const tokenKeys = (shared: boolean): string[] => {
    if (product === "any") {
      return [
        "JIRA_API_TOKEN",
        "CONFLUENCE_API_TOKEN",
        "ATLASSIAN_API_TOKEN",
        "JIRA_PERSONAL_TOKEN",
        "CONFLUENCE_PERSONAL_TOKEN",
        "JIRA_TOKEN",
        "CONFLUENCE_TOKEN",
        "Authorization",
      ];
    }
    const own =
      product === "jira"
        ? ["JIRA_API_TOKEN", "JIRA_PERSONAL_TOKEN", "JIRA_TOKEN"]
        : ["CONFLUENCE_API_TOKEN", "CONFLUENCE_PERSONAL_TOKEN", "CONFLUENCE_TOKEN"];
    const sibling = product === "jira" ? ["CONFLUENCE_API_TOKEN"] : ["JIRA_API_TOKEN"];
    return [...own, "ATLASSIAN_API_TOKEN", "Authorization", ...(shared ? sibling : [])];
  };
  for (const server of servers) {
    const block = env?.[server];
    if (!block || Object.keys(block).length === 0) continue;
    for (const key of tokenKeys(server === "atlassian")) {
      const raw = (block[key] ?? "").trim();
      if (!raw) continue;
      return { token: atlassianToken(raw), where: `${server}.${key}` };
    }
  }
  return { token: "", where: "" };
}

/** The token a header-shaped value carries (`atlassian.credentialOfHeader`). */
function atlassianToken(raw: string): string {
  const scheme = (name: string): string | null =>
    raw.length > name.length &&
    raw.slice(0, name.length).toLowerCase() === name.toLowerCase() &&
    raw[name.length] === " "
      ? raw.slice(name.length + 1).trim()
      : null;
  const bearer = scheme("Bearer");
  if (bearer !== null) return bearer;
  const basic = scheme("Basic");
  if (basic === null) return raw;
  try {
    const payload = atob(basic);
    const at = payload.indexOf(":");
    return at < 0 ? raw : payload.slice(at + 1);
  } catch {
    return raw;
  }
}

/** The email an agent's Atlassian credential is paired with (`atlassian.SeatEmail`). */
export function atlassianSeatEmail(env: McpEnv): string {
  for (const server of ["atlassian", "jira", "confluence"]) {
    const block = env?.[server];
    if (!block || Object.keys(block).length === 0) continue;
    for (const key of [
      "JIRA_USERNAME",
      "JIRA_EMAIL",
      "ATLASSIAN_EMAIL",
      "CONFLUENCE_USERNAME",
      "CONFLUENCE_EMAIL",
    ]) {
      const value = (block[key] ?? "").trim();
      if (value) return value;
    }
  }
  return "";
}
