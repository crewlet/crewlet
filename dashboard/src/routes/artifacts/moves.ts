/**
 * Filing artifacts in a folder, one or many, and taking it back: the gesture
 * a card's menu, the Details panel and the selection bar share.
 *
 * # Each artifact is decided on its own
 *
 * A bulk move is one request per batch of `ARTIFACT_MOVE_MAX_ITEMS`, and the
 * engine answers every artifact in it apart (`results`): one the reader may
 * only view is refused while the rest move. What came of it is said in one
 * toast that names what did not move and why, never as a whole that silently
 * did part of what it said.
 *
 * # Undo is a move back
 *
 * A move changes nobody's access and keeps the artifact's history, so taking
 * it back is the same gesture to where each artifact was, offered on the
 * toast that reports the move.
 */

import { useCallback, useState } from "react";
import { useToast } from "@crewlethq/ui";
import { ARTIFACT_MOVE_MAX_ITEMS } from "~/contract/artifacts.ts";
import { plural } from "~/lib/format.ts";
import { keyedWrite, type KeyedAnswer } from "~/lib/keyedWrite.ts";
import { fileArtifact, fileArtifacts } from "~/lib/artifacts/api.ts";
import { announceFoldersChanged, TOP_LEVEL } from "~/lib/artifacts/folders.ts";
import { newActOpID, type ArtifactMoveResult } from "~/protocol/index.ts";

/** One artifact a move files: its id, its title for the words, and the folder it is in now (`""` at the top level). */
export interface Movable {
  id: string;
  title: string;
  folder: string;
}

/** Where a move files to: a folder's id and name, or the top level (`id` `""`). */
export interface MoveTarget {
  id: string;
  name: string;
}

/** What one move came to. */
export interface MoveReport {
  moved: Movable[];
  refused: { item: Movable; why: string }[];
}

/**
 * What a refusal code says when the engine sent no sentence with it: the
 * outcomes `POST /artifacts/folder-moves` answers, in the reader's words.
 */
const REFUSAL_WORDS: Readonly<Record<string, string>> = {
  not_found: "It is not there any more, or you cannot open it.",
  unauthorized: "Moving it needs the “Can edit” role on it.",
  folder_not_found: "The folder is gone, or you cannot see it.",
  artifact_trashed: "It is in the trash. Restore it first.",
  unavailable: "The engine could not take it just now. Try again.",
};

/** Why one artifact did not move, in a sentence. */
function refusalWords(result: ArtifactMoveResult): string {
  return result.message || REFUSAL_WORDS[result.outcome] || "The engine refused it.";
}

/** One request's answer, as a result per artifact it carried. */
function resultsOf(answer: KeyedAnswer, sent: readonly Movable[]): ArtifactMoveResult[] {
  if (answer.kind === "done") {
    const results = answer.body.results;
    if (Array.isArray(results)) return results as ArtifactMoveResult[];
    // ONE ARTIFACT'S PUT answers the artifact and its folder, not a list.
    return sent.map((m) => ({ artifact: m.id, outcome: "moved" }));
  }
  return sent.map((m) => ({ artifact: m.id, outcome: "refused", message: answer.text }));
}

/**
 * File `items` in `folder` (`""` for the top level): one artifact through its
 * own route, several in batches the engine decides artifact by artifact. A
 * batch nobody could confirm is reported as not moved, in the engine's words
 * about trying again under the same key.
 */
export async function fileEach(
  items: readonly Movable[],
  folder: (m: Movable) => string,
): Promise<MoveReport> {
  const answers: ArtifactMoveResult[] = [];
  if (items.length === 1) {
    const one = items[0]!;
    const answer = await keyedWrite({ ...fileArtifact(one.id, folder(one)), key: newActOpID() });
    answers.push(...resultsOf(answer, items));
  } else {
    for (let at = 0; at < items.length; at += ARTIFACT_MOVE_MAX_ITEMS) {
      const batch = items.slice(at, at + ARTIFACT_MOVE_MAX_ITEMS);
      const answer = await keyedWrite({
        ...fileArtifacts(batch.map((m) => ({ artifact: m.id, folder: folder(m) }))),
        key: newActOpID(),
      });
      answers.push(...resultsOf(answer, batch));
    }
  }
  const byId = new Map(answers.map((r) => [r.artifact, r]));
  const report: MoveReport = { moved: [], refused: [] };
  for (const item of items) {
    const result = byId.get(item.id);
    if (result?.outcome === "moved") report.moved.push(item);
    else
      report.refused.push({
        item,
        why: result ? refusalWords(result) : REFUSAL_WORDS.unavailable!,
      });
  }
  return report;
}

/** Where a toast says the artifacts went: "Press kit", or "the top level". */
function whereWords(target: MoveTarget): string {
  return target.id === "" ? "the top level" : target.name || TOP_LEVEL;
}

/** What a move did, as its toast's title: "Moved “Q3 plan” to Press kit", "Moved 2 of 3 to Press kit". */
export function movedWords(report: MoveReport, target: MoveTarget): string {
  const where = whereWords(target);
  const total = report.moved.length + report.refused.length;
  if (report.moved.length === 0) return total === 1 ? "It was not moved" : "Nothing was moved";
  if (report.refused.length === 0) {
    return total === 1
      ? `Moved “${report.moved[0]!.title}” to ${where}`
      : `Moved ${plural(total, "artifact")} to ${where}`;
  }
  return `Moved ${report.moved.length} of ${total} to ${where}`;
}

/** Why the rest did not move, as the toast's message: the first refusal by name, and how many more. */
export function refusedWords(report: MoveReport): string | undefined {
  const first = report.refused[0];
  if (!first) return undefined;
  const rest = report.refused.length - 1;
  const more = rest > 0 ? ` ${plural(rest, "other was", "others were")} not moved either.` : "";
  return `“${first.item.title}” was not moved. ${first.why}${more}`;
}

/**
 * The move gesture: file artifacts, say what came of it with Undo, and tell
 * every reader of the tree and the caller's listing that filings moved.
 */
export function useMoveArtifacts(onMoved?: () => void) {
  const toast = useToast();
  const [busy, setBusy] = useState(false);

  const undo = useCallback(
    async (report: MoveReport) => {
      const back = await fileEach(report.moved, (m) => m.folder);
      announceFoldersChanged();
      onMoved?.();
      if (back.refused.length === 0) {
        toast.ok(
          back.moved.length === 1 ? `Moved “${back.moved[0]!.title}” back` : "Moved them back",
        );
      } else {
        toast.show({
          variant: "warning",
          title:
            back.moved.length === 0
              ? "Nothing was moved back"
              : `Moved ${back.moved.length} of ${report.moved.length} back`,
          message: refusedWords(back),
        });
      }
    },
    [toast, onMoved],
  );

  const move = useCallback(
    async (items: readonly Movable[], target: MoveTarget): Promise<MoveReport | null> => {
      if (busy || items.length === 0) return null;
      setBusy(true);
      try {
        const report = await fileEach(items, () => target.id);
        announceFoldersChanged();
        onMoved?.();
        const title = movedWords(report, target);
        const action =
          report.moved.length > 0 ? { label: "Undo", onClick: () => void undo(report) } : undefined;
        if (report.refused.length === 0) {
          toast.ok(title, action ? { action } : undefined);
        } else {
          toast.show({
            variant: report.moved.length === 0 ? "danger" : "warning",
            title,
            message: refusedWords(report),
            ...(action ? { action } : {}),
          });
        }
        return report;
      } finally {
        setBusy(false);
      }
    },
    [busy, toast, undo, onMoved],
  );

  return { move, busy };
}
