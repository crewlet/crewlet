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
 * is (`useAct`): the person's tracker access decides, and a refusal is drawn
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
import { KIND_WORDS } from "~/lib/artifacts/words.ts";
import type { ArtifactKind } from "~/protocol/index.ts";
import { newActOpID } from "~/protocol/index.ts";

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
  /** The task's address in the dashboard (`#/work/<item>`), as the receipt names it. */
  item: string;
  /** Its tracker key ("DES-12"), which an artifact's `work_item` names. */
  key: string;
  /**
   * Whether the named artifact was linked, or why not, in a sentence. Null
   * when no artifact was named.
   */
  linked: true | string | null;
}

/**
 * The task's description: the brief, then what to make of it. Said in the
 * agent's terms (the kind's word and its wire name) so a turn reading the task
 * knows which artifact to publish, and, for an artifact already made, which one
 * to change.
 */
export function artifactTaskBody(kind: ArtifactKind, brief: string, artifact?: string): string {
  const word = KIND_WORDS[kind];
  const ask = artifact
    ? `Build it in the ${word} artifact already linked to this task (artifact ${artifact}), and publish your work as new versions of it.`
    : `Publish it as a ${word} artifact (kind \`${kind}\`) for this task.`;
  return `${brief.trim()}\n\n${ask}`;
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
          body: artifactTaskBody(request.kind, request.brief, request.artifact),
        },
        { done: `Asked ${index.byHandle.get(request.seat)?.name ?? request.seat} for “${title}”` },
      );
      if (!result || (result.kind !== "applied" && result.kind !== "pending")) return null;
      const receipt = (result.receipt ?? {}) as { item?: unknown; key?: unknown };
      const key = typeof receipt.key === "string" ? receipt.key : "";
      const item = typeof receipt.item === "string" ? receipt.item : key;
      if (!request.artifact) return { item, key, linked: null };
      if (!key) {
        return {
          item,
          key,
          linked:
            "The task was filed, and the artifact could not be linked: its key is not known yet.",
        };
      }
      setLinking(true);
      try {
        const link = await keyedWrite({
          ...linkWorkItem(request.artifact, key),
          key: newActOpID(),
        });
        return { item, key, linked: link.kind === "done" ? true : link.text };
      } finally {
        setLinking(false);
      }
    },
    [create, viewer.project, index],
  );

  return {
    fileArtifactTask,
    busy: create.busy || linking,
    refusal: create.refusal,
    held: create.access.can ? null : create.access.reason,
  };
}
