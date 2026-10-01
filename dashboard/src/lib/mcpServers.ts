/**
 * MCP servers: what a server's condition MEANS to the person reading it, which
 * seats it reaches, and what an added one is written as.
 *
 * PURE, like `lib/ceilings.ts`: the screen and the add dialog read these, and
 * every rule is a value a suite can pin without a socket.
 *
 * NOTHING HERE DECIDES A STATE OR A GRANT. The state is the engine's
 * (`mcp_servers_status`, from every node's heartbeat) and the grant is the
 * engine's too — each agent seat's `tool_sources` on the pushed org, resolved
 * by `config.MCPServer.Grants`, the ONE rule the engine starts children by.
 * This module only puts both into words, and builds the entity an add sends.
 */

import type { McpServerState, McpServerStatus } from "~/contract/mcp.ts";
import type { ConfigProblem } from "~/protocol/types.ts";
import type { Seat } from "./seats.ts";

/**
 * Each state in words, with the tone its pill wears and what to do about it.
 *
 * EXHAUSTIVE OVER THE CONTRACT'S UNION, so a state the engine adds fails the
 * typecheck here rather than drawing as nothing.
 */
export const SERVER_STATE_WORDS: Record<
  McpServerState,
  { label: string; tone: "success" | "warning" | "danger" | "neutral"; hint: string }
> = {
  running: {
    label: "Running",
    tone: "success",
    hint: "Every instance a node launched started and listed its tools.",
  },
  partial: {
    label: "Partly failing",
    tone: "warning",
    hint: "Some instances started and some did not — look at the node or the seat named beside the failure rather than at the server.",
  },
  failing: {
    label: "Failing",
    tone: "danger",
    hint: "Every instance a node launched failed to start: the command, the address or the credentials every seat shares.",
  },
  not_started: {
    label: "Not started",
    tone: "neutral",
    hint: "No node started it: a per-seat server no seat on a live node declares credentials for, or a change no node has applied yet.",
  },
  unreported: {
    label: "Not reported",
    tone: "neutral",
    hint: "No live node reports its MCP servers — every one runs an older build — so whether it started is unknown.",
  },
};

/** The registry's origin for a server's tools: `mcp:<name>`. */
export function serverOrigin(server: string): string {
  return `mcp:${server}`;
}

/**
 * The agent seats the engine GRANTS a server, from each seat's own
 * `tool_sources` — or null where that is unknown.
 *
 * NULL IS NOT "NOBODY". An agent seat with no `tool_sources` is one the engine
 * did not describe (a node older than the field serves the org), and an empty
 * list drawn over that reads as "nobody can call this", which is a claim
 * about somebody's company. A human seat runs no tools and is never counted.
 */
export function grantedSeats(seats: readonly Seat[], server: string): Seat[] | null {
  const agents = seats.filter((s) => s.kind === "agent");
  if (agents.some((s) => !Array.isArray(s.raw.tool_sources))) return null;
  const origin = serverOrigin(server);
  return agents.filter((s) => (s.raw.tool_sources ?? []).includes(origin));
}

/**
 * Who a server reaches, in words — and whether that is KNOWN.
 *
 * ONE COPY for the servers' grid and a server's own page. A SHARED server
 * reaches every agent seat by the engine's rule (`config.MCPServer.Grants`
 * returns true for every agent when the server is shared), so its reach is
 * known even on a roster that carries no `tool_sources`. A per-seat server's
 * reach on such a roster is UNKNOWN, and is said as unknown: "not on this
 * roster" read as "nobody", which is the very collapse [grantedSeats] exists
 * to refuse.
 */
export function reachOf(
  server: { name: string; shared: boolean },
  seats: readonly Seat[],
): { text: string; known: boolean } {
  const granted = grantedSeats(seats, server.name);
  if (granted === null) {
    return server.shared
      ? { text: "Every agent seat", known: true }
      : { text: "Unknown: this node's roster does not say", known: false };
  }
  const agents = seats.filter((s) => s.kind === "agent").length;
  if (granted.length === 0) return { text: "No seat", known: true };
  if (granted.length === agents) return { text: "Every agent seat", known: true };
  return { text: `${granted.length} ${granted.length === 1 ? "seat" : "seats"}`, known: true };
}

/**
 * The servers line under the catalogue's MCP tile: how many of the servers the
 * engine reports have an instance up somewhere.
 *
 * FROM THE STATUS ANSWER, never from the tools' origins. Counted off the
 * registry, a company with four configured servers all failing read "0 server(s)"
 * directly above a table listing the four — two answers to one question on one
 * screen. A server is up where at least one instance STARTED, which is what
 * `running` and `partial` both mean.
 */
export function serversUpLine(servers: readonly { started: number }[]): string {
  const up = servers.filter((s) => s.started > 0).length;
  const total = servers.length;
  return `${up} of ${total} ${total === 1 ? "server" : "servers"} running`;
}

// ---------------------------------------------------------------------------
// Adding one
// ---------------------------------------------------------------------------

/**
 * The names an add would collide with: the servers the CONFIGURATION carries.
 *
 * NOT EVERY ROW OF THE STATUS ANSWER. A `configured: false` row is a server
 * only a node reports — one running from an earlier revision that node has not
 * left yet — and the engine's create accepts that name, since the create is
 * judged against the configuration alone. Refusing it here would refuse what
 * the engine allows, with a sentence ("already exists") that is false.
 */
export function takenNames(servers: readonly McpServerStatus[] | null | undefined): Set<string> {
  return new Set((servers ?? []).filter((s) => s.configured).map((s) => s.name));
}

/**
 * The names only a node still reports ([takenNames]'s complement in the
 * answer): free to add, and worth a word beside the field, so a reader who
 * saw the name in the list is not left wondering why it was accepted.
 */
export function nodeOnlyNames(servers: readonly McpServerStatus[] | null | undefined): Set<string> {
  return new Set((servers ?? []).filter((s) => !s.configured).map((s) => s.name));
}

/**
 * The example in the add form's Name field.
 *
 * NEVER A NAME THE COMPANY ALREADY HAS: an example is something a reader may
 * copy, and one that equals a taken name walks them straight into "already
 * exists". The candidates are generic nouns rather than vendors, so the
 * example does not read as a recommendation either.
 */
export const NAME_EXAMPLES = ["notes", "files", "calendar", "search", "warehouse"] as const;

export function nameExample(taken: ReadonlySet<string>): string {
  const free = NAME_EXAMPLES.find((n) => !taken.has(n));
  if (free) return free;
  let i = 2;
  while (taken.has(`notes-${i}`)) i++;
  return `notes-${i}`;
}

/**
 * One `KEY: value` row of an environment or a header set.
 *
 * `id` IS THE ROW'S IDENTITY, minted when the row is added and never sent. A
 * row's position is not one: removing a middle row shifts every later index,
 * so a list keyed on it hands one row's field state — a secret's reveal
 * toggle, an open `$` completion — to its neighbour.
 */
export interface Pair {
  id: string;
  key: string;
  value: string;
}

let pairSeq = 0;

/** A new, empty row with an identity no other row in this page holds. */
export function newPair(): Pair {
  pairSeq += 1;
  return { id: `pair-${pairSeq}`, key: "", value: "" };
}

/** What the add form holds, as typed. */
export interface ServerForm {
  name: string;
  transport: "stdio" | "http";
  /** One instance for the company, or one per seat granted it. */
  sharing: "shared" | "per_seat";
  command: string;
  /** One argument per line, so an argument may hold a space. */
  args: string;
  env: Pair[];
  url: string;
  headers: Pair[];
  toolPrefix: string;
}

export function emptyServerForm(): ServerForm {
  return {
    name: "",
    transport: "stdio",
    sharing: "shared",
    command: "",
    args: "",
    env: [],
    url: "",
    headers: [],
    toolPrefix: "",
  };
}

/** The fields a form error or an engine problem is drawn beside. */
export type ServerField = "name" | "command" | "args" | "env" | "url" | "headers" | "toolPrefix";

/** The arguments as the list the config takes: one per non-blank line. */
export function argsOf(text: string): string[] {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
}

/**
 * What stops the form being sent, field by field.
 *
 * ONLY WHAT THE ENGINE WOULD REFUSE ANYWAY, said before the round trip: a
 * missing name, command or address, a name another server already has, and a
 * pair whose key is missing or repeated (a map cannot hold two). Everything
 * else is the engine's to judge — the check runs before any save.
 */
export function formErrors(
  form: ServerForm,
  /** Names the CONFIGURATION carries — see [takenNames]. */
  taken: ReadonlySet<string>,
): Partial<Record<ServerField, string>> {
  const out: Partial<Record<ServerField, string>> = {};
  const name = form.name.trim();
  if (name === "") out.name = "Name the server.";
  else if (taken.has(name)) out.name = `A server called ${name} already exists.`;
  if (form.transport === "stdio") {
    if (form.command.trim() === "") out.command = "Give the command that launches it.";
    const env = pairsError(form.env, "variable");
    if (env) out.env = env;
  } else {
    if (form.url.trim() === "") out.url = "Give the address it answers on.";
    const headers = pairsError(form.headers, "header");
    if (headers) out.headers = headers;
  }
  return out;
}

function pairsError(pairs: readonly Pair[], noun: string): string | undefined {
  const seen = new Set<string>();
  for (const { key, value } of pairs) {
    const k = key.trim();
    if (k === "" && value.trim() === "") continue;
    if (k === "") return `Name every ${noun} that has a value.`;
    if (seen.has(k)) return `${k} is listed twice.`;
    seen.add(k);
  }
  return undefined;
}

function pairsOf(pairs: readonly Pair[]): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  for (const { key, value } of pairs) {
    if (key.trim() === "") continue;
    out[key.trim()] = value.trim();
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/**
 * The `mcp_servers` entry the form describes.
 *
 * ONLY THE CHOSEN TRANSPORT'S FIELDS: the engine refuses a stdio server
 * carrying a URL or headers (and an http one carrying a command or env)
 * rather than ignoring them, because a header nobody sends looks exactly like
 * one that was rejected. `shared` is written only to say FALSE — unset is the
 * shared default, and writing `true` would put today's default into the
 * document as though somebody chose it.
 */
export function serverEntity(form: ServerForm): Record<string, unknown> {
  const out: Record<string, unknown> = { name: form.name.trim(), transport: form.transport };
  if (form.sharing === "per_seat") out.shared = false;
  if (form.transport === "stdio") {
    out.command = form.command.trim();
    const args = argsOf(form.args);
    if (args.length > 0) out.args = args;
    const env = pairsOf(form.env);
    if (env) out.env = env;
  } else {
    out.url = form.url.trim();
    const headers = pairsOf(form.headers);
    if (headers) out.headers = headers;
  }
  if (form.toolPrefix.trim() !== "") out.tool_prefix = form.toolPrefix.trim();
  return out;
}

/** The revision's audit summary: the sentence the history will show. */
export function addSummary(form: ServerForm): string {
  const how = form.sharing === "per_seat" ? "one per seat" : "shared";
  return `Add the MCP server ${form.name.trim()} (${form.transport}, ${how})`;
}

/**
 * The engine's problems placed beside the field each is about.
 *
 * BY THE PROBLEM'S OWN SEGMENTS, never its message: `["mcp_servers", 3,
 * "command"]` is the command, whatever the sentence says. A problem about the
 * rest of the company — the add is validated whole — is `rest`, said above the
 * form rather than dropped, because it is still why nothing was saved.
 */
export function problemsByField(problems: readonly ConfigProblem[]): {
  fields: Partial<Record<ServerField, string>>;
  rest: string[];
} {
  const fields: Partial<Record<ServerField, string>> = {};
  const rest: string[] = [];
  for (const p of problems) {
    const seg = p.segments ?? [];
    const key = seg[0] === "mcp_servers" && typeof seg[2] === "string" ? fieldOf(seg[2]) : null;
    if (key && !fields[key]) fields[key] = p.message;
    else rest.push(p.message);
  }
  return { fields, rest };
}

function fieldOf(key: string): ServerField | null {
  switch (key) {
    case "name":
      return "name";
    case "command":
      return "command";
    case "args":
      return "args";
    case "env":
      return "env";
    case "url":
      return "url";
    case "headers":
      return "headers";
    case "tool_prefix":
      return "toolPrefix";
    default:
      return null;
  }
}
