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
import { asSentence } from "~/lib/format.ts";
import type { AgentAvatar } from "~/lib/avatar.ts";
import { pathSegment, rest, RestError } from "~/protocol/index.ts";
import type { IntegrationRow, IntegrationTool, ReconcileFinding } from "~/contract/integrations.ts";
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
  presentSurfaces,
  rosterOf,
  sectionsFor,
  stuckDisconnecting,
  toolsOf,
  total,
  useSetup,
  type Entry,
  type EntryState,
  type TileAction,
} from "./catalogue.tsx";
import { useRecheck } from "~/routes/settings/recheck.ts";
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
  /** What a person does by hand, beyond typing values in. */
  byHand?: string;
}

export const STORIES: Record<string, ToolStory> = {
  slack: {
    does: [
      "Answer mentions and direct messages",
      "Reply in the thread they were asked in",
      "Show they are working before they answer",
    ],
    identity: "Each agent posts as its own Slack app.",
    byHand: "Create one Slack app per agent from the manifest Crewlet writes.",
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
    byHand: "Create and install one app per agent. GitHub has no API for it.",
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
  surface: string;
  /**
   * An agent's own wait or refusal, which its row on the Agents tab draws
   * with the step's own control; the app's page leaves it there, and the
   * home lists it as something waiting on you.
   */
  seat?: boolean;
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
  /** The values a connect asks for, by label, when the setup listing says. */
  asks: SetupRequirement[];
}

/** An app that can do a built-in service's work instead, by its directory line's key. */
export type ReplacingApp = "jira" | "confluence" | "slack" | "mattermost";

export interface BuiltInService {
  key: "worklet" | "doclet" | "chat";
  name: string;
  /**
   * Who makes it, where the directory names a product beside its maker (Jira
   * beside Atlassian): Crewlet, for Worklet, Doclet and Chatlet.
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
  /** The address vendors deliver to. */
  base: string | null;
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
  isSeat: (handle: string) => boolean,
): Issue[] {
  const out: Issue[] = [];
  for (const p of present) {
    out.push(...surfaceIssues(entry, p));
    for (const f of p.row.reconcile?.findings ?? []) {
      if (advisory(f)) continue;
      const owner = ownerOf(f.actor ?? p.row.reconcile?.actor);
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
        external: f.action_url ? { label: `Open ${entry.name}`, href: f.action_url } : undefined,
        subjects: f.subjects ?? (f.subject && isSeat(f.subject) ? [f.subject] : []),
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
  const add = (kind: string, issue: Omit<Issue, "key" | "subjects" | "surface">) =>
    out.push({ key: `${surface.key}:${kind}`, subjects: [], surface: surface.name, ...issue });
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
  // A PASS THAT FAILED WITH NOTHING TO SHOW FOR IT is still a failed pass: the
  // loop tries again on its own, so the next move is Crewlet's.
  if (row.reconcile?.last_error && kinds.size === 0) {
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
      ...(tracker === "worklet" ? { open: { label: "Open Worklet", path: ["worklet"] } } : {}),
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
      ...(knowledge === "doclet" ? { open: { label: "Open Doclet", path: ["doclet"] } } : {}),
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
      ...(chat === "chatlet" ? { open: { label: "Open Chatlet", path: ["chatlet"] } } : {}),
    },
  ];
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
  /** Whether a handle is a seat on the chart. */
  isSeat: (handle: string) => boolean;
  avatarOf: (handle: string) => AgentAvatar | null;
}

/** One tool, as a person reads it, from what the engine said about it. */
export function deriveTool(entry: Entry, what: ToolInputs): Tool {
  const present = presentSurfaces(entry, what.rows);
  const configured = present.length > 0;
  const sections = sectionsFor(entry, what.setup);
  const setupTools = toolsOf(sections);
  const state = entryState(entry, what.rows, what.rollups);
  const credentials = credentialFindings(present);
  const action = actionFor(state, setupTools, configured, credentials, what.guarded);
  const issues = configured ? issuesOf(entry, present, state, what.isSeat) : [];
  const roster = rosterOf(setupTools);
  const findings = seatFindingsOf(present.flatMap((p) => p.row.reconcile?.findings ?? []));
  const asks = sections
    .filter((s) => s.seat === undefined)
    .flatMap((s) => splitFields(s.tool.requirements, () => "").connect)
    .filter((r, i, all) => all.findIndex((o) => o.field === r.field) === i);
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
    blocked: formBlocked(sections, what.guarded),
    delivered: what.trafficKnown ? total(present, (r) => r.inbound) : null,
    asks,
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
  const [settling, setSettling] = useState(false);
  const { data, loading, error, refusal, refetch } = useQuery("integrations", undefined, {
    pollMs: settling ? 4_000 : 60_000,
    refetchOnFocus: true,
  });
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
          isSeat: (h) => index.byHandle.has(h),
          avatarOf,
        }),
      ),
    [rows, data, setup.byKey, setup.guarded, trafficKnown, avatarOf, index],
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
    base: setup.base?.value ?? null,
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
