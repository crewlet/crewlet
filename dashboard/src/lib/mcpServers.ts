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
import { carriesReference } from "./references.ts";
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
  disabled: {
    label: "Disabled",
    tone: "neutral",
    hint: "Turned off. Nothing starts it and no agent gets its tools until it is turned back on.",
  },
  running: {
    label: "Running",
    tone: "success",
    hint: "All instances started",
  },
  partial: {
    label: "Partly failing",
    tone: "warning",
    hint: "Some instances failed. See the node or seat named beside it.",
  },
  failing: {
    label: "Failing",
    tone: "danger",
    hint: "Every instance failed. Check the command, address or credentials.",
  },
  waiting: {
    label: "Waiting for credentials",
    tone: "neutral",
    hint: "An integration is minting a seat's credential. The server starts once it is sealed.",
  },
  not_started: {
    label: "Not started",
    tone: "neutral",
    hint: "No node has started it yet.",
  },
  unreported: {
    label: "Not reported",
    tone: "neutral",
    hint: "No node has reported its status.",
  },
};

/** The registry's origin for a server's tools: `mcp:<name>`. */
export function serverOrigin(server: string): string {
  return `mcp:${server}`;
}

/**
 * The agent seats the engine GRANTS a server, from each seat's own
 * `tool_sources` — which the engine puts on every agent seat it pushes,
 * resolved by `config.MCPServer.Grants`. A human seat runs no tools and is
 * never counted.
 */
export function grantedSeats(seats: readonly Seat[], server: string): Seat[] {
  const origin = serverOrigin(server);
  return seats.filter((s) => s.kind === "agent" && (s.raw.tool_sources ?? []).includes(origin));
}

/**
 * Who a server reaches, in words. ONE COPY for the servers' grid and a
 * server's own page.
 */
export function reachOf(server: { name: string }, seats: readonly Seat[]): string {
  const granted = grantedSeats(seats, server.name);
  const agents = seats.filter((s) => s.kind === "agent").length;
  if (granted.length === 0) return "No seat";
  if (granted.length === agents) return "Every agent seat";
  return `${granted.length} ${granted.length === 1 ? "seat" : "seats"}`;
}

/**
 * The servers line under the catalogue's MCP tile: how many of the servers the
 * engine reports have an instance up somewhere.
 *
 * FROM THE STATUS ANSWER, never from the tools' origins. Counted off the
 * registry, a company with four configured servers all failing read "0 server(s)"
 * directly above a table listing the four — two answers to one question on one
 * screen. A server is up where at least one instance STARTED, which is what
 * `running` and `partial` both mean. A server turned off is not counted at all:
 * nobody expects it to run, and counted it read as one that should and does not.
 */
export function serversUpLine(servers: readonly { started: number; enabled: boolean }[]): string {
  const on = servers.filter((s) => s.enabled);
  const up = on.filter((s) => s.started > 0).length;
  const total = on.length;
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
 *
 * `optional` is the row's place in the server's `optional_keys`: a value
 * whose `${NAME}` resolves to nothing is then left out of what the server is
 * started with, where any other such value holds the server back unstarted.
 */
export interface Pair {
  id: string;
  key: string;
  value: string;
  optional: boolean;
}

let pairSeq = 0;

/** A new, empty row with an identity no other row in this page holds. */
export function newPair(): Pair {
  pairSeq += 1;
  return { id: `pair-${pairSeq}`, key: "", value: "", optional: false };
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
 * missing name, command or address, a name another server already has, a
 * pair whose key is missing or repeated (a map cannot hold two, and the
 * engine holds a header written twice in two cases to be one), and a pair
 * that may be unset while its value names nothing that could be (an
 * `optional_keys` entry leaving nothing out). Everything else is the
 * engine's to judge — the check runs before any save.
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
    const env = pairsError(form.env, "variable", (key) => key);
    if (env) out.env = env;
  } else {
    if (form.url.trim() === "") out.url = "Give the address it answers on.";
    // HTTP NAMES A HEADER IN ANY CASE, and the engine refuses one header
    // written twice in one map however it is spelled.
    const headers = pairsError(form.headers, "header", (key) => key.toLowerCase());
    if (headers) out.headers = headers;
  }
  return out;
}

/**
 * What stops one transport's rows being sent. `identity` is how two keys name
 * one: as written for a variable, in any case for a header.
 *
 * A row may be unset only where its value carries a reference ANYWHERE in it
 * — the only kind of value that can resolve to nothing — since `Bearer
 * ${TOKEN}` is held back, or left out, for its token exactly as `${TOKEN}`
 * is.
 */
function pairsError(
  pairs: readonly Pair[],
  noun: string,
  identity: (key: string) => string,
): string | undefined {
  const seen = new Map<string, string>();
  for (const { key, value, optional } of pairs) {
    const k = key.trim();
    if (k === "" && value.trim() === "") continue;
    if (k === "") return `Name every ${noun} that has a value.`;
    const was = seen.get(identity(k));
    if (was === k) return `${k} is listed twice.`;
    if (was !== undefined) return `${was} and ${k} are one ${noun}: write it once.`;
    if (optional && !carriesReference(value)) {
      return `${k} names no secret, so it can’t be unset. Untick its “may be unset” box.`;
    }
    seen.set(identity(k), k);
  }
  return undefined;
}

/**
 * The keys of the rows that may be unset, as the server's `optional_keys`.
 * A row with no name is no key, and is sent nowhere.
 */
function optionalOf(pairs: readonly Pair[]): string[] | undefined {
  const out = pairs.filter((p) => p.optional && p.key.trim() !== "").map((p) => p.key.trim());
  return out.length > 0 ? out : undefined;
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
  const pairs = form.transport === "stdio" ? form.env : form.headers;
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
  // THE CHOSEN TRANSPORT'S ROWS ONLY, for the reason its fields are: a key
  // the other transport's rows hold is a key of nothing this entry carries,
  // which the engine refuses rather than ignores.
  const optional = optionalOf(pairs);
  if (optional) out.optional_keys = optional;
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
 * "command"]` is the command, whatever the sentence says. An `optional_keys`
 * entry is drawn beside the rows it was sent from, which are the transport's:
 * the environment's for a command, the headers' for an address. A problem
 * about the rest of the company — the add is validated whole — is `rest`,
 * said above the form rather than dropped, because it is still why nothing was
 * saved.
 */
export function problemsByField(
  problems: readonly ConfigProblem[],
  transport: ServerForm["transport"],
): {
  fields: Partial<Record<ServerField, string>>;
  rest: string[];
} {
  const fields: Partial<Record<ServerField, string>> = {};
  const rest: string[] = [];
  for (const p of problems) {
    const seg = p.segments ?? [];
    const key =
      seg[0] === "mcp_servers" && typeof seg[2] === "string" ? fieldOf(seg[2], transport) : null;
    if (key && !fields[key]) fields[key] = p.message;
    else rest.push(p.message);
  }
  return { fields, rest };
}

function fieldOf(key: string, transport: ServerForm["transport"]): ServerField | null {
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
    case "optional_keys":
      return transport === "stdio" ? "env" : "headers";
    default:
      return null;
  }
}
