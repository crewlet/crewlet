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

/** A vendor the agent acts at: its level, and the permissions it may write with. */
export function integrationWords(i: ToldIntegration): string {
  const writes = Object.entries(i.permissions ?? {})
    .filter(([, level]) => level === "write" || level === "admin")
    .map(([p, level]) => `${i.permission_names?.[p] ?? p.replaceAll("_", " ")}: ${level}`);
  const level = i.level_name ?? i.level;
  const parts = [level, writes.length > 0 ? writes.join(", ") : ""].filter(Boolean);
  return parts.length > 0 ? `${i.kind} (${parts.join("; ")})` : i.kind;
}

/** A tool server: how many tools run at once, and how many wait for a person. */
export function toolServerWords(s: ToldToolServer): string {
  return `${s.server}: ${s.allow.length} run directly, ${s.approve.length} with a person's approval`;
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
