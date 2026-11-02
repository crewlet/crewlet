/**
 * The add or edit wizard's draft, and the one way it becomes a company
 * document entry: what each step holds, the entry it writes, the values it
 * stores, and which step an engine problem belongs to.
 *
 * A VALUE NEVER RIDES THE ENTRY. A header or variable typed here is stored
 * write-only through `PUT /mcp-servers/{name}/values/{key}` once the entry is
 * written, and the engine decides the variable it is sealed under and points
 * the key at it. So a key with a new value is left out of a new entry (the
 * values route adds it), and a key the entry already reads keeps its own
 * spelling (the route reseals what it reads). The draft holds a typed value
 * only until then, and an edit shows a stored one as stored.
 *
 * AN EDIT WRITES BACK WHAT IT READ. The entry the configuration holds (as
 * `GET /config/mcp-servers/{name}` answers it, values masked) is the base,
 * and the draft changes only the fields its steps show: a field this wizard
 * does not draw (timeouts, annotations, `optional_keys`, `enabled`) is
 * written back as it was.
 *
 * THE ENGINE DECIDES. Nothing here judges a name, an address or a key: the
 * engine's dry run and its test do, and their problems land on the fields
 * by the paths they name (`stepOf`, `problemAt`). A pasted configuration is
 * read to fill the form and never run.
 */

import type { KeyValueRow } from "@crewlethq/ui";
import type {
  McpAuthMethod,
  McpCatalogueEntry,
  McpCheckedTool,
  McpToolLevel,
  McpValueState,
} from "~/contract/mcp.ts";
import type { ConfigProblem } from "~/protocol/types.ts";
import { accessEntity, accessOf, EVERY, type Access } from "../GrantPicker.tsx";
import { LEVELS } from "../words.ts";

/** Where a new server comes from. */
export type Source = "catalogue" | "url" | "json" | "community";

/** The wizard's steps, in order. An edit has no Source: a saved server is not re-sourced. */
export type Step =
  "source" | "name" | "connection" | "signin" | "access" | "approvals" | "test" | "review";

const ADD_STEPS: readonly Step[] = [
  "source",
  "name",
  "connection",
  "signin",
  "access",
  "approvals",
  "test",
  "review",
];
const EDIT_STEPS: readonly Step[] = ADD_STEPS.filter((s) => s !== "source");

/** The steps the wizard walks, adding or editing. */
export function stepsFor(editing: boolean): readonly Step[] {
  return editing ? EDIT_STEPS : ADD_STEPS;
}

/** Whether `s` is one of the wizard's steps, for a step the address names. */
export function isStep(s: string): s is Step {
  return (ADD_STEPS as readonly string[]).includes(s);
}

/** One header or variable the server is sent. */
export interface ValueRow extends KeyValueRow {
  /** The configuration already sends the key: its value is stored, and "" keeps it. */
  stored: boolean;
  /** The catalogue says the server needs it. */
  required: boolean;
  /** How a value is spelled, from the catalogue, or "". */
  format: string;
}

let minted = 0;

/** A row for a key, its value empty. */
export function newRow(key = "", options: Partial<Omit<ValueRow, "id" | "key">> = {}): ValueRow {
  minted += 1;
  return {
    id: `mcp-row-${minted}`,
    key,
    value: "",
    stored: false,
    required: false,
    format: "",
    ...options,
  };
}

/** What the address check or the catalogue found about signing in. */
export interface Detected {
  auth: McpAuthMethod;
  from: "server" | "catalogue";
  /** The vendor's name where the catalogue knows the address, else null. */
  title: string | null;
}

export interface Draft {
  source: Source;
  /** The catalogue entry the server is made from, or null. */
  catalogue: McpCatalogueEntry | null;
  name: string;
  description: string;
  transport: "http" | "stdio";
  url: string;
  command: string;
  /** One argument a line, passed as written. */
  args: string;
  /** Added before every tool name, with nothing between. */
  toolPrefix: string;
  /** How the engine proves who it is to the server. */
  signIn: McpAuthMethod;
  /** A remote server's headers. */
  headers: ValueRow[];
  /** A local server's variables. */
  env: ValueRow[];
  /**
   * One instance for the company, or one per agent, chosen on Who gets it. A
   * server an integration signs in for each agent runs one per agent.
   */
  shared: boolean;
  /** Who gets it: every agent, or the teams and agents its `access` names. */
  access: Access;
  /**
   * The level of every tool `levels` does not name (`tools.default`), or null
   * to leave each such tool at the level its hints start it at.
   */
  baseline: McpToolLevel | null;
  /** Each tool's own level, by the server's own name for it (`tools.levels`). */
  levels: Readonly<Record<string, McpToolLevel>>;
  /** The tools the draft's test listed, which the approvals step sets levels on. */
  listed: readonly McpCheckedTool[];
  /**
   * The keys an integration fills for each agent, which an edit writes back
   * as the entry spells them and draws as no row.
   */
  bound: readonly string[];
  detected: Detected | null;
  /** The sign-ins the server offers, from the catalogue; null where nobody has said. */
  supported: readonly McpAuthMethod[] | null;
}

export function emptyDraft(): Draft {
  return {
    source: "catalogue",
    catalogue: null,
    name: "",
    description: "",
    transport: "http",
    url: "",
    command: "",
    args: "",
    toolPrefix: "",
    signIn: "none",
    headers: [],
    env: [],
    shared: true,
    access: EVERY,
    baseline: null,
    levels: {},
    listed: [],
    bound: [],
    detected: null,
    supported: null,
  };
}

/**
 * The sign-in a catalogue entry starts on: its integration where it has one,
 * then the first of the methods it offers this wizard can draw. An OAuth-only
 * entry starts on none, which its Sign-in step says it cannot use.
 */
function firstSignIn(entry: McpCatalogueEntry): McpAuthMethod {
  if (entry.integration) return "integration";
  return entry.auth.find((a) => a === "headers" || a === "none") ?? "none";
}

/**
 * A draft from a catalogue entry: its launch, its sign-in and the keys a
 * person fills. A key the integration fills for each agent, or one the entry
 * sets to a fixed value, is not a row: the first is nobody's to type, the
 * second is written as the entry says (`fixedValues`).
 */
export function fromCatalogue(entry: McpCatalogueEntry, base: Draft): Draft {
  const rows = (inputs: McpCatalogueEntry["headers"]) =>
    inputs
      .filter((i) => !i.from_integration && i.value === "")
      .map((i) => newRow(i.name, { required: i.required, format: i.format }));
  const signIn = firstSignIn(entry);
  return {
    ...base,
    catalogue: entry,
    name: entry.name,
    description: entry.description,
    transport: entry.transport,
    url: entry.url,
    command: entry.command,
    args: entry.args.join("\n"),
    toolPrefix: entry.tool_prefix,
    signIn,
    headers: rows(entry.headers),
    env: rows(entry.env),
    shared: !entry.integration,
    detected: { auth: signIn, from: "catalogue", title: entry.title },
    supported: entry.auth,
  };
}

/** The keys a catalogue entry sets to a fixed value, written as it says. */
export function fixedValues(entry: McpCatalogueEntry | null): Record<string, string> {
  if (!entry) return {};
  const inputs = entry.transport === "http" ? entry.headers : entry.env;
  return Object.fromEntries(inputs.filter((i) => i.value !== "").map((i) => [i.name, i.value]));
}

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

const isLevel = (v: unknown): v is McpToolLevel =>
  typeof v === "string" && (LEVELS as readonly string[]).includes(v);

/** An entry's `tools` block as the draft holds it: its default and each tool's own level. */
function levelsOf(block: unknown): Pick<Draft, "baseline" | "levels"> {
  if (!isRecord(block)) return { baseline: null, levels: {} };
  const levels = isRecord(block.levels)
    ? Object.fromEntries(Object.entries(block.levels).filter(([, v]) => isLevel(v)))
    : {};
  return {
    baseline: isLevel(block.default) ? block.default : null,
    levels: levels as Record<string, McpToolLevel>,
  };
}

/**
 * A draft of a saved server, for an edit: the entry as written, each key the
 * server is sent a row marked stored, and the catalogue entry the engine
 * recognises it as. A key an integration fills for each agent is not a row.
 */
export function fromServer(
  entity: Record<string, unknown>,
  values: readonly McpValueState[],
  entry: McpCatalogueEntry | null,
  signIn: McpAuthMethod,
): Draft {
  const bound = new Set(values.filter((v) => v.bound_by).map((v) => v.key));
  const rows = (block: unknown) =>
    Object.keys(isRecord(block) ? block : {})
      .filter((k) => !bound.has(k))
      .sort((a, b) => a.localeCompare(b))
      .map((k) => newRow(k, { stored: true }));
  const http = entity.transport === "http" || (!entity.command && typeof entity.url === "string");
  const text = (v: unknown) => (typeof v === "string" ? v : "");
  return {
    ...emptyDraft(),
    source: entry ? "catalogue" : "url",
    catalogue: entry,
    name: text(entity.name),
    description: text(entity.description),
    transport: http ? "http" : "stdio",
    url: text(entity.url),
    command: text(entity.command),
    args: Array.isArray(entity.args) ? entity.args.map(String).join("\n") : "",
    toolPrefix: text(entity.tool_prefix),
    signIn,
    headers: http ? rows(entity.headers) : [],
    env: http ? [] : rows(entity.env),
    access: accessOf(entity.access),
    ...levelsOf(entity.tools),
    shared: entity.shared !== false,
    bound: [...bound],
    // The methods the entry offers and the one the server uses, which stays
    // choosable even where the catalogue has since stopped listing it.
    supported: entry ? [...new Set<McpAuthMethod>([...entry.auth, signIn])] : null,
  };
}

/** The rows of the transport the draft has chosen: its headers, or its variables. */
export function rowsOf(d: Draft): ValueRow[] {
  return d.transport === "http" ? d.headers : d.env;
}

/** Whether the draft's rows are sent at all: an integration's server takes none typed here for a remote one. */
function sendsRows(d: Draft): boolean {
  return d.transport === "stdio" || d.signIn === "headers";
}

/** The arguments, one a line, as the entry writes them. */
export function argsOf(text: string): string[] {
  return text
    .split("\n")
    .map((a) => a.trim())
    .filter(Boolean);
}

/** The fields this wizard writes; every other field of the base is written back as it was. */
const OWNED = [
  "name",
  "description",
  "transport",
  "url",
  "command",
  "args",
  "headers",
  "env",
  "tool_prefix",
  "shared",
  "access",
  "tools",
] as const;

/**
 * The entry the draft writes, `base` (an edit's entry as read) under it. A
 * key with a stored value keeps its spelling; a new key is left out, for the
 * values route to add with the value it is sent; a key the entry sets to a
 * fixed value is written as it says. Only the chosen transport's fields are
 * written, since the engine refuses an entry carrying the other's.
 */
export function entityOf(d: Draft, base: Record<string, unknown> | null): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(base ?? {})) {
    if (!(OWNED as readonly string[]).includes(k)) out[k] = v;
  }
  out.name = d.name.trim();
  out.transport = d.transport;
  if (d.description.trim()) out.description = d.description.trim();
  if (d.toolPrefix.trim()) out.tool_prefix = d.toolPrefix.trim();
  // `shared` is written only to say false: unset is the shared default.
  if (!d.shared) out.shared = false;
  // WHO GETS IT, left out for every agent. A server an integration signs in
  // for takes its agents from the integration, and the engine refuses
  // `access` on it.
  const access = d.signIn === "integration" ? undefined : accessEntity(d.access);
  if (access) out.access = access;
  // THE LEVELS, left out where none is set: each tool then starts at the
  // level its hints give it.
  if (d.baseline !== null || Object.keys(d.levels).length > 0) {
    out.tools = {
      ...(d.baseline !== null ? { default: d.baseline } : {}),
      ...(Object.keys(d.levels).length > 0 ? { levels: { ...d.levels } } : {}),
    };
  }
  const written = isRecord(base?.[d.transport === "http" ? "headers" : "env"])
    ? (base![d.transport === "http" ? "headers" : "env"] as Record<string, unknown>)
    : {};
  // THE CATALOGUE'S SWITCHES on a new server only: an edit's are rows, kept as
  // the entry spells them, whatever it was changed to since.
  const block: Record<string, unknown> = base ? {} : { ...fixedValues(d.catalogue) };
  // Keys an integration fills, which are no row, stay as the entry wrote them.
  for (const k of d.bound) {
    if (k in written) block[k] = written[k];
  }
  // AN OAUTH SERVER'S HEADERS are no row here, since this wizard does not set
  // its sign-in up, so they stay as the entry wrote them.
  if (d.signIn === "oauth") Object.assign(block, written);
  if (sendsRows(d)) {
    for (const r of rowsOf(d)) {
      const key = r.key.trim();
      if (key && r.stored && key in written) block[key] = written[key];
    }
  }
  const field = d.transport === "http" ? "headers" : "env";
  if (Object.keys(block).length > 0) out[field] = block;
  if (d.transport === "http") {
    out.url = d.url.trim();
  } else {
    out.command = d.command.trim();
    const args = argsOf(d.args);
    if (args.length > 0) out.args = args;
  }
  // AN OPTIONAL KEY THE ENTRY NO LONGER SENDS is not left behind as one.
  if (Array.isArray(out.optional_keys)) {
    const kept = out.optional_keys.filter((k) => typeof k === "string" && k in block);
    if (kept.length > 0) out.optional_keys = kept;
    else delete out.optional_keys;
  }
  return out;
}

/** The values typed for the draft's keys, trimmed, which are stored after the entry is written. */
export function valuesOf(d: Draft): Record<string, string> {
  if (!sendsRows(d)) return {};
  return Object.fromEntries(
    rowsOf(d)
      .filter((r) => r.key.trim() && r.value.trim())
      .map((r) => [r.key.trim(), r.value.trim()]),
  );
}

/**
 * The field of the entry an engine problem is about, by its path: the
 * entry's own field (`["mcp_servers", 3, "url"]`), or a value the values
 * route refused (`["values", "Authorization"]`). "" for a problem about the
 * rest of the company, which the review says whole.
 */
function fieldOf(p: ConfigProblem): { field: string; key: string } {
  const s = p.segments ?? [];
  if (s[0] === "values") return { field: "values", key: String(s[1] ?? "") };
  if (s[0] === "mcp_servers" && typeof s[2] === "string") {
    return { field: s[2], key: typeof s[3] === "string" ? s[3] : "" };
  }
  return { field: "", key: "" };
}

/** The step an engine problem belongs to, by the field it names. */
export function stepOf(p: ConfigProblem, transport: Draft["transport"]): Step {
  const { field } = fieldOf(p);
  switch (field) {
    case "name":
    case "description":
      return "name";
    case "access":
    case "shared":
      return "access";
    case "tools":
      return "approvals";
    case "url":
    case "command":
    case "args":
    case "transport":
    case "tool_prefix":
      return "connection";
    case "env":
    case "optional_keys":
      return transport === "stdio" ? "connection" : "signin";
    case "headers":
      return "signin";
    case "values":
      return transport === "stdio" ? "connection" : "signin";
    default:
      return "review";
  }
}

/** The problem on one field, and for a header or variable, on one key; undefined for none. */
export function problemAt(
  problems: readonly ConfigProblem[],
  field: string,
  key?: string,
): string | undefined {
  return problems.find((p) => {
    const at = fieldOf(p);
    const fields = field === "rows" ? ["headers", "env", "values", "optional_keys"] : [field];
    return fields.includes(at.field) && (key === undefined || at.key === key);
  })?.message;
}

/** A server name made from a title or a listing's name: what the person may change. */
export function nameFrom(text: string): string {
  return text
    .toLowerCase()
    .replace(/-?mcp(-server)?$/, "")
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-+|-+$/g, "");
}

/**
 * A server name made from an address: its domain's own label, past the
 * `mcp.` a vendor puts in front (`mcp.linear.app` is `linear`).
 */
export function nameFromAddress(url: string): string {
  let host: string;
  try {
    host = new URL(url).hostname;
  } catch {
    return "";
  }
  const labels = host
    .split(".")
    .slice(0, -1)
    .filter((l) => !["mcp", "www", "api"].includes(l));
  return nameFrom(labels.at(-1) ?? "");
}

/** The shapes a pasted configuration arrives in. */
export type PastedFormat = "mcpServers" | "servers" | "server.json";

/** One server a pasted block declares, ready to become the draft. */
export interface PastedServer {
  name: string;
  transport: "http" | "stdio";
  /** Its address or command line, as it was written. */
  where: string;
  /** The header or variable names it carries, never their values. */
  keys: string[];
  draft: Partial<Draft>;
}

export type Pasted =
  | { state: "empty" }
  | { state: "error"; message: string; line: number | null }
  | { state: "read"; format: PastedFormat; servers: PastedServer[] };

/** The line a JSON parse failed on, from the parser's own words where they say. */
function lineOfError(text: string, err: unknown): number | null {
  const words = err instanceof Error ? err.message : "";
  const line = /line (\d+)/.exec(words);
  if (line) return Number(line[1]);
  const at = /position (\d+)/.exec(words);
  return at ? text.slice(0, Number(at[1])).split("\n").length : null;
}

/**
 * Every server a pasted block declares, and which shape it is: `mcpServers`
 * (Claude, Cursor), `servers` (VS Code) or a registry `server.json`. Read as
 * the person types, and NEVER RUN: a command it names is shown, not started,
 * and a value it carries is dropped, since a credential pasted from a file is
 * typed again into a field that stores it write-only. The engine's dry run
 * and test judge the draft it fills.
 */
export function readPasted(text: string): Pasted {
  if (!text.trim()) return { state: "empty" };
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch (err) {
    return { state: "error", message: "Not JSON yet", line: lineOfError(text, err) };
  }
  if (!isRecord(raw)) return { state: "error", message: "Paste a JSON object", line: 1 };
  const block = isRecord(raw.mcpServers) ? "mcpServers" : isRecord(raw.servers) ? "servers" : null;
  if (block) {
    const servers = Object.entries(raw[block] as Record<string, unknown>).flatMap(
      ([name, value]): PastedServer[] => {
        if (!isRecord(value)) return [];
        const http =
          typeof value.url === "string" ||
          value.type === "http" ||
          value.type === "streamable-http";
        const keys = Object.keys((http ? value.headers : value.env) ?? {}).filter(Boolean);
        const args = Array.isArray(value.args) ? value.args.map(String) : [];
        const url = typeof value.url === "string" ? value.url : "";
        const command = typeof value.command === "string" ? value.command : "";
        return [
          {
            name,
            transport: http ? "http" : "stdio",
            where: http ? url : [command, ...args].join(" ").trim(),
            keys,
            draft: {
              catalogue: null,
              name: nameFrom(name),
              transport: http ? "http" : "stdio",
              url,
              command,
              args: args.join("\n"),
              headers: http ? keys.map((k) => newRow(k, { required: true })) : [],
              env: http ? [] : keys.map((k) => newRow(k, { required: true })),
              signIn: http && keys.length > 0 ? "headers" : "none",
              detected: null,
              supported: null,
            },
          },
        ];
      },
    );
    return servers.length > 0
      ? { state: "read", format: block, servers }
      : { state: "error", message: "It names no server", line: null };
  }
  const remote = Array.isArray(raw.remotes) ? raw.remotes.find(isRecord) : undefined;
  if (remote && typeof remote.url === "string") {
    const keys = Array.isArray(remote.headers)
      ? remote.headers
          .filter(isRecord)
          .map((h) => String(h.name ?? ""))
          .filter(Boolean)
      : [];
    const name =
      String(raw.name ?? "server")
        .split("/")
        .pop() || "server";
    return {
      state: "read",
      format: "server.json",
      servers: [
        {
          name,
          transport: "http",
          where: remote.url,
          keys,
          draft: {
            catalogue: null,
            name: nameFrom(name),
            description: typeof raw.description === "string" ? raw.description : "",
            transport: "http",
            url: remote.url,
            command: "",
            args: "",
            headers: keys.map((k) => newRow(k, { required: true })),
            env: [],
            signIn: keys.length > 0 ? "headers" : "none",
            detected: null,
            supported: null,
          },
        },
      ],
    };
  }
  return { state: "error", message: "No mcpServers, servers or remotes", line: null };
}
