/**
 * What the Agents tab says about each lane that the engine's answer does not
 * carry: the product's name, the label the details give the access a seat
 * holds, the sentence a seat that is off reads as, what turning a seat off
 * does there (said in the review, where it is needed), and what a seat's
 * scope is called. Display words only: what a vendor can do (whether a
 * credential rotates) is the engine's catalog, sent on `options`.
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
  /** What rotating a seat's credential does there, said before it runs. */
  rotate: string;
  /**
   * What a seat's scope is: its column heading, a single item, an empty list,
   * where an empty list is literal the hint under a seat that is on with none,
   * and where the vendor has none to offer, what the picker says instead.
   */
  scope?: { noun: string; singular: string; empty: string; emptyOn?: string; none?: string };
  /** The tool a seat uses its credential through, in a few words. */
  tool: string;
  /** Where a person approves a seat's install at the vendor through a fresh link, its button label. */
  install?: string;
  /**
   * Where only a person can make the agent's account at the vendor, and pastes
   * its token here (Expo's robots): the vendor's nouns the Set up sheet uses.
   */
  handoff?: HandoffWords;
  /**
   * What the vendor calls a credential the account still holds besides the
   * sealed one, one of them and several: "An old token", "old tokens".
   */
  retired?: { one: string; many: string };
  /**
   * On a lane that lets a person keep a turned-off agent's account at the
   * vendor (`releasable`), the control that does it: "Keep the robot at Expo".
   */
  release?: string;
}

/** The vendor's own words for the account a person makes for an agent, and the controls they press. */
export interface HandoffWords {
  /** What the agent's account is called at the vendor, in a sentence: "robot". */
  account: string;
  /** The same, as a label: "Robot". */
  accountLabel: string;
  /** The control that makes one: "Add robot". */
  add: string;
  /** The control on its row that makes its token: "Create token". */
  mint: string;
  /** Where its role is changed, in its row's menu: "Edit robot". */
  edit: string;
  /** Why the agent needs one of its own, under the sheet's title. */
  why: string;
  /** The role never to pick, said under the role to pick. */
  never: string;
}

/** What an old credential is called on a lane whose words name none. */
export const RETIRED_GENERIC = { one: "An old credential", many: "old credentials" };

const GENERIC: LaneWords = {
  name: "the vendor",
  access: "Access",
  turnOn: "Turning it on creates its account with the chosen access and its own credential.",
  off: "Crewlet disables the account and revokes its credential. The account and its history stay.",
  remove: "Crewlet removes the account at the vendor.",
  rotate:
    "Crewlet mints a new credential and revokes the old one. Anything still using the old one stops working.",
  tool: "Its tool server, one per agent",
};

export const LANE_WORDS: Record<string, LaneWords> = {
  datadog: {
    name: "Datadog",
    access: "Role at Datadog",
    turnOn: "Turning it on creates a Datadog service account with the chosen role and its own key.",
    off: "Crewlet disables the service account and deletes its application key. Its monitors and dashboards keep their author, and turning it back on mints a new key.",
    remove:
      "Datadog never erases a user, so removing keeps the account disabled and stops Crewlet tracking it.",
    rotate:
      "Crewlet mints a new application key for the service account and deletes the old one. Anything still using the old key stops working.",
    tool: "Datadog MCP server, one per agent",
  },
  github: {
    name: "GitHub",
    access: "Permissions",
    turnOn:
      "Turning it on needs a person to create and install its GitHub App, which Crewlet prepares with the chosen permissions.",
    off: "Crewlet suspends the agent's app installation, so every token it holds stops working at once. The app stays registered, so turning it back on needs no person.",
    remove:
      "Crewlet uninstalls the app and clears its key. GitHub has no API to delete an app registration, so a person deletes it there.",
    rotate: "GitHub issues the app's tokens hourly, so there is nothing to rotate.",
    scope: { noun: "Repositories", singular: "repository", empty: "All installed repositories" },
    tool: "GitHub MCP server, one per agent",
  },
  gitlab: {
    name: "GitLab",
    access: "Role at GitLab",
    turnOn:
      "Turning it on creates a GitLab service account with the chosen role and its own token.",
    off: "Crewlet revokes the service account's tokens. With an instance administrator token it also blocks the account; otherwise it removes the account's memberships. Its commits and comments stay.",
    remove: "Crewlet deletes the account. GitLab keeps its contributions under Ghost User.",
    rotate:
      "Crewlet mints a new token for the service account and revokes the old one. Anything still using the old token stops working.",
    scope: { noun: "Projects", singular: "project", empty: "The whole group" },
    tool: "GitLab MCP server, one per agent",
  },
  jira: {
    name: "Jira",
    access: "Project role",
    turnOn:
      "Turning it on creates an Atlassian service account and adds it to the chosen project role.",
    off: "Crewlet removes the account from its Jira project roles and narrows its API token to Confluence, or revokes the token when Confluence is also off. Atlassian product access is one grant for every agent and stays, since Atlassian has no documented way to remove it.",
    remove:
      "With both products off, Crewlet deletes the service account. Its issues and comments keep its name.",
    rotate:
      "Crewlet mints a new API token for the service account and revokes the old one, for Jira and Confluence alike.",
    scope: { noun: "Projects", singular: "project", empty: "No project" },
    tool: "Crewlet's own Jira tools",
  },
  confluence: {
    name: "Confluence",
    access: "Space role",
    turnOn:
      "Turning it on creates an Atlassian service account and gives it the chosen space role.",
    off: "Crewlet removes the account's Confluence space access and narrows its API token to Jira, or revokes the token when Jira is also off. Atlassian product access is one grant for every agent and stays, since Atlassian has no documented way to remove it.",
    remove:
      "With both products off, Crewlet deletes the service account. Its pages keep their author.",
    rotate:
      "Crewlet mints a new API token for the service account and revokes the old one, for Jira and Confluence alike.",
    scope: { noun: "Spaces", singular: "space", empty: "No space" },
    tool: "Crewlet's own Confluence tools",
  },
  slack: {
    name: "Slack",
    access: "Scopes",
    fixed: "The bot token scopes every agent's app holds",
    turnOn: "Turning it on creates its Slack app, and a person approves the install in Slack.",
    off: "Crewlet revokes the app's bot token, so the agent stops reading and posting. The app stays in your workspace, and turning it back on needs a person to approve the install.",
    remove:
      "Slack has no API to delete an app, so a person deletes it at Slack under Basic Information.",
    rotate:
      "Crewlet revokes the app's bot token. The agent stays off Slack until a person approves the install again.",
    tool: "Slack MCP server, one per agent",
    install: "Install in Slack",
  },
  mattermost: {
    name: "Mattermost",
    access: "Role at Mattermost",
    turnOn: "Turning it on creates a bot account and adds it to the chosen channels.",
    off: "Crewlet revokes the bot's tokens and deactivates it. Its posts stay.",
    remove:
      "Mattermost bots are deactivated, never deleted, so their posts keep their author. Removing stops Crewlet tracking it.",
    rotate: "Crewlet mints a new token for the bot and revokes the old one.",
    scope: {
      noun: "Channels",
      singular: "channel",
      empty: "No channels",
      emptyOn: "In no channel yet, so it cannot read or post anywhere. Choose its channels.",
    },
    tool: "Crewlet's chat transport",
  },
  cloudflare: {
    name: "Cloudflare",
    access: "Token permissions",
    turnOn:
      "Turning it on mints an account API token for the agent with the chosen permissions, limited to the company's zones.",
    off: "Crewlet disables the agent's API token, so it stops working at once, then deletes it. Turning it back on mints a new token.",
    remove:
      "The agent's token is already deleted at Cloudflare, so removing only stops Crewlet tracking it.",
    rotate:
      "Crewlet rolls the token to a new value and seals it. Anything still using the old value stops working.",
    tool: "Cloudflare MCP server, one per agent",
  },
  expo: {
    name: "Expo",
    access: "Role at Expo",
    turnOn:
      "Turning it on asks you to add a robot for the agent at Expo with the chosen role and paste its token here. Crewlet checks the token and keeps it.",
    off: "Crewlet deletes the agent's sealed token at once, so it loses Expo here. Then you delete its robot at Expo, and Crewlet confirms it once the old token stops working.",
    remove: "You delete the agent's robot at Expo, so removing only stops Crewlet tracking it.",
    rotate:
      "You create a new token for the same robot at Expo and paste it here. Crewlet checks it is the same robot and seals it, then asks you to delete the old token.",
    tool: "EAS CLI in its box",
    handoff: {
      account: "robot",
      accountLabel: "Robot",
      add: "Add robot",
      mint: "Create token",
      edit: "Edit robot",
      why: "Each agent acts at Expo as its own robot, so its builds and updates are attributable to it.",
      never: "Never choose Admin: agents never hold it.",
    },
    retired: { one: "An old token", many: "old tokens" },
    release: "Keep the robot at Expo",
  },
};

/** The line a read-only Agents tab shows when the Kubernetes operator manages access. */
export const OPERATOR_MANAGED =
  "Access settings are managed by the operator through each agent's Seat object.";

export function laneWords(lane: string): LaneWords {
  return LANE_WORDS[lane] ?? GENERIC;
}

/** GitHub's own words for each access a permission can hold. */
export const GITHUB_ACCESS_NAMES = {
  none: "No access",
  read: "Read-only",
  write: "Read and write",
} as const;
