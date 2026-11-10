/**
 * What an agent may do and where it runs, in the go-live review's words: one
 * line per vendor it acts at, per tool server, and per box.
 */

import type { ToldBox, ToldIntegration, ToldToolServer } from "~/protocol/goLive.ts";

/** Where each cell runs a box. */
const CELL_WORDS: Record<string, string> = {
  direct: "A directory on the engine host, as the engine's user",
  container: "A container on the engine host",
  e2b: "A remote sandbox",
};

function gigabytes(bytes: number): string {
  const gb = bytes / 2 ** 30;
  return `${Number.isInteger(gb) ? gb : gb.toFixed(1)} GB`;
}

/** A vendor the agent acts at: its level, the permissions it may write with, and where. */
export function integrationWords(i: ToldIntegration): string {
  const writes = Object.entries(i.permissions ?? {})
    .filter(([, level]) => level === "write" || level === "admin")
    .map(([p, level]) => `${i.permission_names?.[p] ?? p.replaceAll("_", " ")}: ${level}`);
  const level = i.level_name ?? i.level;
  const parts = [level, writes.length > 0 ? writes.join(", ") : ""].filter(Boolean);
  const what = parts.length > 0 ? `${i.kind} (${parts.join("; ")})` : i.kind;
  const where = scopeWords(i);
  return where ? `${what} on ${where}` : what;
}

/** Where an integration acts: each scope entry by its label, an SSH host with its read commands. */
function scopeWords(i: ToldIntegration): string {
  const commands = new Map((i.commands ?? []).map((c) => [c.host, c.run.length + (c.more ?? 0)]));
  const entries = (i.scope ?? []).map((s) => {
    const label = i.scope_labels?.[s] ?? s;
    const reads = commands.get(s);
    return reads === undefined ? label : `${label} (${count(reads, "read command")})`;
  });
  if (i.scope_more) entries.push(`${i.scope_more} more`);
  return entries.join(", ");
}

function count(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? "" : "s"}`;
}

/** Every tool the lists do not name, by the server's default level. */
const DEFAULT_WORDS: Record<string, { all: string; rest: string }> = {
  allow: { all: "every tool runs directly", rest: "every other tool runs directly" },
  approve: {
    all: "every tool needs a person's approval",
    rest: "every other tool needs a person's approval",
  },
  blocked: { all: "no tool is offered", rest: "no other tool is offered" },
};
const HINTS = {
  all: "every tool runs at the level its server marks it with",
  rest: "every other tool runs at the level its server marks it with",
};

/** A tool server: the tools its lists name at each level, and what every other tool does. */
export function toolServerWords(s: ToldToolServer): string {
  const named = [
    s.allow.length
      ? `${count(s.allow.length, "tool")} ${s.allow.length === 1 ? "runs" : "run"} directly`
      : "",
    s.approve.length
      ? `${count(s.approve.length, "tool")} ${s.approve.length === 1 ? "needs" : "need"} a person's approval`
      : "",
    s.blocked.length ? `${count(s.blocked.length, "tool")} blocked` : "",
    s.more ? `${count(s.more, "more named tool")} not shown` : "",
  ].filter(Boolean);
  const other = (s.default && DEFAULT_WORDS[s.default]) || HINTS;
  return named.length === 0
    ? `${s.server}: ${other.all}`
    : `${s.server}: ${named.join(", ")}; ${other.rest}`;
}

/** A box: where it runs, its memory, and what a run last found in it. */
export function boxWords(b: ToldBox): string {
  const where = CELL_WORDS[b.cell] ?? b.cell;
  const memory = b.memory_bytes ? `, ${gigabytes(b.memory_bytes)} of memory` : "";
  if (!b.observed) return `${where}${memory}. No run has read it yet.`;
  const browser = b.browser ? `a browser (${b.browser})` : "no browser";
  const runtime = b.container_runtime
    ? `a container runtime (${b.container_runtime})`
    : "no container runtime";
  return `${where}${memory}, with ${browser} and ${runtime}.`;
}
