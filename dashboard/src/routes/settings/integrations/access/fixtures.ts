/**
 * Per-seat access answers as the engine sends them, for the Agents tab's
 * suites: Acme's Datadog (SRE on the Datadog Admin Role, its tool live,
 * the rest off), its GitHub (every agent on, with the app permissions its
 * old tier registered), its GitLab (Dev a Developer in a group that lists no
 * projects), its Cloudflare (SRE able to purge, the rest off), its Expo (QA's
 * robot a Developer, the rest off), its DigitalOcean (SRE's token at Operate,
 * the rest off), its Scaleway (SRE operating the production project, PM off),
 * its AWS (SRE's role on Operate, the rest off) and its Google Cloud (SRE an
 * Operator on production, the rest off), built from the fields a case names.
 *
 * Times are relative to the moment the fixture is built, so "1d ago" reads
 * the same on every run.
 */

import type {
  AccessAnswer,
  AccessBox,
  AccessIdentity,
  AccessIdentityEntry,
  AccessIdentityGuide,
  AccessIdentityMode,
  AccessIdentityStatus,
  AccessLaneKey,
  AccessOptions,
  AccessSeat,
  AccessSeatLane,
} from "~/contract/access.ts";
import { lanesOf, type HandoffStep, type Lane } from "./logic.ts";

const MIN = 60_000;
const DAY = 24 * 60 * MIN;

export const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

const DATADOG_OPTIONS: AccessOptions = {
  lane: "datadog",
  tool_server: "datadog",
  live: true,
  rotatable: true,
  levels: [
    { id: "r-ro", name: "Datadog Read Only Role" },
    { id: "r-std", name: "Datadog Standard Role" },
    { id: "r-admin", name: "Datadog Admin Role", administers: true },
    { id: "r-sre", name: "SRE Responder", custom: true },
  ],
};

const GITHUB_OPTIONS: AccessOptions = {
  lane: "github",
  tool_server: "github",
  live: false,
  permissions: [
    { key: "contents", name: "Contents", administers_on_write: true },
    { key: "issues", name: "Issues" },
    { key: "pull_requests", name: "Pull requests" },
    { key: "checks", name: "Checks" },
    { key: "actions", name: "Actions" },
    { key: "deployments", name: "Deployments" },
    { key: "workflows", name: "Workflows", write_only: true, administers_on_write: true },
  ],
  scoped: true,
  scope_live: true,
  scope: ["acme/backend", "acme/frontend", "acme/mobile-app"],
};

const MATTERMOST_OPTIONS: AccessOptions = {
  lane: "mattermost",
  live: false,
  levels: [
    { id: "system_user", name: "Member" },
    { id: "system_user system_post_all_public", name: "Member who posts in any public channel" },
  ],
  scoped: true,
  scope_live: true,
  scope: ["town-square", "incidents", "releases"],
};

/** A GitLab group whose connection lists no projects, so any project in it may be typed. */
const GITLAB_OPTIONS: AccessOptions = {
  lane: "gitlab",
  tool_server: "gitlab",
  live: true,
  rotatable: true,
  levels: [
    { id: "20", name: "Reporter" },
    { id: "30", name: "Developer" },
    { id: "40", name: "Maintainer", administers: true },
  ],
  scoped: true,
  scope_live: true,
  scope_open: true,
};

/**
 * Cloudflare's four fixed levels, each a set of permission groups the engine
 * resolves, with the engine's notes; Secure waits on groups the provisioner
 * token does not hold.
 */
const CLOUDFLARE_OPTIONS: AccessOptions = {
  lane: "cloudflare",
  tool_server: "cloudflare",
  live: false,
  rotatable: true,
  levels: [
    {
      id: "read",
      name: "Read",
      note: "Reads zones, zone settings, DNS, cache rules, transform rules, firewall and WAF rules, IP lists, SSL and analytics.",
    },
    {
      id: "operate",
      name: "Operate",
      note: "Everything Read does, plus purging the cache and editing cache rules.",
    },
    {
      id: "configure",
      name: "Configure",
      note: "Everything Operate does, plus editing transform rules, compression rules and every zone setting, the SSL mode and security level included.",
    },
    {
      id: "secure",
      name: "Secure",
      administers: true,
      note: "Everything Configure does, plus editing the zones' WAF rules and the account's IP lists. It can leave access behind that outlives the agent's token: an allow or skip rule, or an address added to an IP list.",
      unavailable:
        "The Secure level is unavailable until the provisioner token holds Zone WAF: Edit and Account Filter Lists: Edit",
    },
  ],
};

/** Expo's four levels, each a set of Expo permissions named for the Expo role that holds them. */
const EXPO_TOKENS_PAGE = "https://expo.dev/accounts/acme/settings/access-tokens";

/** Scaleway's two projects, by the IDs a policy names, and the names a person reads. */
export const SCW_PROD = "11111111-1111-4111-8111-111111111111";
export const SCW_STAGING = "22222222-2222-4222-8222-222222222222";

/** Scaleway's three fixed levels, each a set of Scaleway permission sets the engine grants. */
const SCALEWAY_OPTIONS: AccessOptions = {
  lane: "scaleway",
  tool_server: "scaleway",
  live: false,
  rotatable: true,
  levels: [
    {
      id: "read",
      name: "Read",
      note: "Reads servers, storage, networks, DNS and databases in its projects, plus the audit trail across the organization.",
    },
    {
      id: "operate",
      name: "Operate",
      note: "Also restarts servers and changes firewalls, IPs, load balancers and private networks in its projects.",
    },
    {
      id: "administer",
      name: "Administer",
      administers: true,
      note: "Also creates, rebuilds and deletes servers, disks, databases, Kubernetes clusters and registry images, and can leave access behind in them: SSH keys and startup scripts on servers, database users, kubeconfigs with the cluster's admin token, and public images. When you take this level away, rotate what it could reach.",
    },
  ],
  scoped: true,
  scope_live: true,
  scope_required: true,
  scope: [SCW_PROD, SCW_STAGING],
  scope_names: { [SCW_PROD]: "acme-prod", [SCW_STAGING]: "acme-staging" },
};

const EXPO_OPTIONS: AccessOptions = {
  lane: "expo",
  live: false,
  rotatable: true,
  releasable: true,
  action_url: EXPO_TOKENS_PAGE,
  levels: [
    { id: "viewer", name: "Viewer", note: "View all projects" },
    {
      id: "runner",
      name: "Runner",
      note: "View all projects and perform low-risk actions on them",
    },
    { id: "developer", name: "Developer" },
    { id: "release_manager", name: "Release Manager" },
  ],
};

/**
 * DigitalOcean's three levels over a few of its scope picker's sections, each
 * level holding the one before it. With `refused` and `unneeded` they cover
 * every scope of those sections, as the engine's lists cover the whole picker.
 */
const DO_READ = [
  "account:read",
  "droplet:read",
  "firewall:read",
  "kubernetes:read",
  "project:read",
  "snapshot:read",
];
const DO_OPERATE = [
  ...DO_READ,
  "droplet:create",
  "droplet:update",
  "droplet:delete",
  "firewall:create",
  "firewall:update",
  "firewall:delete",
  "kubernetes:create",
  "kubernetes:update",
  "kubernetes:delete",
  "project:assign_resource",
  "snapshot:delete",
];
export const DO_TOKENS_PAGE = "https://cloud.digitalocean.com/account/api/tokens";

const DIGITALOCEAN_OPTIONS: AccessOptions = {
  lane: "digitalocean",
  tool_server: "digitalocean",
  live: false,
  rotatable: true,
  action_url: DO_TOKENS_PAGE,
  vendor_account: "acme",
  levels: [
    { id: "read", name: "Read", grants: DO_READ },
    { id: "operate", name: "Operate", grants: DO_OPERATE },
    {
      id: "administer",
      name: "Administer",
      administers: true,
      note: "Can download cluster credentials, read database passwords and open consoles on Droplets and apps.",
      grants: [...DO_OPERATE, "droplet:admin", "kubernetes:access_cluster"],
    },
  ],
  refused: ["billing:read", "security:read", "security:create", "ssh_key:create"],
  unneeded: ["genai:read", "genai:create", "project:create", "project:update", "project:delete"],
};

/** AWS's three fixed levels, each Crewlet's own customer managed policy, attached additively. */
const AWS_OPTIONS: AccessOptions = {
  lane: "aws",
  tool_server: "aws",
  live: false,
  levels: [
    {
      id: "read",
      name: "Read",
      note: "Reads resource settings and the contents of CloudWatch Logs. It reads no stored objects, items, parameter values or secrets.",
    },
    {
      id: "operate",
      name: "Operate",
      note: "Read, plus starting, stopping and scaling instances and services, invalidating caches, and alarms.",
    },
    {
      id: "administer",
      name: "Administer",
      administers: true,
      note: "Operate, plus the named services whole. It can create access that outlives the agent's session: instance key pairs, user data, database master passwords, bucket and key policies, function URLs, security group openings and snapshot sharing. Shell access, commands and new code work only on resources you tag crewlet:agent-access = administer.",
    },
  ],
};

/** Google Cloud's three fixed levels, each a set of Google's predefined roles, over the projects set up for Crewlet. */
const GOOGLE_CLOUD_OPTIONS: AccessOptions = {
  lane: "google_cloud",
  tool_server: "google-cloud-monitoring",
  live: false,
  levels: [
    {
      id: "viewer",
      name: "Viewer",
      note: "Browser, Cloud SQL Viewer, Compute Viewer, Kubernetes Engine Viewer, DNS Reader, Logs Viewer, MCP Tool User, Monitoring Viewer, Cloud Run Viewer and Storage Bucket Viewer on each chosen project.",
    },
    {
      id: "operator",
      name: "Operator",
      administers: true,
      note: "Viewer's roles, plus Monitoring Editor, Cloud Run Jobs Executor, Storage Object User and Crewlet Operator (restart and scale only).",
    },
    {
      id: "administer",
      name: "Administer",
      administers: true,
      note: "Operator's roles, plus Cloud SQL Admin, Compute Instance Admin (v1), Compute Network Admin, Compute Security Admin, Kubernetes Engine Cluster Admin, DNS Administrator and Storage Admin. It can create access that outlives the agent's token: SSH keys or startup scripts on VMs, database users and certificates, and bucket or DNS zone policies.",
    },
  ],
  scoped: true,
  scope: ["acme-prod", "acme-staging"],
  scope_names: { "acme-prod": "Acme Production", "acme-staging": "Acme Staging" },
  scope_live: true,
  scope_required: true,
};

const laneOf = (options: AccessOptions): Lane =>
  lanesOf({ kind: options.lane, revision: "", syncing: false, options: [options], seats: [] })[0]!;

/** Confluence on a site without space roles: space permissions by operation, a set per seat. */
const CONFLUENCE_OPTIONS: AccessOptions = {
  lane: "confluence",
  live: false,
  multiple: true,
  required_level: "read:space",
  levels: [
    {
      id: "",
      name: "Product access only",
      note: "The account has Confluence and no space role or permission of its own: the site's and each space's permissions decide what it sees.",
    },
    { id: "read:space", name: "View" },
    { id: "create:page", name: "Add pages" },
    { id: "create:blogpost", name: "Add blog posts" },
    { id: "create:comment", name: "Add comments" },
    { id: "administer:space", name: "Admin", administers: true },
  ],
  scoped: true,
  scope_live: true,
  scope: ["ENG", "OPS"],
};

/** Jira's project roles, read live; a seat may also hold Jira with no role. */
const JIRA_OPTIONS: AccessOptions = {
  lane: "jira",
  live: true,
  levels: [
    {
      id: "",
      name: "Product access only",
      note: "The account has Jira and no project role: each project's permission scheme decides what it sees.",
    },
    { id: "10002", name: "Viewer" },
    { id: "10003", name: "Member" },
    { id: "10004", name: "Administrator", administers: true },
  ],
  scoped: true,
  scope_live: true,
  scope: ["ENG", "OPS"],
};

export const DATADOG_LANE = laneOf(DATADOG_OPTIONS);
export const JIRA_LANE = laneOf(JIRA_OPTIONS);
export const CONFLUENCE_LANE = laneOf(CONFLUENCE_OPTIONS);
export const GITHUB_LANE = laneOf(GITHUB_OPTIONS);
export const GITLAB_LANE = laneOf(GITLAB_OPTIONS);
export const MATTERMOST_LANE = laneOf(MATTERMOST_OPTIONS);
export const CLOUDFLARE_LANE = laneOf(CLOUDFLARE_OPTIONS);
export const EXPO_LANE = laneOf(EXPO_OPTIONS);
export const DIGITALOCEAN_LANE = laneOf(DIGITALOCEAN_OPTIONS);
export const SCALEWAY_LANE = laneOf(SCALEWAY_OPTIONS);
export const GOOGLE_CLOUD_LANE = laneOf(GOOGLE_CLOUD_OPTIONS);

/** A lane that is on and in sync, holding what `over` says. */
export function onLane(over: Partial<AccessSeatLane> & { variable: string }): AccessSeatLane {
  const { variable, ...rest } = over;
  return {
    desired: { enabled: true },
    observed: {
      exists: true,
      enabled: true,
      credential: { variable, minted_at: ago(DAY) },
      created_at: ago(DAY),
    },
    observed_at: ago(MIN),
    sync: { at: ago(DAY), outcome: "ok", steps: [] },
    status: "in_sync",
    status_label: "In sync",
    plan: [],
    tool: "live",
    ...rest,
  };
}

/** A lane that is off and has no account. */
export function offLane(level?: string): AccessSeatLane {
  return {
    desired: { enabled: false, ...(level ? { level } : {}) },
    observed: { exists: false, enabled: false },
    status: "off",
    status_label: "Off",
    plan: [],
    tool: "off",
  };
}

export const seat = (handle: string, name: string, lanes: AccessSeat["lanes"]): AccessSeat => ({
  handle,
  name,
  lanes,
});

export function datadogAnswer(sre?: Partial<AccessSeatLane>): AccessAnswer {
  return {
    kind: "datadog",
    revision: "rev-7",
    syncing: false,
    last_sync_at: ago(4 * MIN),
    options: [DATADOG_OPTIONS],
    seats: [
      seat("pm", "PM", { datadog: offLane("r-ro") }),
      seat("qa", "QA", { datadog: offLane("r-ro") }),
      seat("sre", "SRE", {
        datadog: onLane({
          variable: "DATADOG_APP_KEY_SRE",
          desired: { enabled: true, level: "r-admin" },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet-sre@agents.crewlet.invalid",
            level: "r-admin",
            credential: { variable: "DATADOG_APP_KEY_SRE", minted_at: ago(DAY) },
          },
          ...sre,
        }),
      }),
    ],
  };
}

const REVIEW = {
  contents: "read",
  issues: "write",
  pull_requests: "write",
  checks: "read",
  actions: "read",
  deployments: "read",
} as const;

export function githubAnswer(): AccessAnswer {
  return {
    kind: "github",
    revision: "rev-7",
    syncing: false,
    options: [GITHUB_OPTIONS],
    seats: [
      seat("qa", "QA", {
        github: onLane({
          variable: "GITHUB_APP_KEY_QA",
          desired: { enabled: true, permissions: { ...REVIEW }, scope: ["acme/backend"] },
          observed: {
            exists: true,
            enabled: true,
            account: "acme-qa[bot]",
            permissions: { ...REVIEW },
            ceiling: { ...REVIEW },
            scope: ["acme/backend"],
            credential: { variable: "GITHUB_APP_KEY_QA", minted_at: ago(12 * DAY) },
          },
        }),
      }),
    ],
  };
}

/** GitLab with Dev a Developer on one project, in a group that lists none. */
export function gitlabAnswer(): AccessAnswer {
  return {
    kind: "gitlab",
    revision: "rev-7",
    syncing: false,
    options: [GITLAB_OPTIONS],
    seats: [
      seat("dev", "Dev", {
        gitlab: onLane({
          variable: "GITLAB_TOKEN_DEV",
          desired: { enabled: true, level: "30", scope: ["acme/backend"] },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet-acme-dev",
            level: "30",
            scope: ["acme/backend"],
            credential: { variable: "GITLAB_TOKEN_DEV", minted_at: ago(DAY) },
          },
        }),
      }),
    ],
  };
}

/**
 * App Store Connect with Growth operating one app, on a team whose apps the
 * engine listed with an agent's own key: each by name with its bundle ID, and
 * an Apple ID it does not list still typed.
 */
export function appStoreConnectAnswer(): AccessAnswer {
  return {
    kind: "app_store_connect",
    revision: "rev-7",
    syncing: false,
    options: [
      {
        lane: "app_store_connect",
        live: false,
        levels: [
          { id: "read", name: "View" },
          { id: "operate", name: "Manage", administers: true },
          { id: "administer", name: "Administer", administers: true },
        ],
        scoped: true,
        scope_required: true,
        scope_live: true,
        scope_open: true,
        scope: ["6763222842", "1234567890"],
        scope_names: { "6763222842": "Field Notes", "1234567890": "Trail Maps" },
        scope_details: { "6763222842": "com.example.notes", "1234567890": "com.example.maps" },
        scope_typed: "^[0-9]{1,20}$",
      },
    ],
    seats: [
      seat("growth", "Growth", {
        app_store_connect: {
          ...onLane({ variable: "" }),
          desired: { enabled: true, level: "operate", scope: ["6763222842"] },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet/growth",
            level: "operate",
            scope: ["6763222842"],
          },
        },
      }),
    ],
  };
}

/** Apple's sheet for Growth's own key at Manage, as the engine words it: no company key to fall back to. */
export const ASC_GUIDE: AccessIdentityGuide = {
  vendor: "Apple",
  noun: "key",
  records: "its submissions",
  why: "Apple then records its submissions under its own key, and you can revoke it alone.",
  name: "crewlet-growth",
  name_rule: "Lowercase letters, digits and hyphens, at most 30.",
  name_what: "Team key name",
  role: "App Manager",
  never: "Admin",
  steps: [
    {
      text: [
        { text: "Open " },
        {
          ui: ["Users and Access", "Integrations", "Team Keys"],
          url: "https://appstoreconnect.apple.com/access/integrations/api",
        },
        { text: "." },
      ],
    },
    {
      text: [
        { text: "Press " },
        { ui: ["Generate API Key"] },
        { text: ", or " },
        { ui: ["+"] },
        { text: " if the team already has keys." },
      ],
    },
    {
      text: [{ text: "Name:" }],
      copy: {
        label: "key's name",
        value: "crewlet-growth",
        rule: "Lowercase letters, digits and hyphens, at most 30.",
      },
    },
    {
      text: [
        { ui: ["Access"] },
        { text: ": " },
        { ui: ["App Manager"] },
        { text: ", or " },
        { ui: ["Customer Support"] },
        { text: " for an agent that answers reviews. Never " },
        { ui: ["Admin"] },
        { text: "." },
      ],
    },
    { text: [{ text: "Press " }, { ui: ["Generate"] }, { text: "." }] },
    {
      text: [
        { text: "On the new key's row, press " },
        { ui: ["Download API Key"] },
        { text: ". Apple offers the file once." },
      ],
    },
  ],
  note: [
    {
      text: "Only the Account Holder or an Admin can make team keys. A key reaches every app of the team; Crewlet keeps the agent to its own.",
    },
  ],
  back: {
    kind: "file",
    accept: ".p8",
    hint: [{ text: "The file Apple offered: `AuthKey_<Key ID>.p8`" }],
    label: "Key ID",
    detail: {
      label: "Vendor number",
      hint: "Only for a Sales key: digits, on Payments and Financial Reports.",
    },
  },
  checks: [
    { id: "sign", label: "Sign in with the key and the team's issuer" },
    { id: "alive", label: "Make a read every role may make" },
    { id: "role", label: "Read its role from seven reads" },
    { id: "apps", label: "Find the agent's apps in the key's team" },
  ],
};

const IDENTITY_LABEL: Record<AccessIdentityStatus, string> = {
  company: "Company's",
  none: "Not set up",
  preparing: "Getting ready",
  ready: "Ready",
  unchecked: "Role not checked",
  wrong_role: "Wrong role",
  wrong_team: "Wrong team",
  refused: "Refused",
  replacing: "Replacing",
};

/** The box's actions, none offered: each identity below turns on its own. */
const NO_ACTIONS = { set_up: false, replace: false, use_company: false };

/** The setting a seat on its own identity holds, which the route writes before its check. */
const OWN_MODE: AccessIdentityMode = "own";

/** The states an App Store Connect identity takes: its own key or none, never the company's. */
export type AscIdentityStatus = Exclude<AccessIdentityStatus, "company">;

/**
 * Growth's identity at Apple in one state, as the engine answers it. The lane
 * is own-only, as Expo's is: no company key signs for it, so nothing offers
 * the company's and an agent with no working key makes no call.
 */
export function ascIdentity(status: AscIdentityStatus): AccessIdentity {
  const base = {
    ...NO_ACTIONS,
    offered: true,
    status,
    status_label: IDENTITY_LABEL[status],
    needs: "App Manager",
    guide: ASC_GUIDE,
  };
  const needsYou = "Needs you: set up Growth's own key so Apple records its actions.";
  switch (status) {
    case "none":
      return { ...base, mode: OWN_MODE, holds: "own", said: needsYou, set_up: true };
    case "wrong_role":
      return {
        ...base,
        mode: "own",
        holds: "own",
        role: "Marketing",
        said: needsYou,
        set_up: true,
        problem: {
          title: "Wrong role",
          body: "Key 7KQ4M2ZX9P holds the Marketing role, and this agent's level needs App Manager.",
        },
      };
    case "wrong_team":
      return {
        ...base,
        mode: "own",
        holds: "own",
        team: "other",
        said: needsYou,
        set_up: true,
        problem: {
          title: "Wrong team",
          body: "Apple accepts key 7KQ4M2ZX9P with none of the company's teams.",
        },
      };
    case "refused":
      return {
        ...base,
        mode: "own",
        holds: "none",
        name: "crewlet-growth",
        id: "7KQ4M2ZX9P",
        said: "Refused: Apple no longer accepts Growth's own key, so its calls stop until it has a working one.",
        set_up: true,
        problem: { title: "Refused", body: "Apple refuses key 7KQ4M2ZX9P: it was revoked." },
      };
    case "replacing":
      return {
        ...base,
        mode: "own",
        holds: "own",
        name: "crewlet-growth",
        id: "3HN8RT6YQ1",
        role: "App Manager",
        team: "main",
        said: "Apple sees Growth's own key, crewlet-growth.",
        replace: true,
        retiring: [{ id: "7KQ4M2ZX9P", name: "crewlet-growth" }],
        problem: {
          title: "Revoke the old key at Apple",
          body: "crewlet-growth is no longer used. Revoke it at Apple; Crewlet clears this once Apple refuses it.",
        },
      };
    default:
      return {
        ...base,
        mode: "own",
        holds: "own",
        name: "crewlet-growth",
        id: "7KQ4M2ZX9P",
        role: "App Manager",
        team: "main",
        said: "Apple sees Growth's own key, crewlet-growth.",
        replace: true,
      };
  }
}

/** App Store Connect with Growth's identity in one state, its row's status folded in as the engine folds it. */
export function appStoreConnectIdentityAnswer(status: AscIdentityStatus): AccessAnswer {
  const answer = appStoreConnectAnswer();
  const lane = answer.seats[0]!.lanes.app_store_connect!;
  lane.identity = ascIdentity(status);
  if (status === "refused") {
    lane.status = "refused";
    lane.status_label = "Refused";
  } else if (status !== "ready" && status !== "replacing") {
    lane.status = "needs_you";
    lane.status_label = "Needs you";
  }
  return answer;
}

/** Apple Ads, which offers no identity per agent: the company's is the finished state. */
export function appleAdsAnswer(): AccessAnswer {
  return {
    kind: "apple_ads",
    revision: "rev-7",
    syncing: false,
    options: [{ lane: "apple_ads", live: false, levels: [{ id: "read", name: "View" }] }],
    seats: [
      seat("growth", "Growth", {
        apple_ads: {
          ...onLane({ variable: "" }),
          desired: { enabled: true, level: "read" },
          observed: { exists: true, enabled: true, account: "crewlet/growth", level: "read" },
          identity: {
            ...NO_ACTIONS,
            offered: false,
            mode: "company",
            holds: "company",
            status: "company",
            status_label: "Company's",
            said: "Apple Ads sees the company's API user for every agent.",
          },
        },
      }),
    ],
  };
}

/**
 * Expo with PM's robot to make, its identity on the one pattern: offered, its
 * own by construction, the robot's token the thing brought back.
 */
export function expoIdentityAnswer(): AccessAnswer {
  const answer = expoWaitingAnswer("create");
  const pm = answer.seats[0]!.lanes.expo!;
  pm.hand_in = { name: "crewlet-pm" };
  pm.identity = {
    ...NO_ACTIONS,
    offered: true,
    mode: "own",
    holds: "own",
    status: "none",
    status_label: "Not set up",
    said: "Needs you: set up PM's own robot so Expo records its actions.",
    set_up: true,
    needs: "Runner",
    guide: {
      vendor: "Expo",
      noun: "robot",
      records: "its builds",
      why: "Expo then records its builds under its own robot, and you can remove it alone.",
      name: "crewlet-pm",
      name_rule: "Lowercase letters, digits and hyphens, at most 30.",
      name_what: "Robot name",
      role: "Runner",
      steps: [
        {
          text: [
            { text: "Open " },
            {
              ui: ["Access tokens", "Robots"],
              url: "https://expo.dev/accounts/acme/settings/access-tokens",
            },
            { text: ", then press " },
            { ui: ["Add robot"] },
            { text: "." },
          ],
        },
        {
          text: [{ text: "Name:" }],
          copy: {
            label: "robot's name",
            value: "crewlet-pm",
            rule: "Lowercase letters, digits and hyphens, at most 30.",
          },
        },
        { text: [{ ui: ["Role"] }, { text: ": " }, { ui: ["Runner"] }, { text: "." }] },
        { text: [{ text: "Create a token for the robot and copy it." }] },
      ],
      back: { kind: "token", label: "Robot token", hint: [{ text: "Expo shows the token once." }] },
      checks: [
        { id: "robot", label: "Read the robot the token belongs to" },
        { id: "role", label: "Read its role on the account" },
      ],
    },
  };
  return answer;
}

/** Meta's sheet for Growth's own system user at Operate, as the engine words it. */
function metaGuide(canMake: boolean): AccessIdentityGuide {
  const rule = "Lowercase letters, digits and hyphens, at most 30.";
  return {
    vendor: "Meta",
    noun: "system user",
    records: "its changes",
    why: "Meta's activity history then names the agent, and its token can be cut off alone.",
    company: "the company's system user",
    name: "crewlet-growth",
    name_rule: rule,
    name_what: "System user name",
    role: "Manage campaigns and View performance",
    never: "Admin",
    steps: [
      {
        text: [
          { text: "Open " },
          {
            ui: ["Business settings", "Users", "System users"],
            url: "https://business.facebook.com/settings/system-users",
          },
          { text: "." },
        ],
      },
      { text: [{ text: "Press " }, { ui: ["Add"] }, { text: "." }] },
      {
        text: [{ text: "Name:" }],
        copy: { label: "system user's name", value: "crewlet-growth", rule },
      },
      {
        text: [
          { ui: ["Role"] },
          { text: ": " },
          { ui: ["Employee"] },
          { text: ". Press " },
          { ui: ["Create system user"] },
          { text: "." },
        ],
      },
      {
        text: [
          { text: "Press " },
          { ui: ["Assign assets"] },
          { text: ', choose "act_111" and turn on ' },
          { ui: ["Manage campaigns"] },
          { text: " and " },
          { ui: ["View performance"] },
          { text: "." },
        ],
      },
      {
        text: [
          { text: "Press " },
          { ui: ["Generate token"] },
          { text: ". App: 987654321012345. Expiry: 60 days. Tick " },
          { ui: ["ads_management"] },
          { text: " and " },
          { ui: ["ads_read"] },
          { text: "." },
        ],
      },
      { text: [{ text: "Copy the token. Meta shows it once." }] },
    ],
    note: [
      {
        text: "Meta allows a business 10 system users at Full access and 1 at Limited, shared with every integration there.",
      },
    ],
    back: {
      kind: "token",
      label: "Token",
      hint: [{ text: "Crewlet checks it with Meta and seals it." }],
    },
    make: {
      label: "Crewlet can make it for you",
      does: [
        { text: "With the business's admin system user, Crewlet makes crewlet-growth an " },
        { ui: ["Employee"] },
        { text: ' system user, gives it "act_111" at ' },
        { ui: ["Manage campaigns"] },
        { text: " and " },
        { ui: ["View performance"] },
        { text: ", and mints a 60-day token it renews." },
      ],
      ...(canMake
        ? {}
        : {
            unavailable:
              "Paste the Admin system user's token and the business portfolio ID on the Settings tab, and Crewlet makes it for you.",
          }),
      reads: [
        { id: "make", label: "Make crewlet-growth, an Employee system user" },
        { id: "assign", label: 'Give it "act_111": Manage campaigns and View performance' },
        { id: "mint", label: 'Mint its token: "ads_management" and "ads_read"' },
      ],
    },
    checks: [
      { id: "token", label: "Read the token: the company's app, its permissions" },
      { id: "user", label: "Read its system user: an Employee of the business" },
      {
        id: "accounts",
        label: 'Read its ad accounts: Manage campaigns and View performance on "act_111"',
      },
    ],
  };
}

/**
 * Meta Ads with Growth at Operate on one ad account: on the company's system
 * user, past Meta's system user cap, on its own and refused (which offers the
 * company's for now), or on its own, ready.
 */
export function metaAdsIdentityAnswer(
  state: "company" | "blocked" | "refused" | "ready",
  canMake = true,
): AccessAnswer {
  const guide = metaGuide(canMake);
  const needsYou =
    "Needs you: set up Growth's own system user so Meta records its changes. Until then Meta sees the company's system user.";
  const identity: AccessIdentity =
    state === "refused"
      ? {
          ...NO_ACTIONS,
          offered: true,
          mode: "own",
          holds: "none",
          status: "refused",
          status_label: "Refused",
          name: "crewlet-growth",
          id: "100200300",
          needs: guide.role,
          said: "Refused: Meta no longer accepts Growth's own system user, so its calls stop until it has a working one.",
          set_up: true,
          use_company: true,
          problem: { title: "Refused", body: "Meta refuses the token: it was revoked." },
          guide,
        }
      : state === "ready"
        ? {
            ...NO_ACTIONS,
            offered: true,
            mode: "own",
            holds: "own",
            status: "ready",
            status_label: "Ready",
            name: "crewlet-growth",
            id: "100200300",
            role: "Manage campaigns and View performance",
            needs: guide.role,
            team: "business 5550001",
            said: "Meta sees Growth's own system user, crewlet-growth.",
            replace: true,
            guide,
          }
        : {
            ...NO_ACTIONS,
            offered: true,
            mode: state === "blocked" ? "own" : "company",
            holds: "company",
            status: state === "blocked" ? "none" : "company",
            status_label: state === "blocked" ? "Not set up" : "Company's",
            needs: guide.role,
            said:
              state === "blocked"
                ? "Meta allows this business 10 system users and all 10 are used, so Growth stays on the company's system user. Meta sees the company's system user."
                : needsYou,
            ...(state === "blocked"
              ? {
                  blocked:
                    "Meta allows this business 10 system users and all 10 are used, so Growth stays on the company's system user.",
                }
              : { set_up: true }),
            guide,
          };
  return {
    kind: "meta_ads",
    revision: "rev-7",
    syncing: false,
    options: [
      {
        lane: "meta_ads",
        live: false,
        levels: [
          { id: "read", name: "Read" },
          { id: "operate", name: "Operate", administers: true },
          { id: "administer", name: "Administer", administers: true },
        ],
        scoped: true,
        scope_required: true,
        scope_open: true,
        scope: ["act_111"],
      },
    ],
    seats: [
      seat("growth", "Growth", {
        meta_ads: {
          ...onLane({ variable: "" }),
          desired: { enabled: true, level: "operate", scope: ["act_111"] },
          observed: {
            exists: true,
            enabled: true,
            account: state === "ready" ? "crewlet-growth" : "crewlet/growth",
            level: "operate",
            scope: ["act_111"],
          },
          ...(state === "ready"
            ? {}
            : state === "refused"
              ? { status: "refused", status_label: "Refused" }
              : { status: "needs_you", status_label: "Needs you" }),
          identity,
        },
      }),
    ],
  };
}

/** Mattermost with one bot on and in no channel yet. */
export function mattermostAnswer(): AccessAnswer {
  return {
    kind: "mattermost",
    revision: "rev-7",
    syncing: false,
    options: [MATTERMOST_OPTIONS],
    seats: [
      seat("pm", "PM", {
        mattermost: onLane({
          variable: "MATTERMOST_TOKEN_PM",
          desired: { enabled: true, level: "system_user", scope: [] },
        }),
      }),
    ],
  };
}

/** Atlassian's Confluence lane of sets: PM on with View and Add pages, QA off with product access only. */
export function confluenceAnswer(): AccessAnswer {
  const held = "create:page read:space";
  return {
    kind: "atlassian",
    revision: "rev-7",
    syncing: false,
    options: [CONFLUENCE_OPTIONS],
    seats: [
      seat("pm", "PM", {
        confluence: onLane({
          variable: "ATLASSIAN_TOKEN_PM",
          desired: { enabled: true, level: held, scope: ["ENG"] },
          observed: {
            exists: true,
            enabled: true,
            level: held,
            scope: ["ENG"],
            credential: { variable: "ATLASSIAN_TOKEN_PM", minted_at: ago(DAY) },
          },
        }),
      }),
      seat("qa", "QA", { confluence: offLane() }),
    ],
  };
}

/** Atlassian's Jira lane: PM on with product access only, QA a Member on ENG. */
export function jiraAnswer(): AccessAnswer {
  return {
    kind: "atlassian",
    revision: "rev-7",
    syncing: false,
    options: [JIRA_OPTIONS],
    seats: [
      seat("pm", "PM", {
        jira: onLane({ variable: "ATLASSIAN_TOKEN_PM", desired: { enabled: true, level: "" } }),
      }),
      seat("qa", "QA", {
        jira: onLane({
          variable: "ATLASSIAN_TOKEN_QA",
          desired: { enabled: true, level: "10003", scope: ["ENG"] },
        }),
      }),
    ],
  };
}

/** Cloudflare with SRE's token on Operate and its tool live, PM off at the default level. */
export function cloudflareAnswer(): AccessAnswer {
  return {
    kind: "cloudflare",
    revision: "rev-7",
    syncing: false,
    last_sync_at: ago(4 * MIN),
    options: [CLOUDFLARE_OPTIONS],
    seats: [
      seat("pm", "PM", { cloudflare: offLane("read") }),
      seat("sre", "SRE", {
        cloudflare: onLane({
          variable: "CLOUDFLARE_API_TOKEN_SRE",
          desired: { enabled: true, level: "operate" },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet-acme-sre",
            level: "operate",
            credential: { variable: "CLOUDFLARE_API_TOKEN_SRE", minted_at: ago(DAY) },
          },
        }),
      }),
    ],
  };
}

/**
 * Cloudflare whose catalogue offers grants beside a level: Origin Rules read
 * and edit on its one zone, and DNS Write, which the provisioner does not
 * hold; SRE holds Origin Rules: Edit on example.com for good.
 */
export function cloudflareGrantsAnswer(): AccessAnswer {
  const answer = cloudflareAnswer();
  answer.options = [
    {
      ...CLOUDFLARE_OPTIONS,
      grants: [
        { id: "Origin Read", name: "Origin Rules: Read", group: "Zone", scope_kind: "zone" },
        {
          id: "Origin Write",
          name: "Origin Rules: Edit",
          group: "Zone",
          scope_kind: "zone",
          write: true,
        },
        {
          id: "DNS Write",
          name: "DNS Write",
          group: "Zone",
          scope_kind: "zone",
          write: true,
          unavailable: "The provisioner token does not hold DNS Write",
          missing: ["DNS Write"],
        },
      ],
      grant_scope: ["example.com"],
      grant_whole: "every zone the company's agents reach",
      grant_account: "the account",
    },
  ];
  const sre = answer.seats.find((s) => s.handle === "sre")!.lanes.cloudflare!;
  sre.desired = {
    ...sre.desired,
    grants: [
      {
        id: "gaaaaaaaaaa",
        permission: "Origin Write",
        scope: ["example.com"],
        granted_by: "Ada",
        granted_at: ago(DAY),
        reason: "an origin rule for the new backend",
      },
    ],
  };
  return answer;
}

/**
 * Scaleway whose catalogue offers grants beside a level: Kubernetes full
 * access on projects, support tickets on the Organization, and a bucket's
 * settings for one change of at most four hours; SRE holds Kubernetes full
 * access on its own projects for good.
 */
export function scalewayGrantsAnswer(): AccessAnswer {
  const answer = scalewayAnswer();
  answer.options = [
    {
      ...SCALEWAY_OPTIONS,
      grants: [
        {
          id: "KubernetesFullAccess",
          name: "KubernetesFullAccess",
          group: "Kubernetes",
          scope_kind: "project",
          write: true,
          in_level: "administer",
        },
        {
          id: "SupportTicketsReadOnly",
          name: "SupportTicketsReadOnly",
          group: "Support",
          scope_kind: "account",
        },
        {
          id: "ObjectStorageBucketsWrite",
          name: "ObjectStorageBucketsWrite",
          group: "Storage",
          scope_kind: "project",
          write: true,
          note: "It changes a bucket's ACL and visibility, and also changes their lifecycle rules, which can expire and delete objects.",
          max_hours: 4,
        },
      ],
      grant_scope: [SCW_PROD, SCW_STAGING],
      grant_whole: "the agent's own projects",
      grant_account: "the Organization",
    },
  ];
  const sre = answer.seats.find((s) => s.handle === "sre")!.lanes.scaleway!;
  sre.desired = {
    ...sre.desired,
    grants: [
      {
        id: "gbbbbbbbbbb",
        permission: "KubernetesFullAccess",
        granted_by: "Ada",
        granted_at: ago(DAY),
        reason: "upgrade the cluster's node pool",
      },
    ],
  };
  return answer;
}

/** The token Crewlet sets in an on seat's box for the EAS CLI, as the engine sends it. */
const expoBox = (handle: string): AccessBox => ({
  env: [{ name: "EXPO_TOKEN", value: `\${EXPO_TOKEN_${handle}}`, tool: true }],
});

/**
 * Expo with QA's robot on Developer, its token in its box so its tool is live,
 * and PM off at the default level.
 */
export function expoAnswer(qa?: Partial<AccessSeatLane>): AccessAnswer {
  return {
    kind: "expo",
    revision: "rev-7",
    syncing: false,
    last_sync_at: ago(4 * MIN),
    options: [EXPO_OPTIONS],
    seats: [
      seat("pm", "PM", { expo: offLane("viewer") }),
      seat("qa", "QA", {
        expo: onLane({
          variable: "EXPO_TOKEN_QA",
          box: expoBox("QA"),
          desired: { enabled: true, level: "developer" },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet-acme-qa",
            level: "developer",
            credential: { variable: "EXPO_TOKEN_QA", minted_at: ago(DAY) },
          },
          ...qa,
        }),
      }),
    ],
  };
}

const PM_ROBOT = "crewlet-acme-pm";

/** The engine's sentence for each wait the Set up sheet opens on, for PM's robot at Runner. */
const EXPO_WAITS: Record<HandoffStep, string> = {
  create: `Add a robot named ${PM_ROBOT} with the Runner role at Expo`,
  mint: `Create a new access token for robot ${PM_ROBOT}, then paste it here`,
  rerole:
    "This robot is a Developer on acme, and you chose Runner. Change its role with Edit robot at Expo, or choose Developer here",
};

/**
 * Expo with PM turned on at Runner and waiting on a person for its robot at
 * one step, QA as in [expoAnswer]. The robot's name is `observed.account`,
 * the name to give it while it does not exist yet; `named: false` leaves it
 * off, as an engine that sends none would.
 */
export function expoWaitingAnswer(
  step: HandoffStep = "create",
  { renew = step !== "create", named = true }: { renew?: boolean; named?: boolean } = {},
): AccessAnswer {
  const answer = expoAnswer();
  const robot = {
    ...(named ? { account: PM_ROBOT } : {}),
    ...(step === "create" ? {} : { account_id: "robot-pm" }),
  };
  answer.seats[0] = seat("pm", "PM", {
    expo: {
      desired: { enabled: true, level: "runner" },
      observed:
        step === "create"
          ? { exists: false, enabled: false, ...robot }
          : {
              exists: step === "rerole",
              enabled: step === "rerole",
              ...robot,
              ...(step === "rerole" ? { level: "developer" } : {}),
              ...(renew
                ? {
                    credential: { variable: "EXPO_TOKEN_PM", minted_at: ago(DAY) },
                    credential_gone: step === "mint",
                  }
                : {}),
            },
      sync: {
        at: ago(MIN),
        outcome: "waiting",
        steps: [],
        error: {
          step,
          said: EXPO_WAITS[step],
          remedy: "",
          actor: "you",
          action_url: EXPO_TOKENS_PAGE,
        },
      },
      status: "needs_you",
      status_label: "Needs you",
      plan: [],
      tool: "off",
    },
  });
  return answer;
}

/** QA's robot in sync and holding old tokens besides the sealed one, which Expo still accepts. */
export function expoRetiredAnswer(labels: string[] = ["9f3c1a…"]): AccessAnswer {
  const answer = expoAnswer();
  const qa = answer.seats[1]!.lanes.expo!;
  qa.observed.retired = labels.map((label) => ({ label, since: "2026-11-01T09:30:00Z" }));
  qa.sync = {
    at: ago(MIN),
    outcome: "waiting",
    steps: [],
    error: {
      step: "revoke",
      said: `Delete robot crewlet-acme-qa's other tokens ${labels.join(", ")} at Expo`,
      remedy: "",
      actor: "you",
      action_url: EXPO_TOKENS_PAGE,
    },
  };
  qa.status = "needs_you";
  return answer;
}

/**
 * A LANE NO VENDOR IS, for the shared mechanics no vendor sends yet (a
 * credential issued at use, a tool's reason, the box's files). Its key is one
 * this build has no words for, so it reads in the generic words, and no case
 * asserts a shape some vendor's engine does not send.
 */
const NEUTRAL_OPTIONS: AccessOptions = {
  lane: "neutral" as AccessLaneKey,
  live: false,
  levels: [{ id: "use", name: "Use" }],
};

export const NEUTRAL_LANE = laneOf(NEUTRAL_OPTIONS);
export const AWS_LANE = laneOf(AWS_OPTIONS);

/** The lines Crewlet puts in a neutral seat's box: a variable naming a token file, and the file. */
export const neutralBox = (handle: string): AccessBox => ({
  env: [{ name: "VENDOR_TOKEN_FILE", value: "/home/crewlet/.config/crewlet/token" }],
  files: [
    {
      path: ".config/crewlet/token",
      value: `\${VENDOR_TOKEN_${handle}}`,
      rewritten: true,
    },
  ],
});

/**
 * The neutral lane with SRE on, its token issued at use rather than sealed and
 * a token file in its box, and PM off. `sre` replaces what a case is about.
 */
export function neutralAnswer(sre?: Partial<AccessSeatLane>): AccessAnswer {
  return {
    kind: "neutral",
    revision: "rev-7",
    syncing: false,
    last_sync_at: ago(4 * MIN),
    options: [NEUTRAL_OPTIONS],
    seats: [
      seat("pm", "PM", { [NEUTRAL_OPTIONS.lane]: offLane("use") }),
      seat("sre", "SRE", {
        [NEUTRAL_OPTIONS.lane]: onLane({
          variable: "VENDOR_TOKEN_SRE",
          desired: { enabled: true, level: "use" },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet-sre",
            level: "use",
            credential: { variable: "VENDOR_TOKEN_SRE", source: "issued" },
            created_at: ago(DAY),
          },
          box: neutralBox("SRE"),
          ...sre,
        }),
      }),
    ],
  };
}

/** The token Crewlet sets in an on seat's box for doctl, as the engine sends it. */
const doBox = (handle: string): AccessBox => ({
  env: [
    { name: "DIGITALOCEAN_ACCESS_TOKEN", value: `\${DIGITALOCEAN_TOKEN_${handle}}`, tool: true },
  ],
});

/** Whose token every DigitalOcean seat holds: the person who made it, on the company's team. */
const DO_OWNER = "Alex on acme";

/**
 * DigitalOcean with SRE's token at Operate, live through its box, and PM off
 * at Read. `observed.account` is whose token it is, never the token's name,
 * which only `hand_in` carries.
 */
export function digitaloceanAnswer(sre?: Partial<AccessSeatLane>): AccessAnswer {
  return {
    kind: "digitalocean",
    revision: "rev-7",
    syncing: false,
    last_sync_at: ago(4 * MIN),
    options: [DIGITALOCEAN_OPTIONS],
    seats: [
      seat("pm", "PM", {
        digitalocean: {
          ...offLane("read"),
          hand_in: { name: "crewlet-acme-pm-read" },
        },
      }),
      seat("sre", "SRE", {
        digitalocean: onLane({
          variable: "DIGITALOCEAN_TOKEN_SRE",
          box: doBox("SRE"),
          hand_in: { name: "crewlet-acme-sre-operate" },
          desired: { enabled: true, level: "operate" },
          observed: {
            exists: true,
            enabled: true,
            account: DO_OWNER,
            account_id: "hmac-sre",
            level: "operate",
            credential: { variable: "DIGITALOCEAN_TOKEN_SRE", minted_at: ago(DAY) },
          },
          ...sre,
        }),
      }),
    ],
  };
}

/**
 * DigitalOcean with PM turned on at Operate and waiting on a person for its
 * token: its first at `create`, a new one at `mint` (the old one refused, or
 * a level changed). `named: false` leaves `hand_in` off, as an engine that
 * sends none would.
 */
export function digitaloceanWaitingAnswer(
  step: "create" | "mint" = "create",
  { named = true }: { named?: boolean } = {},
): AccessAnswer {
  const answer = digitaloceanAnswer();
  const renew = step === "mint";
  answer.seats[0] = seat("pm", "PM", {
    digitalocean: {
      desired: { enabled: true, level: "operate" },
      observed: renew
        ? {
            exists: false,
            enabled: false,
            account: DO_OWNER,
            credential: { variable: "DIGITALOCEAN_TOKEN_PM", minted_at: ago(DAY) },
            credential_gone: true,
          }
        : { exists: false, enabled: false },
      sync: {
        at: ago(MIN),
        outcome: "waiting",
        steps: [],
        error: {
          step,
          said: renew
            ? "Create a new token crewlet-acme-pm-operate with the Operate scopes, then paste it here"
            : "Create a personal access token named crewlet-acme-pm-operate with Custom Scopes for the Operate level, then paste it here",
          remedy: "",
          actor: "you",
        },
      },
      status: "needs_you",
      status_label: "Needs you",
      plan: [],
      tool: "off",
      box: doBox("PM"),
      ...(named ? { hand_in: { name: "crewlet-acme-pm-operate" } } : {}),
    },
  });
  return answer;
}

/** What Crewlet puts in an on Scaleway seat's box: the key scw reads, then the settings beside it. */
const scalewayBox = (handle: string): AccessBox => ({
  env: [
    { name: "SCW_ACCESS_KEY", value: `\${SCALEWAY_ACCESS_KEY_${handle}}`, tool: true },
    { name: "SCW_SECRET_KEY", value: `\${SCALEWAY_SECRET_KEY_${handle}}`, tool: true },
    {
      name: "SCW_DEFAULT_ORGANIZATION_ID",
      value: "33333333-3333-4333-8333-333333333333",
    },
    { name: "SCW_DEFAULT_PROJECT_ID", value: SCW_PROD },
    { name: "SCW_DISABLE_CHECK_VERSION", value: "true" },
  ],
});

/** Scaleway with SRE operating the production project and its tool live, PM off at the default level. */
export function scalewayAnswer(sre?: Partial<AccessSeatLane>): AccessAnswer {
  return {
    kind: "scaleway",
    revision: "rev-7",
    syncing: false,
    last_sync_at: ago(4 * MIN),
    options: [SCALEWAY_OPTIONS],
    seats: [
      seat("pm", "PM", {
        scaleway: offLane("read"),
      }),
      seat("sre", "SRE", {
        scaleway: onLane({
          variable: "SCALEWAY_SECRET_KEY_SRE",
          box: scalewayBox("SRE"),
          desired: { enabled: true, level: "operate", scope: [SCW_PROD] },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet-acme-sre",
            level: "operate",
            scope: [SCW_PROD],
            credential: {
              variable: "SCALEWAY_SECRET_KEY_SRE",
              others: ["SCALEWAY_ACCESS_KEY_SRE"],
              minted_at: ago(DAY),
            },
          },
          ...sre,
        }),
      }),
    ],
  };
}

/**
 * The files Crewlet puts in an AWS seat's box, as the engine sends them: the
 * issued credentials it rewrites, and the CLI profile that reads them. No
 * environment line, since an AWS_* variable would reach a harness's sign-in.
 */
const awsBox = (handle: string): AccessBox => ({
  env: [],
  files: [
    {
      path: ".aws/config",
      value:
        "[profile crewlet]\nregion = us-east-2\ncredential_process = sh -c 'cat \"$HOME/.aws/crewlet-credentials.json\"'\n",
      rewritten: false,
    },
    {
      path: ".aws/crewlet-credentials.json",
      value: `\${AWS_CREDENTIALS_${handle}}`,
      rewritten: true,
    },
  ],
});

/**
 * AWS with SRE's IAM role on Operate, its credentials issued at use and the
 * AWS MCP Server bound, and PM off at the default level.
 */
export function awsAnswer(sre?: Partial<AccessSeatLane>): AccessAnswer {
  return {
    kind: "aws",
    revision: "rev-3",
    syncing: false,
    last_sync_at: ago(4 * MIN),
    options: [AWS_OPTIONS],
    seats: [
      seat("pm", "PM", { aws: offLane("read") }),
      seat("sre", "SRE", {
        aws: onLane({
          variable: "AWS_CREDENTIALS_SRE",
          desired: { enabled: true, level: "operate" },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet/acme/seat/crewlet-sre-7c2e",
            account_id: "AROA4EXAMPLE7C2EROLE",
            level: "operate",
            credential: { variable: "AWS_CREDENTIALS_SRE", source: "issued" },
            created_at: ago(DAY),
          },
          box: awsBox("SRE"),
          ...sre,
        }),
      }),
    ],
  };
}

/** The lines Crewlet puts in a Google Cloud seat's box for gcloud, as the engine sends them. */
const googleCloudBox = (handle: string): AccessBox => ({
  env: [
    {
      name: "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE",
      value: "~/.config/crewlet/gcloud-token",
    },
    { name: "CLOUDSDK_CORE_PROJECT", value: "acme-web" },
  ],
  files: [
    {
      path: ".config/crewlet/gcloud-token",
      value: `\${GOOGLE_CLOUD_ACCESS_TOKEN_${handle}}`,
      rewritten: true,
    },
  ],
});

/**
 * Google Cloud with SRE's service account an Operator on production, its token
 * issued at use and its MCP server bound, and PM off at the default level.
 * `options` replaces the lane's offer, for the cases about the project list.
 */
export function googleCloudAnswer(
  sre?: Partial<AccessSeatLane>,
  options?: Partial<AccessOptions>,
): AccessAnswer {
  return {
    kind: "google_cloud",
    revision: "rev-7",
    syncing: false,
    last_sync_at: ago(4 * MIN),
    options: [{ ...GOOGLE_CLOUD_OPTIONS, ...options }],
    seats: [
      seat("pm", "PM", { google_cloud: offLane("viewer") }),
      seat("sre", "SRE", {
        google_cloud: onLane({
          variable: "GOOGLE_CLOUD_ACCESS_TOKEN_SRE",
          desired: { enabled: true, level: "operator", scope: ["acme-prod"] },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet-sre-3f9a@acme-crewlet-id.iam.gserviceaccount.com",
            account_id: "104829377165514428863",
            level: "operator",
            scope: ["acme-prod"],
            credential: { variable: "GOOGLE_CLOUD_ACCESS_TOKEN_SRE", source: "issued" },
            created_at: ago(DAY),
          },
          box: googleCloudBox("SRE"),
          ...sre,
        }),
      }),
    ],
  };
}

/** Google Analytics' property ID shape, as the engine sends it (config.GoogleAnalyticsPropertyPattern). */
export const GA_PROPERTY_SHAPE = "^[0-9]{1,20}$";

/** The properties Crewlet's company reader sees: two accounts, by name. */
export const GA_PROPERTIES: Record<string, { name: string; account: string }> = {
  "412345678": { name: "Field Notes web", account: "Example Apps" },
  "412345679": { name: "Field Notes app", account: "Example Apps" },
  "398765432": { name: "Trail Maps", account: "Trail Co" },
};

const GA_ADMIN = "https://analytics.google.com/analytics/web/";

/** Google Analytics with the reader's listing, or none (`listed` empty), and Marketer's seat. */
export function googleAnalyticsAnswer(
  listed: string[] = Object.keys(GA_PROPERTIES),
  unlisted?: string,
): AccessAnswer {
  const names: Record<string, string> = {};
  const groups: Record<string, string> = {};
  for (const id of listed) {
    names[id] = GA_PROPERTIES[id]!.name;
    groups[id] = GA_PROPERTIES[id]!.account;
  }
  return {
    kind: "google_analytics",
    revision: "rev-3",
    syncing: false,
    options: [
      {
        lane: "google_analytics",
        live: false,
        levels: [
          { id: "read", name: "Read" },
          { id: "operate:marketer", name: "Marketer", administers: true },
          { id: "operate", name: "Operate", administers: true },
          { id: "administer", name: "Administer", administers: true },
        ],
        scoped: true,
        scope_required: true,
        scope_live: unlisted === undefined,
        scope_open: true,
        scope_typed: GA_PROPERTY_SHAPE,
        ...(listed.length > 0 ? { scope: listed, scope_names: names, scope_groups: groups } : {}),
        ...(unlisted ? { scope_unlisted: unlisted } : {}),
      },
    ],
    seats: [seat("marketer", "Digital Marketer", { google_analytics: offLane("read") })],
  };
}

/** Google Analytics' sheet for Digital Marketer's identity, its entries as the last check found them. */
export function gaGuide(entries: AccessIdentityEntry[], readyAt?: string): AccessIdentityGuide {
  return {
    vendor: "Google Analytics",
    noun: "Google identity",
    records: "its changes under its own email in the change history",
    why: "Google Analytics lets Digital Marketer in as its own Google identity and records its changes under its own email in the change history.",
    name: GA_IDENTITY,
    name_rule: "Crewlet made it; it cannot be renamed.",
    name_what: "Google identity",
    role: "Editor",
    steps: [
      {
        text: [
          { text: "Open each property below with its button, or go to " },
          { ui: ["Google Analytics", "Admin", "Property access management"], url: GA_ADMIN },
          { text: " with the property chosen." },
        ],
      },
      { text: [{ text: "Press " }, { ui: ["+", "Add users"] }, { text: "." }] },
      {
        text: [{ ui: ["Email address"] }, { text: ":" }],
        copy: { label: "identity's email", value: GA_IDENTITY },
      },
      {
        text: [
          { text: "Untick " },
          { ui: ["Notify new users by email"] },
          { text: ": nobody reads the identity's mail." },
        ],
      },
      {
        text: [
          { ui: ["Role"] },
          { text: ": " },
          { ui: ["Editor"] },
          { text: ". Press " },
          { ui: ["Add"] },
          { text: "." },
        ],
      },
    ],
    entries,
    wider: {
      label: "Add it once to the account instead",
      does: [
        { text: "Under " },
        { ui: ["Account access management"] },
        {
          text: ", one add covers every property of the account, now and later. That is broader access: the identity reaches every property there, though Crewlet still holds the agent to its own.",
        },
      ],
      links: [
        {
          label: "Example Apps",
          url: `${GA_ADMIN}#/a301p412345678/admin/suiteusermanagement/account`,
        },
      ],
    },
    ...(readyAt ? { ready_at: readyAt } : {}),
    note: [{ text: "Only an " }, { ui: ["Administrator"] }, { text: " can add users." }],
    back: {
      kind: "nothing",
      text: [{ text: "Nothing to bring back: Crewlet checks Google Analytics as the identity." }],
    },
    make: {
      label: "Add it for me",
      does: [{ text: "With a Google Analytics Administrator's sign-in, Crewlet adds it." }],
      unavailable: "A Google Analytics Administrator signs in on the Settings tab first.",
    },
    checks: [
      { id: "token", label: `Sign in as ${GA_IDENTITY}` },
      { id: "reach", label: "Find each of the agent's properties" },
      { id: "role", label: "Read its role on each property" },
    ],
  };
}

export const GA_IDENTITY = "crewlet-marketer-0a1b2c@example-crewlet-id.iam.gserviceaccount.com";

/** One Analytics property row as the engine lists it in the sheet. */
export function gaEntry(id: string, state: AccessIdentityEntry["state"]): AccessIdentityEntry {
  const p = GA_PROPERTIES[id]!;
  return {
    id,
    name: p.name,
    group: p.account,
    state,
    add_url: `${GA_ADMIN}#/a301p${id}/admin/suiteusermanagement/property`,
    group_url: `${GA_ADMIN}#/a301p${id}/admin/suiteusermanagement/account`,
  };
}

/** Digital Marketer at operate on two properties, its identity in one state, its row folded in as the engine folds it. */
export function googleAnalyticsIdentityAnswer(
  status: "preparing" | "none" | "unchecked" | "ready",
  entries: AccessIdentityEntry[] = [
    gaEntry("412345678", "missing"),
    gaEntry("412345679", "missing"),
  ],
): AccessAnswer {
  const answer = googleAnalyticsAnswer();
  const readyAt = status === "preparing" ? new Date(Date.now() + 70_000).toISOString() : undefined;
  const said: Record<typeof status, string> = {
    preparing:
      "Getting ready: Crewlet made Digital Marketer's Google identity a moment ago; Google Analytics can find it in about a minute, then add it there.",
    none: "Needs you: add Digital Marketer's own Google identity in Google Analytics. Until then Google Analytics refuses its calls.",
    unchecked:
      "Digital Marketer reaches every one of its properties, but its role on Field Notes web (412345678) and Field Notes app (412345679) is not checked: Google Analytics shows it only to a Google Analytics Administrator. Make sure it holds Editor there in Google Analytics. With a Google Analytics Administrator's sign-in on the Settings tab, Crewlet checks it on every pass.",
    ready: `Google Analytics sees Digital Marketer's own Google identity, ${GA_IDENTITY}.`,
  };
  const row: Record<typeof status, [AccessSeatLane["status"], string]> = {
    preparing: ["preparing", "Getting ready"],
    none: ["needs_you", "Needs you"],
    unchecked: ["role_unchecked", "Role not checked"],
    ready: ["in_sync", "In sync"],
  };
  const works = status === "unchecked" || status === "ready";
  answer.seats[0]!.lanes.google_analytics = {
    ...onLane({ variable: "" }),
    desired: { enabled: true, level: "operate", scope: ["412345678", "412345679"] },
    observed: {
      exists: true,
      enabled: true,
      account: GA_IDENTITY,
      level: "operate",
      scope: ["412345678", "412345679"],
    },
    status: row[status][0],
    status_label: row[status][1],
    identity: {
      ...NO_ACTIONS,
      offered: true,
      mode: OWN_MODE,
      holds: "own",
      status,
      status_label: IDENTITY_LABEL[status],
      needs: "Editor",
      said: said[status],
      set_up: !works,
      ...(works ? { name: GA_IDENTITY, id: GA_IDENTITY } : {}),
      guide: gaGuide(entries, readyAt),
    },
  };
  return answer;
}
