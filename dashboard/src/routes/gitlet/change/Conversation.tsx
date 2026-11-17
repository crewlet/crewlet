/**
 * A change's conversation, as a code host lays one out: one timeline down a
 * rail, its description first (a card opened by its owner), then everything
 * said and done about it in the order it landed, then whether it may merge
 * (the merge box, `Merge.tsx`) and a comment of the reader's own, and beside
 * it all the rail of whom it asked, who wrote it, its work item and its
 * branches (`Rail.tsx`).
 *
 * THE TIMELINE IS THE CODE LOG'S ORDER: each review, each comment, each
 * revision and each ask for review is a record, and they are drawn by the
 * position each landed at, never by a clock a node read. A comment's replies
 * are drawn under the comment that started its thread, as the engine keeps
 * them (thread order).
 *
 * EVERY PAGE THE READER LOADS IS ONE TIMELINE: the engine pages each part
 * oldest first, so what one page leaves out is the newest, and Load more asks
 * each part's page after the one it holds. The timeline is built over every
 * page loaded, so a reply on a later page joins its root.
 */

import { Fragment, useEffect, useRef, useState, type ReactNode } from "react";
import { AsideLayout, Button, Stack, Tag, Textarea, useSendOnEnter } from "@crewlethq/ui";
import {
  CheckGlyph,
  EyeGlyph,
  FileDiffGlyph,
  GitCommitVerticalGlyph,
  GitCompareGlyph,
  MessageSquareGlyph,
  ShieldUserGlyph,
  UserGlyph,
  UserPlusGlyph,
} from "@crewlethq/icons/glyphs";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { useDraftState } from "~/app/assistant/draft.ts";
import { PageFailed } from "~/components/common.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useNow } from "~/lib/clock.ts";
import { plural } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type {
  CodeChangeDetail,
  CodeCommentRow,
  CodeReviewRow,
  CodeReviewerRow,
  CodeRevisionRow,
} from "~/protocol/index.ts";
import { ownedPathList } from "../owners.ts";
import { changeHref } from "../paths.ts";
import { Byline, Face, ShaChip, useWhoName } from "../ui.tsx";
import { Description } from "./Description.tsx";
import { MergeBox } from "./Merge.tsx";
import { ChangeRail } from "./Rail.tsx";
import {
  Disc,
  Said,
  TimelineEvent,
  TimelineItem,
  TimelineList,
  type DiscTone,
} from "./Timeline.tsx";
import {
  partRefusal,
  positionBefore,
  refusedWords,
  usePagedComments,
  usePagedReviewers,
  usePagedReviews,
} from "./reads.ts";
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
  | { kind: "revision"; at: CodeRevisionRow["position"]; revision: CodeRevisionRow }
  | { kind: "ask"; at: CodeReviewerRow["position"]; ask: CodeReviewerRow; again: boolean };

/**
 * The timeline's entries, in the order their records landed: each review, each
 * thread by its first comment with its replies under it, each revision, and
 * each ask that stands or was withdrawn. A thread on a finding is drawn with
 * its finding; an answered or ended ask's position is the record that moved it.
 * An ask after a review of the reviewer's own is one asked `again`.
 */
export function timelineOf(
  reviews: readonly CodeReviewRow[],
  comments: readonly CodeCommentRow[],
  revisions: readonly CodeRevisionRow[],
  asks: readonly CodeReviewerRow[] = [],
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
    ...asks
      .filter((ask) => ask.state === "requested" || ask.state === "withdrawn")
      .map((ask) => ({
        kind: "ask" as const,
        at: ask.position,
        ask,
        again: reviews.some(
          (r) =>
            r.reviewer === ask.reviewer &&
            r.reviewer_kind === ask.kind &&
            positionBefore(r.position, ask.position),
        ),
      })),
  ];
  return entries.sort((a, b) =>
    positionBefore(a.at, b.at) ? -1 : positionBefore(b.at, a.at) ? 1 : 0,
  );
}

/** An entry's key: its record's identity, which one record's several asks do not share. */
export function entryKey(entry: Entry): string {
  switch (entry.kind) {
    case "review":
      return `review-${entry.review.id}`;
    case "thread":
      return `thread-${entry.root.id}`;
    case "revision":
      return `revision-${entry.revision.n}`;
    case "ask":
      return `ask-${entry.ask.kind}:${entry.ask.reviewer}`;
  }
}

/** A review's disc tone: its verdict's, the verdict said in words beside it. */
export function verdictTone(verdict: string): DiscTone {
  return verdict === "request_changes" ? "danger" : verdict === "approve" ? "success" : "neutral";
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
  // ONE READ for the timeline's asks and the rail's reviewers.
  const asked = usePagedReviewers(change);
  const entries = timelineOf(reviews.items, comments.items, revisions, asked.items);
  const askedRefused = partRefusal(asked);
  const parts = [reviews, comments, asked];
  const more = parts.some((p) => p.more);
  const paging = parts.some((p) => p.paging);
  const pageError = parts.find((p) => p.pageError)?.pageError ?? null;
  // EVERY PART'S NEXT PAGE AT ONE PRESS: the timeline interleaves them by
  // where each landed on the code log, so one part read further than another
  // would draw a gap the reader could not see.
  const loadMore = () => {
    for (const p of parts) if (p.more) p.next();
  };
  return (
    <AsideLayout
      fit="page"
      asideLabel="About this change"
      aside={
        <ChangeRail change={change} revisions={revisions} reviews={reviews.items} asked={asked} />
      }
    >
      <Stack gap={5}>
        <section aria-label="Timeline" className="code-activity">
          <TimelineList label="What was said and done">
            <TimelineItem face={change.owner}>
              <Description
                change={change}
                onQuote={(text) => setQuoted((was) => ({ text, n: (was?.n ?? 0) + 1 }))}
              />
            </TimelineItem>
            {entries.map((entry) => (
              <TimelineEntry key={entryKey(entry)} entry={entry} change={change} now={now} />
            ))}
          </TimelineList>
          {more && (
            <span>
              <Button variant="secondary" onClick={loadMore} disabled={paging} loading={paging}>
                Load more
              </Button>
            </span>
          )}
          {/* A PAGE THAT FAILED is said beside the press that asks it again. */}
          {pageError && <PageFailed error={pageError} />}
          {askedRefused !== null && (
            <p className="muted">
              {refusedWords("Who was asked to review is unavailable", askedRefused)}
            </p>
          )}
        </section>
        <hr className="code-tl-break" />
        <MergeBox change={change} />
        <CommentComposer change={change} quote={quoted} />
      </Stack>
    </AsideLayout>
  );
}

/** Why an ask carries no time: said as what is known, never guessed. */
export const EARLIER_TITLE =
  "When this was recorded is not kept here: its notices have passed their horizon, or it told nobody.";

/** An ask whose time is not kept: earlier than what follows it, and no more. */
function Earlier() {
  return (
    <span className="code-byline__when" title={EARLIER_TITLE}>
      earlier
    </span>
  );
}

/** Why a path owner was asked: its paths, each as code, or that its facts predate them. */
function ownerWords(ask: CodeReviewerRow): ReactNode {
  const { paths, more } = ownedPathList(ask);
  if (paths.length === 0) return "as an owner of paths it touches";
  const said: ReactNode[] = paths.map((path) => (
    <code key={path} className="code-tl-path">
      {path}
    </code>
  ));
  if (more > 0) said.push(`${more} more`);
  return (
    <>
      as the owner of{" "}
      {said.map((part, i) => (
        <Fragment key={i}>
          {i === 0 ? "" : i === said.length - 1 ? " and " : ", "}
          {part}
        </Fragment>
      ))}
    </>
  );
}

/** What a review did, in words: the verdict is never told by the disc's tone alone. */
function reviewVerb(review: CodeReviewRow, files: string): ReactNode {
  const revision = (
    <a className="prose-link code-tl-strong" href={files}>
      revision {review.revision}
    </a>
  );
  switch (review.verdict) {
    case "request_changes":
      return <>requested changes on {revision}</>;
    case "approve":
      return <>approved {revision}</>;
    case "comment":
      return <>reviewed {revision}</>;
  }
  return (
    <>
      reviewed {revision} ({review.verdict})
    </>
  );
}

/** A review's disc glyph, by its verdict. */
function reviewGlyph(verdict: string): ReactNode {
  if (verdict === "request_changes") return <FileDiffGlyph size="sm" />;
  if (verdict === "approve") return <CheckGlyph size="sm" />;
  return <EyeGlyph size="sm" />;
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
        <TimelineEvent
          disc={<Disc glyph={reviewGlyph(r.verdict)} tone={verdictTone(r.verdict)} />}
          end={
            <a className="t-link" href={files}>
              View reviewed changes
            </a>
          }
          under={
            <article className="code-review-box" aria-label={`Review by ${r.reviewer}`}>
              {r.summary ? (
                <div className="prose md">{renderMarkdown(r.summary)}</div>
              ) : (
                <p className="muted">No summary.</p>
              )}
              {r.findings > 0 && (
                <a className="t-link" href={files}>
                  {plural(r.findings, "finding")} on revision {r.revision}
                </a>
              )}
            </article>
          }
        >
          <Byline who={r.reviewer} verb={reviewVerb(r, files)} at={r.at} now={now}>
            {r.verdict === "comment" && (
              <Tag size="sm" appearance="outline" leadingIcon={<MessageSquareGlyph size="xs" />}>
                Comment
              </Tag>
            )}
          </Byline>
        </TimelineEvent>
      );
    }
    case "revision": {
      const v = entry.revision;
      const head = v.n === change.revision;
      return (
        <TimelineEvent
          disc={<Disc glyph={<GitCommitVerticalGlyph size="sm" />} size="small" />}
          end={
            <>
              {v.n > 1 && (
                // WHAT THE PUSH CHANGED, the engine's interdiff since the revision before.
                <a
                  className="t-link code-tl-compare"
                  href={changeHref(change.repo, change.number, {
                    tab: "files",
                    rev: v.n,
                    since: v.n - 1,
                  })}
                >
                  <GitCompareGlyph size="xs" /> Compare with revision {v.n - 1}
                </a>
              )}
              <ShaChip oid={v.head} />
            </>
          }
        >
          <Byline
            who={v.pusher}
            verb={
              <>
                pushed{" "}
                <a
                  className="prose-link code-tl-strong"
                  href={changeHref(change.repo, change.number, {
                    tab: "files",
                    ...(head ? {} : { rev: v.n }),
                  })}
                >
                  revision {v.n}
                </a>
              </>
            }
            at={v.at}
            now={now}
          >
            {head && (
              <Tag size="sm" appearance="outline">
                Head
              </Tag>
            )}
          </Byline>
        </TimelineEvent>
      );
    }
    case "ask":
      return <AskEntry ask={entry.ask} again={entry.again} now={now} />;
    case "thread":
      return (
        <TimelineItem face={entry.root.author}>
          <Thread root={entry.root} replies={entry.replies} change={change} onRail />
        </TimelineItem>
      );
  }
}

/**
 * An ask for review where it was made: by whom, of whom, why and when. One
 * asked `again` names no asker: the engine keeps the first ask's asker and note.
 */
function AskEntry({ ask, again, now }: { ask: CodeReviewerRow; again: boolean; now: number }) {
  const at = ask.at;
  const when = at ? undefined : <Earlier />;
  if (ask.state === "withdrawn") {
    // A WITHDRAWAL TELLS NOBODY, so its time is mostly not kept: its place says when.
    return (
      <TimelineEvent disc={<Disc glyph={<UserGlyph size="sm" />} size="small" />}>
        <Byline who={ask.reviewer} verb="is no longer asked to review" at={at} now={now} />
      </TimelineEvent>
    );
  }
  const asked = again ? "was asked to review again" : "was asked to review";
  if (ask.reason === "path_owner") {
    return (
      <TimelineEvent disc={<Disc glyph={<ShieldUserGlyph size="sm" />} size="small" />}>
        <Byline
          who={ask.reviewer}
          verb={
            at ? (
              <>
                {asked}, {ownerWords(ask)}
              </>
            ) : (
              <>
                {asked} <Earlier />, {ownerWords(ask)}
              </>
            )
          }
          at={at}
          now={now}
        />
      </TimelineEvent>
    );
  }
  if (again) {
    return (
      <TimelineEvent disc={<Disc glyph={<UserPlusGlyph size="sm" />} size="small" />}>
        <Byline who={ask.reviewer} verb={asked} at={at} now={now}>
          {when}
        </Byline>
      </TimelineEvent>
    );
  }
  return (
    <TimelineEvent
      disc={<Disc glyph={<UserPlusGlyph size="sm" />} size="small" />}
      under={ask.note ? <blockquote className="code-tl-note">{ask.note}</blockquote> : undefined}
    >
      <Byline who={ask.requested_by} verb={<AskedWhom reviewer={ask.reviewer} />} at={at} now={now}>
        {when}
      </Byline>
    </TimelineEvent>
  );
}

/** "asked [face] Code Reviewer for a review". */
function AskedWhom({ reviewer }: { reviewer: string }) {
  const name = useWhoName()(reviewer);
  return (
    <span className="code-tl-whom">
      asked <Face who={reviewer} />
      <span className="code-byline__who">{name}</span> for a review
    </span>
  );
}

/**
 * A thread, as a card: its first comment (where it is about a line, which
 * line), its replies under it, each with its writer's face, and for a reader
 * a reply of their own and Resolve conversation. `onRail`: its writer's face
 * is on the timeline's rail, not in its head.
 */
export function Thread({
  root,
  replies,
  change,
  onRail = false,
}: {
  root: CodeCommentRow;
  replies: readonly CodeCommentRow[];
  change: CodeChangeDetail;
  onRail?: boolean;
}) {
  const now = useNow();
  const resolve = useAct("code_comment_on_change");
  const authors = new Set(change.authors.map((a) => a.author));
  const verb = root.anchor
    ? `commented on ${root.anchor.path}:${root.anchor.line} (revision ${root.anchor.revision})`
    : "commented";
  return (
    <article className="code-thread" aria-label={`Thread by ${root.author}`}>
      <div className="code-thread__head">
        {onRail ? (
          <Said who={root.author} verb={verb} at={root.at} now={now} />
        ) : (
          <Byline who={root.author} verb={verb} at={root.at} now={now} />
        )}
        {root.resolved && <Tag appearance="outline">Resolved</Tag>}
      </div>
      <div className="code-thread__body">
        <div className="prose md">{renderMarkdown(root.body)}</div>
      </div>
      {replies.length > 0 && (
        <ol className="code-thread__replies" aria-label="Replies">
          {replies.map((c) => (
            <li key={c.id} className="code-thread__reply">
              <span className="code-thread__said">
                <Byline who={c.author} at={c.at} now={now} />
                {authors.has(c.author) && (
                  <Tag size="sm" appearance="outline">
                    Author
                  </Tag>
                )}
              </span>
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
          variant="secondary"
          leadingIcon={root.resolved ? undefined : <CheckGlyph size="sm" />}
          onPress={() =>
            void resolve.run(
              { change: change.ref, thread: root.id, resolve: !root.resolved },
              { done: root.resolved ? "Opened the thread again" : "Resolved the thread" },
            )
          }
        >
          {root.resolved ? "Reopen thread" : "Resolve conversation"}
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
