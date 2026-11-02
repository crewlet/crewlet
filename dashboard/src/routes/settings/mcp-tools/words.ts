/**
 * The words Settings › MCP & tools says things in, in one place, so the
 * table, a server's page, the alarms and the dialogs name each state, risk,
 * failure and gesture the same way.
 *
 * WORDS FOR WHAT THE ENGINE DECIDED, never a decision. A server's state, a
 * tool's risk, a failure's class and an alarm's fix are the engine's answer
 * (`contract/mcp.ts`, held to the engine by Go gates); each record here is
 * exhaustive over that union, so a value the engine adds fails the typecheck
 * here rather than drawing as nothing.
 */

import type {
  McpAlarmKind,
  McpAuth,
  McpAuthMethod,
  McpCallOutcome,
  McpFailureClass,
  McpFixKind,
  McpInstanceState,
  McpRisk,
  McpServerState,
  McpStepState,
  McpTestStage,
} from "~/contract/mcp.ts";
import type { StepState } from "@crewlethq/ui";
import type { BuiltinGate } from "~/contract/tools.ts";
import { managedSentence, unsettledSentence } from "~/protocol/configAnswer.ts";
import type { ConfigWriteOutcome } from "~/protocol/configWrite.ts";
import { configGuardedReason } from "~/lib/useWriteAccess.ts";

type Tone = "neutral" | "info" | "success" | "warning" | "danger";

/**
 * A server's condition in a word, the tone its dot wears. Status is health
 * alone: whether anybody uses a server is the Used by column's question.
 */
export const STATE: Record<McpServerState, { label: string; tone: Tone }> = {
  running: { label: "Running", tone: "success" },
  partial: { label: "Partial", tone: "warning" },
  failing: { label: "Failed", tone: "danger" },
  waiting: { label: "Waiting", tone: "info" },
  disabled: { label: "Disabled", tone: "neutral" },
  needs_sign_in: { label: "Needs sign-in", tone: "warning" },
  not_started: { label: "Not started", tone: "neutral" },
  unreported: { label: "Not reported", tone: "neutral" },
};

/** One instance's condition on its node. */
export const INSTANCE: Record<McpInstanceState, { label: string; tone: Tone }> = {
  running: { label: "Running", tone: "success" },
  failed: { label: "Not running", tone: "danger" },
  waiting: { label: "Waiting", tone: "info" },
};

/** How much a call can change, as the engine reads the tool's hints. */
export const RISK: Record<McpRisk, { label: string; tone: Tone }> = {
  read_only: { label: "Read only", tone: "success" },
  additive: { label: "Writes", tone: "info" },
  destructive: { label: "Destructive", tone: "danger" },
};

/** How the engine signs in to a server. */
export const AUTH: Record<McpAuth, string> = {
  none: "None",
  headers: "Values the configuration sets",
  integration: "The integration's own credential, per agent",
  oauth: "OAuth, signed in for the company",
};

/** A stage a test walks, in a word: what the engine did there. */
export const STAGE: Record<McpTestStage, string> = {
  address: "Address",
  resolve: "Resolve",
  connect: "Connect",
  tls: "Certificate",
  launch: "Launch",
  discover: "Discover",
  list: "List tools",
};

/**
 * How a stage went, as the kit's step list draws it: a stage the test never
 * reached is still to come, a skipped one (a certificate over plain http)
 * has nothing to report.
 */
export const STAGE_STATE: Record<McpStepState, StepState> = {
  passed: "done",
  failed: "failed",
  skipped: "pending",
  not_reached: "pending",
};

/** A sign-in method in a few words, for a card, a fact or a sentence. */
export const SIGN_IN: Record<McpAuthMethod, { label: string; noun: string }> = {
  integration: { label: "Integration", noun: "an integration" },
  oauth: { label: "OAuth", noun: "OAuth" },
  headers: { label: "Header", noun: "a header" },
  none: { label: "None", noun: "no sign-in" },
};

/** How a call ended. */
export const OUTCOME: Record<McpCallOutcome, { label: string; tone: Tone }> = {
  answered: { label: "Answered", tone: "success" },
  failed: { label: "Failed", tone: "danger" },
  unavailable: { label: "Unavailable", tone: "warning" },
  refused: { label: "Refused", tone: "neutral" },
};

/** What a failure is, in a sentence, in a few words, and what fixes it. */
export interface FailureWords {
  /** What went wrong, in a sentence: an alarm, a server's page. */
  title: string;
  /**
   * The same cause in a few words, for a table cell or a list's end, where a
   * sentence is cut to its first three words; the sentence is its tooltip.
   */
  short: string;
  remedy: string;
}

/**
 * Each failure class the engine tells apart, with the one thing that fixes
 * it. The engine decides the class; these are the words an alarm and a
 * server's page both read, so one cause is never named two ways.
 */
export const FAILURE: Record<McpFailureClass, FailureWords> = {
  blocked_address: {
    title: "The server's sign-in points at an address the engine does not follow",
    short: "Address blocked",
    remedy:
      "Its OAuth metadata sends the engine to a private, loopback or cloud metadata address. The engine follows only public addresses a remote server names; the vendor fixes its metadata.",
  },
  dns: {
    title: "Host not found",
    short: "Host not found",
    remedy:
      "Check the spelling of the host. A host on a private network must resolve from the engine's own network.",
  },
  unreachable: {
    title: "The server did not respond",
    short: "No response",
    remedy:
      "The engine connected nowhere in time. Check the server is up, and that it accepts connections from the engine's address.",
  },
  tls: {
    title: "The server's certificate is not valid",
    short: "Certificate not valid",
    remedy:
      "The certificate expired, is self-signed or names another host. The server's owner renews or replaces it.",
  },
  needs_sign_in: {
    title: "Sign-in required",
    short: "Sign-in required",
    remedy:
      "The server asks for an OAuth sign-in before it lists any tool. Until the engine signs in to it, it answers nothing.",
  },
  credential_rejected: {
    title: "Credential rejected or expired",
    short: "Credential rejected",
    remedy:
      "Replace the value, and check the header name: Sentry reads Sentry-Bearer, and Atlassian personal tokens use Basic.",
  },
  missing_permission: {
    title: "Missing permission",
    short: "Missing permission",
    remedy:
      "The credential signs in but lacks the scope or permission the server asks for. Grant it at the vendor, then restart the server.",
  },
  no_endpoint: {
    title: "No MCP endpoint at this address",
    short: "No MCP endpoint",
    remedy:
      "The path answers 404 or 405. Retired SSE addresses (Atlassian's /v1/sse) fail this way; use the server's /mcp address.",
  },
  not_mcp: {
    title: "This address did not answer as an MCP server",
    short: "Not an MCP server",
    remedy:
      "It answered with a web page, which is usually a login page, a proxy or a captive portal in front of the server.",
  },
  old_protocol: {
    title: "The server uses an older MCP version",
    short: "Old MCP version",
    remedy:
      "It speaks none of the versions the engine supports. Update the server, or ask its owner which address serves the current version.",
  },
  rate_limited: {
    title: "Rate limited",
    short: "Rate limited",
    remedy:
      "The server asked the engine to slow down. Nothing is broken; the engine tries again when the server allows.",
  },
  empty_tools: {
    title: "The server lists no tools",
    short: "No tools listed",
    remedy:
      "It answered but offers nothing to this credential. Some servers list tools by scope or plan; check what the credential may use.",
  },
  command_not_found: {
    title: "Command not found",
    short: "Command not found",
    remedy:
      "The command is not on the engine host's PATH, or the box image lacks it. Install it there, or give its full path.",
  },
  exited: {
    title: "The server exited while starting",
    short: "Exited while starting",
    remedy:
      "The process stopped before it answered. Its last lines of output say why; a missing environment value is the usual cause.",
  },
};

/** A failure the engine could not put in a class: its sentence is the whole of what is known. */
const UNCLASSED: FailureWords = {
  title: "The server failed",
  short: "Failed",
  remedy: "The engine could not tell why. What the server or its process said is below.",
};

/**
 * GitHub's 422 for an agent's installation token: the agent's own GitHub App
 * asked for more permissions than its installation has accepted. Only an
 * owner of the GitHub organization can approve them, at GitHub.
 */
const GITHUB_PERMISSIONS: FailureWords = {
  title: "GitHub has not approved this agent's new permissions",
  short: "Permissions pending",
  remedy:
    "The agent's GitHub App asks for permissions its installation has not accepted, so GitHub refuses its token. An owner of the GitHub organization approves them in the app's installation settings; the agent starts on the next try.",
};

/**
 * The words for a failure, with GitHub's installation refusal told apart from
 * any other missing permission by what the server said, which only an
 * administrator's reading carries.
 */
export function failureWords(cls: McpFailureClass | "", said = ""): FailureWords {
  if (cls === "") return UNCLASSED;
  if (cls === "missing_permission" && /installations\/\d+\/access_tokens: 422/.test(said)) {
    return GITHUB_PERMISSIONS;
  }
  return FAILURE[cls];
}

/**
 * Who the engine lets call a built-in, in a few words for the table and a
 * sentence for the tool's own drawer. The engine names the gate; holding a
 * tool is not being allowed to call it.
 */
export const GATE: Record<BuiltinGate, { label: string; detail: string }> = {
  everybody: { label: "Every agent", detail: "Any agent may call it." },
  self: {
    label: "Its own",
    detail: "It reads or writes only the calling agent's own notes, turns and skills.",
  },
  worklet: { label: "Worklet access", detail: "Agents whose role holds Worklet access." },
  worklet_asked: {
    label: "Worklet access",
    detail: "Agents with Worklet access, or one a colleague asked on the item.",
  },
  project_lead: {
    label: "Project lead",
    detail: "The lead of the project the work or file is in, or an admin.",
  },
  doclet: { label: "Doclet access", detail: "Agents whose role holds Doclet access." },
  chatlet: {
    label: "Chatlet access",
    detail: "Agents whose role holds Chatlet access, in the rooms they are in.",
  },
  artifacts: { label: "Artifacts access", detail: "Agents whose role holds Artifacts access." },
  artifact_viewer: {
    label: "Its viewers",
    detail: "Anyone the artifact is shared with, as a viewer or above.",
  },
  artifact_editor: {
    label: "Its editors",
    detail: "Editors of the artifact who hold Artifacts access, and its owner.",
  },
  artifact_owner: { label: "Its owner", detail: "The artifact's owner." },
};

/** What an alarm row is about: the server by its title, and the agent or node it names. */
export interface AlarmSubject {
  server: string;
  /** The agent whose instance it names, by name; "" for none. */
  who: string;
  /** The node a shared server's instance is down on; "" where none is named. */
  node: string;
  /** How many tools, for a refused name. */
  count: number;
}

/**
 * Each MCP alarm on this page: its tone and what it says about one server in
 * a line. NOT RUNNING IS A PROBLEM for whoever it stops, however many others
 * it serves, so a partial outage is drawn as a failure for that agent or node.
 */
export const ALARM: Record<
  McpAlarmKind,
  { tone: "danger" | "warning" | "info"; title: (about: AlarmSubject) => string }
> = {
  mcp_server_failing: {
    tone: "danger",
    title: ({ server }) => `${server} is not running`,
  },
  mcp_server_partial: {
    tone: "danger",
    title: ({ server, who, node }) =>
      who
        ? `${server} is not running for ${who}`
        : node
          ? `${server} is not running on ${node}`
          : `${server} is not running everywhere`,
  },
  mcp_server_waiting_long: {
    tone: "warning",
    title: ({ server, who }) =>
      who
        ? `${server} is still waiting for ${who}'s credential`
        : `${server} is still waiting for a credential`,
  },
  mcp_tools_refused: {
    tone: "warning",
    title: ({ server, count }) =>
      count === 1 ? `1 ${server} tool is not offered` : `${count} ${server} tools are not offered`,
  },
  mcp_oauth_needs_sign_in: {
    tone: "warning",
    title: ({ server }) => `${server} needs somebody to sign in`,
  },
  mcp_oauth_expiring: {
    tone: "info",
    title: ({ server }) => `${server}'s sign-in runs out soon`,
  },
};

/** Each fix's button, where the page itself carries it out. */
export const FIX: Record<McpFixKind, string> = {
  restart: "Restart",
  integration: "See its sign-in",
  edit: "Change its prefix",
  sign_in: "See its sign-in",
};

/**
 * A configuration write's refusal in one sentence a person can act on; an
 * answer that refused nothing says nothing. `stage` is what was asked: a
 * check stores nothing whatever came back, and only the write itself may have
 * landed when its answer did not.
 */
export function writeRefusalWords(
  outcome: ConfigWriteOutcome | { kind: "missing" },
  name: string,
  stage: "check" | "write",
): string {
  switch (outcome.kind) {
    case "valid":
    case "saved":
      return "";
    case "missing":
      return `The configuration no longer has a server called ${name}.`;
    case "guarded":
      return configGuardedReason(outcome.code);
    case "managed":
      return managedSentence(outcome.managedBy);
    case "conflict":
      return outcome.reason === "no_active_revision"
        ? "No company is configured any more, so there is no server to change."
        : "The configuration changed while this was open, so nothing was stored. Close this and try again.";
    case "draining":
      return `This node is draining and stored nothing${outcome.detail ? `: ${outcome.detail}` : "."}`;
    case "starting":
      return `This node is still starting and stored nothing${outcome.detail ? `: ${outcome.detail}` : "."}`;
    case "unavailable":
      return `Nothing was stored${outcome.detail ? `: ${outcome.detail}` : "."} Try again in a moment.`;
    case "unsettled":
      return stage === "check"
        ? `${unsettledSentence(outcome)} Nothing was changed.`
        : `${unsettledSentence(outcome)} The change may or may not have landed. Close this and look at the list before trying again.`;
    case "problems":
      return outcome.problems.map((p) => p.message).join(" ");
  }
}
