/**
 * Words for a proposal's taint: the surface whose outside text the turn read
 * before proposing (D25). A caution, never a verdict: nothing here holds Approve.
 */

import type { AssistantTaintSource } from "~/contract/assistant.ts";

/** Each surface, as "Prepared after reading …" finishes it. */
export const TAINT_SOURCE_WORDS: Readonly<Record<AssistantTaintSource, string>> = {
  page_body: "a page",
  page_comment: "a comment on a page",
  work_description: "a task's description",
  work_comment: "a comment on a task",
  project_file: "a project's file",
  chat_thread: "chat messages",
  chat_room: "a chat room's topic",
  artifact_file: "an artifact's file",
  artifact_row: "an artifact's data",
  artifact_comment: "a comment on an artifact",
  change_note: "a note about a change",
  alert: "a monitoring alert",
  notification: "a message from an integration",
  a2a_message: "a message between agents",
  executor: "an agent's question",
  executor_session: "an agent's earlier run",
  brand_voice: "the brand voice",
  api_answer: "records the API answered with",
  page_context: "the screen you have open",
  repo_file: "a file in a repository",
  repo_patch: "a file's changes in a repository",
  repo_commit: "a commit message",
  change_description: "a code change's description",
  change_review: "a code change's review",
  change_comment: "a comment on a code change",
  check_log: "a check's log",
  turn_ending: "how an agent's earlier turn ended",
  server_output: "a server's output",
  delegated_request: "an agent's delegated request",
  atlas: "the Atlas",
  web_page: "a web page",
  browser_page: "a page in an agent's browser",
  assistant_instructions: "Ask Crewlet's instructions",
};

/** The sentence a proposal prepared after reading outside text leads with. */
export function cautionOf(taint: AssistantTaintSource): string {
  return `Prepared after reading ${TAINT_SOURCE_WORDS[taint]}, which may hold words written outside the company. Check that this is the change you asked for.`;
}
