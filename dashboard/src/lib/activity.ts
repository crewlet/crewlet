/**
 * An executor's activity transcript (`internal/activityline`'s grammar) read
 * into the rows a `ToolCallLog` draws.
 */

import type { ToolCallLogCall, ToolCallLogEntry } from "@crewlethq/ui";
import { ACTIVITY_TAGS } from "~/contract/activity.ts";
import { indentJSON } from "~/lib/jsontext.ts";
import { utf8Bytes } from "~/lib/format.ts";

type Tag = (typeof ACTIVITY_TAGS)[number];

/** One line of the transcript, as `activityline.Parse` reads it. */
export interface ActivityLine {
  /** `null` for the agent's own words. */
  tag: Tag | null;
  seq: number;
  tool: string;
  text: string;
  more: number;
  tookMs: number | null;
}

const MORE_SUFFIX = /^(.*) \(\+(\d+) more line\(s\)\)$/s;

/** Reads one line; anything not well formed is the agent's words, whole. */
export function parseActivityLine(line: string): ActivityLine {
  const words: ActivityLine = { tag: null, seq: 0, tool: "", text: line, more: 0, tookMs: null };
  const end = line.indexOf("]");
  if (!line.startsWith("[") || end < 0) return words;
  const fields = line.slice(1, end).split(" ").filter(Boolean);
  const tag = fields[0] as Tag | undefined;
  if (!tag || !(ACTIVITY_TAGS as readonly string[]).includes(tag)) return words;
  const out: ActivityLine = { ...words, tag, text: line.slice(end + 1).replace(/^ /, "") };
  let args = fields.slice(1);
  if (args.length > 0 && !args[0]!.endsWith("ms")) {
    if (!/^\d+$/.test(args[0]!) || Number(args[0]) <= 0) return words;
    out.seq = Number(args[0]);
    args = args.slice(1);
  }
  if (args.length > 0) {
    const ms = args[0]!.slice(0, -2);
    if (!/^\d+$/.test(ms)) return words;
    out.tookMs = Number(ms);
    args = args.slice(1);
  }
  if (args.length > 0) return words;
  switch (tag) {
    case "steer":
    case "error":
    case "note":
      return out.seq === 0 && out.tookMs === null ? out : words;
    case "input":
      return out.seq > 0 && out.tookMs === null ? out : words;
    case "tool": {
      if (out.seq === 0 || out.tookMs !== null || out.text === "") return words;
      const colon = out.text.indexOf(": ");
      out.tool = colon < 0 ? out.text : out.text.slice(0, colon);
      const detail = colon < 0 ? "" : out.text.slice(colon + 2);
      const more = MORE_SUFFIX.exec(detail);
      out.text = more ? more[1]! : detail;
      out.more = more ? Number(more[2]) : 0;
      return out;
    }
    default:
      return out;
  }
}

/** A failure's first line, its count of the lines the record does not keep read as a cut. */
function failure(text: string): string {
  const more = MORE_SUFFIX.exec(text);
  return more ? `${more[1]}…` : text;
}

/** The tools a run of calls folds into one row for: lookups, which read alike. */
const LOOKUPS = new Set([
  "read",
  "grep",
  "glob",
  "ls",
  "list",
  "webfetch",
  "websearch",
  "toolsearch",
]);

const words = (s: string) =>
  s
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .split(/[_\s-]+/)
    .filter(Boolean);

const capital = (s: string) => (s ? s[0]!.toUpperCase() + s.slice(1) : s);

/**
 * A tool as a person reads it: a harness's own tool by its name ("Bash",
 * "Read"), an MCP server's as "Server · Its tool" ("Crewlet · Get work item").
 */
export function toolLabel(name: string): string {
  const mcp = /^mcp__(.+?)__(.+)$/.exec(name);
  if (!mcp) return capital(name);
  const server = words(mcp[1]!).map(capital).join(" ");
  const tool = capital(words(mcp[2]!).join(" ").toLowerCase());
  return `${server} · ${tool}`;
}

/** The tools whose detail is a file's path: cut at its start, so the file's name stays in view. */
const ON_A_PATH = new Set(["read", "write", "edit", "multiedit", "notebookedit", "ls", "list"]);

/** What a call's group is: the tool for a lookup or an MCP call, else none. */
function groupOf(name: string, key: string): string {
  return LOOKUPS.has(name.toLowerCase()) || name.startsWith("mcp__") ? toolLabel(name) : key;
}

/**
 * The rows a transcript reads as; while `live` a call with no result is running.
 * `base` is the bytes before `text`, so keys hold as a live view drops its start.
 */
export function activityEntries(text: string, live: boolean, base = 0): ToolCallLogEntry[] {
  const entries: ToolCallLogEntry[] = [];
  type Held = { entry: ToolCallLogCall; input: string[]; more: number };
  const calls = new Map<number, Held>();
  const held: Held[] = [];
  const lines = text.replace(/\n$/, "").split("\n");
  const used = new Set<string>();
  let offset = base;
  lines.forEach((raw) => {
    const line = parseActivityLine(raw);
    const at = `o${offset}`;
    offset += utf8Bytes(raw) + 1;
    const key = line.tag === "tool" && !used.has(`t${line.seq}`) ? `t${line.seq}` : at;
    used.add(key);
    switch (line.tag) {
      case null: {
        const last = entries.at(-1);
        if (last?.kind === "say") last.text += `\n${line.text}`;
        else if (line.text.trim() !== "") entries.push({ kind: "say", key, text: line.text });
        return;
      }
      case "tool": {
        const entry: ToolCallLogCall = {
          kind: "call",
          key,
          tool: toolLabel(line.tool),
          group: groupOf(line.tool, key),
          detail: line.text || undefined,
          detailCut: ON_A_PATH.has(line.tool.toLowerCase()) ? "start" : "end",
          status: live ? "running" : "unknown",
        };
        const call = { entry, input: [], more: line.more };
        calls.set(line.seq, call);
        held.push(call);
        entries.push(entry);
        return;
      }
      case "input":
        calls.get(line.seq)?.input.push(line.text);
        return;
      case "ok":
      case "failed": {
        const call = calls.get(line.seq);
        if (!call) {
          if (line.tag === "failed")
            entries.push({
              kind: "note",
              key,
              tone: "danger",
              text: `A call failed: ${failure(line.text)}`,
            });
          return;
        }
        call.entry.status = line.tag === "ok" ? "ok" : "error";
        if (line.tookMs !== null) call.entry.durationMs = line.tookMs;
        if (line.tag === "failed" && line.text) call.entry.error = failure(line.text);
        calls.delete(line.seq);
        return;
      }
      case "steer":
        entries.push({
          kind: "note",
          key,
          tone: "neutral",
          text: `A person's note reached it: ${line.text}`,
        });
        return;
      case "error":
        entries.push({ kind: "note", key, tone: "danger", text: line.text });
        return;
      case "note":
        entries.push({ kind: "note", key, tone: "neutral", text: line.text });
        return;
    }
  });
  // A call's input lines follow it, so its input is whole once all are read.
  for (const { entry, input, more } of held) {
    if (input.length === 0) continue;
    const whole = input.join("\n");
    entry.input = whole.startsWith("{") ? indentJSON(whole) : whole;
    const unkept = more + 1 - input.length;
    if (more > 0 && unkept > 0) entry.inputUnkept = unkept;
  }
  return entries;
}
