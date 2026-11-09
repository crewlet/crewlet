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
