/** A new thread being written: beside its pin, or in the panel on a phone. */

import { useState } from "react";
import { Button, Card, Checkbox, Composer, Spinner } from "@crewlethq/ui";
import { openComment } from "~/lib/artifacts/api.ts";
import { useComments, useCommentWrite } from "./state.tsx";
import { useDictation } from "~/lib/dictation/source.ts";

/** Where the picture of the part is: being drawn, shown, or none could be drawn. */
export type PictureState = "drawing" | "shown" | "none";

export function pictureState(crop: string | null | undefined): PictureState {
  return crop === undefined ? "drawing" : crop ? "shown" : "none";
}

export function DraftComposer({ beside = false }: { beside?: boolean }) {
  const c = useComments();
  const write = useCommentWrite();
  const [text, setTyped] = useState(() => c?.draftWords.current ?? "");
  const setText = (next: string) => {
    if (c) c.draftWords.current = next;
    setTyped(next);
  };
  const [withCrop, setWithCrop] = useState(true);
  const dictation = useDictation();
  if (!c?.draft) return null;
  const d = c.draft;
  const picture = pictureState(d.crop);
  const post = async () => {
    if (!text.trim() || write.busy) return;
    const crop = withCrop && d.crop ? d.crop : undefined;
    const ok = await write.run(
      openComment(c.detail.id, {
        body: text,
        version: c.version,
        anchor: d.anchor,
        quote: d.quote,
        crop,
      }),
      `Commented on ${d.label}`,
    );
    if (ok) {
      setText("");
      // POSTED, COMMENT MODE IS DONE: the next comment is a new gesture.
      c.setPlacing(false);
    }
  };
  return (
    <div className="artifact-comment-draft" data-beside={beside || undefined}>
      <p className="artifact-comment-where">
        <strong>{d.label}</strong> on v{c.version}
      </p>
      {d.quote && <blockquote className="artifact-comment-quote clamp">{d.quote}</blockquote>}
      {/* ONE HEIGHT IN EVERY STATE, so the panel does not jump when the picture arrives. */}
      <Card
        variant="inset"
        padding="tight"
        className="artifact-comment-preview"
        data-picture={picture}
        aria-busy={picture === "drawing" || undefined}
      >
        {picture === "shown" ? (
          <img
            className="artifact-comment-crop"
            src={d.crop ?? undefined}
            alt="A picture of this part, as you see it"
          />
        ) : picture === "drawing" ? (
          <p className="artifact-comment-note" role="status">
            <Spinner size="sm" /> Drawing a picture of this part…
          </p>
        ) : (
          <p className="artifact-comment-note">No picture of this part could be drawn.</p>
        )}
      </Card>
      <Checkbox
        size="sm"
        label="Include this picture"
        description="Everyone who can read the artifact sees it."
        checked={withCrop && picture === "shown"}
        disabled={picture !== "shown"}
        onChange={(event) => setWithCrop(event.currentTarget.checked)}
      />
      <Composer
        label={`Comment on ${d.label}`}
        value={text}
        onChange={setText}
        placeholder="Say what should change"
        // THE ACTIONS KEEP ONE ROW at a draft's width; the field's description still names the keys.
        sendHint={false}
        dictation={dictation}
        onSubmit={() => void post()}
        actions={
          <>
            <Button size="small" variant="ghost" onClick={() => c.clearDraft()}>
              Cancel
            </Button>
            <Button
              size="small"
              disabled={!text.trim()}
              loading={write.busy}
              onClick={() => void post()}
            >
              Comment
            </Button>
          </>
        }
      />
      <p className="artifact-comment-note">{c.ownerName} is told, and answers in a new version.</p>
    </div>
  );
}
