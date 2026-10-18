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
 */

import { useMemo } from "react";
import { CommentThread, type CommentThreadComment } from "@crewlethq/ui";
import { RefusalNote } from "~/components/WriteButton.tsx";
import { renderMarkdown } from "~/lib/markdown.ts";
import { indexOrg, principalOfSeat } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useAct } from "~/lib/useAct.ts";
import type { PageComment } from "~/protocol/index.ts";

export function PageCommentThread({
  pageID,
  title,
  comments,
  now,
}: {
  pageID: string;
  title: string;
  comments: PageComment[];
  now: number;
}) {
  const write = useAct("comment_on_page");
  const org = useOrg();
  const rows = useMemo<CommentThreadComment[]>(() => {
    const seats = indexOrg(org).byHandle;
    return comments.map((c) => ({
      id: c.id,
      author: c.author ? principalOfSeat(seats.get(c.author), c.author) : null,
      body: renderMarkdown(c.body),
      createdAt: c.created_at,
      editedAt: c.updated_at,
      replyTo: c.reply_to,
    }));
  }, [comments, org]);

  return (
    <CommentThread
      className="knowledge-comments"
      comments={rows}
      subject={title}
      now={now}
      // ALWAYS GIVEN, so a reader who cannot write sees the field and why it
      // is closed to them, rather than a thread that silently has no composer.
      onSubmit={async (body, replyTo) => {
        const result = await write.run(
          { page: pageID, body, ...(replyTo ? { reply_to: replyTo.id } : {}) },
          { done: `Commented on “${title}”` },
        );
        return !!result && (result.kind === "applied" || result.kind === "pending");
      }}
      disabledReason={write.access.can ? undefined : write.access.reason}
      busy={write.busy}
      hint="The page’s watchers are told, and anyone you mention."
      notice={<RefusalNote write={write} />}
      labels={{ system: "The engine", placeholder: "Comment, or @mention a seat" }}
    />
  );
}
