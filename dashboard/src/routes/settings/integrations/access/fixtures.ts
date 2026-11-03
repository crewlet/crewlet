/**
 * Per-seat access answers as the engine sends them, for the Agents tab's
 * suites: GoPhrase's Datadog (SRE on the Datadog Admin Role, its tool live,
 * the rest off), its GitHub (every agent on, with the app permissions its
 * old tier registered), its GitLab (Dev a Developer in a group that lists no
 * projects), its Cloudflare (SRE able to purge, the rest off), its Expo
 * (QA's robot a Developer, the rest off), its Google Cloud (SRE an Operator
 * on production, the rest off) and its AWS (SRE's role on Operate, the rest
 * off), built from the fields a case names.
 *
 * Times are relative to the moment the fixture is built, so "1d ago" reads
 * the same on every run.
 */

import type {
  AccessAnswer,
  AccessBox,
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
  scope: ["gophrase/backend", "gophrase/frontend", "gophrase/mobile-app"],
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
 * Cloudflare's three fixed levels, each a set of permission groups the engine
 * resolves; Configure waits on groups the provisioner token does not hold.
 */
const CLOUDFLARE_OPTIONS: AccessOptions = {
  lane: "cloudflare",
  tool_server: "cloudflare",
  live: false,
  rotatable: true,
  levels: [
    { id: "read", name: "Read" },
    { id: "operate", name: "Operate" },
    {
      id: "configure",
      name: "Configure",
      unavailable:
        "The Configure level is unavailable until the provisioner token holds Zone Settings: Edit and Response Compression: Edit",
    },
  ],
};

/** Expo's four levels, each a set of Expo permissions named for the Expo role that holds them. */
const EXPO_TOKENS_PAGE = "https://expo.dev/accounts/gophrase/settings/access-tokens";

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
  scope: ["gophrase-prod", "gophrase-staging"],
  scope_names: { "gophrase-prod": "GoPhrase Production", "gophrase-staging": "GoPhrase Staging" },
  scope_live: true,
  scope_required: true,
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
      administers: true,
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
export const GOOGLE_CLOUD_LANE = laneOf(GOOGLE_CLOUD_OPTIONS);
export const AWS_LANE = laneOf(AWS_OPTIONS);

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
    sync: { at: ago(DAY), outcome: "ok", steps: [] },
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
          desired: { enabled: true, permissions: { ...REVIEW }, scope: ["gophrase/backend"] },
          observed: {
            exists: true,
            enabled: true,
            account: "gophrase-qa[bot]",
            permissions: { ...REVIEW },
            ceiling: { ...REVIEW },
            scope: ["gophrase/backend"],
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
          desired: { enabled: true, level: "30", scope: ["gophrase/backend"] },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet-gophrase-dev",
            level: "30",
            scope: ["gophrase/backend"],
            credential: { variable: "GITLAB_TOKEN_DEV", minted_at: ago(DAY) },
          },
        }),
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
            account: "crewlet-gophrase-sre",
            level: "operate",
            credential: { variable: "CLOUDFLARE_API_TOKEN_SRE", minted_at: ago(DAY) },
          },
        }),
      }),
    ],
  };
}

/** The line Expo's EAS CLI needs in a seat's box, as the engine sends it. */
const expoBox = (handle: string): AccessBox => ({
  env: [{ name: "EXPO_TOKEN", value: `\${EXPO_TOKEN_${handle}}`, by: "you" }],
});

/**
 * Expo with QA's robot on Developer, its token named in its sandbox env so its
 * tool is live, and PM off at the default level.
 */
export function expoAnswer(qa?: Partial<AccessSeatLane>): AccessAnswer {
  return {
    kind: "expo",
    revision: "rev-7",
    syncing: false,
    last_sync_at: ago(4 * MIN),
    options: [EXPO_OPTIONS],
    seats: [
      seat("pm", "PM", { expo: { ...offLane("viewer"), box: expoBox("PM") } }),
      seat("qa", "QA", {
        expo: onLane({
          variable: "EXPO_TOKEN_QA",
          box: expoBox("QA"),
          desired: { enabled: true, level: "developer" },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet-gophrase-qa",
            level: "developer",
            credential: { variable: "EXPO_TOKEN_QA", minted_at: ago(DAY) },
          },
          ...qa,
        }),
      }),
    ],
  };
}

const PM_ROBOT = "crewlet-gophrase-pm";

/** The engine's sentence for each wait the Set up sheet opens on, for PM's robot at Runner. */
const EXPO_WAITS: Record<HandoffStep, string> = {
  create: `Add a robot named ${PM_ROBOT} with the Runner role at Expo`,
  mint: `Create a new access token for robot ${PM_ROBOT}, then paste it here`,
  rerole:
    "This robot is a Developer on gophrase, and you chose Runner. Change its role with Edit robot at Expo, or choose Developer here",
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
      said: `Delete robot crewlet-gophrase-qa's other tokens ${labels.join(", ")} at Expo`,
      remedy: "",
      actor: "you",
      action_url: EXPO_TOKENS_PAGE,
    },
  };
  return answer;
}

/** The lines Crewlet puts in a Google Cloud seat's box for gcloud, as the engine sends them. */
const googleCloudBox = (handle: string): AccessBox => ({
  env: [
    {
      name: "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE",
      value: "/home/crewlet/.config/crewlet/gcloud-token",
      by: "crewlet",
    },
    { name: "CLOUDSDK_CORE_PROJECT", value: "gophrase-prod", by: "crewlet" },
  ],
  files: [
    {
      path: ".config/crewlet/gcloud-token",
      value: `\${GOOGLE_CLOUD_ACCESS_TOKEN_${handle}}`,
      rewritten: true,
      by: "crewlet",
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
          desired: { enabled: true, level: "operator", scope: ["gophrase-prod"] },
          observed: {
            exists: true,
            enabled: true,
            account: "crewlet-sre-3f9a@gophrase-crewlet-id.iam.gserviceaccount.com",
            account_id: "104829377165514428863",
            level: "operator",
            scope: ["gophrase-prod"],
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

/**
 * The files Crewlet puts in an AWS seat's box, as the engine sends them: the
 * issued credentials it rewrites, and the CLI profile that reads them. No
 * environment line, since an AWS_* variable would reach a harness's sign-in.
 */
const awsBox = (handle: string): AccessBox => ({
  env: [],
  files: [
    {
      path: ".config/crewlet/aws-credentials.json",
      value: `\${AWS_CREDENTIALS_${handle}}`,
      rewritten: true,
      by: "crewlet",
    },
    {
      path: ".aws/config",
      value:
        "[profile crewlet]\nregion = us-east-2\ncredential_process = cat /home/crewlet/.config/crewlet/aws-credentials.json\n",
      rewritten: false,
      by: "crewlet",
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
            account: "crewlet/gophrase/seat/crewlet-sre-7c2e",
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
