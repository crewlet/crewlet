/**
 * The toast a saved company-document change is confirmed by, as every write
 * a screen makes is confirmed (`lib/useAct.ts`): raised when the revision is
 * stored, and replaced in place, under the same id, once this node applies it
 * or a later activation passes it.
 *
 * A node that refused the revision takes the toast away: the refusal is drawn
 * beside the setting it concerns (`components/SettingWriteStatus.tsx`), never
 * in a toast that goes by itself.
 */

import { useEffect, useRef } from "react";
import { useToast } from "@crewlethq/ui";
import type { Standing } from "./nodeStanding.ts";

/** What the toast says while this node has not applied the revision. */
export const APPLYING_SENTENCE = "Applying it on this node.";

/** What it says once this node runs it, where the screen has nothing more exact to say. */
export const APPLIED_SENTENCE = "Applied on this node.";

/** What it says once a later activation passed the revision before this node applied it. */
export const PASSED_SENTENCE = "Saved. A later change has been activated since.";

/** A screen's own words for its save. */
export interface SavedWords {
  /** The toast's title, past tense, naming what was saved; the revision's summary otherwise. */
  readonly done?: string;
  /** What the change does once this node runs it: "Ask Crewlet follows them from the next question." */
  readonly applied?: string;
}

/** A stored revision and where this node stands on it. */
export interface SavedRevision {
  readonly revisionId: string;
  readonly standing: Standing["kind"];
  /** The revision's summary, as its history shows it. */
  readonly summary: string;
}

/** Confirms `saved` with the toast, followed until this node settles it; null raises nothing. */
export function useSavedToast(saved: SavedRevision | null, words?: SavedWords): void {
  const toast = useToast();
  const said = useRef(words);
  said.current = words;
  const revision = saved?.revisionId ?? null;
  const standing = saved?.standing ?? null;
  const summary = saved?.summary ?? "";
  useEffect(() => {
    if (revision === null) return;
    const id = `saved-${revision}`;
    const title = said.current?.done ?? summary;
    if (standing === "applying") {
      toast.show({ id, variant: "info", title, message: APPLYING_SENTENCE });
    } else if (standing === "applied") {
      toast.show({
        id,
        variant: "success",
        title,
        message: said.current?.applied ?? APPLIED_SENTENCE,
      });
    } else if (standing === "passed") {
      toast.show({ id, variant: "info", title, message: PASSED_SENTENCE });
    } else {
      toast.dismiss(id);
    }
  }, [revision, standing, summary, toast]);
}
