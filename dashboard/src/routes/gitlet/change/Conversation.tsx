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

import { Fragment, useEffect, useId, useRef, useState, type ReactNode } from "react";
import {
  AsideLayout,
  Button,
  Composer,
  Kbd,
  MarkdownEditor,
  Stack,
  Tag,
  type MarkdownEditorApi,
} from "@crewlethq/ui";
import {
  CheckGlyph,
  EyeGlyph,
  FileDiffGlyph,
  FlagGlyph,
  GitBranchGlyph,
  GitCommitVerticalGlyph,
  GitCompareGlyph,
  GitPullRequestClosedGlyph,
  GitPullRequestDraftGlyph,
  GitPullRequestGlyph,
  ListOrderedGlyph,
  MessageSquareGlyph,
  PencilGlyph,
  ShieldUserGlyph,
  SquareKanbanGlyph,
  UserGlyph,
  UserPlusGlyph,
} from "@crewlethq/icons/glyphs";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { useDraftState } from "~/app/assistant/draft.ts";
import { PageFailed } from "~/components/common.tsx";
import { FINDING_SEVERITIES, SOURCE_DELETED } from "~/contract/code.ts";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { ago } from "~/lib/ago.ts";
import { ejectReasonWord } from "~/lib/checks.ts";
import { changeLive, refName } from "~/lib/code.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, plural } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useAct } from "~/lib/useAct.ts";
import { useDictationBox } from "~/lib/dictation/source.ts";
import { useViewer } from "~/lib/viewer.ts";
import type {
  CodeChangeDetail,
  CodeChangeMoveRow,
  CodeChangeQueueEntryRow,
  CodeCommentRow,
  CodeFindingRow,
  CodeReviewRow,
  CodeReviewerRow,
  CodeRevisionRow,
} from "~/protocol/index.ts";
import { namesSaid, ownedPathList } from "../owners.ts";
import { changeHref, repoHref } from "../paths.ts";
import { BranchChip, Byline, Face, GITLET, ShaChip, useWhoName } from "../ui.tsx";
import { Description } from "./Description.tsx";
import { MergeBox, MergeDisc, type MergeFacts } from "./Merge.tsx";
import { reRequestable, reviewerStandings } from "./mergeSections.ts";
import { ChangeRail } from "./Rail.tsx";
import {
  Disc,
  Said,
  TimelineEvent,
  TimelineItem,
  TimelineList,
  TimelineMark,
  type DiscTone,
} from "./Timeline.tsx";
import { CommentCard } from "./CommentCard.tsx";
import { FindingCard, type ExcerptRead } from "./Finding.tsx";
import {
  partMovedBy,
  partRefusal,
  positionBefore,
  refusedWords,
  usePagedComments,
  usePagedExcerpts,
  usePagedFindings,
  usePagedMoves,
  usePagedQueue,
  usePagedReviewers,
  usePagedReviews,
  type LatestChecks,
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
  | { kind: "ask"; at: CodeReviewerRow["position"]; ask: CodeReviewerRow; again: boolean }
  | { kind: "move"; at: CodeChangeMoveRow["position"]; move: CodeChangeMoveRow }
  | {
      kind: "queue";
      at: CodeChangeQueueEntryRow["enqueued_position"];
      entry: CodeChangeQueueEntryRow;
      ended: boolean;
    };

/**
 * The timeline's entries, in the order their records landed: each review, each
 * thread by its first comment with its replies under it, each revision, each
 * ask that stands or was withdrawn, each move of its lifecycle, and each queue
 * entry's enqueue and, where it ended, its end. A thread on a finding is drawn with
 * its finding; an answered or ended ask's position is the record that moved it.
 * An ask after a review of the reviewer's own is one asked `again`.
 */
export function timelineOf(
  reviews: readonly CodeReviewRow[],
  comments: readonly CodeCommentRow[],
  revisions: readonly CodeRevisionRow[],
  asks: readonly CodeReviewerRow[] = [],
  moves: readonly CodeChangeMoveRow[] = [],
  queued: readonly CodeChangeQueueEntryRow[] = [],
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
    ...moves.map((move) => ({ kind: "move" as const, at: move.position, move })),
    ...queued.flatMap((entry) => [
      { kind: "queue" as const, at: entry.enqueued_position, entry, ended: false },
      ...(entry.ended_position
        ? [{ kind: "queue" as const, at: entry.ended_position, entry, ended: true }]
        : []),
    ]),
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
    case "move":
      return `move-${entry.move.position.generation}:${entry.move.position.seq}:${entry.move.kind}`;
    case "queue":
      return `queue-${entry.entry.entry_id}-${entry.ended ? "ended" : "added"}`;
  }
}

/** A review's disc tone: its verdict's, the verdict said in words beside it. */
export function verdictTone(verdict: string): DiscTone {
  return verdict === "request_changes" ? "danger" : verdict === "approve" ? "success" : "neutral";
}

export function Conversation({
  change,
  revisions,
  checks,
}: {
  change: CodeChangeDetail;
  revisions: readonly CodeRevisionRow[];
  /** The head's latest checks, read once by the page for the box, the rail and the tab. */
  checks: LatestChecks;
}) {
  const now = useNow();
  const viewer = useViewer();
  const [quoted, setQuoted] = useState<Quote | null>(null);
  const reviews = usePagedReviews(change);
  const comments = usePagedComments(change);
  // ONE READ for the timeline's asks and the rail's reviewers.
  const asked = usePagedReviewers(change);
  const moves = usePagedMoves(change);
  const queued = usePagedQueue(change);
  // A FINDING ROW MOVES ONLY WITH WHAT SOMEBODY SAID: the placements a push
  // moves are Files changed's to read.
  const findings = usePagedFindings(change, change.revision, partMovedBy(change, "said"));
  const excerpts = usePagedExcerpts(change);
  const entries = timelineOf(
    reviews.items,
    comments.items,
    revisions,
    asked.items,
    moves.items,
    queued.items,
  );
  const askedRefused = partRefusal(asked);
  const parts = [reviews, comments, asked, findings, excerpts, moves, queued];
  const excerptsRefused = partRefusal(excerpts);
  const excerptOf = (id: string): ExcerptRead => {
    const found = excerpts.items.find((e) => e.id === id);
    if (found) return found;
    if (excerpts.error && excerpts.items.length === 0) {
      // AN OLDER FLEET knows no excerpts part: it refuses the parameter.
      if (excerptsRefused === null) return "reading";
      return excerpts.error === "bad_params" ? "refused" : { failed: excerptsRefused };
    }
    return excerpts.loading || excerpts.paging ? "reading" : "missing";
  };
  const findingsRefused = partRefusal(findings);
  const said: Said = {
    findingsRead:
      findings.items.length > 0
        ? "read"
        : findingsRefused !== null
          ? { refused: findingsRefused }
          : findings.loading || findings.error
            ? "reading"
            : "read",
    findingsOf: (review) => findings.items.filter((f) => f.review_id === review),
    repliesTo: (finding) => comments.items.filter((c) => c.finding_id === finding),
    excerptOf,
  };
  const standings = reviewerStandings(change, reviews.items);
  const merge: MergeFacts = {
    revisions,
    checks,
    standings,
    again: reRequestable(change, standings, asked.items, viewer.handle),
    severities: openSeverities(change, findings),
  };
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
        <ChangeRail
          change={change}
          revisions={revisions}
          standings={standings}
          again={merge.again}
          asked={asked}
          checks={checks}
        />
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
              <TimelineEntry
                key={entryKey(entry)}
                entry={entry}
                change={change}
                said={said}
                now={now}
              />
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
        <TimelineList label="Merge and comment" tail>
          <TimelineMark disc={<MergeDisc change={change} checks={checks.rows} />}>
            <MergeBox change={change} facts={merge} />
          </TimelineMark>
          <CommentComposer change={change} quote={quoted} />
        </TimelineList>
      </Stack>
    </AsideLayout>
  );
}

/** The open findings counted by weight, heaviest first; null until every open one is loaded. */
function openSeverities(
  change: CodeChangeDetail,
  findings: { items: readonly CodeFindingRow[]; more: boolean },
): Map<string, number> | null {
  const open = findings.items.filter((f) => f.state === "open");
  if (findings.more || open.length !== (change.conversation?.open_findings ?? 0)) return null;
  const counted = new Map<string, number>();
  for (const severity of FINDING_SEVERITIES) {
    const n = open.filter((f) => f.severity === severity).length;
    if (n > 0) counted.set(severity, n);
  }
  for (const f of open) {
    if (!counted.has(f.severity))
      counted.set(f.severity, open.filter((g) => g.severity === f.severity).length);
  }
  return counted;
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

/** What a review's box draws beside it: its findings, each one's replies and excerpt. */
interface Said {
  /** Whether the findings' first page is in, still out, or refused (the engine's sentence). */
  findingsRead: "read" | "reading" | { refused: string };
  findingsOf: (review: string) => CodeFindingRow[];
  repliesTo: (finding: string) => CodeCommentRow[];
  excerptOf: (finding: string) => ExcerptRead;
}

/** A review's findings under it: a head that counts them, then each card. */
function ReviewFindings({
  change,
  review,
  said,
}: {
  change: CodeChangeDetail;
  review: CodeReviewRow;
  said: Said;
}) {
  const loaded = said.findingsOf(review.id);
  const open = loaded.filter((f) => f.state === "open").length;
  const state = said.findingsRead;
  // NOTHING IS COUNTED AS LOADED OR LATER until the first page is in.
  const later = state === "read" ? review.findings - loaded.length : 0;
  return (
    <section
      className="code-review-findings"
      aria-label={`Findings of the review by ${review.reviewer}`}
    >
      <div className="code-review-findings__head">
        <FlagGlyph size="sm" />
        <span>
          <strong>{plural(review.findings, "finding")}</strong> on revision {review.revision}
          {later > 0 && `, ${loaded.length} loaded`}
        </span>
        {state === "read" && <span className="code-review-findings__open">{open} open</span>}
      </div>
      {typeof state === "object" && (
        <p className="muted">{refusedWords("Its findings are unavailable", state.refused)}</p>
      )}
      {loaded.map((f) => (
        <FindingCard
          key={f.id}
          change={change}
          finding={f}
          replies={said.repliesTo(f.id)}
          applicable={changeLive(change.state) && f.revision === change.revision}
          excerpt={said.excerptOf(f.id)}
          where="review"
        />
      ))}
      {later > 0 && <p className="muted">{later} more on a later page.</p>}
    </section>
  );
}

/** One entry, as the timeline draws it. */
function TimelineEntry({
  entry,
  change,
  said,
  now,
}: {
  entry: Entry;
  change: CodeChangeDetail;
  said: Said;
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
              {r.findings > 0 && <ReviewFindings change={change} review={r} said={said} />}
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
      // A REBASE IS THE ENGINE'S ONE PUSH THAT REWRITES A BRANCH (gitlet.WriteRebaseChange),
      // onto the target its facts were measured against, else the change's.
      const rebased = v.via === "engine" && v.forced;
      const onto = refName(v.facts?.target_ref ?? change.target_ref).name;
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
                {rebased ? "rebased" : v.forced ? "force-pushed" : "pushed"}{" "}
                <a
                  className="prose-link code-tl-strong"
                  href={changeHref(change.repo, change.number, {
                    tab: "files",
                    ...(head ? {} : { rev: v.n }),
                  })}
                >
                  revision {v.n}
                </a>
                {rebased && (
                  <>
                    {" "}
                    onto <BranchChip name={onto} href={repoHref(change.repo, { ref: onto })} />
                  </>
                )}
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
    case "move":
      return <MoveEntry move={entry.move} change={change} now={now} />;
    case "queue":
      return <QueueEntry entry={entry.entry} ended={entry.ended} change={change} now={now} />;
    case "thread":
      return (
        <TimelineItem face={entry.root.author}>
          <Thread root={entry.root} replies={entry.replies} change={change} onRail />
        </TimelineItem>
      );
  }
}

/** Each move's disc glyph; a kind a newer engine names draws the change's own. */
const MOVE_GLYPHS: Readonly<Record<string, ReactNode>> = {
  closed: <GitPullRequestClosedGlyph size="sm" />,
  reopened: <GitPullRequestGlyph size="sm" />,
  drafted: <GitPullRequestDraftGlyph size="sm" />,
  readied: <GitPullRequestGlyph size="sm" />,
  retitled: <PencilGlyph size="sm" />,
  described: <PencilGlyph size="sm" />,
  work_item: <SquareKanbanGlyph size="sm" />,
  retargeted: <GitBranchGlyph size="sm" />,
};

/** What a move did, after who made it: "closed it", "changed its target from main to release". */
export function moveVerb(move: CodeChangeMoveRow, repo: string): ReactNode {
  switch (move.kind) {
    case "closed":
      return move.reason === SOURCE_DELETED ? "closed it by deleting its branch" : "closed it";
    case "reopened":
      return "reopened it";
    case "drafted":
      return "marked it as a draft";
    case "readied":
      return "marked it ready for review";
    case "retitled":
      return "edited its title";
    case "described":
      return "edited its description";
    case "work_item":
      return move.became ? (
        <>
          linked it to <span className="code-tl-strong">{move.became}</span>
        </>
      ) : (
        "unlinked its work item"
      );
    case "retargeted": {
      const was = refName(move.was ?? "").name;
      const became = refName(move.became ?? "").name;
      return (
        <>
          changed its target from <BranchChip name={was} href={repoHref(repo, { ref: was })} /> to{" "}
          <BranchChip name={became} href={repoHref(repo, { ref: became })} />
        </>
      );
    }
  }
  return move.kind.replaceAll("_", " ");
}

/** A move of the change's lifecycle where it was made: who, what, when, and a close's reason under it. */
function MoveEntry({
  move,
  change,
  now,
}: {
  move: CodeChangeMoveRow;
  change: CodeChangeDetail;
  now: number;
}) {
  const said = move.kind === "closed" && move.reason && move.reason !== SOURCE_DELETED;
  return (
    <TimelineEvent
      disc={
        <Disc glyph={MOVE_GLYPHS[move.kind] ?? <GitPullRequestGlyph size="sm" />} size="small" />
      }
      under={said ? <blockquote className="code-tl-note">{move.reason}</blockquote> : undefined}
    >
      <Byline who={move.by || GITLET} verb={moveVerb(move, change.repo)} at={move.at} now={now} />
    </TimelineEvent>
  );
}

/** Why an entry left its queue other than by landing or an ejection, after "It left the queue". */
const DEQUEUE_WORDS: Readonly<Record<string, string>> = {
  closed: "as it was closed",
  drafted: "as it was made a draft",
  retargeted: "as its target changed",
  merged: "as a push merged it",
};

/** What an ejection found, in words: its reason, and the checks or paths it named. */
export function ejectedWords(entry: CodeChangeQueueEntryRow): string {
  const said = ejectReasonWord(entry.reason ?? "");
  const named = entry.checks?.length ? entry.checks : entry.paths?.length ? entry.paths : [];
  if (named.length === 0) return said;
  return `${said}: ${namesSaid(named.slice(0, 3), Math.max(0, named.length - 3))}`;
}

/** A queue entry: who queued it, then how it ended; a dequeue names nobody, only why. */
function QueueEntry({
  entry,
  ended,
  change,
  now,
}: {
  entry: CodeChangeQueueEntryRow;
  ended: boolean;
  change: CodeChangeDetail;
  now: number;
}) {
  const target = refName(entry.target_ref).name;
  const chip = <BranchChip name={target} href={repoHref(change.repo, { ref: target })} />;
  const disc = <Disc glyph={<ListOrderedGlyph size="sm" />} size="small" />;
  if (!ended) {
    return (
      <TimelineEvent disc={disc}>
        <Byline
          who={entry.enqueued_by.handle}
          verb={<>added it to the queue for {chip}</>}
          at={entry.enqueued_at}
          now={now}
        />
      </TimelineEvent>
    );
  }
  if (entry.state === "landed") {
    return (
      <TimelineEvent
        disc={disc}
        end={entry.commit_oid ? <ShaChip oid={entry.commit_oid} /> : undefined}
      >
        <Byline
          who={GITLET}
          verb={<>landed it on {chip} through the queue</>}
          at={entry.ended_at}
          now={now}
        />
      </TimelineEvent>
    );
  }
  if (entry.state === "ejected") {
    return (
      <TimelineEvent disc={disc}>
        <Byline
          who={GITLET}
          verb={`took it out of the queue: ${ejectedWords(entry)}`}
          at={entry.ended_at}
          now={now}
        />
      </TimelineEvent>
    );
  }
  const why = DEQUEUE_WORDS[entry.reason ?? ""];
  return (
    <TimelineEvent disc={disc}>
      <span className="code-byline">
        <span className="code-byline__verb">
          {why ? `It left the queue ${why}` : "It was taken out of the queue"}
        </span>
        {entry.ended_at && (
          <time
            className="code-byline__when"
            dateTime={entry.ended_at}
            title={fmtDateTime(entry.ended_at)}
          >
            {ago(entry.ended_at, now, { clock: true })}
          </time>
        )}
      </span>
    </TimelineEvent>
  );
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
 * line), its replies under it, each with its writer's face and its menu (`CommentCard.tsx`), and
 * for a reader a reply of their own and Resolve conversation. `onRail`: its writer's face
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
      <CommentCard
        comment={root}
        change={change}
        layout="root"
        byline={
          onRail ? (
            <Said who={root.author} verb={verb} at={root.at} now={now} />
          ) : (
            <Byline who={root.author} verb={verb} at={root.at} now={now} />
          )
        }
        marks={root.resolved && <Tag appearance="outline">Resolved</Tag>}
      />
      {replies.length > 0 && (
        <ol className="code-thread__replies" aria-label="Replies">
          {replies.map((c) => (
            <li key={c.id} className="code-thread__reply">
              <CommentCard
                comment={c}
                change={change}
                layout="reply"
                byline={<Byline who={c.author} at={c.at} now={now} />}
                marks={
                  authors.has(c.author) && (
                    <Tag size="sm" appearance="outline">
                      Author
                    </Tag>
                  )
                }
              />
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
  focus = false,
}: {
  change: CodeChangeDetail;
  target: { thread: string } | { finding: string };
  /** Takes focus as it opens, where a press opened it. */
  focus?: boolean;
}) {
  const write = useAct("code_comment_on_change");
  const [body, setBody] = useDraftState("");
  // EACH REPLY BOX IS A FIELD of the change page's one draft
  // (`ChangePage.tsx`), keyed by what it answers: a draft's path is
  // lowercase letters, digits, dots and underscores, so the id is spelled
  // in those.
  const answers = "thread" in target ? `thread ${target.thread}` : `finding ${target.finding}`;
  const { dictation, refusal: dictating } = useDictationBox();
  useDraftBox({
    path: `reply.${answers.toLowerCase().replace(/[^a-z0-9]+/g, "_")}`.slice(0, 64),
    label: "thread" in target ? "Reply in the thread" : "Reply to the finding",
    hint: "Markdown",
    rank: 2,
    value: body,
    set: setBody,
    refuses: () => dictating,
  });
  const blocked = body.trim() ? undefined : "Write the reply first.";
  const send = async () => {
    const result = await write.run({ change: change.ref, body, ...target }, { done: "Replied" });
    if (result && (result.kind === "applied" || result.kind === "pending")) setBody("");
  };
  // Enter sends, as in every message box; the button's own gate holds it.
  return (
    <div className="code-reply">
      <Composer
        label="Reply"
        autoFocus={focus}
        placeholder="Reply…"
        value={body}
        onChange={setBody}
        onSubmit={() => void send()}
        canSubmit={pressable(write, blocked)}
        sendHint={false}
        dictation={dictation}
        trailing={
          <WriteButton
            write={write}
            size="small"
            variant="secondary"
            showRefusal={false}
            blocked={blocked}
            onPress={() => void send()}
          >
            Reply
          </WriteButton>
        }
      />
      <RefusalNote write={write} />
    </div>
  );
}

/** Words quoted into the comment box; `n` counts the presses, so each lands once. */
interface Quote {
  text: string;
  n: number;
}

/** A comment of its own on the change, which starts a thread: the kit's markdown editor, sent on Mod+Enter. */
function CommentComposer({ change, quote }: { change: CodeChangeDetail; quote: Quote | null }) {
  const viewer = useViewer();
  const write = useAct("code_comment_on_change");
  const [body, setBody] = useDraftState("");
  const editor = useRef<MarkdownEditorApi>(null);
  const landed = useRef(0);
  const keys = useId();
  // A QUOTE IS APPENDED ONCE per press, and the editor takes focus at its end.
  useEffect(() => {
    if (!quote || quote.n === landed.current) return;
    landed.current = quote.n;
    setBody((was) => (was && !was.endsWith("\n") ? `${was}\n\n` : was) + quote.text);
    queueMicrotask(() => {
      editor.current?.focus();
      const box = document.querySelector<HTMLTextAreaElement>(".code-compose textarea");
      box?.setSelectionRange(box.value.length, box.value.length);
    });
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
  const press = (
    <WriteButton write={write} variant="secondary" blocked={blocked} onPress={() => void send()}>
      Comment
    </WriteButton>
  );
  return (
    <li className="code-timeline__item">
      {viewer.owner && (
        <span className="code-tl-face" aria-hidden="true">
          <Face who={viewer.owner} size="md" />
        </span>
      )}
      <section aria-label="Comment on the change" className="code-compose">
        <MarkdownEditor
          ref={editor}
          label="Comment"
          placeholder="Leave a comment. Markdown works."
          value={body}
          rows={4}
          onChange={setBody}
          aria-describedby={keys}
          onSubmit={() => {
            if (pressable(write, blocked)) void send();
          }}
          renderPreview={(value) => <div className="prose md">{renderMarkdown(value)}</div>}
          footer={
            <span className="code-compose__foot">
              <span className="t-caption" id={keys}>
                <Kbd keys={["Mod", "Enter"]} /> sends
              </span>
              {press}
            </span>
          }
        />
      </section>
    </li>
  );
}
