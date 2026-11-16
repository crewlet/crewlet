/**
 * What every pin layer draws: a thread's pin, the draft's pin with the kit's
 * Popover holding its composer, and a part's outline.
 */

import { useLayoutEffect } from "react";
import { CommentPin, LAYER_REPOSITION_EVENT, Popover, useLayerContainer } from "@crewlethq/ui";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import type { Box } from "~/lib/artifacts/comments.ts";
import type { ArtifactCommentThread } from "~/protocol/index.ts";
import { DraftComposer } from "./DraftComposer.tsx";
import { useComments } from "./state.tsx";

/** How much of a thread's first remark its pin's name says. */
const PIN_WORDS = 80;

/** One thread's pin at a point of the layer; pressing it opens the thread in the panel. */
export function ThreadPin({ t, x, y }: { t: ArtifactCommentThread; x: number; y: number }) {
  const c = useComments()!;
  const people = useArtifactPeople();
  const first = t.remarks.find((r) => !r.removed)?.body ?? "";
  const said = first.length > PIN_WORDS ? `${first.slice(0, PIN_WORDS).trim()}…` : first;
  return (
    <CommentPin
      className="artifact-comment-pin"
      mark={String(c.numberOf(t.id))}
      label={`${people.seatName(t.created_by)} on ${t.label}${said ? `: ${said}` : ""}`}
      state={t.status === "resolved" ? "resolved" : "open"}
      selected={c.selected === t.id}
      style={{ left: x, top: y }}
      onClick={() => c.openThread(t.id)}
    />
  );
}

/**
 * The draft's pin, and its composer beside it while the reader writes there:
 * on a phone, or once the pin left the view, the composer is the panel's.
 */
export function DraftPin({ x, y }: { x: number; y: number }) {
  const c = useComments()!;
  const draft = c.draft;
  if (!draft) return null;
  const beside = !c.narrow && !draft.docked;
  return (
    <Popover
      label={`New comment: ${draft.label}`}
      open={beside}
      align="start"
      onOpenChange={(open, reason) => {
        if (open) return;
        // ESCAPE LETS THE DRAFT GO; a pin carried out of view hands it to the
        // panel. A press elsewhere keeps it, since its words are still there.
        if (reason === "escape") c.clearDraft();
        else if (reason === "anchor-gone") c.dockDraft();
      }}
      trigger={() => (
        <CommentPin
          className="artifact-comment-pin"
          mark="+"
          label={`Your new comment, on ${draft.label}`}
          state="draft"
          selected
          style={{ left: x, top: y }}
        />
      )}
    >
      <DraftComposer />
    </Popover>
  );
}

/** A part outlined: the one under the pointer while placing, named, or the selected thread's. */
export function Outline({ box, label }: { box: Box; label?: string | undefined }) {
  return (
    <div
      className="artifact-comment-outline"
      data-hover={label !== undefined || undefined}
      style={{ left: box.x, top: box.y, width: box.w, height: box.h }}
    >
      {label !== undefined && <span className="artifact-comment-outline-label">{label}</span>}
    </div>
  );
}

/**
 * Tells the layer a popup is drawn in that what it is anchored to moved,
 * whenever `stamp` changes: a page scrolled inside its frame, which the
 * window never hears, or a new pick moved the draft.
 */
export function Repositioned({ stamp }: { stamp: string }) {
  const container = useLayerContainer();
  useLayoutEffect(() => {
    container?.dispatchEvent(new CustomEvent(LAYER_REPOSITION_EVENT));
  }, [container, stamp]);
  return null;
}
