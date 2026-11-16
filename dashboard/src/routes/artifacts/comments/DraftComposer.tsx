/**
 * A new thread being written: where it points, the part's words and picture,
 * and the words typed; beside its pin, or in the panel on a phone.
 */

import { useState } from "react";
import { Button, Checkbox, Composer } from "@crewlethq/ui";
import { openComment } from "~/lib/artifacts/api.ts";
import { useComments, useCommentWrite } from "./state.tsx";

export function DraftComposer() {
  const c = useComments();
  const write = useCommentWrite();
  const [text, setTyped] = useState(() => c?.draftWords.current ?? "");
  const setText = (next: string) => {
    if (c) c.draftWords.current = next;
    setTyped(next);
  };
  const [withCrop, setWithCrop] = useState(true);
  if (!c?.draft) return null;
  const d = c.draft;
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
    <div className="artifact-comment-draft">
      <p className="artifact-comment-where">
        <strong>{d.label}</strong> on v{c.version}
      </p>
      {d.quote && <blockquote className="artifact-comment-quote clamp">{d.quote}</blockquote>}
      {d.crop === undefined ? (
        <p className="artifact-comment-note">Drawing a picture of this part…</p>
      ) : d.crop ? (
        <div className="artifact-comment-picture">
          <img
            className="artifact-comment-crop"
            src={d.crop}
            alt="A picture of this part, as you see it"
          />
          <Checkbox
            size="sm"
            label="Include this picture"
            description="Everyone who can read the artifact sees it."
            checked={withCrop}
            onChange={(event) => setWithCrop(event.currentTarget.checked)}
          />
        </div>
      ) : null}
      <Composer
        label={`Comment on ${d.label}`}
        value={text}
        onChange={setText}
        placeholder="Say what should change"
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
