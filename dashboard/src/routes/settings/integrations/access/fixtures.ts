/**
 * Per-seat access answers as the engine sends them, for the Agents tab's
 * suites: GoPhrase's Datadog (SRE on the Datadog Admin Role, its tool live,
 * the rest off) and its GitHub (every agent on, with the app permissions its
 * old tier registered), built from the fields a case names.
 *
 * Times are relative to the moment the fixture is built, so "1d ago" reads
 * the same on every run.
 */

import type { AccessAnswer, AccessOptions, AccessSeat, AccessSeatLane } from "~/contract/access.ts";
import { lanesOf, type Lane } from "./logic.ts";

const MIN = 60_000;
const DAY = 24 * 60 * MIN;

export const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

const DATADOG_OPTIONS: AccessOptions = {
  lane: "datadog",
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
  scope: ["gophrase/backend", "gophrase/frontend", "gophrase/mobile-app"],
};

const MATTERMOST_OPTIONS: AccessOptions = {
  lane: "mattermost",
  live: false,
  levels: [
    { id: "system_user", name: "Member" },
    { id: "system_user system_post_all_public", name: "Member who posts in any public channel" },
  ],
  scope: ["town-square", "incidents", "releases"],
};

const laneOf = (options: AccessOptions): Lane =>
  lanesOf({ kind: options.lane, revision: "", syncing: false, options: [options], seats: [] })[0]!;

/** Confluence on a site without space roles: space permissions by operation, a set per seat. */
const CONFLUENCE_OPTIONS: AccessOptions = {
  lane: "confluence",
  live: false,
  multiple: true,
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
  scope: ["ENG", "OPS"],
};

export const DATADOG_LANE = laneOf(DATADOG_OPTIONS);
export const JIRA_LANE = laneOf(JIRA_OPTIONS);
export const CONFLUENCE_LANE = laneOf(CONFLUENCE_OPTIONS);
export const GITHUB_LANE = laneOf(GITHUB_OPTIONS);
export const MATTERMOST_LANE = laneOf(MATTERMOST_OPTIONS);

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
