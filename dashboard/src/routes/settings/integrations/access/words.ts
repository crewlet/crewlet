import type { DocSection } from "../docs.ts";

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
   * and where the vendor has none to offer, what the picker says instead and
   * the docs section that says how to make one there.
   */
  scope?: {
    noun: string;
    singular: string;
    empty: string;
    emptyOn?: string;
    none?: string;
    noneLink?: DocSection & { label: string };
  };
  /** The tool a seat uses its credential through, in a few words. */
  tool: string;
  /**
   * How long a credential Crewlet issues at use lasts there, in a few words
   * ("an hour"), where the lane's credential is issued rather than sealed.
   */
  issued?: string;
  /**
   * What the details add beside the settings Crewlet puts in a seat's box,
   * where the CLI they serve has to come with the box image.
   */
  boxNote?: string;
  /** Where a person approves a seat's install at the vendor through a fresh link, its button label. */
  install?: string;
  /**
   * Where only a person can make the agent's credential at the vendor, and
   * pastes it here (Expo's robots, DigitalOcean's tokens): the vendor's nouns
   * the Set up sheet uses.
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

/** The vendor's own words for what a person makes for an agent there, and the controls they press. */
export interface HandoffWords {
  /** Why the agent needs one of its own, under the sheet's title. */
  why: string;
  /** What never to pick at the vendor, said under what to pick. */
  never: string;
  /**
   * Where the agent has an account of its own there, which the person makes
   * before its token (Expo's robots). Absent where the token is the agent's
   * whole identity (DigitalOcean's), so the sheet starts at the token.
   */
  account?: HandoffAccountWords;
  /** The token the person makes there and pastes here. */
  token: HandoffTokenWords;
}

/** The vendor's words for an agent's own account there. */
export interface HandoffAccountWords {
  /** What it is called in a sentence: "robot". */
  noun: string;
  /** The same, as a label: "Robot". */
  label: string;
  /** The control that makes one: "Add robot". */
  add: string;
  /** Where its role is changed, in its row's menu: "Edit robot". */
  edit: string;
}

/** The vendor's words for the token a person makes for an agent. */
export interface HandoffTokenWords {
  /** The vendor's verb for making one, in the sheet's step: "Create", "Generate". */
  verb: string;
  /** The control that makes it: on the account's row ("Create token"), or on the vendor's tokens page ("Generate New Token"). */
  mint: string;
  /** Where the vendor renews a token in place, keeping its name and scopes, the control in its row's menu: "Regenerate Token". */
  regenerate?: string;
  /** That the vendor shows its value once, and when to copy it. */
  once: string;
  /** Where it is made on a form of the vendor's (DigitalOcean's), that form's words. */
  form?: HandoffFormWords;
}

/** The fields of a vendor's token form, and the controls of its scope picker. */
export interface HandoffFormWords {
  /** Where to switch to what the company connected (`vendor_account`) before making the token. */
  switchTo: (account: string | undefined) => string;
  /** The field the token's name goes in: "Token name". */
  name: string;
  /** The expiry field, and what to choose in it. */
  expiry: { label: string; advice: string };
  /** The scopes field, and the choice that lets a person tick them: "Custom Scopes". */
  scopes: { label: string; custom: string };
  /** The picker's bulk control, its every-read choice, and the other choices never to use. */
  bulk: { control: string; reads: string; others: string };
  /** The picker's search, which finds a section by its resource. */
  search: string;
  /** A dialog the vendor may raise when the token is made, and the right answer to it. */
  confirm?: string;
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
    boxNote:
      "git and gh read the agent's app token, and the file is kept current for runs longer than an hour; git-auth must be set up in the box",
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
  scaleway: {
    name: "Scaleway",
    access: "Permission sets",
    turnOn:
      "Turning it on creates a Scaleway IAM application for the agent, with a policy holding the chosen permission sets on its projects and its own API key.",
    off: "Crewlet deletes the agent's API key, so it stops working at once, then its policy and IAM application. Turning it back on creates new ones.",
    remove:
      "The agent's application is already deleted at Scaleway, so removing only stops Crewlet tracking it.",
    rotate:
      "Crewlet creates a new API key for the application, seals it, then deletes the old one. Anything still using the old key stops working.",
    scope: {
      noun: "Projects",
      singular: "project",
      empty: "No project",
      emptyOn: "Not in any project yet, so it can reach nothing. Choose its projects.",
      none: "No projects in the organization",
    },
    tool: "the Scaleway MCP server, or scw in its box",
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
      why: "Each agent acts at Expo as its own robot, so its builds and updates are attributable to it.",
      never: "Never choose Admin: agents never hold it.",
      account: { noun: "robot", label: "Robot", add: "Add robot", edit: "Edit robot" },
      token: {
        verb: "Create",
        mint: "Create token",
        once: "Expo shows the token's value once, so copy it before you close that dialog.",
      },
    },
    retired: { one: "An old token", many: "old tokens" },
    release: "Keep the robot at Expo",
  },
  digitalocean: {
    name: "DigitalOcean",
    access: "Token scopes",
    turnOn:
      "Turning it on asks you to create a DigitalOcean token for the agent with the chosen level's scopes and paste it here. Crewlet checks the token and keeps it.",
    off: "Crewlet revokes the agent's token at DigitalOcean, so it stops working at once, and clears its sealed copy. Turning it back on asks you for a new token.",
    remove:
      "Crewlet has already revoked the agent's token at DigitalOcean, so removing only stops Crewlet tracking it.",
    rotate:
      "You regenerate the agent's token at DigitalOcean, or create a new one, and paste it here. Crewlet checks it, seals it and revokes the old one if it still works.",
    tool: "the DigitalOcean MCP server, or doctl in its box",
    handoff: {
      why: "Each agent gets its own token, so you can cut off one agent without touching the others. DigitalOcean records its actions under the person who created the token; Crewlet records which agent took each one.",
      never: "Never choose Full Access or Read Only: Crewlet refuses both.",
      token: {
        verb: "Generate",
        mint: "Generate New Token",
        regenerate: "Regenerate Token",
        once: "DigitalOcean shows the token once, so copy it before you leave the page.",
        form: {
          switchTo: (team) =>
            `In the team menu at the top of the control panel, switch to ${team ?? "the team Crewlet is connected to"}.`,
          name: "Token name",
          expiry: {
            label: "Expiration",
            advice:
              "The longest your policy allows. When it runs out, this agent shows Needs you here and you paste a new token.",
          },
          scopes: { label: "Scopes", custom: "Custom Scopes" },
          bulk: {
            control: "Quick bulk scope select",
            reads: "Select all Reads",
            others: "Select all Creates, Updates or Deletes",
          },
          search: "Search by resource type",
          confirm:
            "If DigitalOcean shows Required scopes when you generate the token, choose Add scopes and continue.",
        },
      },
    },
    retired: { one: "An old token", many: "old tokens" },
  },
  aws: {
    name: "AWS",
    access: "Permissions policies",
    turnOn:
      "Turning it on creates an IAM role for the agent, limited by the Crewlet boundary, and attaches the chosen level's policies. Crewlet signs the agent in as that role for 15 minutes at a time.",
    off: "Crewlet attaches AWS's deny-all policy to the agent's role and detaches its level policies, so it stops within about 2 minutes of saving. The role and its CloudTrail history stay, so turning it back on keeps the same identity.",
    remove:
      "Crewlet deletes the agent's IAM role, which ends its sessions at once. CloudTrail keeps its entries.",
    rotate:
      "AWS issues the agent's credentials for 15 minutes at a time, so there is nothing to rotate.",
    tool: "AWS MCP Server, one per agent",
    issued: "15 minutes",
    boxNote:
      "AWS CLI configuration provided in ~/.aws/config as the profile crewlet, so commands use --profile crewlet; the AWS CLI v2 must be in the box image",
  },
  google_cloud: {
    name: "Google Cloud",
    access: "Roles on its projects",
    turnOn:
      "Turning it on creates a Google Cloud service account for the agent and grants it the chosen level's roles on its projects.",
    off: "Crewlet disables the agent's service account, so it loses Google Cloud at once, and removes its roles on every chosen project. The account and its audit history stay, so turning it back on keeps the same identity.",
    remove: "Crewlet deletes the service account. Cloud Audit Logs keep its entries.",
    rotate: "Google issues the agent's token for an hour at a time, so there is nothing to rotate.",
    scope: {
      noun: "Projects",
      singular: "project",
      empty: "No project",
      none: "No projects are set up for Crewlet yet",
      noneLink: {
        label: "Prepare a project",
        doc: "google-cloud",
        anchor: "prepare-a-project-for-crewlet",
      },
    },
    tool: "Google Cloud MCP servers, one set per agent",
    issued: "an hour",
    boxNote: "gcloud token file provided; the Google Cloud CLI must be in the box image",
  },
  ssh: {
    name: "SSH",
    access: "Level on its servers",
    turnOn:
      "Turning it on lets the agent reach the chosen servers at the chosen level. Crewlet signs a certificate for each session; nothing is created on a server.",
    off: "Crewlet stops signing the agent's certificates at once, and any it already holds expire within 15 minutes.",
    remove: "There is nothing to remove: the agent's certificates expire on their own.",
    rotate:
      "Crewlet signs the agent's certificates for minutes at a time, so there is nothing to rotate.",
    scope: {
      noun: "Servers",
      singular: "server",
      empty: "No server",
      none: "No servers are added yet",
      noneLink: { label: "Add a server", doc: "ssh", anchor: "add-a-server" },
    },
    tool: "ssh_read, ssh_operate and ask_to_operate, built in",
    issued: "15 minutes",
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
