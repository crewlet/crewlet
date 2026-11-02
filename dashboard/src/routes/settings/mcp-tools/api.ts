/**
 * Settings › MCP & tools' calls: one server whole (`GET /mcp-servers/{name}`),
 * its restart (`POST /mcp-servers/{name}/retry`), turning it off and on again
 * (the entity route, `enabled`), and its removal
 * (`DELETE /config/mcp-servers/{name}`). The list is the `mcp_servers_status`
 * question, which `data.ts` asks.
 *
 * EVERY ANSWER IS A VALUE. A refusal comes back as one, with the engine's code
 * and sentence, so a flow branches on what came back rather than catching.
 * Only the caller's own abort rejects. A `step_up_required` never reaches
 * here: the transport asks for the second factor and replays the request.
 *
 * A WRITE THAT HAS STARTED IS SENT WITHOUT A SIGNAL, so it runs to its answer
 * whatever happens to the screen: a removal stopped part way could leave a
 * person unsure whether it happened. The signal a write takes is for a dry
 * run the screen may abandon.
 */

import type {
  McpMemberServerAnswer,
  McpRemovalCheck,
  McpRemovalWritten,
  McpRestartAnswer,
  McpServerAnswer,
} from "~/contract/mcp.ts";
import { answerOf, getEntity, outcomeOf, putEntity } from "~/protocol/configWrite.ts";
import type { ConfigWriteOutcome } from "~/protocol/configWrite.ts";
import { isAbort, pathSegment, rest, RestError, type RestResponse } from "~/protocol/rest.ts";

/** One server whole, in the reading the caller is answered. */
export type ServerDetail = McpServerAnswer | McpMemberServerAnswer;

/** What one MCP route answered: its body, or the engine's refusal. */
export type McpAnswer<T> =
  | { kind: "ok"; status: number; value: T }
  | {
      kind: "refused";
      status: number;
      /** The engine's code: `not_found`, `conflict`, `no_such_seat`, ... */
      code: string;
      /** The engine's sentence for it, what a person is shown. */
      sentence: string;
    };

async function settle<T>(request: Promise<RestResponse>): Promise<McpAnswer<T>> {
  try {
    const answer = await request;
    return { kind: "ok", status: answer.status, value: answer.body as T };
  } catch (err) {
    if (isAbort(err)) throw err;
    if (err instanceof RestError) {
      return {
        kind: "refused",
        status: err.status,
        code: err.code,
        sentence: err.sentence || err.detail,
      };
    }
    return {
      kind: "refused",
      status: 0,
      code: "unreachable",
      sentence: "The engine could not be reached.",
    };
  }
}

/**
 * `GET /mcp-servers/{name}`: rejects on a refusal, as every read `useRest`
 * drives does, so the page draws the refusal where the server would be.
 */
export async function readServer(name: string, signal: AbortSignal): Promise<ServerDetail> {
  const answer = await rest.request("GET", `/mcp-servers/${pathSegment(name)}`, { signal });
  return answer.body as ServerDetail;
}

/**
 * `POST /mcp-servers/{name}/retry`: every instance restarted, or with `seat`
 * that agent's alone. `202` with the request as recorded; each node acts on
 * it as it hears it.
 */
export async function restartServer(
  name: string,
  seat: string,
): Promise<McpAnswer<McpRestartAnswer>> {
  return settle(
    rest.request("POST", `/mcp-servers/${pathSegment(name)}/retry`, {
      body: seat ? { seat } : {},
    }),
  );
}

/**
 * Turn a server off (`enabled: false`) or back on, through the entity route:
 * the entry read with its own revision's tag, written back with only `enabled`
 * changed, so nothing else the entry carries moves. On is the default, so
 * turning a server on leaves the field out rather than writing `true`.
 * `missing`: the active revision carries no server of that name.
 */
export async function setEnabled(
  name: string,
  enabled: boolean,
  summary: string,
  signal: AbortSignal,
): Promise<ConfigWriteOutcome | { kind: "missing" }> {
  const read = await getEntity("mcp-servers", name, signal);
  if (read.kind !== "entity") return read;
  const { enabled: _was, ...entry } = read.entity;
  return putEntity(
    "mcp-servers",
    name,
    enabled ? entry : { ...entry, enabled: false },
    read.etag,
    summary,
    signal,
  );
}

/** What a removal clears with the server, and which stored values go with it. */
export interface Removal {
  clear: boolean;
  deleteSecrets: readonly string[];
}

/** What a removal or its dry run came to. */
export interface RemovalAnswer {
  outcome: ConfigWriteOutcome;
  /** A dry run's answer whole, for its dependants; null on any other answer. */
  check: McpRemovalCheck | null;
  /** A written removal's answer whole, for what became of each value; null otherwise. */
  written: McpRemovalWritten | null;
}

/**
 * `DELETE /config/mcp-servers/{name}`: the server removed with every field
 * that names it alone and the values asked for, in one revision, or with
 * `dryRun` the same checked and stored nowhere. The removal states the
 * revision the dialog read its dependants from (`If-Match`).
 */
export async function removeServer(
  name: string,
  removal: Removal,
  options: { etag: string; summary: string; dryRun: boolean },
  signal?: AbortSignal,
): Promise<RemovalAnswer> {
  const answer = await answerOf(
    rest.request("DELETE", `/config/mcp-servers/${pathSegment(name)}`, {
      body: {
        clear: removal.clear,
        delete_secrets: removal.deleteSecrets,
        ...(options.dryRun ? {} : { _summary: options.summary }),
      },
      headers: options.etag ? { "If-Match": options.etag } : {},
      ...(options.dryRun ? { query: { dry_run: "true" } } : {}),
      ...(signal ? { signal } : {}),
    }),
  );
  const outcome = outcomeOf(answer);
  return {
    outcome,
    check: outcome.kind === "valid" ? (answer.body as McpRemovalCheck) : null,
    written: outcome.kind === "saved" ? (answer.body as McpRemovalWritten) : null,
  };
}
