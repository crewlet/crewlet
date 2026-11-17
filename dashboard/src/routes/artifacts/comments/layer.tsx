/** What every pin layer draws: a thread's pin, the draft's pin and composer, a part's outline. */

import { useLayoutEffect, useMemo } from "react";
import {
  CommentPin,
  LAYER_REPOSITION_EVENT,
  Popover,
  commentPinCursorAt,
  useLayerContainer,
} from "@crewlethq/ui";
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

/** The draft's pin and composer; on a phone or once the pin is off view, the panel holds it. */
export function DraftPin({ x, y }: { x: number; y: number }) {
  const c = useComments()!;
  const draft = c.draft;
  if (!draft) return null;
  const beside = !c.narrow && !draft.docked;
  return (
    <Popover
      label={`New comment: ${draft.label}`}
      open={beside}
      // BESIDE THE PIN, level with it, so the draft never covers what it is about;
      // as tall as it needs where the frame has room, and never past its edge.
      side="right"
      align="start"
      height="layer"
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
      <DraftComposer beside />
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

/** Repositions the layer's popups when `stamp` changes: the window never hears a frame scroll. */
export function Repositioned({ stamp }: { stamp: string }) {
  const container = useLayerContainer();
  useLayoutEffect(() => {
    container?.dispatchEvent(new CustomEvent(LAYER_REPOSITION_EVENT));
  }, [container, stamp]);
  return null;
}

/** The pointer while placing: the draft pin, in this frame's theme and brand. */
export function usePlacingCursor(placing: boolean): string | undefined {
  return useMemo(() => (placing ? commentPinCursorAt(document.body) : undefined), [placing]);
}
