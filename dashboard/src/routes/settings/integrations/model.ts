/**
 * What Company › Integrations knows about the company's integrations, derived once.
 *
 * NOTHING HERE IS NEW STATE. Each value is read off the two answers the page
 * reads (the `integrations` question and `/setup/integrations`) through the
 * helpers in `catalogue.tsx`, which the peek and the Home widget read too,
 * so no surface claims something about a tool another does not. The one judgement made here is
 * the bucket a tool falls in (`Tool.bucket`), which is the engine's roll-up
 * state renamed for a person, and the issues a tool raises (`Tool.issues`),
 * which are the reconcile loop's own findings, each paired with the one
 * control that answers it.
 */

import { useCallback, useEffect, useMemo, useState, type ComponentProps } from "react";
import type { QueryState } from "~/components/common.tsx";
import type { Tone } from "@crewlethq/ui";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useManagedConfig } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";
import { SERVICE_NAMES, chatVendorName, useBackends, type OrgServices } from "~/lib/services.ts";
import { useStableValue } from "~/lib/useStableValue.ts";
import { asSentence } from "~/lib/format.ts";
import type { AgentAvatar } from "~/lib/avatar.ts";
import { pathSegment, rest, RestError } from "~/protocol/index.ts";
import type { IntegrationRow, IntegrationTool, ReconcileFinding } from "~/contract/integrations.ts";
import type { VendorTextPart } from "~/contract/vendortext.ts";
import type { SetupRequirement, SetupSeatState, SetupToolState } from "~/protocol/types.ts";
import {
  CATALOG,
  IN_FLIGHT,
  ROTATE_FINDINGS,
  actionFor,
  advisory,
  credentialFindings,
  disconnectOrder,
  entryState,
  formBlocked,
  heldProducts,
  presentSurfaces,
  rosterOf,
  sectionsFor,
  stuckDisconnecting,
  stuckOn,
  toolsOf,
  total,
  useSetup,
  type Entry,
  type EntryState,
  type TileAction,
} from "./catalogue.tsx";
import { useRecheck } from "~/routes/settings/recheck.ts";
import { workspaceRow, type Workspace } from "~/app/nav.ts";
import { splitFields } from "../SetupForm.tsx";

/**
 * What an agent can do once a tool is connected, and how it gets in.
 *
 * Every line is something the engine's own package for that tool does
 * (its summary on the setup listing, its prompt and its reconcile pass), said
 * for a person choosing whether to connect it.
 */
export interface ToolStory {
  /** Three short lines on what agents do there. */
  does: string[];
  /** How each agent gets its own identity there. */
  identity: string;
  /** What a person does by hand, beyond typing values in, its vendor UI names marked. */
  byHand?: VendorTextPart[];
}

export const STORIES: Record<string, ToolStory> = {
  meta_ads: {
    does: [
      "Read ad accounts, campaigns, ad sets, ads and insights in the ad accounts you choose",
      "Pause, and lower budgets and bids, where you allow it",
      "Ask you before any change that can raise spending, every time",
    ],
    identity:
      "Agents never hold a Meta token. Crewlet keeps one system user's tokens from your own Meta app and makes every call for them.",
    byHand: [
      { text: "In your business portfolio, create a Business app and an " },
      { ui: ["Employee"] },
      { text: " system user, assign it the ad accounts, generate its tokens and paste them here." },
    ],
  },
  apple_ads: {
    does: [
      "Read campaigns, keywords, search terms and performance in the ad accounts you choose",
      "Pause, lower bids and budgets, and add negative keywords where you allow it",
      "Ask you before any change that can raise spending, every time",
    ],
    identity:
      "Agents never hold Apple Ads access. Crewlet keeps the company's API users and makes every call for them.",
    byHand: [
      {
        text: "Invite an API user in Apple Ads, make its key pair with OpenSSL, upload the public key under that user's ",
      },
      { ui: ["Account Settings", "API"] },
      {
        text: ", and paste the private key and the IDs Apple shows here. Apple has no API that creates users or keys.",
      },
    ],
  },
  app_store_connect: {
    does: [
      "Read your apps, listings, reviews, TestFlight builds and sales",
      "Edit drafts and internal testing at once, where you allow operate",
      "Ask you before any reply, submission, price or account change customers or testers see",
    ],
    identity:
      "Each agent's calls are signed with its own team key, which Crewlet keeps sealed. The company holds no key.",
    byHand: [
      { text: "In App Store Connect, " },
      { ui: ["Users and Access", "Integrations"] },
      {
        text: ", the Account Holder requests API access once; then make a team key for each agent there. Apple has no API that creates them.",
      },
    ],
  },
  slack: {
    does: [
      "Answer mentions and direct messages",
      "Reply in the thread they were asked in",
      "Show they are working before they answer",
    ],
    identity: "Each agent posts as its own Slack app.",
    byHand: [{ text: "Create one Slack app per agent from the manifest Crewlet writes." }],
  },
  mattermost: {
    does: [
      "Answer mentions and direct messages",
      "Post in the team's channels",
      "Work with no public address to reach",
    ],
    identity: "Crewlet makes a bot account for each agent.",
  },
  github: {
    does: [
      "Review pull requests and answer comments",
      "Pick up issues they are assigned",
      "Look into failed checks on their own work",
    ],
    identity: "Each agent acts through its own GitHub App.",
    byHand: [{ text: "Create and install one app per agent. GitHub has no API for it." }],
  },
  gitlab: {
    does: [
      "Open and review merge requests",
      "Follow pipelines on their own work",
      "Work in every project of your group",
    ],
    identity: "Crewlet makes a service account for each agent in your group.",
  },
  atlassian: {
    does: [
      "Work Jira issues they are assigned",
      "Read and write Confluence pages",
      "Hear about changes as they happen",
    ],
    identity: "Crewlet makes an Atlassian account for each agent.",
  },
  datadog: {
    does: [
      "Wake the agent a monitor is tagged for",
      "Send untagged alerts to a fallback seat",
      "Give each agent its own Datadog account",
    ],
    identity: "Crewlet makes a service account for each agent.",
  },
  cloudflare: {
    does: [
      "Read zone analytics, cache status and DNS",
      "Check rules and settings across your zones",
      "Purge the cache and edit cache rules, where you allow it",
    ],
    identity: "Crewlet mints a Cloudflare API token for each agent, limited to your zones.",
    byHand: [
      {
        text: "Create one account API token at Cloudflare for Crewlet to mint the agents' tokens from. It needs every permission an agent can be given.",
      },
    ],
  },
  scaleway: {
    does: [
      "Read servers, storage, networks, DNS and databases in the projects you choose",
      "Restart servers and change firewalls, IPs, load balancers and private networks, where you allow it",
      "Use the Scaleway CLI or MCP server as themselves, with their own API key",
    ],
    identity:
      "Crewlet makes a Scaleway IAM application for each agent, with its own policy and API key.",
    byHand: [
      { text: "Create an IAM application for Crewlet with an API key, and a policy giving it " },
      { ui: ["IAMApplicationManager"] },
      { text: ", " },
      { ui: ["IAMPolicyManager"] },
      { text: " and " },
      { ui: ["ProjectReadOnly"] },
      {
        text: " on your Organization. That key is equivalent to an Organization administrator for IAM applications and policies, so give it a short expiry and rotate it.",
      },
    ],
  },
  expo: {
    does: [
      "Read EAS builds, their logs and submissions",
      "Start builds and publish updates, where you allow it",
      "Use the EAS CLI as themselves, with no login",
    ],
    identity: "Each agent acts at Expo as its own robot, with its own access token.",
    byHand: [
      {
        text: "Add a robot for each agent at Expo with the role chosen here, and paste its token. Expo has no API for it.",
      },
    ],
  },
  digitalocean: {
    does: [
      "Read Droplets, Kubernetes clusters, databases and DNS",
      "Create, resize and remove infrastructure, where you allow it",
      "Use doctl or the DigitalOcean MCP server as themselves",
    ],
    identity:
      "Each agent gets its own DigitalOcean token, so you can cut off one agent without touching the others.",
    byHand: [
      {
        text: "Create a personal access token for each agent in DigitalOcean's control panel, with the scopes Crewlet lists, and paste it here. DigitalOcean has no API to create one.",
      },
    ],
  },
  aws: {
    does: [
      "Read EC2, ECS, EKS, Lambda, RDS, S3 and CloudWatch, log contents included, in the Regions you allow",
      "Restart and scale instances and services, and manage alarms, where you allow it",
      "Use the AWS MCP Server and the AWS CLI as themselves, with no access key to keep",
    ],
    identity:
      "Crewlet makes an IAM role for each agent, limited by a permissions boundary, so CloudTrail names the agent for every call.",
    byHand: [
      {
        text: "Run the setup block the form gives you in AWS CloudShell. It creates one CloudFormation stack with Crewlet's sign-in, its provisioner role, the boundary and the level policies, and prints the line you paste back.",
      },
    ],
  },
  google_cloud: {
    does: [
      "Read VMs, GKE, Cloud Run, Cloud SQL, metrics and logs in the projects you choose",
      "Restart and scale VMs and workloads, and run Cloud Run jobs, where you allow it",
      "Use Google's MCP servers and gcloud as themselves, with no key to keep",
    ],
    identity:
      "Crewlet makes a Google Cloud service account for each agent, in a project that holds only Crewlet's accounts.",
    byHand: [
      {
        text: "Create an empty project for Crewlet's accounts and run the setup block the form gives you. Then, on each project agents may use, run the project block from the docs, which creates Crewlet's Operator role and its limited grants.",
      },
    ],
  },
  ssh: {
    does: [
      "Run each server's allowlisted read commands, such as service status, disk and logs",
      "Run any command a person approves, once, exactly as approved, where you allow operate",
      "Reach only the servers you grant each agent, each one pinned by its host key",
    ],
    identity:
      "Crewlet signs a certificate for each agent's session that names the agent and its turn, so each server's log says which agent ran each command.",
    byHand: [
      {
        text: "Save to create Crewlet's certificate authority, then add each server, compare its host key with the server's console, and run the server setup Crewlet gives you as root.",
      },
    ],
  },
  google_analytics: {
    does: [
      "Read traffic, engagement, key events and revenue reports for the properties you choose",
      "Change settings, key events, custom definitions and audiences where you allow operate",
      "Ask you before any account, access, link or irreversible change, every time",
    ],
    identity:
      "Each agent has its own Google identity in your Google identity project; you add it to the property at the role it may act with. Its tokens never leave Crewlet.",
    byHand: [
      {
        text: "Connect Google Cloud first and run its setup block again, which turns on the Analytics APIs, then add each agent's email in ",
      },
      { ui: ["Admin", "Property access management"] },
      { text: "." },
    ],
  },
  admob: {
    does: [
      "Read network, mediation and campaign reports for the publisher accounts you choose",
      "Create apps, ad units and mappings, and tune mediation groups and A/B experiments where you allow operate",
      "Ask you before linking an app to its store or removing a mediation line, every time",
    ],
    identity:
      "A person who manages your AdMob account signs in with Google once, as your own OAuth client; every agent reaches AdMob through that sign-in, which never leaves Crewlet.",
    byHand: [
      { text: "Make an OAuth client (" },
      { ui: ["Web application"] },
      {
        text: ") in a Google Cloud project you control, turn on the AdMob API there, register the redirect URI and audience the form gives, then sign in with Google here.",
      },
    ],
  },
  adsense: {
    does: [
      "Read earnings reports, sites, alerts, policy issues and payments for the publisher accounts you choose",
      "Create and rename ad units and custom channels where you allow operate",
      "Ask you before a live ad unit's size changes, a channel is deleted or a platform's sub-account changes, every time",
    ],
    identity:
      "A person who manages your AdSense account signs in with Google once, as your own OAuth client; every agent reaches AdSense through that sign-in, which never leaves Crewlet.",
    byHand: [
      { text: "Make an OAuth client (" },
      { ui: ["Web application"] },
      {
        text: ") in a Google Cloud project you control, turn on the AdSense Management API there, register the redirect URI and audience the form gives, then sign in with Google here.",
      },
    ],
  },
  google_ads: {
    does: [
      "Read campaign, ad, keyword and search term reports for the accounts you choose",
      "Pause campaigns and ads, lower budgets and bids, and add negatives where you allow operate",
      "Ask you before any change that can raise spend, publishes ad text or removes, held to your caps",
    ],
    identity:
      "Each agent has its own Google identity in your Google identity project; you add it to the account at the access level it may act with. Its tokens never leave Crewlet.",
    byHand: [
      {
        text: "Connect Google Cloud first and run its setup block again, which turns on the Google Ads API, then add each agent's email in ",
      },
      { ui: ["Admin", "Access and security"] },
      { text: ", and set each account's spending caps." },
    ],
  },
  search_console: {
    does: [
      "Read search performance, sitemaps and URL inspection for the sites you choose",
      "Submit sitemaps, and notify Google of job posting and livestream pages, where you allow operate",
      "Ask you before adding or removing a property, verifying a site or changing its owners, every time",
    ],
    identity:
      "Each agent has its own Google identity in your Google identity project; you add it to each property at the permission it may act with. Its tokens never leave Crewlet.",
    byHand: [
      {
        text: "Connect Google Cloud first and run its setup block again, which turns on the Search Console APIs, then add each agent's email in ",
      },
      { ui: ["Settings", "Users and permissions"] },
      { text: "." },
    ],
  },
  google_play: {
    does: [
      "Read reviews, listings, releases, products, vitals and the monthly reports for the apps you choose",
      "Draft releases, halt rollouts and manage testers and draft products where you allow operate",
      "Ask you before any reply, listing, image, release, price or refund: building and uploading stays with Expo",
    ],
    identity:
      "Each agent has its own Google identity in your Google identity project; you invite it in Play Console with the app permissions it may act with. Its tokens never leave Crewlet.",
    byHand: [
      {
        text: "Connect Google Cloud first and run its setup block again, which turns on the Play APIs, then invite each agent's email in Play Console, ",
      },
      { ui: ["Users and permissions"] },
      { text: "." },
    ],
  },
};

/** Where a tool stands, in the four words a person sorts by. */
export type Bucket = "available" | "attention" | "working" | "ready";

export const BUCKET_WORDS: Record<Bucket, { label: string; tone: Tone }> = {
  available: { label: "Not connected", tone: "neutral" },
  attention: { label: "Needs attention", tone: "warning" },
  working: { label: "Setting up", tone: "info" },
  ready: { label: "Connected", tone: "success" },
};

/** One agent on one tool, as a person reads it. */
export type SeatStatus = "ready" | "pending" | "action" | "missing";

export interface SeatRow {
  handle: string;
  name: string;
  avatar: AgentAvatar | null;
  status: SeatStatus;
  /** Who this agent is at the tool, or what is missing, in the engine's words. */
  detail: string;
  /** The engine's step for an app the agent needs a person to make. */
  seat?: SetupSeatState;
  /** The surface key the agent's step is taken on (what the app route is keyed on). */
  toolKey?: string;
  finding?: ReconcileFinding;
}

/** What opening the connect flow is for. */
export type Intent = "connect" | "continue" | "rotate" | "settings" | "agents";

/** Who the next move belongs to. */
export type Owner = "you" | "crewlet" | "vendor";

/** One thing wrong with a tool, and the one control that answers it. */
export interface Issue {
  key: string;
  /** The engine's finding kind, where the issue is one of the loop's findings. */
  kind?: string;
  tone: "danger" | "warning";
  /** What is wrong, as a sentence. */
  problem: string;
  /** What to do about it, as a sentence, when the engine says. */
  remedy?: string;
  owner: Owner;
  /** The control that fixes it here. */
  fix?: { label: string; intent: Intent };
  /** Where it is fixed at the vendor, when nothing here can. */
  external?: { label: string; href: string };
  /** What it is about, as the engine names each: an agent's handle, an address, a project. */
  subjects: string[];
  /** The agents among [subjects] the chart has, each by its name. */
  agents: IssueAgent[];
  surface: string;
  /**
   * An agent's own wait or refusal, which its row on the Agents tab draws
   * with the step's own control; the app's page leaves it there, and the
   * home lists it as something waiting on you.
   */
  seat?: boolean;
}

/** An agent an issue is about, as the chart names it. */
export interface IssueAgent {
  handle: string;
  name: string;
  kind: "agent" | "human";
  avatar: AgentAvatar | null | undefined;
}

/** A seat on the chart, by its handle: its name and kind, or nothing for a handle it does not have. */
export type SeatOf = (handle: string) => Omit<IssueAgent, "handle"> | undefined;

/** The agents the chart has among what an issue is about. */
function agentsOf(subjects: readonly string[], seatOf: SeatOf): IssueAgent[] {
  return subjects.flatMap((handle) => {
    const seat = seatOf(handle);
    return seat ? [{ handle, ...seat }] : [];
  });
}

/**
 * Whether the company's own connection to the tool is set but not finished:
 * a block written with something it requires still unanswered (a step at the
 * vendor, a value to paste). Its agents cannot be given access through it yet.
 */
export function connectionUnfinished(tool: Tool): boolean {
  return tool.sections.some((s) => !s.seat && s.tool.configured && !s.tool.satisfied);
}

export interface Tool {
  entry: Entry;
  story: ToolStory;
  configured: boolean;
  state: EntryState;
  action: TileAction;
  bucket: Bucket;
  /** The settings form's sections, for the connect flow. */
  sections: { name: string; tool: SetupToolState; seat?: string }[];
  roster?: SetupToolState | undefined;
  seats: SeatRow[];
  issues: Issue[];
  /** Advisory notes on a working tool (permissions wider than asked, and the like). */
  notes: ReconcileFinding[];
  /** Why the connect flow cannot open, or undefined. */
  blocked?: string | undefined;
  /** Deliveries that reached a seat, counted only where the node read its event log. */
  delivered: number | null;
  /** What to have ready to connect, when the setup listing says: see [haveReadyOf]. */
  haveReady: string[];
  /** Every product the tool reaches, where a Crewlet service holds each: nothing is left to connect. */
  held: { name: string; heldBy: string }[];
}

/** An app that can do a built-in service's work instead, by its directory line's key. */
export type ReplacingApp = "jira" | "confluence" | "slack" | "mattermost";

export interface BuiltInService {
  key: "worklet" | "doclet" | "chat" | "gitlet";
  name: string;
  /**
   * Who makes it, where the directory names a product beside its maker (Jira
   * beside Atlassian): Crewlet, for Worklet, Doclet, Chatlet and Gitlet.
   */
  vendor?: string;
  /** Whether the engine has said what the company runs: false on a node running no company. */
  known: boolean;
  /** In use here, or run elsewhere (or not at all). */
  on: boolean;
  /**
   * What the service is, whether or not the company uses it: what runs in
   * its place is said once, by the page beneath the header.
   */
  sentence: string;
  /**
   * What does this service's work instead, where the company runs something:
   * the apps it runs in its place (Jira, Confluence, the chat vendors it talks
   * on), each by its line's key, and their names as one phrase.
   */
  replacedBy?: { keys: ReplacingApp[]; name: string };
  open?: { label: string; path: string[] };
}

/** Everything the page draws, and the gestures it can make. */
export interface Model {
  loading: boolean;
  error: string | null;
  refusal: ComponentProps<typeof QueryState>["refusal"];
  tools: Tool[];
  byKey: Map<string, Tool>;
  /** The `integrations` rows by surface key, for the facts each surface counts. */
  rows: Map<string, IntegrationRow>;
  /** The address vendors deliver to, and whether it is one only this machine reaches. */
  base: { value: string; loopback: boolean } | null;
  /** True when the setup listing was refused: a reader with no Admin. */
  guarded: boolean;
  /** The refusal, when [guarded] — said through `lib/refusal.ts`'s `authorityWords`. */
  refused: RestError | null;
  managed: string | null;
  admin: boolean;
  trafficKnown: boolean;
  trafficSince: string | null;
  avatarOf: (handle: string) => AgentAvatar | null;
  /** Ask the reconcile loop to look at one tool now. */
  check: (tool: Tool) => Promise<string>;
  /** What the disconnect dialog needs for one tool. */
  dropFor: (tool: Tool) => DropPlan;
  watch: () => void;
}

export interface DropPlan {
  name: string;
  kinds: string[];
  stuck: string;
  /** What holds it ([stuckOn]), "" where nothing does. */
  stuckOn: "vendor" | "settings" | "";
  apps: { handle: string; name: string; url: string }[];
  appPath?: string | undefined;
}

/** A finding about one agent, read off its subject. */
function seatFindingsOf(issuesSource: ReconcileFinding[]): Map<string, ReconcileFinding> {
  const out = new Map<string, ReconcileFinding>();
  for (const f of issuesSource) {
    if (f.subject && !advisory(f) && !out.has(f.subject)) out.set(f.subject, f);
    for (const s of f.subjects ?? []) if (!advisory(f) && !out.has(s)) out.set(s, f);
  }
  return out;
}

function ownerOf(actor: string | undefined): Owner {
  if (actor === "engine") return "crewlet";
  if (actor === "provider") return "vendor";
  return "you";
}

export const OWNER_WORDS: Record<Owner, string> = {
  you: "You",
  crewlet: "Crewlet",
  vendor: "The app",
};

/** What a finding kind is called in a sentence, when the engine sends no detail. */
const KIND_WORDS: Record<string, string> = {
  credential_rejected: "The key Crewlet holds was refused",
  credential_expiring: "The key Crewlet holds expires soon",
  credential_missing: "A key is missing",
  approval_required: "Something is waiting on a person",
  ingress_blocked: "Deliveries cannot reach this engine",
  ingress_pending: "Deliveries are being set up",
  identity_missing: "An agent has no account yet",
  identity_failed: "An agent's account could not be made",
  grant_pending: "A permission is waiting on approval",
  grant_short: "An agent has less access than it needs",
  credential_shared: "Agents share one key",
};

/**
 * The issues a tool raises, each with its fix: the reconcile loop's own
 * findings in the engine's own words, never an advisory, each paired with the
 * one control that answers it.
 *
 * A FINDING'S SUBJECT IS AN AGENT ONLY WHEN THE CHART HAS IT. The engine puts
 * a seat's handle there, and also a config path or a phrase ("agents without
 * an app"), so the agents an issue names are its `subjects` where it lists
 * them and its subject where that is a seat on the chart.
 *
 * A FIX IS OFFERED ONLY WHERE ONE CONTROL HERE ANSWERS THE KIND: a key to
 * replace or add. A wait on a person has its control on the agent's own row
 * (the engine marks it `seat`), and one at the vendor its link; "Create app"
 * was offered for every GitHub wait, a permission to accept included, and
 * opened the settings form.
 */
function issuesOf(
  entry: Entry,
  present: ReturnType<typeof presentSurfaces>,
  state: EntryState,
  seatOf: SeatOf,
): Issue[] {
  const out: Issue[] = [];
  for (const p of present) {
    out.push(...surfaceIssues(entry, p));
    for (const f of p.row.reconcile?.findings ?? []) {
      if (advisory(f)) continue;
      const owner = ownerOf(f.actor ?? p.row.reconcile?.actor);
      const subjects = f.subjects ?? (f.subject && seatOf(f.subject) ? [f.subject] : []);
      let fix: Issue["fix"];
      if (ROTATE_FINDINGS.has(f.kind)) fix = { label: "Replace key", intent: "rotate" };
      else if (f.kind === "credential_missing" && !f.subject)
        fix = { label: "Add the key", intent: "continue" };
      out.push({
        key: `${p.surface.key}:${f.kind}:${f.subject ?? ""}`,
        kind: f.kind,
        tone: f.phase === "unconfigured" || f.kind === "credential_rejected" ? "danger" : "warning",
        problem: asSentence(
          f.detail ||
            (f.kind === "credential_rejected" ? (p.row.reconcile?.detail ?? "") : "") ||
            KIND_WORDS[f.kind] ||
            f.kind.replace(/_/g, " "),
        ),
        remedy:
          (f.remedy ? asSentence(f.remedy) : undefined) ??
          (f.kind === "credential_rejected"
            ? `Make a new key at ${entry.name}, then paste it here. Nothing else changes.`
            : undefined),
        owner,
        fix,
        // THE BUTTON SAYS THE FIX: the address is where the person the
        // finding names goes to make it, so it is the fix, at the vendor.
        external: f.action_url ? { label: `Fix on ${entry.name}`, href: f.action_url } : undefined,
        subjects,
        agents: agentsOf(subjects, seatOf),
        surface: p.surface.name,
        ...(f.seat ? { seat: true } : {}),
      });
    }
  }
  // THE ROLL-UP'S OWN REASON, where it owes something no finding states.
  if (out.length === 0 && state.reason && !state.busy && state.tone === "warning") {
    out.push({
      key: `${entry.key}:rollup`,
      tone: "warning",
      problem: asSentence(state.reason),
      owner: "you",
      fix: { label: "Finish setup", intent: "continue" },
      subjects: [],
      agents: [],
      surface: entry.name,
    });
  }
  return out;
}

/**
 * What one surface reports beside the loop's findings: the facts its row
 * states about its own edge, each a FALSE and never a missing value, since
 * every one of these fields is three-valued and `null` is "nothing here can
 * say". A finding the loop raised about the same edge says it in the loop's
 * own words, so the row's fact is not said twice.
 */
function surfaceIssues(entry: Entry, p: ReturnType<typeof presentSurfaces>[number]): Issue[] {
  const { row, surface } = p;
  const kinds = new Set((row.reconcile?.findings ?? []).map((f) => f.kind));
  const ingress = [...kinds].some((k) => k.startsWith("ingress_"));
  const out: Issue[] = [];
  const add = (kind: string, issue: Omit<Issue, "key" | "subjects" | "agents" | "surface">) =>
    out.push({
      key: `${surface.key}:${kind}`,
      subjects: [],
      agents: [],
      surface: surface.name,
      ...issue,
    });
  if (row.secret_usable === false && !kinds.has("credential_missing")) {
    add("secret", {
      tone: "danger",
      problem: `A secret ${surface.name} needs is not set, so its deliveries are refused.`,
      owner: "you",
      fix: { label: "Finish setup", intent: "continue" },
    });
  }
  if (row.endpoint_current === false && !ingress) {
    add("endpoint", {
      tone: "warning",
      problem: `${surface.name} delivers to an old address, so its deliveries are lost.`,
      remedy: `Set the address at ${entry.name} to the engine's, at the foot of the list.`,
      owner: "you",
    });
  }
  // ROUTED IS A FACT ABOUT THE RUNNING ENGINE, not a setting: whether
  // anything here turns this app's deliveries into work for a seat. So it is
  // said with no remedy and no control, since no field in the form moves it.
  if (row.routes === false) {
    add("routes", {
      tone: "warning",
      problem: `${surface.name} deliveries arrive but reach no agent.`,
      owner: "you",
    });
  }
  // A DISCONNECT THE COMPANY'S SETTINGS HOLD is the person's to move, and is
  // said in words: the engine's fault is a list of setting paths, which drawn
  // whole was a wall of them on the card, and "the last check failed" blamed a
  // check. Disconnect opens with the settings it names.
  const heldBySettings =
    row.reconcile?.disconnecting && row.reconcile.teardown_held_by === "settings";
  if (heldBySettings) {
    add("teardown", {
      tone: "warning",
      problem: `Disconnecting ${surface.name} is held: the company's settings refused the change that removes it.`,
      remedy: "Open Disconnect to see the settings it names, change them, and disconnect again.",
      owner: "you",
    });
  }
  // A PASS THAT FAILED WITH NOTHING TO SHOW FOR IT is still a failed pass: the
  // loop tries again on its own, so the next move is Crewlet's.
  if (row.reconcile?.last_error && kinds.size === 0 && !heldBySettings) {
    add("pass", {
      tone: "warning",
      problem: asSentence(`The last check of ${surface.name} failed: ${row.reconcile.last_error}`),
      owner: "crewlet",
    });
  }
  return out;
}

function bucketOf(configured: boolean, state: EntryState, issues: Issue[]): Bucket {
  if (!configured) return "available";
  if (issues.some((i) => i.owner === "you")) return "attention";
  if (state.busy || state.tone === "warning")
    return issues.length > 0 || state.busy ? "working" : "attention";
  return "ready";
}

function seatRowsOf(
  roster: SetupToolState | undefined,
  findings: Map<string, ReconcileFinding>,
  avatarOf: (h: string) => AgentAvatar | null,
): SeatRow[] {
  return (roster?.seats ?? []).map((seat) => {
    const finding = findings.get(seat.handle);
    let status: SeatStatus = seat.satisfied ? "ready" : "missing";
    if (finding) status = finding.actor === "engine" ? "pending" : "action";
    if (seat.step === "create_app" || seat.step === "install_app") status = "action";
    return {
      handle: seat.handle,
      name: seat.name || seat.handle,
      avatar: avatarOf(seat.handle),
      status,
      // A FINDING IS A CLAUSE, made a sentence; THE SEAT'S DETAIL IS A VALUE
      // (the repositories an app reaches, an account's address), printed as
      // the engine sent it, since capitalised it would name another repository.
      detail: finding?.detail
        ? asSentence(finding.detail)
        : seat.detail || (status === "ready" ? "Set up" : "Nothing set up for this agent yet"),
      seat,
      toolKey: roster?.key,
      finding,
    };
  });
}

/**
 * The built-in services, from the engine's projection (everybody's), with the
 * chat vendors named by the `viewer` answer, which tells them to a reader
 * holding Chatlet.
 */
export function useBuiltIns(): BuiltInService[] {
  const backends = useBackends();
  const vendors = useViewer().chatVendors;
  return useMemo(() => builtIns(backends, vendors), [backends, vendors]);
}

/** A node running no company has nothing to say about any service. */
const NO_COMPANY = "This node runs no company yet.";

/**
 * Crewlet's own services, from what the engine said the company runs. Until it
 * has said (a node running no company), each says so rather than guessing
 * either way.
 */
export function builtIns(
  backends: OrgServices | null,
  chatVendors: readonly string[] = [],
): BuiltInService[] {
  const tracker = backends?.tracker;
  const knowledge = backends?.knowledge;
  const chat = backends?.chat;
  const code = backends?.code;
  // THE LINES A VENDOR CHAT IS LISTED ON: the ones the directory has. A vendor
  // nobody named is still a vendor's chat, said in words, with no line to open.
  const vendorLines = chatVendors.filter(
    (v): v is "slack" | "mattermost" => v === "slack" || v === "mattermost",
  );
  return [
    {
      key: "worklet",
      name: SERVICE_NAMES.tracker,
      vendor: "Crewlet",
      known: tracker !== undefined,
      on: tracker === "worklet",
      sentence: tracker === undefined ? NO_COMPANY : "Projects, tasks, your inbox and My work.",
      ...(tracker === "jira" ? { replacedBy: { keys: ["jira"], name: "Jira" } } : {}),
      ...(tracker === "worklet" ? { open: openService("worklet", "Open Worklet") } : {}),
    },
    {
      key: "doclet",
      name: SERVICE_NAMES.knowledge,
      vendor: "Crewlet",
      known: knowledge !== undefined,
      on: knowledge === "doclet",
      sentence:
        knowledge === undefined ? NO_COMPANY : "Spaces and pages, agent skills and diaries.",
      ...(knowledge === "confluence"
        ? { replacedBy: { keys: ["confluence"], name: "Confluence" } }
        : {}),
      ...(knowledge === "doclet" ? { open: openService("doclet", "Open Doclet") } : {}),
    },
    {
      key: "chat",
      name: "Chatlet",
      vendor: "Crewlet",
      known: chat !== undefined,
      on: chat === "chatlet",
      sentence:
        chat === undefined ? NO_COMPANY : "Rooms, threads and direct messages with your agents.",
      ...(chat === "vendor"
        ? { replacedBy: { keys: vendorLines, name: chatVendorName(chatVendors) } }
        : {}),
      ...(chat === "chatlet" ? { open: openService("chatlet", "Open Chatlet") } : {}),
    },
    {
      // NOTHING IS EVER IN GITLET'S PLACE: a company's repositories on GitHub
      // or GitLab are integrations its seats call, never a host the engine
      // runs instead (`contract/services.ts`), so Gitlet off names no
      // replacement, whatever else the company connects.
      key: "gitlet",
      name: SERVICE_NAMES.code,
      vendor: "Crewlet",
      known: code !== undefined,
      on: code === "gitlet",
      sentence:
        code === undefined
          ? NO_COMPANY
          : "Git repositories, their changes and checks, hosted here.",
      ...(code === "gitlet" ? { open: openService("gitlet", "Open Gitlet") } : {}),
    },
  ];
}

/**
 * The way into a service built into Crewlet: its workspace's own landing, as
 * the sidebar's row opens it (`app/nav.ts`), so Open Chatlet lands on Activity
 * as the row does.
 */
function openService(key: Workspace, label: string): { label: string; path: string[] } {
  return { label, path: workspaceRow(key)?.path ?? [key] };
}

/** What a tool's derivation reads: the two answers, and the chart. */
export interface ToolInputs {
  /** The `integrations` rows by surface key. */
  rows: Map<string, IntegrationRow>;
  /** The engine's roll-up of every tool. */
  rollups: IntegrationTool[] | undefined;
  /** The setup listing's tools by surface key. */
  setup: Map<string, SetupToolState>;
  /** The setup listing was refused. */
  guarded: boolean;
  trafficKnown: boolean;
  /** A handle's seat on the chart, or nothing for one it does not have. */
  seatOf: SeatOf;
  avatarOf: (handle: string) => AgentAvatar | null;
}

/**
 * What a connect asks for, each field by its label, and no label said twice:
 * several connections ask the same question once each.
 */
export function haveReadyOf(reqs: SetupRequirement[]): string[] {
  return [...new Set(reqs.map((r) => r.label))];
}

/** One tool, as a person reads it, from what the engine said about it. */
export function deriveTool(entry: Entry, what: ToolInputs): Tool {
  const present = presentSurfaces(entry, what.rows);
  const configured = present.length > 0;
  const sections = sectionsFor(entry, what.setup);
  const held = heldProducts(entry, sections);
  const setupTools = toolsOf(sections);
  const state = entryState(entry, what.rows, what.rollups);
  const credentials = credentialFindings(present);
  const action = actionFor(state, setupTools, configured, credentials, what.guarded);
  const issues = configured ? issuesOf(entry, present, state, what.seatOf) : [];
  const roster = rosterOf(setupTools);
  const findings = seatFindingsOf(present.flatMap((p) => p.row.reconcile?.findings ?? []));
  const haveReady = haveReadyOf(
    sections
      .filter((s) => s.seat === undefined)
      .flatMap((s) => splitFields(s.tool.requirements, () => "").connect),
  );
  return {
    entry,
    story: STORIES[entry.key] ?? { does: [], identity: "" },
    configured,
    state,
    action,
    bucket: bucketOf(configured, state, issues),
    sections,
    roster,
    seats: seatRowsOf(roster, findings, what.avatarOf),
    issues,
    notes: present.flatMap((p) => (p.row.reconcile?.findings ?? []).filter(advisory)),
    blocked:
      formBlocked(sections, what.guarded) ??
      (held.length > 0
        ? `Crewlet's own services run in ${entry.name}'s place, so there is nothing to connect.`
        : undefined),
    held,
    delivered: what.trafficKnown ? total(present, (r) => r.inbound) : null,
    haveReady,
  };
}

export function useIntegrations(): Model {
  const viewer = useViewer();
  const managed = useManagedConfig();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const avatarOf = useCallback(
    (handle: string) => index.byHandle.get(handle)?.avatar ?? null,
    [index],
  );
  const seatOf = useCallback<SeatOf>(
    (handle) => {
      const seat = index.byHandle.get(handle);
      return seat ? { name: seat.name, kind: seat.kind, avatar: seat.avatar } : undefined;
    },
    [index],
  );
  const [settling, setSettling] = useState(false);
  const read = useQuery("integrations", undefined, {
    pollMs: settling ? 4_000 : 60_000,
    refetchOnFocus: true,
  });
  const { loading, error, refusal, refetch } = read;
  // AN UNCHANGED RE-READ IS THE SAME READING, so no tool is derived again for it.
  const data = useStableValue(read.data);
  const setup = useSetup();
  const { watching, watch } = useRecheck(
    useCallback(() => {
      refetch();
      setup.reload();
    }, [refetch, setup]),
  );
  const rows = useMemo(
    () => new Map((data?.integrations ?? []).map((r) => [r.key, r] as [string, IntegrationRow])),
    [data],
  );
  const moving = [...rows.values()].some((r) => IN_FLIGHT.has(r.reconcile?.phase ?? ""));
  useEffect(() => setSettling(moving || watching), [moving, watching]);
  const trafficKnown = data?.traffic_known ?? false;

  const tools = useMemo<Tool[]>(
    () =>
      CATALOG.map((entry) =>
        deriveTool(entry, {
          rows,
          rollups: data?.tools,
          setup: setup.byKey,
          guarded: setup.guarded,
          trafficKnown,
          seatOf,
          avatarOf,
        }),
      ),
    [rows, data, setup.byKey, setup.guarded, trafficKnown, avatarOf, seatOf],
  );
  const byKey = useMemo(() => new Map(tools.map((t) => [t.entry.key, t])), [tools]);

  const check = useCallback(
    async (tool: Tool): Promise<string> => {
      // ONLY A SURFACE THAT RUNS A PASS: the route answers 409 for one that
      // does not (Slack's apps are made by hand), which is no answer to "look".
      const kinds = tool.entry.surfaces
        .filter((s) => setup.byKey.get(s.key)?.can_provision)
        .map((s) => s.key);
      try {
        for (const kind of kinds)
          await rest.post(`/setup/integrations/${pathSegment(kind)}/check`, {});
        watch();
        return "";
      } catch (err) {
        return err instanceof RestError ? err.detail || err.code || "Refused." : String(err);
      }
    },
    [setup.byKey, watch],
  );

  const dropFor = useCallback(
    (tool: Tool): DropPlan => ({
      name: tool.entry.name,
      kinds: disconnectOrder(tool.entry, rows, tool.sections),
      stuck: stuckDisconnecting(tool.entry, rows),
      stuckOn: stuckOn(tool.entry, rows),
      apps: toolsOf(tool.sections)
        .flatMap((t) => t.seats ?? [])
        .filter((seat) => seat.manage_url)
        .map((seat) => ({
          handle: seat.handle,
          name: seat.name || seat.handle,
          url: seat.manage_url as string,
        })),
      appPath: tool.sections.find((s) => s.tool.manage_path)?.tool.manage_path,
    }),
    [rows],
  );

  return {
    // LOADING IS THE FIRST ANSWER ONLY. A re-read after a write sets the
    // setup half loading again, and a page that blanked to a skeleton then
    // unmounted the connect flow somebody was in the middle of.
    loading: (loading && !data) || (setup.loading && setup.byKey.size === 0 && !setup.guarded),
    error,
    refusal,
    tools,
    byKey,
    rows,
    base: setup.base ? { value: setup.base.value, loopback: setup.base.loopback === true } : null,
    guarded: setup.guarded,
    refused: setup.refused,
    managed,
    admin: viewer.admin,
    trafficKnown,
    trafficSince: data?.traffic_since ?? null,
    avatarOf,
    check,
    dropFor,
    watch,
  };
}

/** The tool a route segment names: its own key, or any surface it covers. */
export function toolFor(model: Model, kind: string | undefined): Tool | undefined {
  if (!kind) return undefined;
  return model.tools.find(
    (t) => t.entry.key === kind || t.entry.surfaces.some((s) => s.key === kind),
  );
}
