/**
 * One comment of a thread and its Edit/Delete menu; a deleted one stays as a tombstone.
 */

import { useRef, useState, type ReactNode } from "react";
import { Button, Composer, Menu, Stack } from "@crewlethq/ui";
import { EllipsisGlyph, PencilGlyph, TrashGlyph } from "@crewlethq/icons/glyphs";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { useDraftState } from "~/app/assistant/draft.ts";
import { ConfirmModal } from "~/components/Modal.tsx";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { ago } from "~/lib/ago.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useAct } from "~/lib/useAct.ts";
import { useDictationBox } from "~/lib/dictation/source.ts";
import { menuHold } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeChangeDetail, CodeCommentRow } from "~/protocol/index.ts";
import { useWhoName } from "../ui.tsx";
import "~/styles/gitlet.css";

/** Why Save is held while the comment's words moved under the reader. */
export const COMMENT_MOVED =
  "This comment changed since you began editing. Start again from the latest to keep both.";

/** The reader wrote the comment, as the engine judges an edit: the same seat, a person's. */
export function ownComment(comment: CodeCommentRow, viewer: { handle: string }): boolean {
  return (
    viewer.handle !== "" && comment.author === viewer.handle && comment.author_kind === "person"
  );
}

export function CommentCard({
  comment,
  change,
  layout,
  byline,
  marks,
}: {
  comment: CodeCommentRow;
  change: CodeChangeDetail;
  /** A thread's first comment, in its card's head and body; or a reply, one row. */
  layout: "root" | "reply";
  byline: ReactNode;
  /** Tags beside the byline: Resolved on a root, Author on a reply. */
  marks?: ReactNode;
}) {
  const now = useNow();
  const viewer = useViewer();
  const edit = useAct("code_comment_on_change");
  const remove = useAct("code_delete_comment");
  const [editing, setEditing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const head = useRef<HTMLSpanElement>(null);
  const deleted = Boolean(comment.deleted_at);
  const own = ownComment(comment, viewer);
  const whoName = useWhoName();
  // FOCUS GOES BACK TO THE MENU'S TRIGGER once the editor closes.
  const backToMenu = () =>
    queueMicrotask(() => head.current?.querySelector<HTMLElement>("[aria-haspopup]")?.focus());
  const items = [
    ...(own
      ? [
          {
            key: "edit",
            label: "Edit",
            icon: <PencilGlyph size="sm" />,
            onSelect: () => setEditing(true),
            ...menuHold(edit.access),
          },
        ]
      : []),
    {
      key: "delete",
      label: "Delete",
      icon: <TrashGlyph size="sm" />,
      danger: true,
      onSelect: () => setConfirming(true),
      ...menuHold(remove.access),
    },
  ];
  const edited = comment.edited_at && !deleted && (
    <span className="code-comment__edited" title={`Edited ${fmtDateTime(comment.edited_at)}`}>
      edited
    </span>
  );
  const menu = !deleted && (
    <span className="code-comment__menu" ref={head}>
      <Menu
        label="More actions on the comment"
        icon={<EllipsisGlyph size="sm" />}
        triggerVariant="ghost"
        align="end"
        items={items}
      />
    </span>
  );
  const words = deleted ? (
    <p className="code-comment__gone">
      This comment was deleted
      {comment.deleted_by ? ` by ${whoName(comment.deleted_by)}` : ""}
      {comment.deleted_at && (
        <>
          {" "}
          <time dateTime={comment.deleted_at} title={fmtDateTime(comment.deleted_at)}>
            {ago(comment.deleted_at, now, { clock: true })}
          </time>
        </>
      )}
      .
    </p>
  ) : editing ? (
    <EditComment
      comment={comment}
      change={change}
      onDone={() => {
        setEditing(false);
        backToMenu();
      }}
    />
  ) : (
    <div className="prose md">{renderMarkdown(comment.body)}</div>
  );
  const confirm = (
    <ConfirmModal
      open={confirming}
      destructive
      title="Delete this comment?"
      message={
        own
          ? "Its words are cleared for everybody. Its place in the thread and every reply stay, and the thread says you deleted it."
          : `${whoName(comment.author)}'s words are cleared for everybody. Its place in the thread and every reply stay, and the thread says you deleted it.`
      }
      cancelLabel="Keep it"
      confirmLabel="Delete comment"
      onClose={() => setConfirming(false)}
      submitting={remove.busy}
      onConfirm={async () => {
        await remove.run(
          { change: change.ref, comment: comment.id },
          { done: "Deleted the comment" },
        );
        setConfirming(false);
      }}
    />
  );
  if (layout === "root") {
    return (
      <>
        <div className="code-thread__head">
          {byline}
          {edited}
          {marks}
          {menu}
        </div>
        <div className="code-thread__body">{words}</div>
        {confirm}
      </>
    );
  }
  return (
    <>
      <span className="code-thread__said">
        {byline}
        {marks}
        {edited}
        {menu}
      </span>
      {words}
      {confirm}
    </>
  );
}

/** A comment's words edited in place, held to the words it had as it opened. */
function EditComment({
  comment,
  change,
  onDone,
}: {
  comment: CodeCommentRow;
  change: CodeChangeDetail;
  onDone: () => void;
}) {
  const write = useAct("code_comment_on_change");
  const [opened, setOpened] = useState(comment.body);
  const { dictation, refusal: dictating } = useDictationBox();
  const [body, setBody] = useDraftState(opened);
  // A draft's path is lowercase letters, digits, dots and underscores.
  useDraftBox({
    path: `comment_edit.${comment.id.toLowerCase().replace(/[^a-z0-9]+/g, "_")}`.slice(0, 64),
    label: "The comment you are editing",
    hint: "Markdown",
    rank: 1,
    value: body,
    set: setBody,
    refuses: () => dictating,
  });
  const stale = comment.body !== opened;
  const blocked = stale
    ? COMMENT_MOVED
    : !body.trim()
      ? "A comment says something: write it, or delete the comment."
      : body === opened
        ? "Change the comment first."
        : undefined;
  const save = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      { change: change.ref, edit: comment.id, body },
      { done: "Saved the comment" },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onDone();
  };
  const again = () => {
    setOpened(comment.body);
    setBody(comment.body);
  };
  return (
    <form
      aria-label="Edit the comment"
      onSubmit={(event) => {
        event.preventDefault();
        void save();
      }}
      onKeyDown={(event) => {
        if (event.key === "Escape" && body === opened) {
          event.preventDefault();
          onDone();
        }
      }}
    >
      <Stack gap={2}>
        <Composer
          label="Comment"
          value={body}
          rows={3}
          autoFocus
          onChange={setBody}
          dictation={dictation}
          onKeyDown={(event) => {
            if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
              event.preventDefault();
              void save();
            }
          }}
          actions={
            <>
              {stale && (
                <Button size="small" variant="ghost" onClick={again}>
                  Start again from the latest
                </Button>
              )}
              <Button size="small" variant="secondary" onClick={onDone} disabled={write.busy}>
                Cancel
              </Button>
              <WriteButton
                write={write}
                size="small"
                variant="primary"
                type="submit"
                showRefusal={false}
                blocked={blocked}
                onPress={() => void save()}
              >
                Save
              </WriteButton>
            </>
          }
        />
        <RefusalNote write={write} />
        {blocked && <p className="muted t-caption">{blocked}</p>}
      </Stack>
    </form>
  );
}
