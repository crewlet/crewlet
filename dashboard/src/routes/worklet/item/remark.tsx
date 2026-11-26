/**
 * A person's own remark on a task: rewriting its words, and taking it down.
 *
 * # Each through the engine's own route
 *
 * An edit is `PATCH /worklet/items/{key}/comments/{cid}` with the new words,
 * and a removal `DELETE` on the same address, each keyed so a retry after a
 * lost answer is the same write ([useKeyedGesture]). Neither is a tool, so
 * neither goes through the act route: the engine decides both on the remark's
 * stored author, and its refusal is drawn where the press was made.
 *
 * # Only on the reader's own remark, and never on a task in the trash
 *
 * A remark is the reader's when its author is the name their writes are
 * recorded under (`iam.ActorFor`): their seat when the directory binds them
 * to one, their login when it does not, which is [ViewerState.owner] and, for
 * a login acting as itself, [ViewerState.login]. A task in the trash takes no
 * change at all until it is restored, so nothing is offered there.
 *
 * # A question or an answer is edited, never taken down
 *
 * The engine refuses to take down a remark that asks somebody something (its
 * asker withdraws it, saying why) or answers a question (the question would
 * be left closed with no answer). So Delete is not offered on either, and Edit
 * is, since rewording keeps what the remark asked and what it answered.
 */

import { useState, type ReactNode } from "react";
import { Button, Callout, Composer, Text, useToast } from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { PencilGlyph, TrashGlyph } from "@crewlethq/icons/glyphs";
import { useKeyedGesture, type KeyedAnswer } from "~/lib/keyedWrite.ts";
import { ownsRemark, useViewer } from "~/lib/viewer.ts";
import { pathSegment, type WorkComment, type WorkItem } from "~/protocol/index.ts";
import { session } from "~/protocol/session.ts";
import { useDictationBox } from "~/lib/dictation/source.ts";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { useDraftState } from "~/app/assistant/draft.ts";

/** The remark's own address on its task. */
export function remarkRoute(item: Pick<WorkItem, "id">, comment: Pick<WorkComment, "id">): string {
  return `/worklet/items/${pathSegment(item.id)}/comments/${pathSegment(comment.id)}`;
}

/** Whether the engine takes a remark down: never one that asks or answers. */
export function removable(comment: Pick<WorkComment, "ask" | "answers">): boolean {
  return !comment.ask && !comment.answers;
}

/**
 * A keyed tracker write that landed: the thread and the feed are asked again,
 * past the position it landed at, as an act's reads are.
 */
function landed(answer: KeyedAnswer): boolean {
  if (answer.kind !== "done") return false;
  const position = typeof answer.body.position === "string" ? answer.body.position : null;
  session.written("tracker", position, []);
  return true;
}

/** What a keyed write came to, where it did not land: the engine's words. */
function KeyedNote({ answer }: { answer: KeyedAnswer | null }) {
  if (!answer || answer.kind === "done") return null;
  return (
    <Callout variant={answer.kind === "unknown" ? "warning" : "danger"} role="alert">
      {answer.text}
    </Callout>
  );
}

/** What a remark row draws for its author's own gestures. */
export interface OwnRemark {
  /** The header's controls: Edit and, where the engine takes it down, Delete. */
  controls: ReactNode;
  /** The editor in place of the body while the remark is being rewritten, or null. */
  editor: ReactNode;
  /** The removal's confirmation, while it is open. */
  dialog: ReactNode;
}

/**
 * The author's own gestures on one remark, or nothing for a remark that is
 * not the reader's, is already taken down, or sits on a task in the trash.
 */
export function useOwnRemark(comment: WorkComment, item: WorkItem): OwnRemark {
  const viewer = useViewer();
  const [editing, setEditing] = useState(false);
  const [removing, setRemoving] = useState(false);
  const mine =
    !viewer.loading &&
    viewer.worklet &&
    !comment.removed &&
    !item.removed &&
    ownsRemark(comment, viewer);
  if (!mine) return { controls: null, editor: null, dialog: null };
  const named = comment.ask || comment.answers ? "your question or answer" : "your comment";
  return {
    controls: editing ? null : (
      <>
        <Button
          size="small"
          variant="ghost"
          leadingIcon={<PencilGlyph size="xs" />}
          aria-label={`Edit ${named}`}
          onClick={() => setEditing(true)}
        >
          Edit
        </Button>
        {removable(comment) && (
          <Button
            size="small"
            variant="ghost"
            leadingIcon={<TrashGlyph size="xs" />}
            aria-label="Delete your comment"
            onClick={() => setRemoving(true)}
          >
            Delete
          </Button>
        )}
      </>
    ),
    editor: editing ? (
      <RemarkEditor comment={comment} item={item} onClose={() => setEditing(false)} />
    ) : null,
    dialog: removing ? (
      <RemoveRemarkDialog comment={comment} item={item} onClose={() => setRemoving(false)} />
    ) : null,
  };
}

/** What the send key says in a box whose Enter saves rather than sends. */
const SAVE_KEYS = { send: "to save" };

/**
 * The remark's words, rewritten in place in the kit's message box, with Cancel
 * and Save in its footer: Enter saves and Shift+Enter writes a line, as in
 * every box a person says something in, and Escape gives up the rewrite.
 */
function RemarkEditor({
  comment,
  item,
  onClose,
}: {
  comment: WorkComment;
  item: WorkItem;
  onClose: () => void;
}) {
  const write = useKeyedGesture();
  const toast = useToast();
  const [body, setBody] = useDraftState(comment.body);
  const { dictation, refusal: dictating } = useDictationBox();
  // A FIELD of the task page's one draft, after its title and description.
  useDraftBox({
    path: `remark.${comment.id.toLowerCase().replace(/[^a-z0-9]+/g, "_")}`.slice(0, 64),
    label: `Your comment on ${item.key}`,
    hint: "Markdown",
    rank: 2,
    value: body,
    set: setBody,
    refuses: () => dictating,
  });
  const wanted = body.trim();
  const held =
    wanted === ""
      ? removable(comment)
        ? "A comment needs words. Delete it instead."
        : "A comment needs words."
      : wanted === comment.body.trim()
        ? "Nothing has changed yet."
        : undefined;
  const save = async () => {
    // THE BUTTON'S OWN GATE, because the box submits on its own key: a second
    // Enter while the first answer was out would send the rewrite again.
    if (held || write.busy) return;
    const answer = await write.run({
      method: "PATCH",
      path: remarkRoute(item, comment),
      body: { body: wanted },
    });
    if (answer && landed(answer)) {
      toast.ok(`Edited your comment on ${item.key}`);
      onClose();
    }
  };
  return (
    <div className="col gap-2">
      <Composer
        label={`Your comment on ${item.key}`}
        value={body}
        onChange={setBody}
        autoFocus
        canSubmit={!held && !write.busy}
        onSubmit={() => void save()}
        sendLabels={SAVE_KEYS}
        dictation={dictation}
        onKeyDown={(event) => {
          if (event.key !== "Escape" || write.busy) return;
          event.preventDefault();
          event.stopPropagation();
          onClose();
        }}
        actions={
          <>
            <Button size="small" variant="ghost" onClick={onClose} disabled={write.busy}>
              Cancel
            </Button>
            <Button
              size="small"
              variant="primary"
              loading={write.busy}
              disabledReason={held}
              onClick={() => void save()}
            >
              Save
            </Button>
          </>
        }
      />
      <KeyedNote answer={write.answer} />
    </div>
  );
}

/** The removal, confirmed: what goes and what stays. */
function RemoveRemarkDialog({
  comment,
  item,
  onClose,
}: {
  comment: WorkComment;
  item: WorkItem;
  onClose: () => void;
}) {
  const write = useKeyedGesture();
  const toast = useToast();
  const remove = async () => {
    if (write.busy) return;
    const answer = await write.run({ method: "DELETE", path: remarkRoute(item, comment) });
    if (answer && landed(answer)) {
      toast.ok(`Deleted your comment on ${item.key}`);
      onClose();
    }
  };
  return (
    <Modal
      open
      size="sm"
      title="Delete your comment?"
      icon={<TrashGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      stackBody
      onSubmit={() => void remove()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" loading={write.busy}>
            Delete
          </Button>
        </>
      }
    >
      <Text as="p" variant="body">
        {`Its words are taken down for everybody, here and in the inbox. The thread keeps its place, so replies to it still read in order.`}
      </Text>
      <KeyedNote answer={write.answer} />
    </Modal>
  );
}
