/**
 * Settings › MCP & tools' calls: one server whole (`GET /mcp-servers/{name}`),
 * its restart (`POST /mcp-servers/{name}/retry`), its test
 * (`POST /mcp-servers/{name}/check`), a value stored for it
 * (`PUT /mcp-servers/{name}/values/{key}`), turning it off and on again and
 * who gets it (the entity route, `enabled` and `access`), its tools' levels
 * (`PUT /mcp-servers/{name}/tools`), its changed tools reviewed
 * (`POST /mcp-servers/{name}/changes`), and its removal
 * (`DELETE /config/mcp-servers/{name}`);
 * and for the add and edit wizard, the catalogue (`GET /mcp-catalogue`), an
 * address checked before it is added (`POST /mcp-probe`) and a draft's test
 * (`POST /mcp-checks`). The list is the `mcp_servers_status` question, which
 * `data.ts` asks.
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
  McpCatalogueAnswer,
  McpChangesAnswer,
  McpCheckAnswer,
  McpMemberServerAnswer,
  McpProbeAnswer,
  McpRemovalCheck,
  McpRemovalWritten,
  McpRestartAnswer,
  McpServerAnswer,
  McpToolLevel,
  McpValueAnswer,
} from "~/contract/mcp.ts";
import { answerOf, getEntity, outcomeOf, putEntity } from "~/protocol/configWrite.ts";
import type { ConfigWriteOutcome } from "~/protocol/configWrite.ts";
import { isAbort, pathSegment, rest, RestError, type RestResponse } from "~/protocol/rest.ts";
import { accessEntity, type Access } from "./GrantPicker.tsx";

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
      /** The refusal's body whole, for a field a flow reads (a failed test, the problems). */
      body: Record<string, unknown>;
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
        body: err.body,
      };
    }
    return {
      kind: "refused",
      status: 0,
      code: "unreachable",
      sentence: "The engine could not be reached.",
      body: {},
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

/**
 * `GET /mcp-catalogue`: the curated catalogue whole, and with `q` the
 * company's community registry searched for it. Rejects on a refusal, as a
 * read does.
 */
export async function readCatalogue(q: string, signal: AbortSignal): Promise<McpCatalogueAnswer> {
  const answer = await rest.request("GET", "/mcp-catalogue", {
    ...(q ? { query: { q } } : {}),
    signal,
  });
  return answer.body as McpCatalogueAnswer;
}

/**
 * `POST /mcp-probe`: an address checked before a server is added at it, with
 * no credential sent. The engine dials it as the server would be dialled once
 * added, so it asks the rights a write does.
 */
export async function probeAddress(
  url: string,
  signal?: AbortSignal,
): Promise<McpAnswer<McpProbeAnswer>> {
  return settle(
    rest.request("POST", "/mcp-probe", { body: { url }, ...(signal ? { signal } : {}) }),
  );
}

/**
 * `POST /mcp-servers/{name}/check`: the saved server tested on every node
 * that runs it, or a per-agent one as `seat`. Recorded as its latest test.
 */
export async function testServer(name: string, seat: string): Promise<McpAnswer<McpCheckAnswer>> {
  return settle(
    rest.request("POST", `/mcp-servers/${pathSegment(name)}/check`, {
      body: seat ? { seat } : {},
    }),
  );
}

/**
 * `POST /mcp-checks`: a draft entry tested as a write would leave it, with
 * the values typed for its keys and not stored yet, each read through the
 * variable it will be stored under and never written. A draft the engine's
 * dry run refuses comes back as its write would (`validation_error` with
 * its problems).
 */
export async function testDraft(
  entity: Record<string, unknown>,
  values: Readonly<Record<string, string>>,
  seat: string,
  signal?: AbortSignal,
): Promise<McpAnswer<McpCheckAnswer>> {
  return settle(
    rest.request("POST", "/mcp-checks", {
      body: {
        entity,
        ...(seat ? { seat } : {}),
        ...(Object.keys(values).length > 0 ? { values } : {}),
      },
      ...(signal ? { signal } : {}),
    }),
  );
}

/**
 * `PUT /mcp-servers/{name}/values/{key}`: a value stored write-only, tested
 * first unless `test` is false, and the key pointed at the variable it is
 * sealed under (or that revision re-activated). Sent without a signal: once
 * sent, it runs to its answer. A value the server refuses is
 * `value_test_failed`, with the test in its body, and stores nothing.
 */
export async function storeValue(
  name: string,
  key: string,
  value: string,
  options: { seat?: string; test: boolean },
): Promise<McpAnswer<McpValueAnswer>> {
  return settle(
    rest.request("PUT", `/mcp-servers/${pathSegment(name)}/values/${pathSegment(key)}`, {
      body: { value },
      query: {
        ...(options.seat ? { seat: options.seat } : {}),
        ...(options.test ? {} : { test: "false" }),
      },
    }),
  );
}

/** A change of a server's tool levels: its default, and each tool's own by the server's name for it (null clears). */
export interface LevelsChange {
  default?: McpToolLevel | null;
  levels?: Readonly<Record<string, McpToolLevel | null>>;
}

/**
 * `PUT /mcp-servers/{name}/tools`: the levels the change names, written as
 * one revision under `summary`; every level it leaves out stays. A
 * configuration write, so the transport asks for a fresh step-up first.
 */
export async function setLevels(
  name: string,
  change: LevelsChange,
  summary: string,
): Promise<McpAnswer<unknown>> {
  return settle(
    rest.request("PUT", `/mcp-servers/${pathSegment(name)}/tools`, {
      body: { ...change, _summary: summary },
    }),
  );
}

/**
 * Write who gets a server, through the entity route: the entry read with its
 * own revision's tag and written back with only `access` changed (left out
 * for every agent), so nothing else the entry carries moves.
 */
export async function saveAccess(
  name: string,
  access: Access,
  summary: string,
  signal?: AbortSignal,
): Promise<ConfigWriteOutcome | { kind: "missing" }> {
  const controller = signal ? null : new AbortController();
  const used = signal ?? controller!.signal;
  const read = await getEntity("mcp-servers", name, used);
  if (read.kind !== "entity") return read;
  const { access: _was, ...entry } = read.entity;
  const written = accessEntity(access);
  return putEntity(
    "mcp-servers",
    name,
    written ? { ...entry, access: written } : entry,
    read.etag,
    summary,
    used,
  );
}

/** A review of a server's changed tools: each tool by the server's own name, and the digest reviewed (null for a removal). */
export interface ChangesDecision {
  accept?: Readonly<Record<string, string | null>>;
  block?: Readonly<Record<string, string | null>>;
}

/**
 * `POST /mcp-servers/{name}/changes`: each change accepted, or kept blocked,
 * by the digest of the definition reviewed, written under `summary`. A
 * configuration write, so the transport asks for a fresh step-up first. A
 * definition the server swapped since the review is `409 stale_version`, and
 * nothing is written.
 */
export async function reviewChanges(
  name: string,
  decision: ChangesDecision,
  summary: string,
): Promise<McpAnswer<McpChangesAnswer>> {
  return settle(
    rest.request("POST", `/mcp-servers/${pathSegment(name)}/changes`, {
      body: { ...decision, _summary: summary },
    }),
  );
}
