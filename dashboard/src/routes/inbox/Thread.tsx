/**
 * The conversation around the open row, read a page at a time from the task's
 * own thread (`work_comments`). The pane names it in the heading over it.
 *
 * WHICH COMMENTS ARE "THE THREAD" depends on what the row is about:
 *
 *  - an ASK: the replies in the ask's own thread, what answered it or was
 *    said back to it (`answers` or `reply_to` naming the ask);
 *  - a notice about a COMMENT: the replies to it, and the comment itself only
 *    where the pane quotes no more than a preview of it (`whole` false);
 *  - any other notice: the task's latest conversation, since the change it
 *    announces is not a comment and the talk around it is what a reader
 *    opens a notice to catch up on.
 *
 * THE ENGINE PAGES THAT THREAD (`thread`, `with_root`), never the task's
 * whole stream filtered here: a page of the task's comments kept to one
 * question's replies carried the TASK's cursor, so a question nobody had
 * replied to offered "Earlier comments" on any task with more than a page.
 *
 * PAGED BACKWARDS, never cut. The detail read returns the newest twenty and a
 * cursor nothing followed, so a conversation past twenty was invisible on
 * every screen; each "Earlier" press reads the page before the oldest one
 * shown, and the counts say "on the pages loaded" until the first comment is.
 */

import { useCallback, useEffect, useState } from "react";
import { Button, Skeleton, Text } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { CHECKED_WORDS, renderMarkdown } from "~/lib/markdown.ts";
import { fmtDateTime } from "~/lib/format.ts";
import type { OrgIndex } from "~/lib/seats.ts";
import type { WorkComment } from "~/protocol/index.ts";
import { moment, whoOf } from "./model.ts";

/** What the thread is around. */
export type ThreadOf =
  | { kind: "ask"; comment: string; options?: readonly { id: string; label: string }[] }
  | {
      kind: "comment";
      comment: string;
      /** The pane quotes the comment whole, so it is not listed again here. */
      whole: boolean;
      options?: readonly { id: string; label: string }[];
    }
  | { kind: "item" };

/** What `work_comments` is asked to narrow its pages to, for one thread. */
export function threadParams(of: ThreadOf): { thread?: string; with_root?: boolean } {
  switch (of.kind) {
    case "ask":
      return { thread: of.comment };
    case "comment":
      return of.whole ? { thread: of.comment } : { thread: of.comment, with_root: true };
    case "item":
      return {};
  }
}

/** How often the open thread is asked again: a reply lands, and nothing pushes it. */
const THREAD_POLL_MS = 30_000;

export function Thread({
  item,
  of,
  index,
  now,
  onTitle,
}: {
  /** The task's key or id. */
  item: string;
  of: ThreadOf;
  index: OrgIndex;
  now: number;
  /** Told the task's title, which the newest page carries. */
  onTitle?: (title: string) => void;
}) {
  // THE CURSORS OF THE PAGES SHOWN, newest first; "" is the newest page.
  const [cursors, setCursors] = useState<string[]>([""]);
  const [next, setNext] = useState<Record<string, string | null>>({});
  const [counts, setCounts] = useState<Record<string, number>>({});
  const onPage = useCallback((cursor: string, following: string | null, count: number) => {
    setNext((n) => (n[cursor] === following ? n : { ...n, [cursor]: following }));
    setCounts((c) => (c[cursor] === count ? c : { ...c, [cursor]: count }));
  }, []);
  const oldest = cursors[cursors.length - 1]!;
  const earlier = next[oldest];
  const total = cursors.reduce((n, c) => n + (counts[c] ?? 0), 0);
  const whole = earlier === null;
  return (
    <div className="inbox-thread">
      {earlier && (
        <Button
          size="small"
          variant="ghost"
          className="inbox-thread-earlier"
          onClick={() => setCursors((c) => [...c, earlier])}
        >
          Earlier comments
        </Button>
      )}
      {/* OLDEST PAGE FIRST, so the conversation reads down in the order it
          was written. */}
      {[...cursors].reverse().map((cursor) => (
        <ThreadPage
          key={cursor || "newest"}
          item={item}
          cursor={cursor}
          of={of}
          index={index}
          now={now}
          onPage={onPage}
          onTitle={cursor ? undefined : onTitle}
        />
      ))}
      {whole && total === 0 && (
        <Text as="p" variant="caption">
          {of.kind === "item" ? "Nobody has commented on this task yet." : "No replies yet."}
        </Text>
      )}
    </div>
  );
}

function ThreadPage({
  item,
  cursor,
  of,
  index,
  now,
  onPage,
  onTitle,
}: {
  item: string;
  cursor: string;
  of: ThreadOf;
  index: OrgIndex;
  now: number;
  onPage: (cursor: string, next: string | null, count: number) => void;
  onTitle?: (title: string) => void;
}) {
  const read = useQuery(
    "work_comments",
    { item, ...threadParams(of), ...(cursor ? { cursor } : {}), limit: 50 },
    // ONLY THE NEWEST PAGE IS POLLED: an older page is history, and a reply
    // lands on the newest.
    cursor ? {} : { pollMs: THREAD_POLL_MS },
  );
  const comments = read.data?.comments ?? [];
  const following = read.data ? (read.data.next_cursor ?? null) : undefined;
  const count = comments.length;
  useEffect(() => {
    if (following !== undefined) onPage(cursor, following, count);
  }, [cursor, following, count, onPage]);
  const title = read.data?.title ?? "";
  useEffect(() => {
    if (title) onTitle?.(title);
  }, [title, onTitle]);
  if (read.loading && !read.data)
    return <Skeleton variant="text" rows={2} label="Loading the thread" />;
  // AN EMPTY PAGE DRAWS NO LIST: an empty <ol> is still a flex item, and the
  // thread's gap on either side of it pushed "No replies yet." down by one.
  return (
    <QueryState error={read.error} refusal={read.refusal} loading={false}>
      {count > 0 && (
        <ol className="inbox-thread-list">
          {comments.map((c) => (
            <ThreadEntry
              key={c.id}
              comment={c}
              chose={
                c.choice && of.kind !== "item"
                  ? (of.options?.find((o) => o.id === c.choice)?.label ?? c.choice)
                  : ""
              }
              index={index}
              now={now}
            />
          ))}
        </ol>
      )}
    </QueryState>
  );
}

function ThreadEntry({
  comment,
  chose,
  index,
  now,
}: {
  comment: WorkComment;
  /** The option this answer chose, by its words, or "". */
  chose: string;
  index: OrgIndex;
  now: number;
}) {
  // WHOEVER THE RECORD NAMES (`iam.ActorFor`): a person the identity directory
  // binds to a seat comments AS that seat, and anybody bound to none under
  // their own login, a name, drawn as one. A comment with no author is the
  // engine's.
  const who = whoOf(index, comment.author, comment.author_kind);
  return (
    <li className="inbox-thread-entry">
      <SeatAvatar
        name={who?.name ?? comment.author}
        kind={who?.kind ?? "agent"}
        avatar={who?.avatar}
        size={26}
        decorative
      />
      <div className="inbox-thread-body">
        <div className="inbox-thread-meta">
          <strong>{who?.name ?? "The engine"}</strong>
          {/* THE READER'S OWN CLOCK, to the minute: a time alone today, the
              date with it before. */}
          <time dateTime={comment.created_at} title={fmtDateTime(comment.created_at)}>
            {moment(comment.created_at, now)}
          </time>
          {chose && comment.body.trim() && <span className="t-caption">{`chose “${chose}”`}</span>}
        </div>
        <div className="inbox-thread-text prose md">
          {comment.removed ? (
            <span className="muted">(this comment was removed)</span>
          ) : comment.body.trim() ? (
            renderMarkdown(comment.body, CHECKED_WORDS)
          ) : chose ? (
            // AN ANSWER THAT IS ONLY A CHOICE has no words, and the choice
            // is the whole of what it said.
            <p>{`Chose “${chose}”.`}</p>
          ) : null}
        </div>
      </div>
    </li>
  );
}
