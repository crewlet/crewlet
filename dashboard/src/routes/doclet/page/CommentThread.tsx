/**
 * A page's thread, and a person's remark on it (`comment_on_page`, as the
 * signed-in person).
 *
 * THE THREAD IS UILET'S [CommentThread]; this file owns the contract around
 * it: the write, the mapping of the page's comments onto the thread's rows,
 * and the words the knowledge base says about who is told.
 *
 * A COMMENT DOES NOT SUBSCRIBE ITS WRITER here, which is the knowledge base's
 * own rule and the opposite of the tracker's: a page a hundred people remarked
 * on does not wake a hundred seats when somebody fixes a heading. The composer
 * says who is told instead: the page's watchers, and anybody mentioned.
 *
 * A comment is drawn as WHOEVER THE RECORD NAMES (`iam.ActorFor`), as every
 * other row of a page is: a person comments AS their seat, so the author is
 * the seat's handle, and a credential bound to no seat comments under its
 * login. A comment with no author is the engine's.
 *
 * THE READER'S OWN REMARK IS EDITED AND TAKEN DOWN IN PLACE, by the kit's
 * thread: an edit is `PATCH /doclet/pages/{id}/comments/{cid}` with the new
 * words and a removal `DELETE` on the same address, each keyed so a retry
 * after a lost answer is the same write. A remark is the reader's when its
 * author is the name their writes are recorded under ([ownsRemark]); who may
 * is the engine's rule, and its refusal is said in a toast, since the press
 * came from a control that closes when it lands.
 */

import { useMemo } from "react";
import { CommentThread, useToast, type CommentThreadComment } from "@crewlethq/ui";
import { RefusalNote } from "~/components/WriteButton.tsx";
import { useKeyedGesture, type KeyedAnswer } from "~/lib/keyedWrite.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { indexOrg, principalOfSeat } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useAct } from "~/lib/useAct.ts";
import { ownsRemark, useViewer } from "~/lib/viewer.ts";
import { pathSegment, type PageComment } from "~/protocol/index.ts";
import { session } from "~/protocol/session.ts";
import { useDictation } from "~/lib/dictation/source.ts";

/** A remark's own address on its page. */
export function pageRemarkRoute(pageID: string, commentID: string): string {
  return `/doclet/pages/${pathSegment(pageID)}/comments/${pathSegment(commentID)}`;
}

/**
 * A keyed page write that landed: the page and its thread are asked again,
 * past the position it landed at, as an act's reads are.
 */
function landed(answer: KeyedAnswer | null): boolean {
  if (answer?.kind !== "done") return false;
  const position = typeof answer.body.position === "string" ? answer.body.position : null;
  session.written("pages", position, ["knowledge"]);
  return true;
}

export function PageCommentThread({
  pageID,
  title,
  comments,
  now,
  hold,
}: {
  pageID: string;
  title: string;
  comments: PageComment[];
  now: number;
  /**
   * The page's own reason it takes no comment (it is in the trash). The thread
   * is then read only: no field at all, since the page says above why nothing
   * changes, and a box anybody can type into and nobody can send is no
   * answer to a page that is read only for everyone.
   */
  hold?: string | undefined;
}) {
  const write = useAct("comment_on_page");
  const remark = useKeyedGesture();
  const toast = useToast();
  const dictation = useDictation();
  const viewer = useViewer();
  const org = useOrg();
  const rows = useMemo<CommentThreadComment[]>(() => {
    const seats = indexOrg(org).byHandle;
    return comments.map((c) => {
      // A PAGE IN THE TRASH IS READ ONLY, its reader's own remarks included:
      // the engine refuses an edit or a removal there as it refuses a comment.
      const own = !hold && ownsRemark(c, viewer);
      return {
        id: c.id,
        author: c.author ? principalOfSeat(seats.get(c.author), c.author) : null,
        body: renderMarkdown(c.body),
        createdAt: c.created_at,
        editedAt: c.updated_at,
        replyTo: c.reply_to,
        source: c.body,
        editable: own,
        removable: own,
      };
    });
  }, [comments, org, viewer, hold]);

  // A REFUSAL IS SAID IN A TOAST, in the engine's words: the control that made
  // the press stays where it was, and the thread keeps the words typed.
  const said = (answer: KeyedAnswer | null, done: string): boolean => {
    if (landed(answer)) {
      toast.ok(done);
      return true;
    }
    if (answer && answer.kind !== "done") {
      toast.show({
        variant: answer.kind === "unknown" ? "warning" : "danger",
        title: answer.kind === "unknown" ? "Not confirmed yet" : "Not changed",
        message: answer.text,
        duration: 0,
      });
    }
    return false;
  };

  return (
    <CommentThread
      className="knowledge-comments"
      comments={rows}
      subject={title}
      now={now}
      dictation={dictation}
      // GIVEN WHENEVER THE PAGE TAKES A COMMENT, so a reader who cannot write
      // sees the field and why it is closed to them, rather than a thread
      // that silently has no composer. A page in the trash takes none.
      onSubmit={
        hold
          ? undefined
          : async (body, replyTo) => {
              const result = await write.run(
                { page: pageID, body, ...(replyTo ? { reply_to: replyTo.id } : {}) },
                { done: `Commented on “${title}”` },
              );
              return !!result && (result.kind === "applied" || result.kind === "pending");
            }
      }
      onEdit={async (comment, body) =>
        said(
          await remark.run({
            method: "PATCH",
            path: pageRemarkRoute(pageID, comment.id),
            body: { body },
          }),
          `Edited your comment on “${title}”`,
        )
      }
      onRemove={async (comment) =>
        said(
          await remark.run({ method: "DELETE", path: pageRemarkRoute(pageID, comment.id) }),
          `Deleted your comment on “${title}”`,
        )
      }
      disabledReason={write.access.can ? undefined : write.access.reason}
      busy={write.busy}
      hint="The page’s watchers are told, and anyone you mention."
      notice={<RefusalNote write={write} />}
      labels={{ system: "The engine", placeholder: "Comment, or @mention a seat" }}
    />
  );
}
