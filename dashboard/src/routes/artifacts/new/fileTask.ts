/**
 * Asking an agent to build an artifact: one REAL tracker task, filed to the
 * agent's seat in a project, whose title is what to make and whose
 * description carries the reader's brief and the kind, so the agent knows
 * what to publish and where it belongs.
 *
 * AN ARTIFACT ALREADY MADE IS HANDED OVER WITH THE TASK. A page that creates
 * the artifact first (a design's empty canvas, a dashboard the agent refines)
 * names it, and the artifact is linked to the new task (`linkWorkItem`), so
 * the task's Artifacts card lists it and the agent starts from it rather than
 * from nothing. The task is filed first: an artifact linked to a task that was
 * never made would name a key nobody holds.
 *
 * The create is the tracker's own `create_work_item`, sent as the signed-in
 * person, so it is held, refused and reported exactly as the New task sheet's
 * is (`useAct`): the person's Worklet access decides, and a refusal is drawn
 * beside the control that sent it.
 */

import { useCallback, useMemo, useState } from "react";
import { useAct } from "~/lib/useAct.ts";
import type { Refusal } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { keyedWrite } from "~/lib/keyedWrite.ts";
import { linkWorkItem } from "~/lib/artifacts/api.ts";
import { KIND_ARTICLES, KIND_WORDS } from "~/lib/artifacts/words.ts";
import type { ArtifactKind } from "~/protocol/index.ts";
import { newActOpID } from "~/protocol/index.ts";
import { briefFolder, useSaveIn } from "./create.ts";

/** What a create page asks an agent for. */
export interface ArtifactTaskRequest {
  kind: ArtifactKind;
  /** The task's title: what to make, in a line ("Onboarding screens for the mobile app"). */
  title: string;
  /** What the reader wants, in their words: the description's body. */
  brief: string;
  /** The handle of the agent seat the task is filed to. */
  seat: string;
  /** The project it is filed in; the reader's own project when left out. */
  project?: string | undefined;
  /** An artifact already made, linked to the task so the agent starts from it. */
  artifact?: string | undefined;
}

/** The task that was filed. */
export interface FiledArtifactTask {
  /** The task's address in the dashboard (`#/worklet/<item>`), as the receipt names it. */
  item: string;
  /** Its tracker key ("DES-12"), which an artifact's `work_item` names. */
  key: string;
  /**
   * Whether the named artifact was linked, or, when it was not, the sentence
   * a page shows (`notAttached`). Null when no artifact was named.
   */
  linked: true | string | null;
}

/**
 * The task's description: the brief, then what to make of it. Said in the
 * agent's terms (the kind's word and its wire name) so a turn reading the task
 * knows which artifact to publish, and, for an artifact already made, which one
 * to change.
 */
export function artifactTaskBody(
  kind: ArtifactKind,
  brief: string,
  artifact?: string,
  folder?: string,
): string {
  const word = KIND_WORDS[kind];
  // WHERE A NEW ONE IS FILED, by the folder's id alone, which
  // `create_artifact`'s `folder` takes and which a rename cannot change. Never
  // by its path: a task is read by people the folder may not be shown to (an
  // "Only you" folder), and its path would tell them the names in it. An
  // artifact already made was filed when it was made.
  const filed =
    folder && !artifact
      ? ` File it in the folder whose id is ${folder} (\`folder\` on \`create_artifact\`).`
      : "";
  const ask = artifact
    ? `Build it in the ${word} artifact already linked to this task (artifact ${artifact}), and publish your work as new versions of it.`
    : `Publish it as ${KIND_ARTICLES[kind]} ${word} artifact (kind \`${kind}\`) for this task.${filed}`;
  return `${brief.trim()}\n\n${ask}`;
}

/**
 * What a page says when the task was filed and the artifact was not linked to
 * it: in a person's words, never the request's own detail (a route, a status),
 * with what to do about it. The task stands either way, so it is never said as
 * a failure of the whole ask.
 */
export function notAttached(kind: ArtifactKind): string {
  const what = kind === "html" ? "HTML page" : KIND_WORDS[kind].toLowerCase();
  return `The task was filed, but the ${what} could not be attached to it. Attach it from the task.`;
}

/** File an artifact's task, and what the last press came to. */
export interface FileArtifactTask {
  /** File the task; resolves with it, or null when it was refused, held or not confirmed. */
  fileArtifactTask: (request: ArtifactTaskRequest) => Promise<FiledArtifactTask | null>;
  busy: boolean;
  /** The engine's refusal of the last task, for the control that sent it to draw. */
  refusal: Refusal | null;
  /** Why this browser may not file one, or null when it may. */
  held: string | null;
}

export function useFileArtifactTask(): FileArtifactTask {
  const create = useAct("create_work_item");
  const viewer = useViewer();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const saveIn = useSaveIn();
  const [linking, setLinking] = useState(false);

  const fileArtifactTask = useCallback(
    async (request: ArtifactTaskRequest): Promise<FiledArtifactTask | null> => {
      const title = request.title.trim();
      const project = request.project || viewer.project;
      const result = await create.run(
        {
          title,
          project,
          assignee: request.seat,
          body: artifactTaskBody(
            request.kind,
            request.brief,
            request.artifact,
            briefFolder(saveIn),
          ),
        },
        { done: `Asked ${index.byHandle.get(request.seat)?.name ?? request.seat} for “${title}”` },
      );
      if (!result || (result.kind !== "applied" && result.kind !== "pending")) return null;
      const receipt = (result.receipt ?? {}) as { item?: unknown; key?: unknown };
      const key = typeof receipt.key === "string" ? receipt.key : "";
      const item = typeof receipt.item === "string" ? receipt.item : key;
      if (!request.artifact) return { item, key, linked: null };
      if (!key) return { item, key, linked: notAttached(request.kind) };
      setLinking(true);
      try {
        const link = await keyedWrite({
          // AT THE CREATE'S OWN POSITION: the engine waits for the tracker
          // to hold the new task before it checks it, so a node that has not
          // applied the create yet does not answer that there is no task.
          ...linkWorkItem(request.artifact, key, result.position ?? undefined),
          key: newActOpID(),
        });
        return { item, key, linked: link.kind === "done" ? true : notAttached(request.kind) };
      } finally {
        setLinking(false);
      }
    },
    [create, viewer.project, index, saveIn.folder, saveIn.path],
  );

  return {
    fileArtifactTask,
    busy: create.busy || linking,
    refusal: create.refusal,
    held: create.access.can ? null : create.access.reason,
  };
}
