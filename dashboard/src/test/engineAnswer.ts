/**
 * What a stubbed `fetch` answers WITH WHEN IT PLAYS THE ENGINE.
 *
 * The engine marks every answer its API writes ([ENGINE_HEADER]), and the
 * dashboard reads an answer without the mark as something in front of the
 * engine (`protocol/engineAnswer.ts`): a refusal a stub writes without it is a
 * proxy's, and reads as an outcome nobody knows. So a stub standing in for the
 * engine says so, through these headers; one standing in for a proxy leaves
 * them off, which is the whole of the difference.
 */

import { ENGINE_HEADER, ENGINE_HEADER_VALUE } from "~/contract/engine.ts";

/** The headers every answer the engine writes carries. */
export const ENGINE_HEADERS: Readonly<Record<string, string>> = {
  [ENGINE_HEADER]: ENGINE_HEADER_VALUE,
};

/** A JSON answer as the engine writes one: its status, its body, the engine's mark. */
export function engineJson(
  payload: unknown,
  status: number,
  headers: Record<string, string> = {},
): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json", ...ENGINE_HEADERS, ...headers },
  });
}
