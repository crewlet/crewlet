/** The words a repository's rail draws, from rows it reads (`Rail.tsx`). */

import { firstUnready } from "~/lib/code.ts";
import type { CodeChangeSummary, CodePushers } from "~/protocol/index.ts";

/** A state a placing word says: someone is needed, it is working, or it is done. */
export type PlacingTone = "warning" | "info" | "success";

export interface Placing {
  word: string;
  tone?: PlacingTone;
}

/**
 * Where a live change stands, in a word or two: the readiness buckets ("waits on review"), "waits
 * on you" where its first unmet condition waits on the reader's seat, "draft", or "in the queue".
 */
export function placingWord(
  change: Pick<CodeChangeSummary, "state" | "readiness">,
  reader: string,
): Placing {
  if (change.state === "draft") return { word: "draft" };
  const readiness = change.readiness;
  if (!readiness) return { word: change.state };
  const next = firstUnready(readiness);
  if (!next) return { word: "ready to merge", tone: "success" };
  if (next.state === "unmet" && reader !== "" && next.waits_on?.some((p) => p.handle === reader)) {
    return { word: "waits on you", tone: "warning" };
  }
  switch (next.kind) {
    case "approval":
    case "human_approval":
    case "owners_approval":
      return { word: "waits on review" };
    case "check":
      return { word: "waits on checks" };
    case "blocker":
      return { word: "blocked by findings" };
    case "draft":
    case "mergeable":
    case "merge_authority":
      return { word: next.state === "computing" ? "being judged" : "waits on a person" };
    case "queue":
      return { word: "in the queue", tone: "info" };
    case "facts":
      return { word: "being judged" };
  }
  return { word: next.state === "computing" ? "being judged" : readiness.first };
}

/**
 * A mirror's address as the rail draws it: an `http(s)` one as a link to the host's page (no
 * `.git`, no credentials), any other as its text alone, its password dropped.
 */
export function mirrorLink(url: string): { href: string | null; text: string } {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return { href: null, text: url };
  }
  if (parsed.protocol !== "https:" && parsed.protocol !== "http:") {
    parsed.password = "";
    return { href: null, text: parsed.href };
  }
  const path = parsed.pathname.replace(/\.git$/, "").replace(/\/$/, "");
  return { href: `${parsed.protocol}//${parsed.host}${path}`, text: `${parsed.host}${path}` };
}

/** The pushers past the first `shown`, and their pushes together; null where none are. */
export function pusherRest(
  pushers: CodePushers,
  shown: number,
): { pushers: number; pushes: number } | null {
  const rest = (pushers.top ?? []).slice(shown);
  const count = rest.length + (pushers.omitted ?? 0);
  if (count === 0) return null;
  return {
    pushers: count,
    pushes: rest.reduce((sum, p) => sum + p.pushes, 0) + (pushers.omitted_pushes ?? 0),
  };
}
