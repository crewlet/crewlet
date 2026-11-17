/**
 * Gitlet › Repositories' listing as the engine reads it: the question a kind, a state and a sort
 * ask, the kind counts beside the control, and what a row says of its last push.
 */

import { PUSH_HISTORY_DAYS } from "~/lib/code.ts";
import type { CodeLastPush, CodeRepoCard, CodeRepoKindCounts } from "~/protocol/index.ts";
import { REPO_KINDS, REPO_SORTS, REPO_STATUSES } from "~/contract/code.ts";

export type RepoKind = (typeof REPO_KINDS)[number];
type RepoSort = (typeof REPO_SORTS)[number];

/** The engine's two orders, in its order (held by the gate on REPO_SORTS). */
const [BY_NAME, BY_PUSH] = REPO_SORTS;

const DAY_MS = 86_400_000;

/** The states only a listing by name reads (`gitlet.validateRepos`): no last push orders them. */
const BY_NAME_ONLY: ReadonlySet<string> = new Set(["deleted", "purging"]);

/** Whether the state chosen holds the sort to Name, as the engine refuses `sort=pushed` there. */
export function sortHeld(status: string): boolean {
  return BY_NAME_ONLY.has(status);
}

/** The order asked: by last push unless Name is chosen or the state is listed by name only. */
export function listingSort(status: string, sort: string): RepoSort {
  return sort === BY_NAME || sortHeld(status) ? BY_NAME : BY_PUSH;
}

/** The kind the address names, or "" (All) for none or one this build does not know. */
export function listingKind(kind: string): RepoKind | "" {
  return (REPO_KINDS as readonly string[]).includes(kind) ? (kind as RepoKind) : "";
}

/** The state the address names, or "" (the live ones) for none or one this build does not know. */
export function listingStatus(status: string): string {
  return (REPO_STATUSES as readonly string[]).includes(status) ? status : "";
}

/** The listing's question: every row's facts on the reader's days, the engine's page size. */
export function listingParams(
  status: string,
  kind: string,
  sort: string,
  bounds: readonly string[],
): Record<string, string | boolean> {
  const k = listingKind(kind);
  const state = listingStatus(status);
  return {
    ...(state ? { status: state } : {}),
    ...(k ? { kind: k } : {}),
    sort: listingSort(state, sort),
    repo_facts: true,
    day_bounds: bounds.join(","),
  };
}

/**
 * How many repositories of a kind ("" both) are in a state ("" the listing's default, the live
 * ones): the engine's whole for the reader; undefined where not counted or a state it does not know.
 */
export function kindCount(
  counts: CodeRepoKindCounts | undefined,
  kind: RepoKind | "",
  status: string,
): number | undefined {
  if (!counts) return undefined;
  const field = status === "" ? "live" : status;
  const of = (k: RepoKind): number | undefined => {
    const n = (counts[k] as unknown as Record<string, unknown> | undefined)?.[field];
    return typeof n === "number" ? n : undefined;
  };
  if (kind) return of(kind);
  const project = of("project");
  const scratch = of("scratch");
  return project === undefined || scratch === undefined ? undefined : project + scratch;
}

/** What a row says of its last push, from its card and its last push's instant. */
export type LastPushWords =
  /** Nobody pushed to it. */
  | { kind: "never" }
  /** The card was not answered: the instant alone, nothing claimed of who. */
  | { kind: "bare"; at: string }
  /** Its last push moved a ref, by the pusher named. */
  | { kind: "applied"; at: string; push: CodeLastPush }
  /** Its last push was refused: nothing moved. */
  | { kind: "refused"; at: string; push: CodeLastPush; title: string }
  /** Who pushed is past the history, or was never kept: the instant and why. */
  | { kind: "unknown"; at: string; title: string };

const APPLIED = ["created", "moved", "forced", "deleted"] as const;

/** The last push in words: applied, refused, unknown, no longer kept, or never. */
export function lastPushWords(
  card: CodeRepoCard | undefined,
  lastAt: string | undefined,
  now: number,
): LastPushWords {
  const push = card?.last_push;
  if (push) {
    const moved = APPLIED.some((o) => (push.outcome[o] ?? 0) > 0);
    return !moved && (push.outcome.refused ?? 0) > 0
      ? {
          kind: "refused",
          at: push.at,
          push,
          title: "Its last push was refused: nothing moved.",
        }
      : { kind: "applied", at: push.at, push };
  }
  if (!lastAt) return { kind: "never" };
  if (!card) return { kind: "bare", at: lastAt };
  return now - Date.parse(lastAt) < PUSH_HISTORY_DAYS * DAY_MS
    ? {
        kind: "unknown",
        at: lastAt,
        title: "Who pushed is not known: the pushes after it were the engine's expiries.",
      }
    : {
        kind: "unknown",
        at: lastAt,
        title: `Who pushed is no longer kept: pushes are kept ${PUSH_HISTORY_DAYS} days.`,
      };
}
