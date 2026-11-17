/**
 * A change's conversation, as a code host lays one out: what it proposes —
 * its description, a card opened by its owner — then whether it may merge,
 * the merge box with every condition and the controls (`Merge.tsx`), then
 * everything said about it on a rail in the order it was said, a comment of
 * the reader's own at the foot, and beside it all the rail of whom it asked,
 * who wrote it, its work item and its branches (`Rail.tsx`).
 *
 * THE TIMELINE IS THE CODE LOG'S ORDER: each review, each comment and each
 * revision is a record, and they are drawn by the position each landed at,
 * never by a clock a node read. A comment's replies are drawn under the
 * comment that started its thread, as the engine keeps them (thread order).
 * A comment on a line is drawn where its line is, on the Files tab, and here
 * only as a pointer to it.
 *
 * EVERY PAGE THE READER LOADS IS ONE TIMELINE: the engine pages reviews and
 * comments oldest first, so what one page leaves out is the newest, and Load
 * more asks each part's page after the one it holds. The timeline is built
 * over every page loaded, so a reply on a later page joins its root.
 */

import { useEffect, useRef, useState } from "react";
import { AsideLayout, Button, Stack, Tag, Textarea, useSendOnEnter } from "@crewlethq/ui";
import { GitBranchGlyph } from "@crewlethq/icons/glyphs";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { useDraftState } from "~/app/assistant/draft.ts";
import { PageFailed } from "~/components/common.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { verdictWord } from "~/lib/code.ts";
import { useNow } from "~/lib/clock.ts";
import { plural } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type {
  CodeChangeDetail,
  CodeCommentRow,
  CodeReviewRow,
  CodeRevisionRow,
} from "~/protocol/index.ts";
import { changeHref } from "../paths.ts";
import { Byline, Face, ShaChip } from "../ui.tsx";
import { Description } from "./Description.tsx";
import { MergeBox } from "./Merge.tsx";
import { ChangeRail } from "./Rail.tsx";
import { TimelineCard, TimelineEvent, TimelineList } from "./Timeline.tsx";
import { positionBefore, usePagedComments, usePagedReviews } from "./reads.ts";
import "~/styles/gitlet.css";

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
  const now = useNow();
  const [quoted, setQuoted] = useState<Quote | null>(null);
  const reviews = usePagedReviews(change);
  const comments = usePagedComments(change);
  const entries = timelineOf(reviews.items, comments.items, revisions);
  const more = reviews.more || comments.more;
  const paging = reviews.paging || comments.paging;
  const pageError = reviews.pageError ?? comments.pageError;
  // BOTH PARTS' NEXT PAGES AT ONE PRESS: the timeline interleaves them by
  // where each landed on the code log, so one part read further than the
  // other would draw a gap the reader could not see.
  const loadMore = () => {
    if (reviews.more) reviews.next();
    if (comments.more) comments.next();
  };
  return (
    <AsideLayout
      fit="page"
      asideLabel="About this change"
      aside={<ChangeRail change={change} revisions={revisions} reviews={reviews.items} />}
    >
      <Stack gap={5}>
        <Description
          change={change}
          onQuote={(text) => setQuoted((was) => ({ text, n: (was?.n ?? 0) + 1 }))}
        />
        <MergeBox change={change} />
        <section aria-label="Timeline" className="code-activity">
          <h2 className="code-activity__title">Activity</h2>
          {entries.length === 0 ? (
            <p className="muted">Nothing has been said about it yet.</p>
          ) : (
            <TimelineList label="What was said and done">
              {entries.map((entry) => (
                <TimelineEntry
                  key={`${entry.kind}-${entry.at.generation}-${entry.at.seq}`}
                  entry={entry}
                  change={change}
                  now={now}
                />
              ))}
            </TimelineList>
          )}
          {more && (
            <span>
              <Button variant="secondary" onClick={loadMore} disabled={paging} loading={paging}>
                Load more
              </Button>
            </span>
          )}
          {/* A PAGE THAT FAILED is said beside the press that asks it again. */}
          {pageError && <PageFailed error={pageError} />}
        </section>
        <CommentComposer change={change} quote={quoted} />
      </Stack>
    </AsideLayout>
  );
}

/** What a review's verdict is called on its card: the outlined word, never a hue. */
function verdictMark(verdict: string): string {
  switch (verdict) {
    case "approve":
      return "Approved";
    case "request_changes":
      return "Changes asked";
    case "comment":
      return "Comment";
  }
  return verdict;
}

/** One entry, as the timeline draws it. */
function TimelineEntry({
  entry,
  change,
  now,
}: {
  entry: Entry;
  change: CodeChangeDetail;
  now: number;
}) {
  switch (entry.kind) {
    case "review": {
      const r = entry.review;
      const files = changeHref(change.repo, change.number, {
        tab: "files",
        ...(r.revision !== change.revision ? { rev: r.revision } : {}),
      });
      return (
        <TimelineCard
          label={`Review by ${r.reviewer}`}
          head={
            <Byline
              who={r.reviewer}
              verb={`${verdictWord(r.verdict)} on revision ${r.revision}`}
              at={r.at}
              now={now}
            />
          }
          marks={<Tag appearance="outline">{verdictMark(r.verdict)}</Tag>}
          foot={
            r.findings > 0 ? (
              <a className="t-link" href={files}>
                {plural(r.findings, "finding")} on revision {r.revision} →
              </a>
            ) : undefined
          }
        >
          {r.summary ? (
            <div className="prose md">{renderMarkdown(r.summary)}</div>
          ) : (
            <p className="muted">No summary.</p>
          )}
        </TimelineCard>
      );
    }
    case "revision": {
      const v = entry.revision;
      return (
        <TimelineEvent glyph={<GitBranchGlyph size="sm" />}>
          <Byline who={v.pusher} verb={`pushed revision ${v.n}`} at={v.at} now={now}>
            <ShaChip oid={v.head} />
            {v.n > 1 && (
              // WHAT THE PUSH CHANGED, the engine's interdiff of this
              // revision since the one before it.
              <a
                className="t-link"
                href={changeHref(change.repo, change.number, {
                  tab: "files",
                  rev: v.n,
                  since: v.n - 1,
                })}
              >
                What it changed
              </a>
            )}
          </Byline>
        </TimelineEvent>
      );
    }
    case "thread":
      return (
        <li className="code-timeline__item">
          <Thread root={entry.root} replies={entry.replies} change={change} />
        </li>
      );
  }
}

/**
 * A thread, as a card: its first comment — where it is about a line, which
 * line, since its line is on the Files tab — its replies under it, each with
 * its writer's face, and for a reader a Resolve and a reply of their own.
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
  const now = useNow();
  const resolve = useAct("code_comment_on_change");
  const verb = root.anchor
    ? `commented on ${root.anchor.path}:${root.anchor.line} (revision ${root.anchor.revision})`
    : "commented";
  return (
    <article className="code-thread" aria-label={`Thread by ${root.author}`}>
      <div className="code-thread__head">
        <Byline who={root.author} verb={verb} at={root.at} now={now} />
        {root.resolved && <Tag appearance="outline">Resolved</Tag>}
      </div>
      <div className="code-thread__body">
        <div className="prose md">{renderMarkdown(root.body)}</div>
      </div>
      {replies.length > 0 && (
        <ol className="code-thread__replies" aria-label="Replies">
          {replies.map((c) => (
            <li key={c.id} className="code-thread__reply">
              <Byline who={c.author} at={c.at} now={now} />
              <div className="prose md">{renderMarkdown(c.body)}</div>
            </li>
          ))}
        </ol>
      )}
      <div className="code-thread__foot">
        <ReplyComposer change={change} target={{ thread: root.id }} />
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
  const [body, setBody] = useDraftState("");
  // EACH REPLY BOX IS A FIELD of the change page's one draft
  // (`ChangePage.tsx`), keyed by what it answers: a draft's path is
  // lowercase letters, digits, dots and underscores, so the id is spelled
  // in those.
  const answers = "thread" in target ? `thread ${target.thread}` : `finding ${target.finding}`;
  useDraftBox({
    path: `reply.${answers.toLowerCase().replace(/[^a-z0-9]+/g, "_")}`.slice(0, 64),
    label: "thread" in target ? "Reply in the thread" : "Reply to the finding",
    hint: "Markdown",
    rank: 2,
    value: body,
    set: setBody,
    refuses: () => null,
  });
  const blocked = body.trim() ? undefined : "Write the reply first.";
  const send = async () => {
    const result = await write.run({ change: change.ref, body, ...target }, { done: "Replied" });
    if (result && (result.kind === "applied" || result.kind === "pending")) setBody("");
  };
  // Enter sends, as in every message box; the button's own gate holds it.
  const keys = useSendOnEnter({ onSend: () => void send(), canSend: pressable(write, blocked) });
  return (
    <div className="code-reply">
      <span className="code-reply__field">
        <Textarea
          aria-label="Reply"
          aria-describedby={keys.describedBy}
          placeholder="Reply…"
          value={body}
          rows={1}
          autoResize
          onChange={(event) => setBody(event.target.value)}
          onKeyDown={keys.onKeyDown}
        />
        {keys.description}
      </span>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        blocked={blocked}
        onPress={() => void send()}
      >
        Reply
      </WriteButton>
    </div>
  );
}

/** Words quoted into the comment box; `n` counts the presses, so each lands once. */
interface Quote {
  text: string;
  n: number;
}

/** A comment of its own on the change, which starts a thread. */
function CommentComposer({ change, quote }: { change: CodeChangeDetail; quote: Quote | null }) {
  const viewer = useViewer();
  const write = useAct("code_comment_on_change");
  const [body, setBody] = useDraftState("");
  const field = useRef<HTMLTextAreaElement>(null);
  const landed = useRef(0);
  // A QUOTE IS APPENDED ONCE per press, and the box takes focus at its end.
  useEffect(() => {
    if (!quote || quote.n === landed.current) return;
    landed.current = quote.n;
    setBody((was) => (was && !was.endsWith("\n") ? `${was}\n\n` : was) + quote.text);
    const box = field.current;
    if (box) {
      box.focus();
      queueMicrotask(() => box.setSelectionRange(box.value.length, box.value.length));
    }
  }, [quote, setBody]);
  useDraftBox({
    path: "comment",
    label: `Comment on ${change.ref}`,
    hint: "Markdown",
    rank: 0,
    value: body,
    set: setBody,
    refuses: () => null,
  });
  const send = async () => {
    const result = await write.run(
      { change: change.ref, body },
      { done: `Commented on ${change.ref}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) setBody("");
  };
  const blocked = body.trim() ? undefined : "Write the comment first.";
  const keys = useSendOnEnter({ onSend: () => void send(), canSend: pressable(write, blocked) });
  return (
    <section aria-label="Comment on the change" className="code-compose">
      {viewer.owner && <Face who={viewer.owner} size="sm" />}
      <div className="code-compose__box">
        <Textarea
          ref={field}
          aria-label="Comment"
          aria-describedby={keys.describedBy}
          placeholder="Comment on the change"
          value={body}
          rows={3}
          onChange={(event) => setBody(event.target.value)}
          onKeyDown={keys.onKeyDown}
        />
        <div className="code-compose__foot">
          {keys.description}
          <WriteButton
            write={write}
            variant="primary"
            blocked={blocked}
            onPress={() => void send()}
          >
            Comment
          </WriteButton>
        </div>
      </div>
    </section>
  );
}
