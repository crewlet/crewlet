/** Who moved a ref, in a word and a sentence: one rule for a branch's byline and a repository's last push. */

import { PUSH_HISTORY_DAYS } from "~/lib/code.ts";
import type { CodeRefMover } from "~/protocol/index.ts";
import { GITLET, type Who } from "./ui.tsx";

const DAY_MS = 86_400_000;

/** Who moved a branch, in a word and a sentence: Gitlet for a merge or a queue landing; never a guess. */
export function moverOf(
  mover: CodeRefMover | undefined,
  movedAt: string,
  now: number,
  name: string,
  whoName: (who: Who) => string,
): { who: Who | null; words: string; title: string } {
  if (!mover) {
    return now - Date.parse(movedAt) < PUSH_HISTORY_DAYS * DAY_MS
      ? {
          who: null,
          words: "Unknown",
          title: "Who moved it is unknown: one record moved it away and back.",
        }
      : {
          who: null,
          words: "No longer kept",
          title: `Who moved it is no longer kept: pushes are kept ${PUSH_HISTORY_DAYS} days.`,
        };
  }
  const by = mover.actor_seat || mover.actor;
  switch (mover.via) {
    case "merge":
      return {
        who: GITLET,
        words: "Gitlet",
        title: `Gitlet moved ${name} when ${whoName(by)} merged a change`,
      };
    case "queue":
      return {
        who: GITLET,
        words: "Gitlet",
        title: `Gitlet moved ${name} landing its merge queue's train, for ${whoName(by)}`,
      };
    case "mirror":
      return { who: by, words: whoName(by), title: "Moved by the mirror's sync" };
    case "import":
      return { who: by, words: whoName(by), title: "Moved by the import" };
    case "engine":
      return { who: by, words: whoName(by), title: "Written by a Gitlet tool" };
    default:
      return { who: by, words: whoName(by), title: "Pushed with git" };
  }
}
