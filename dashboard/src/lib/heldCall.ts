/**
 * A call an agent's run holds for a person's approval (ADR-0077), and the one
 * way to answer it: `POST /mcp-approvals/{id}`.
 *
 * ONLY THIS ROUTE ANSWERS A HELD CALL. The run waits on it as it waits on a
 * question, but the run's own answer path (`answer_executor`, a chat reply)
 * refuses or spends an answer to a held call, so a screen that drew one as a
 * plain question would offer a button the engine refuses.
 *
 * ALWAYS ALLOW IS A CONFIGURATION WRITE: it also sets the tool's level to
 * Always allow, so the engine asks for a fresh step-up first. The transport
 * asks for it and replays the answer (`app/StepUp.tsx`), so nothing here
 * draws its own.
 */

import type { McpApprovalAnswer, McpHeldWhole } from "~/contract/mcp.ts";
import type { ExecutorRow } from "~/protocol/index.ts";
import { releaseRequestOf } from "./delegation.ts";
import { pathSegment, rest } from "~/protocol/rest.ts";

/** How a held call is answered: made once, made and its tool set to Always allow, or refused. */
export type HeldAnswer = "once" | "always" | "deny";

/**
 * Answer the held call `id`. Rejects with the engine's refusal: `409
 * stale_version` where it is no longer waiting (answered, or its run moved
 * on), `403` where the reader is neither an administrator nor a lead of the
 * agent's team.
 */
export async function answerHeldCall(
  id: string,
  answer: HeldAnswer,
  argumentsSha256?: string,
): Promise<McpApprovalAnswer> {
  const sent = await rest.request("POST", `/mcp-approvals/${pathSegment(id)}`, {
    body: argumentsSha256 ? { answer, arguments_sha256: argumentsSha256 } : { answer },
  });
  return sent.body as McpApprovalAnswer;
}

/**
 * The held call `id` whole, as whoever may answer it reads it before Allow
 * once of a call its tool asks about every time: every argument, and the
 * digest the answer names.
 */
export async function readHeldCall(id: string): Promise<McpHeldWhole> {
  const read = await rest.request("GET", `/mcp-approvals/${pathSegment(id)}`);
  return read.body as McpHeldWhole;
}

/** What an answer did, in a sentence for the row it was given on. */
export function heldAnswerWords(answer: HeldAnswer, sent: McpApprovalAnswer): string {
  if (sent.outcome === "unknown") {
    return "Sent, and it may not have reached the run yet. Answering again is safe.";
  }
  switch (answer) {
    case "once":
      return "Allowed once. The run carries on with the call made.";
    case "always":
      return "Always allowed. The run carries on, and the next call runs without asking.";
    case "deny":
      return "Denied. The agent is told the call was refused and carries on without it.";
  }
}

/** What a held call was answered, said, for one somebody answered already (`once`, `always`, `deny`). */
export function answeredWords(answer: string, by: string): string {
  const what =
    answer === "once"
      ? "Allowed once"
      : answer === "always"
        ? "Always allowed"
        : answer === "deny"
          ? "Denied"
          : "Answered";
  return by ? `${what} by ${by}.` : `${what}.`;
}

/** What a held call's title is read from: its act, its tool and what the reader is shown of it. */
export interface HeldCallSubject {
  /** What approving it does (`held_call.act`); absent for an MCP tool's call. */
  act?: string | undefined;
  /**
   * A call its tool asks about every time (`held_call.fixed`): an engine
   * tool's, whose question the tool words itself.
   */
  fixed?: boolean | undefined;
  tool: string;
  /** The server as a person knows it. */
  server: string;
  /** What it would send, where this reader is shown it. */
  args?: Record<string, unknown> | null | undefined;
  /** The engine's own question for it, where the caller has the run's. */
  question?: string | undefined;
}

/**
 * A GitHub pull request's address or a change's reference (`web!42`), as a
 * person names it; anything else is said as written.
 */
function changeWords(change: string): string {
  const pull = /^https?:\/\/[^/]+\/([^/]+\/[^/]+)\/pull\/(\d+)(?:[/?#].*)?$/i.exec(change.trim());
  if (pull) return `pull request ${pull[2]} in ${pull[1]}`;
  const ref = /^([^!\s]+)!(\d+)$/.exec(change.trim());
  return ref ? `change ${ref[2]} in ${ref[1]}` : change.trim();
}

/**
 * What a held call asks, as its title: ONE SOURCE for the decision row, the
 * Inbox's list and its reading pane, so the three never word one call two ways.
 *
 * IN PRODUCT WORDS FROM THE CALL'S OWN ARGUMENTS where the reader is shown
 * them: a merge names its change, a deploy its repository and environment, a
 * command its server. A reader not shown them, and any other call its tool
 * asks about every time, is asked in the engine's own question, which that
 * tool words. Only a call neither says (an MCP tool's) is named by its tool,
 * which is all anybody knows it by. The raw call stays under Arguments
 * ([heldCallNamed]).
 */
export function heldCallTitle(call: HeldCallSubject): string {
  const request = releaseRequestOf(call.act, call.args);
  if (request?.act === "pr_merge") return `Merge ${changeWords(request.change)}?`;
  if (request?.act === "deploy") return `Deploy ${request.repo} to ${request.environment}?`;
  const host = call.act === "ssh_operate" ? call.args?.host : undefined;
  if (typeof host === "string" && host.trim()) return `Run a command on ${host.trim()}?`;
  // THE TOOL'S OWN QUESTION, for every call its tool words: one it asks about
  // every time, whatever its act. An MCP tool's question is only its name again.
  const question = (call.question ?? "").trim();
  const worded = call.fixed === true || (call.act !== undefined && call.act !== "other");
  if (question && worded) return question;
  return heldCallNamed(call);
}

/** The call by its tool and server, as it was made: "Call ask_to_merge on crewlet?". */
export function heldCallNamed(call: Pick<HeldCallSubject, "tool" | "server">): string {
  return `Call ${call.tool} on ${call.server}?`;
}

/** The call a parked run holds, as its title is read from it, or null for a run parked on a question. */
export function heldRunCall(
  run: Pick<ExecutorRow, "held_call" | "held_arguments" | "question">,
): HeldCallSubject | null {
  const held = run.held_call;
  if (!held) return null;
  return {
    act: held.act,
    fixed: held.fixed,
    tool: held.tool,
    server: held.server,
    args: run.held_arguments,
    question: run.question,
  };
}

/** The title of the call a parked run holds ([heldCallTitle]), or null for a run parked on a question. */
export function heldRunTitle(
  run: Pick<ExecutorRow, "held_call" | "held_arguments" | "question">,
): string | null {
  const call = heldRunCall(run);
  return call ? heldCallTitle(call) : null;
}
