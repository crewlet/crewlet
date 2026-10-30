/**
 * Per-seat access answers as the engine sends them, for the Agents tab's
 * suites: GoPhrase's Datadog (SRE on the Datadog Admin Role, its tool live,
 * the rest off) and its GitHub (every agent on, with the app permissions its
 * old tier registered), built from the fields a case names.
 *
 * Times are relative to the moment the fixture is built, so "1d ago" reads
 * the same on every run.
 */

import type { AccessAnswer, AccessLane, AccessSeat, AccessSeatLane } from "~/contract/access.ts";

const MIN = 60_000;
const DAY = 24 * 60 * MIN;

export const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

export const DATADOG_LANE: AccessLane = {
  key: "datadog",
  name: "Datadog",
  levels: [
    { id: "r-ro", name: "Datadog Read Only Role", hint: "Reads monitors, dashboards and logs" },
    { id: "r-std", name: "Datadog Standard Role", hint: "Also writes monitors and dashboards" },
    { id: "r-admin", name: "Datadog Admin Role", hint: "Everything, including users", admin: true },
    { id: "r-sre", name: "SRE Responder", custom: true },
  ],
  levels_source: { live: true, text: "Live from your Datadog organization, custom roles included" },
  rotatable: true,
  tool_server: "datadog",
  console_url: "https://app.datadoghq.com/organization-settings/users",
};

export const GITHUB_LANE: AccessLane = {
  key: "github",
  name: "GitHub",
  levels: [],
  levels_source: { live: false, text: "GitHub's permissions for a GitHub App" },
  permissions: [
    { key: "contents", name: "Contents", admin_write: true },
    { key: "issues", name: "Issues" },
    { key: "pull_requests", name: "Pull requests" },
    { key: "checks", name: "Checks" },
    { key: "actions", name: "Actions" },
    { key: "workflows", name: "Workflows", write_only: true, admin_write: true },
  ],
  scope_options: ["gophrase/backend", "gophrase/frontend", "gophrase/mobile-app"],
  rotatable: false,
  tool_server: "github",
  console_url: "https://github.com/organizations/gophrase/settings/apps",
};

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
    credential_variable: variable,
    ...rest,
  };
}

/** A lane that is off and has no account. */
export function offLane(variable: string, level?: string): AccessSeatLane {
  return {
    desired: { enabled: false, level },
    observed: { exists: false, enabled: false },
    plan: [],
    tool: "off",
    credential_variable: variable,
  };
}

const seat = (handle: string, name: string, lanes: AccessSeat["lanes"]): AccessSeat => ({
  handle,
  name,
  lanes,
});

export function datadogAnswer(sre?: Partial<AccessSeatLane>): AccessAnswer {
  return {
    kind: "datadog",
    revision_id: "rev-7",
    syncing: false,
    last_sync_at: ago(4 * MIN),
    lanes: [DATADOG_LANE],
    seats: [
      seat("pm", "PM", { datadog: offLane("DATADOG_APP_KEY_PM", "r-ro") }),
      seat("qa", "QA", { datadog: offLane("DATADOG_APP_KEY_QA", "r-ro") }),
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
} as const;

export function githubAnswer(): AccessAnswer {
  return {
    kind: "github",
    revision_id: "rev-7",
    syncing: false,
    lanes: [GITHUB_LANE],
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
