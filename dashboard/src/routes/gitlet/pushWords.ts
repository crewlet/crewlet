/** A push said as a sentence: who pushed, what it did to which refs, and its day's count. */

import { commandWord, omittedOf, refName } from "~/lib/code.ts";
import { fmtCount, plural, readerDay } from "~/lib/format.ts";
import type { CodeChangeName, CodePushRow } from "~/protocol/index.ts";

/** One part of a sentence: words, a ref's chip, a ref that is gone (no chip), or a change. */
export type PushPart = string | { ref: string } | { gone: string } | { change: CodeChangeName };

export interface PushSentence {
  parts: PushPart[];
  /** Every command was refused: nothing was stored. */
  attempt: boolean;
  /** Commands refused where others were not, or none. */
  refused: number;
}

/** Who a push's sentence starts with: Gitlet, a seat (a person's included), or a bare credential. */
export type PushSubject =
  { kind: "gitlet" } | { kind: "seat"; handle: string } | { kind: "credential"; login: string };

/** The ways Gitlet's own account moves refs, which name Gitlet as the pusher (engine's counting). */
const GITLET_VIAS: ReadonlySet<string> = new Set(["merge", "queue", "import", "mirror", "expiry"]);

const commits = (n: number) => `${fmtCount(n)} ${n === 1 ? "commit" : "commits"}`;
const refs = (n: number) => `${fmtCount(n)} ${n === 1 ? "ref" : "refs"}`;

export function pushSubject(push: CodePushRow): PushSubject {
  if (GITLET_VIAS.has(push.via)) return { kind: "gitlet" };
  if (push.actor_seat) return { kind: "seat", handle: push.actor_seat };
  if (push.actor_kind === "agent") return { kind: "seat", handle: push.actor };
  return { kind: "credential", login: push.actor };
}

/** What a push did, after its subject: never "0 commits", an absent count said as none. */
export function pushWords(push: CodePushRow): PushSentence {
  const shown = push.commands ?? [];
  const m = shown.length + omittedOf(push.omitted).total;
  const ng = shown.filter((c) => c.result === "ng").length + (push.omitted?.refused ?? 0);
  const attempt = m > 0 && ng === m;
  const n = push.introduced_count ?? 0;
  const o = push.offered ?? 0;
  const done = (parts: PushPart[]): PushSentence => ({
    parts,
    attempt,
    refused: attempt ? 0 : ng,
  });
  const first = shown[0];
  const target = (shown.find((c) => c.result === "ok") ?? first)?.ref;
  const into = (lead: PushPart[], tail: PushPart[] = []) =>
    done(target ? [...lead, { ref: target }, ...tail] : lead);

  if (push.via === "merge") {
    return push.change
      ? into(["merged ", { change: push.change }, " into "])
      : into(["merged a change into "]);
  }
  if (push.via === "queue") {
    return push.change
      ? into(["landed ", { change: push.change }, " on "], [" from its queue"])
      : into(["landed its queue's train on "]);
  }
  if (push.via === "expiry") {
    return m === 1 && first
      ? done(["deleted the idle seat branch ", { gone: refName(first.ref).name }])
      : done([`deleted ${fmtCount(m)} idle seat branches`]);
  }
  if (push.via === "import") return done([`imported ${refs(m)}`]);
  if (push.via === "mirror") return done([`synced ${refs(m)} from the repository it mirrors`]);

  const merging: PushPart[] = push.change ? [", merging ", { change: push.change }] : [];
  if (attempt) {
    const offered = o > 0 ? `tried to push ${commits(o)} to ` : "tried to push to ";
    return m === 1 && first ? done([offered, { ref: first.ref }]) : done([`${offered}${refs(m)}`]);
  }
  if (m !== 1 || !first) {
    const words = n > 0 ? `pushed ${commits(n)} to ${refs(m)}` : `updated ${refs(m)}`;
    return done([m === 0 && n === 0 ? "pushed" : words, ...merging]);
  }
  const ref = { ref: first.ref };
  const kind = refName(first.ref).kind;
  switch (commandWord(first)) {
    case "unchanged":
      return done(["pushed ", ref, " unchanged", ...merging]);
    case "created":
      if (kind === "tag") return done(["created the tag ", ref, ...merging]);
      if (kind === "ref") return done(["created ", ref, ...merging]);
      return n > 0
        ? done([`pushed ${commits(n)} to a new branch `, ref, ...merging])
        : done(["created the branch ", ref, ...merging]);
    case "deleted": {
      const what = kind === "tag" ? "the tag " : kind === "branch" ? "the branch " : "";
      return done([`deleted ${what}`, { gone: refName(first.ref).name }, ...merging]);
    }
    case "forced":
      return n > 0
        ? done([`force-pushed ${commits(n)} to `, ref, ...merging])
        : done(["force-pushed ", ref, ...merging]);
    case "moved":
      return n > 0
        ? done([`pushed ${commits(n)} to `, ref, ...merging])
        : done(["moved ", ref, ...merging]);
    default:
      return done([`${commandWord(first)} `, ref, ...merging]);
  }
}

/** A credential's kind in words, by the prefix its login carries. */
export function credentialWords(login: string): string {
  if (login.startsWith("pat:")) return "Personal access token";
  if (login.startsWith("token:")) return "Deployment token";
  if (login.startsWith("session:")) return "Signed-in session";
  return "Credential";
}

/** What `by=` asks of the history: Gitlet's pushes, or one seat's or person's. */
export function pusherParams(
  by: string,
  kindOf: (handle: string) => string | undefined,
): { pusher_kind?: string; pusher?: string } {
  if (by === "") return {};
  if (by === "gitlet") return { pusher_kind: "gitlet" };
  return { pusher_kind: kindOf(by) ?? "agent", pusher: by };
}

/** Each of the reader's days the bounds start, by its key, with the engine's count of it. */
export function dayCounts(
  bounds: readonly string[],
  counts: readonly number[] | undefined,
): Map<string, number> {
  const out = new Map<string, number>();
  if (!counts) return out;
  counts.forEach((n, i) => {
    const at = Date.parse(bounds[i] ?? "");
    if (i < bounds.length - 1 && !Number.isNaN(at)) out.set(readerDay(at), n);
  });
  return out;
}

/** A day heading's count: whole ("8"), part of the day ("2 of 13"), or uncounted ("3 loaded"). */
export function dayAside(
  loaded: number,
  counted: number | undefined,
  day: string,
): { text: string; title?: string } {
  if (counted === undefined) return { text: `${fmtCount(loaded)} loaded` };
  if (loaded >= counted) {
    return { text: fmtCount(loaded), title: `${plural(loaded, "push", "pushes")} on ${day}` };
  }
  return {
    text: `${fmtCount(loaded)} of ${fmtCount(counted)}`,
    title: `${plural(counted, "push", "pushes")} on ${day}, ${fmtCount(loaded)} of them loaded`,
  };
}
