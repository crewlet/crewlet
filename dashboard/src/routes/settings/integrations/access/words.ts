/**
 * What the Agents tab says about each lane that the engine's answer does not
 * carry: the product's name, the label the details give the access a seat
 * holds, the sentence a seat that is off reads as, what turning a seat off
 * does there (said in the review, where it is needed), what a seat's scope is
 * called, and whether Crewlet can rotate a seat's credential there.
 *
 * KEYED BY LANE, the integration's own product key, so a vendor's list of
 * levels and names stays the engine's (read live where the vendor has one)
 * and only the sentences live here. A lane this table does not know draws
 * with the generic words, never with nothing.
 */

export interface LaneWords {
  /** The product, as the vendor names it. */
  name: string;
  /** What the details call the access a seat holds there, in the vendor's word. */
  access: string;
  /** The access every seat holds where the vendor offers no choice (Slack's bot token scopes). */
  fixed?: string;
  /** What a seat that is off and has no account reads as, after "Off.". */
  turnOn: string;
  /** What turning a seat off does there, said in the review. */
  off: string;
  /** What removing a seat's account does there, said in the remove dialog. */
  remove: string;
  /**
   * What a seat's scope is: its column heading, a single item, an empty list,
   * and where an empty list is literal, the hint under a seat that is on with
   * none.
   */
  scope?: { noun: string; singular: string; empty: string; emptyOn?: string };
  /** The tool a seat uses its credential through, in a few words. */
  tool: string;
  /** The `mcp_servers` entry a seat's tool is started from, as the warning names it. */
  server: string;
  /** Whether Crewlet can mint a seat a new credential and revoke the old. */
  rotatable: boolean;
  /** Where a person approves a seat's install at the vendor through a fresh link, its button label. */
  install?: string;
}

const GENERIC: LaneWords = {
  name: "the vendor",
  access: "Access",
  turnOn: "Turning it on creates its account with the chosen access and its own credential.",
  off: "Crewlet disables the account and revokes its credential. The account and its history stay.",
  remove: "Crewlet removes the account at the vendor.",
  tool: "Its tool server, one per agent",
  server: "vendor",
  rotatable: false,
};

export const LANE_WORDS: Record<string, LaneWords> = {
  datadog: {
    name: "Datadog",
    access: "Role at Datadog",
    turnOn: "Turning it on creates a Datadog service account with the chosen role and its own key.",
    off: "Crewlet disables the service account and deletes its application key. Its monitors and dashboards keep their author, and turning it back on mints a new key.",
    remove:
      "Datadog never erases a user, so removing keeps the account disabled and stops Crewlet tracking it.",
    tool: "Datadog MCP server, one per agent",
    server: "datadog",
    rotatable: true,
  },
  github: {
    name: "GitHub",
    access: "Permissions",
    turnOn:
      "Turning it on needs a person to create and install its GitHub App, which Crewlet prepares with the chosen permissions.",
    off: "Crewlet suspends the agent's app installation, so every token it holds stops working at once. The app stays registered, so turning it back on needs no person.",
    remove:
      "Crewlet uninstalls the app and clears its key. GitHub has no API to delete an app registration, so a person deletes it there.",
    scope: { noun: "Repositories", singular: "repository", empty: "All installed repositories" },
    tool: "GitHub MCP server, one per agent",
    server: "github",
    rotatable: false,
  },
  gitlab: {
    name: "GitLab",
    access: "Role at GitLab",
    turnOn:
      "Turning it on creates a GitLab service account with the chosen role and its own token.",
    off: "With an instance administrator token, Crewlet blocks the service account. With a group Owner token, it revokes the account's tokens and removes its project memberships instead. Its commits and comments stay.",
    remove: "Crewlet deletes the account. GitLab keeps its contributions under Ghost User.",
    scope: { noun: "Projects", singular: "project", empty: "The whole group" },
    tool: "GitLab MCP server, one per agent",
    server: "gitlab",
    rotatable: true,
  },
  jira: {
    name: "Jira",
    access: "Project role",
    turnOn:
      "Turning it on creates an Atlassian service account and adds it to the chosen project role.",
    off: "Crewlet removes the account from its Jira project roles and mints its API token again without Jira's scopes. With Confluence also off, it revokes the token. The account keeps its Jira product access at Atlassian, which has no API to remove it, so an organization admin removes it there if needed.",
    remove:
      "With both products off, Crewlet deletes the service account. Its issues and comments keep its name.",
    scope: { noun: "Projects", singular: "project", empty: "No project" },
    tool: "Crewlet's own Jira tools",
    server: "jira",
    rotatable: true,
  },
  confluence: {
    name: "Confluence",
    access: "Space role",
    turnOn:
      "Turning it on creates an Atlassian service account and gives it the chosen space role.",
    off: "Crewlet removes the account's Confluence space access and mints its API token again without Confluence's scopes. With Jira also off, it revokes the token. The account keeps its Confluence product access at Atlassian, which has no API to remove it, so an organization admin removes it there if needed.",
    remove:
      "With both products off, Crewlet deletes the service account. Its pages keep their author.",
    scope: { noun: "Spaces", singular: "space", empty: "No space" },
    tool: "Crewlet's own Confluence tools",
    server: "confluence",
    rotatable: true,
  },
  slack: {
    name: "Slack",
    access: "Scopes",
    fixed: "The bot token scopes every agent's app holds",
    turnOn: "Turning it on creates its Slack app, and a person approves the install in Slack.",
    off: "Crewlet revokes the app's bot token, so the agent stops reading and posting. The app stays in your workspace, and turning it back on needs a person to approve the install.",
    remove:
      "Slack has no API to delete an app, so a person deletes it at Slack under Basic Information.",
    tool: "Slack MCP server, one per agent",
    server: "slack",
    rotatable: false,
    install: "Install in Slack",
  },
  mattermost: {
    name: "Mattermost",
    access: "Role at Mattermost",
    turnOn: "Turning it on creates a bot account and adds it to the chosen channels.",
    off: "Crewlet revokes the bot's tokens and deactivates it. Its posts stay.",
    remove:
      "Mattermost bots are deactivated, never deleted, so their posts keep their author. Removing stops Crewlet tracking it.",
    scope: {
      noun: "Channels",
      singular: "channel",
      empty: "No channels",
      emptyOn: "In no channel yet, so it cannot read or post anywhere. Choose its channels.",
    },
    tool: "Crewlet's chat transport",
    server: "mattermost",
    rotatable: true,
  },
};

/** The line a read-only Agents tab shows when the Kubernetes operator manages access. */
export const OPERATOR_MANAGED =
  "Access is managed by the operator through each agent's Seat object, so it is shown here read-only.";

export function laneWords(lane: string): LaneWords {
  return LANE_WORDS[lane] ?? GENERIC;
}

/** GitHub's own words for each access a permission can hold. */
export const GITHUB_ACCESS_NAMES = {
  none: "No access",
  read: "Read-only",
  write: "Read and write",
} as const;
