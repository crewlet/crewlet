/**
 * A change's conversation: whether it may merge, what it proposes, and
 * everything said about it in the order it was said.
 *
 * THE TIMELINE IS THE CODE LOG'S ORDER: each review, each comment and each
 * revision is a record, and they are drawn by the position each landed at,
 * never by a clock a node read. A comment's replies are drawn under the
 * comment that started its thread, as the engine keeps them (thread order).
 * A comment on a line is drawn where its line is, on the Files tab, and here
 * only as a pointer to it.
 */

import { useState } from "react";
import { Callout, Stack, Tag, Textarea } from "@crewlethq/ui";
import { WriteButton } from "~/components/WriteButton.tsx";
import { changeLive, shortOID, verdictWord } from "~/lib/code.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useAct } from "~/lib/useAct.ts";
import type {
  CodeChangeDetail,
  CodeCommentRow,
  CodeReviewRow,
  CodeRevisionRow,
} from "~/protocol/index.ts";
import { Readiness } from "../ChangePeek.tsx";
import { positionBefore, useChangePart } from "./reads.ts";

/** One entry of the timeline, by the record that made it. */
type Entry =
  | { kind: "review"; at: CodeReviewRow["position"]; review: CodeReviewRow }
  | {
      kind: "thread";
      at: CodeCommentRow["position"];
      root: CodeCommentRow;
      replies: CodeCommentRow[];
    }
  | { kind: "revision"; at: CodeRevisionRow["position"]; revision: CodeRevisionRow };

/**
 * The timeline's entries, in the order their records landed: each review, each
 * thread by its first comment with its replies under it, and each revision.
 * A thread on a finding is drawn with its finding, on the Files tab.
 */
export function timelineOf(
  reviews: readonly CodeReviewRow[],
  comments: readonly CodeCommentRow[],
  revisions: readonly CodeRevisionRow[],
): Entry[] {
  const threads = new Map<string, { root: CodeCommentRow; replies: CodeCommentRow[] }>();
  for (const c of comments) {
    if (c.finding_id) continue;
    if (!c.parent_id) threads.set(c.id, { root: c, replies: [] });
    else threads.get(c.parent_id)?.replies.push(c);
  }
  const entries: Entry[] = [
    ...reviews.map((review) => ({ kind: "review" as const, at: review.position, review })),
    ...[...threads.values()].map((t) => ({ kind: "thread" as const, at: t.root.position, ...t })),
    ...revisions.map((revision) => ({
      kind: "revision" as const,
      at: revision.position,
      revision,
    })),
  ];
  return entries.sort((a, b) =>
    positionBefore(a.at, b.at) ? -1 : positionBefore(b.at, a.at) ? 1 : 0,
  );
}

export function Conversation({
  change,
  revisions,
}: {
  change: CodeChangeDetail;
  revisions: readonly CodeRevisionRow[];
}) {
  const reviews = useChangePart(change, "reviews");
  const comments = useChangePart(change, "comments");
  const entries = timelineOf(reviews.data?.reviews ?? [], comments.data?.comments ?? [], revisions);
  const more = Boolean(reviews.data?.next || comments.data?.next);
  return (
    <Stack gap={4}>
      {changeLive(change.state) && change.readiness && <Readiness readiness={change.readiness} />}
      <section aria-label="Description">
        {change.body ? (
          <div className="prose md">{renderMarkdown(change.body)}</div>
        ) : (
          <p className="muted">No description.</p>
        )}
      </section>
      <section aria-label="Timeline">
        <ol className="code-timeline">
          {entries.map((entry) => (
            <li key={`${entry.kind}-${entry.at.generation}-${entry.at.seq}`}>
              <TimelineEntry entry={entry} change={change} />
            </li>
          ))}
        </ol>
        {more && (
          <Callout variant="neutral">
            More was said than one page holds: the newest reviews and comments are not drawn here.
          </Callout>
        )}
      </section>
      <CommentComposer change={change} />
    </Stack>
  );
}

/** One entry, as the timeline draws it. */
function TimelineEntry({ entry, change }: { entry: Entry; change: CodeChangeDetail }) {
  const seatBadge = useSeatBadgeOf();
  switch (entry.kind) {
    case "review": {
      const r = entry.review;
      return (
        <article className="code-entry">
          <p>
            <strong>{seatBadge(r.reviewer).name}</strong> {verdictWord(r.verdict)} revision{" "}
            {r.revision}
            {r.findings > 0 && ` with ${r.findings} finding${r.findings === 1 ? "" : "s"}`}{" "}
            <span className="muted">{fmtDateTime(r.at)}</span>
          </p>
          {r.summary && <div className="prose md">{renderMarkdown(r.summary)}</div>}
        </article>
      );
    }
    case "revision": {
      const v = entry.revision;
      return (
        <p className="code-entry muted">
          {seatBadge(v.pusher).name} pushed revision {v.n},{" "}
          <span className="mono">{shortOID(v.head)}</span> {fmtDateTime(v.at)}
        </p>
      );
    }
    case "thread":
      return <Thread root={entry.root} replies={entry.replies} change={change} />;
  }
}

/**
 * A thread: its first comment, its replies, and — for a reader — a reply and
 * Resolve. A thread on a line says which, since its line is on the Files tab.
 */
export function Thread({
  root,
  replies,
  change,
}: {
  root: CodeCommentRow;
  replies: readonly CodeCommentRow[];
  change: CodeChangeDetail;
}) {
  const seatBadge = useSeatBadgeOf();
  const resolve = useAct("code_comment_on_change");
  return (
    <article className="code-entry">
      {root.anchor && (
        <p className="muted mono">
          {root.anchor.path}:{root.anchor.line} (revision {root.anchor.revision})
        </p>
      )}
      {[root, ...replies].map((c) => (
        <div key={c.id}>
          <p>
            <strong>{seatBadge(c.author).name}</strong>{" "}
            <span className="muted">{fmtDateTime(c.at)}</span>
          </p>
          <div className="prose md">{renderMarkdown(c.body)}</div>
        </div>
      ))}
      <div className="row gap-2">
        {root.resolved && <Tag appearance="outline">Resolved</Tag>}
        <WriteButton
          write={resolve}
          size="small"
          variant="ghost"
          onPress={() =>
            void resolve.run(
              { change: change.ref, thread: root.id, resolve: !root.resolved },
              { done: root.resolved ? "Opened the thread again" : "Resolved the thread" },
            )
          }
        >
          {root.resolved ? "Reopen thread" : "Resolve"}
        </WriteButton>
      </div>
      <ReplyComposer change={change} target={{ thread: root.id }} />
    </article>
  );
}

/**
 * A reply: to a thread by its first comment's id, or to a finding by its id.
 * Sent once; the field empties only once the engine has taken it.
 */
export function ReplyComposer({
  change,
  target,
}: {
  change: CodeChangeDetail;
  target: { thread: string } | { finding: string };
}) {
  const write = useAct("code_comment_on_change");
  const [body, setBody] = useState("");
  const send = async () => {
    const result = await write.run({ change: change.ref, body, ...target }, { done: "Replied" });
    if (result && (result.kind === "applied" || result.kind === "pending")) setBody("");
  };
  return (
    <div className="code-reply">
      <Textarea
        aria-label="Reply"
        placeholder="Reply"
        value={body}
        rows={2}
        onChange={(event) => setBody(event.target.value)}
      />
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={body.trim() ? undefined : "Write the reply first."}
        onPress={() => void send()}
      >
        Reply
      </WriteButton>
    </div>
  );
}

/** A comment of its own on the change, which starts a thread. */
function CommentComposer({ change }: { change: CodeChangeDetail }) {
  const write = useAct("code_comment_on_change");
  const [body, setBody] = useState("");
  const send = async () => {
    const result = await write.run(
      { change: change.ref, body },
      { done: `Commented on ${change.ref}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) setBody("");
  };
  return (
    <section aria-label="Comment on the change" className="code-reply">
      <Textarea
        aria-label="Comment"
        placeholder="Comment on the change"
        value={body}
        rows={3}
        onChange={(event) => setBody(event.target.value)}
      />
      <WriteButton
        write={write}
        variant="secondary"
        blocked={body.trim() ? undefined : "Write the comment first."}
        onPress={() => void send()}
      >
        Comment
      </WriteButton>
    </section>
  );
}
