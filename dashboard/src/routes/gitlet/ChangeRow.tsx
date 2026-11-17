/**
 * A change as every Gitlet list draws it: its state, title and tags, the line that places it and its facts.
 * One row wherever changes are listed, on CodeList.
 */

import type { ComponentType, ReactNode } from "react";
import {
  CircleAlertGlyph,
  CircleDashedGlyph,
  CircleXGlyph,
  FileDiffGlyph,
  GitMergeGlyph,
  GitPullRequestClosedGlyph,
  GitPullRequestDraftGlyph,
  GitPullRequestGlyph,
  ListOrderedGlyph,
  MessageSquareGlyph,
  SquareKanbanGlyph,
  UserGlyph,
  type GlyphProps,
} from "@crewlethq/icons/glyphs";
import {
  EmptyValue,
  SegmentedMeter,
  Spinner,
  Tag,
  VisuallyHidden,
  type AvatarVerdict,
  type SegmentedMeterTone,
} from "@crewlethq/ui";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { href } from "~/app/router.tsx";
import { ago } from "~/lib/ago.ts";
import { ejectReasonWord } from "~/lib/checks.ts";
import { useNow } from "~/lib/clock.ts";
import {
  EVERY_STATE,
  changeLive,
  changePath,
  changeStateWord,
  knownChangeState,
  parseChangeRef,
  refName,
  type KnownChangeState,
} from "~/lib/code.ts";
import { fmtDateTime, plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useViewer } from "~/lib/viewer.ts";
import type {
  CodeChangeName,
  CodeChangeState,
  CodeChangeSummary,
  CodeCheckRollup,
  CodePrecondition,
  CodeReviewerFace,
} from "~/protocol/index.ts";
import { CodeList, type CodeListProps } from "./CodeList.tsx";
import { namesSaid, ownedPaths } from "./owners.ts";
import { changeObject, changesHref, repoHref } from "./paths.ts";
import { preconditionSentence, readinessCount, readinessNext } from "./readinessWords.ts";
import { BranchChip, Face, ROLLUP_LOOK, useWhoName } from "./ui.tsx";
import "~/styles/gitlet.css";

/** Each state's glyph, the code host's own. */
export const STATE_GLYPHS: Readonly<Record<KnownChangeState, ComponentType<GlyphProps>>> = {
  draft: GitPullRequestDraftGlyph,
  open: GitPullRequestGlyph,
  closed: GitPullRequestClosedGlyph,
  merged: GitMergeGlyph,
};

/** A change's state as a row's lead: its glyph in the state's ink, named by its word. */
export function ChangeStateGlyph({ state }: { state: CodeChangeState }) {
  const known = knownChangeState(state);
  const Glyph = known ? STATE_GLYPHS[state] : CircleDashedGlyph;
  const word = changeStateWord(state);
  return (
    <span
      className="code-state-glyph"
      data-state={known ? state : undefined}
      role="img"
      aria-label={word}
      title={word}
    >
      <Glyph size="sm" aria-hidden />
    </span>
  );
}

/**
 * A change's state: the kit's change colours on its pill, with the state's
 * glyph and word (the seventh exception); a state this build does not know is
 * its word on the neutral outline.
 */
export function ChangeStateTag({ state }: { state: CodeChangeState }) {
  if (!knownChangeState(state)) return <Tag appearance="outline">{state}</Tag>;
  const Glyph = STATE_GLYPHS[state];
  return (
    <Tag className="code-state" data-state={state} leadingIcon={<Glyph size="xs" />}>
      {changeStateWord(state)}
    </Tag>
  );
}

/**
 * A change by its reference on its state's pill (`web!41`), opening the change:
 * how a commit or a branch names the change it belongs to.
 */
export function ChangeRefPill({ change }: { change: CodeChangeName }) {
  const at = parseChangeRef(change.ref);
  const Glyph = knownChangeState(change.state) ? STATE_GLYPHS[change.state] : null;
  const pill = Glyph ? (
    <Tag className="code-state" data-state={change.state} leadingIcon={<Glyph size="xs" />}>
      {change.ref}
    </Tag>
  ) : (
    <Tag appearance="outline">{change.ref}</Tag>
  );
  const title = `Change ${change.ref}, ${changeStateWord(change.state).toLowerCase()}: ${change.title}`;
  return at ? (
    <a className="code-change-ref" href={href(changePath(at.repo, at.number))} title={title}>
      {pill}
    </a>
  ) : (
    <span title={title}>{pill}</span>
  );
}

/** Where one condition to merge stands, as its segment's tone: who has to move it decides. */
export function preconditionTone(p: CodePrecondition): SegmentedMeterTone {
  if (p.state === "met") return "success";
  if (p.state === "computing") return "info";
  if (p.state === "unmet" && (p.kind === "check" || p.kind === "mergeable")) return "danger";
  return (p.waits_on ?? []).some((w) => w.kind === "agent") ? "info" : "warning";
}

type ReadinessChange = Pick<
  CodeChangeSummary,
  "state" | "owner" | "owner_kind" | "revision" | "target_ref" | "readiness"
>;

/**
 * A live change's readiness: one segment per condition to merge, in the engine's
 * order and toned by where it stands, its count beside it; nothing once it ended.
 */
export function ReadinessMeter({
  change,
  nameOf,
  me,
}: {
  change: ReadinessChange;
  nameOf: (handle: string) => string;
  /** The reader's handle, whom the sentence calls "you". */
  me?: string;
}) {
  const readiness = change.readiness;
  if (!changeLive(change.state) || !readiness) return null;
  const said = `${readinessCount(readiness)}. ${readinessNext(readiness, { change, nameOf, me })}`;
  return (
    <span className="code-readiness" title={said}>
      <SegmentedMeter
        className="code-readiness__bar"
        size="compact"
        label={said}
        total={readiness.total}
        segments={readiness.preconditions.map((p, i) => ({
          id: `${i}-${p.kind}`,
          value: 1,
          tone: preconditionTone(p),
          label: p.state,
        }))}
      />
      <span className="code-readiness__count" aria-hidden="true">
        {readiness.met}/{readiness.total}
      </span>
    </span>
  );
}

/** "First" to "Ninth", then "#12": a place in a queue as a sentence starts it. */
const ORDINALS = [
  "First",
  "Second",
  "Third",
  "Fourth",
  "Fifth",
  "Sixth",
  "Seventh",
  "Eighth",
  "Ninth",
];

/** What one reviewer's face says: their verdict on a revision, or the ask that waits. */
export function faceSentence(face: CodeReviewerFace, name: string, you: boolean): string {
  const who = you ? "You" : name;
  const n = face.verdict_revision;
  let said: string;
  switch (face.verdict) {
    case "approve":
      said = `${who} approved revision ${n}`;
      break;
    case "request_changes":
      said = `${who} requested changes on revision ${n}`;
      break;
    case "comment":
      said = `${who} commented on revision ${n}`;
      break;
    default:
      if (!face.asked) said = `${who} reviewed it`;
      else if (you) said = "You were asked to review: it waits on you";
      else said = `${who} was asked to review`;
  }
  if (face.verdict && face.asked) said += `, and ${you ? "you are" : "is"} asked to review again`;
  const paths = face.reason === "path_owner" ? ownedPaths(face) : "";
  if (paths !== "") said += `, as the owner of ${paths}`;
  return said;
}

/** The kit's corner for a verdict; a comment judges nothing and wears none. */
function verdictCorner(verdict: string | undefined): AvatarVerdict | undefined {
  if (verdict === "approve") return "approved";
  if (verdict === "request_changes") return "changes-requested";
  return undefined;
}

/** The faces a row draws before it says "+N" for the rest. */
const FACES_SHOWN = 3;

/** Who reviews it: up to three faces with the kit's verdict corner, the reader's ringed. */
export function ReviewerFaces({
  change,
  me,
  nameOf,
}: {
  change: Pick<CodeChangeSummary, "card">;
  me: string;
  nameOf: (handle: string) => string;
}) {
  const faces = change.card?.reviewers ?? [];
  if (faces.length === 0) {
    return (
      <span
        className="code-change-nobody"
        role="img"
        aria-label="No reviewer asked"
        title="No reviewer asked"
      >
        <UserGlyph size="xs" aria-hidden />
      </span>
    );
  }
  const shown = faces.slice(0, FACES_SHOWN);
  const more = faces.length - shown.length + (change.card?.reviewers_omitted ?? 0);
  const said = faces.map((f) => faceSentence(f, nameOf(f.handle), f.handle === me));
  const name = `Reviewers: ${said.join("; ")}${more > 0 ? `; and ${more} more` : ""}`;
  return (
    <span className="code-change-faces" role="img" aria-label={name} title={said.join("\n")}>
      {shown.map((f, i) => (
        <span key={f.handle} className="code-change-face" title={said[i]}>
          <Face
            who={f.handle}
            verdict={verdictCorner(f.verdict)}
            ring={f.handle === me ? "brand" : undefined}
          />
        </span>
      ))}
      {more > 0 && (
        <span className="code-change-faces__more" aria-hidden="true">
          +{more}
        </span>
      )}
    </span>
  );
}

/** The reader's own face on a change's card, if it holds one. */
function ownFace(change: Pick<CodeChangeSummary, "card">, me: string) {
  return me === "" ? undefined : change.card?.reviewers?.find((f) => f.handle === me);
}

/** Whether a change waits on the reader: an ask of their face, or its next condition naming them. */
export function waitsOnReader(change: CodeChangeSummary, me: string): boolean {
  if (me === "") return false;
  if (ownFace(change, me)?.asked) return true;
  const next = change.readiness?.preconditions.find((p) => p.state === "unmet");
  return (next?.waits_on ?? []).some((w) => w.handle === me);
}

/** Why a change waits on the reader, in a sentence: their review asked, their fix, or what is next. */
export function waitingReason(
  change: CodeChangeSummary,
  me: string,
  nameOf: (handle: string) => string,
): string {
  const owner = nameOf(change.owner);
  const face = ownFace(change, me);
  if (face?.asked && face.reason === "path_owner") {
    // FACTS OLDER THAN THE OWNED PATHS name none, and say so without a guess.
    const paths = ownedPaths(face);
    return paths === ""
      ? `${owner}'s change touches paths you own: your review is asked`
      : `${owner}'s change touches ${paths}, which you own: your review is asked`;
  }
  if (face?.asked) return `${owner} asks your review`;
  if (me !== "" && change.owner === me) {
    const asked = change.card?.reviewers?.find((f) => f.verdict === "request_changes");
    if (asked) {
      return `${nameOf(asked.handle)} requested changes on revision ${asked.verdict_revision}: it waits on your fix`;
    }
    if (change.open_findings > 0) {
      const n = change.open_findings;
      return `${plural(n, "open finding")} ${n === 1 ? "waits" : "wait"} on your answer`;
    }
  }
  return change.readiness
    ? readinessNext(change.readiness, { change, nameOf, me })
    : changeStateWord(change.state);
}

/** The ejection's tag title: "Taken out of main's queue 20 minutes ago: e2e failed". */
function ejectedSentence(change: CodeChangeSummary, now: number): string {
  const e = change.card!.ejected!;
  const target = refName(change.target_ref).name;
  const why = e.checks?.length
    ? `${namesSaid(e.checks, e.checks_more)} failed`
    : ejectReasonWord(e.reason);
  return `Taken out of ${target}'s queue ${ago(e.at, now)}: ${why}`;
}

/** Who requested changes, by the faces that hold that verdict. */
function changesRequestedSentence(
  change: CodeChangeSummary,
  nameOf: (h: string) => string,
): string {
  const faces = (change.card?.reviewers ?? []).filter((f) => f.verdict === "request_changes");
  const n = Math.max(change.card?.changes_requested ?? 0, faces.length);
  if (faces.length === 1 && n === 1) {
    return `${nameOf(faces[0]!.handle)} requested changes on revision ${faces[0]!.verdict_revision}`;
  }
  return `${plural(n, "reviewer")} requested changes`;
}

/** What a title's tags say, in order: waiting on you, the queue, changes requested, findings, a draft. */
export function ChangeTags({
  change,
  me,
  nameOf,
  now,
}: {
  change: CodeChangeSummary;
  me: string;
  nameOf: (handle: string) => string;
  now: number;
}) {
  const card = change.card;
  const queue = change.readiness?.preconditions.find((p) => p.kind === "queue")?.queue;
  const tags: ReactNode[] = [];
  if (waitsOnReader(change, me)) {
    tags.push(
      <Tag
        key="you"
        size="xs"
        variant="warning"
        leadingIcon={<UserGlyph size="xs" />}
        title={waitingReason(change, me, nameOf)}
      >
        Waiting on you
      </Tag>,
    );
  }
  if (card?.ejected) {
    tags.push(
      <Tag
        key="ejected"
        size="xs"
        variant="danger"
        leadingIcon={<CircleXGlyph size="xs" />}
        title={ejectedSentence(change, now)}
      >
        Taken out of the queue
      </Tag>,
    );
  }
  if (queue) {
    const testing = queue.state === "testing";
    const target = refName(queue.target_ref || change.target_ref).name;
    const place = ORDINALS[queue.place - 1] ?? `#${queue.place}`;
    tags.push(
      <Tag
        key="queue"
        size="xs"
        {...(testing ? { variant: "info" as const } : { appearance: "outline" as const })}
        leadingIcon={<ListOrderedGlyph size="xs" />}
        title={`${place} in ${target}'s merge queue, ${testing ? "being tested" : "waiting"}`}
      >
        In the queue · #{queue.place}
      </Tag>,
    );
  }
  if ((card?.changes_requested ?? 0) > 0) {
    tags.push(
      <Tag
        key="changes"
        size="xs"
        appearance="outline"
        leadingIcon={<FileDiffGlyph size="xs" />}
        title={changesRequestedSentence(change, nameOf)}
      >
        Changes requested
      </Tag>,
    );
  }
  if (change.open_findings > 0) {
    tags.push(
      <Tag
        key="findings"
        size="xs"
        appearance="outline"
        leadingIcon={<CircleAlertGlyph size="xs" />}
        title={`${plural(change.open_findings, "finding")} none of its authors has answered`}
      >
        {plural(change.open_findings, "open finding")}
      </Tag>,
    );
  }
  if (change.state === "draft") {
    tags.push(
      <Tag
        key="draft"
        size="xs"
        appearance="outline"
        leadingIcon={<GitPullRequestDraftGlyph size="xs" />}
        title="A draft: it merges once it is marked ready for review"
      >
        Draft
      </Tag>,
    );
  }
  return tags.length > 0 ? <span className="code-change-tags">{tags}</span> : null;
}

/** A fact a host fixes, so its rows leave it out: a task's tab names no work item. */
export type ChangeFact = "work";

/** The middle dot that parts a meta line's parts, drawn with its part's last item. */
const META_DOT = (
  <span className="code-change-meta__dot" aria-hidden="true">
    ·
  </span>
);

/** A part's last item and the dot after it, kept on one line so a dot never stands alone. */
export function MetaEnd({ dot, children }: { dot: boolean; children: ReactNode }) {
  return (
    <span className="code-change-meta__end">
      {children}
      {dot && META_DOT}
    </span>
  );
}

/** A meta line of parts, each given whether a dot follows it. */
export function MetaLine({ parts }: { parts: readonly ((dot: boolean) => ReactNode)[] }) {
  return (
    <span className="code-change-meta">{parts.map((part, i) => part(i < parts.length - 1))}</span>
  );
}

/** The line that places a change: its reference, who opened it when, its branches, revision and work item. */
export function ChangeMeta({
  change,
  now,
  omit = [],
}: {
  change: CodeChangeSummary;
  now: number;
  omit?: readonly ChangeFact[];
}) {
  const whoName = useWhoName();
  const source = refName(change.source_ref).name;
  const target = refName(change.target_ref).name;
  const work = change.work_item;
  const parts: ((dot: boolean) => ReactNode)[] = [
    (dot) => (
      <MetaEnd key="ref" dot={dot}>
        <span className="mono">{change.ref}</span>
      </MetaEnd>
    ),
    (dot) => (
      <span key="opened">
        opened{" "}
        <time dateTime={change.opened_at} title={fmtDateTime(change.opened_at)}>
          {ago(change.opened_at, now)}
        </time>{" "}
        by{" "}
        <MetaEnd dot={dot}>
          <a className="code-change-owner prose-link" href={changesHref({ owner: change.owner })}>
            <Face who={change.owner} />
            {whoName(change.owner)}
          </a>
        </MetaEnd>
      </span>
    ),
    (dot) => (
      <span key="branches" className="code-change-branches">
        <BranchChip name={source} href={repoHref(change.repo, { ref: source })} />
        <MetaEnd dot={dot}>
          {"→ "}
          <BranchChip name={target} href={repoHref(change.repo, { ref: target })} />
        </MetaEnd>
      </span>
    ),
  ];
  if (change.revision > 1) {
    parts.push((dot) => (
      <MetaEnd key="revision" dot={dot}>
        revision {change.revision}
      </MetaEnd>
    ));
  }
  if (work && !omit.includes("work")) {
    parts.push((dot) => (
      <MetaEnd key="work" dot={dot}>
        <a
          className="code-change-work prose-link"
          href={changesHref({ work: `${work.backend}:${work.id}`, state: EVERY_STATE })}
        >
          <SquareKanbanGlyph size="xs" aria-hidden />
          {work.key || work.id}
        </a>
      </MetaEnd>
    ));
  }
  return <MetaLine parts={parts} />;
}

/** Where a change's head's checks stand, in a sentence: the required counted, the failed named. */
export function checksSentence(r: CodeCheckRollup): string {
  const failing = r.failing ?? [];
  const required = failing.filter((f) => f.required);
  const other = failing.filter((f) => !f.required);
  // THE UNNAMED REST follow the last one named, so they join its group; after a required
  // one they may be either, and are said as failed alone.
  const more = r.failing_more ?? 0;
  const otherMore = other.length > 0 ? more : 0;
  const requiredMore = other.length > 0 ? 0 : more;
  const counts = r.counts as Record<string, number | undefined>;
  const parts: string[] = [];
  if (required.length > 0) {
    const rest = requiredMore > 0 ? `, and ${requiredMore} more failed` : "";
    parts.push(`${namesSaid(required.map((f) => f.name))} failed (required)${rest}`);
  } else if (r.required > 0) {
    parts.push(`${r.required_passed} of ${plural(r.required, "required check")} passed`);
  } else {
    const total = Object.values(counts).reduce<number>((n, v) => n + (v ?? 0), 0);
    parts.push(`${counts.passed ?? 0} of ${plural(total, "check")} passed`);
  }
  if (other.length > 0) {
    const names = namesSaid(
      other.map((f) => f.name),
      otherMore,
    );
    parts.push(
      `${names} failed, and ${other.length + otherMore === 1 ? "is" : "are"} not required`,
    );
  }
  const running = counts.running ?? 0;
  if (running > 0)
    parts.push(`${plural(running, "check")} ${running === 1 ? "is" : "are"} running`);
  const waiting = counts.queued ?? 0;
  if (waiting > 0) parts.push(`${plural(waiting, "check")} waiting to run`);
  const said = `${parts.join("; ")}.`;
  return r.more ? `${said} Counted over the checks one read holds.` : said;
}

/** The checks' count a row draws: the required passed of the required, else every passed of all. */
function checksCount(r: CodeCheckRollup): string {
  if (r.required > 0) return `${r.required_passed}/${r.required}`;
  const counts = Object.values(r.counts as Record<string, number | undefined>);
  return `${r.counts.passed ?? 0}/${counts.reduce<number>((n, v) => n + (v ?? 0), 0)}`;
}

const NO_CHECK_KEPT = "No check of its head is kept: none has run, or they ended over 30 days ago";

/** The checks fact: the rollup's glyph and count, titled with the sentence. */
function ChecksFact({ rollup }: { rollup: CodeCheckRollup | undefined }) {
  if (!rollup) {
    return (
      <span className="code-change-checks" data-tone="neutral" title={NO_CHECK_KEPT}>
        <CircleDashedGlyph size="sm" aria-hidden />
        <VisuallyHidden>{NO_CHECK_KEPT}</VisuallyHidden>
      </span>
    );
  }
  const look = ROLLUP_LOOK[rollup.state];
  // A STATE THIS BUILD DOES NOT KNOW draws the dashed circle, so the glyph always leads.
  const Glyph = look?.glyph ?? CircleDashedGlyph;
  const said = checksSentence(rollup);
  return (
    <span className="code-change-checks" data-tone={look?.tone ?? "neutral"} title={said}>
      {Glyph === "spinner" ? <Spinner size="sm" /> : <Glyph size="sm" aria-hidden />}
      <span aria-hidden="true">{checksCount(rollup)}</span>
      <VisuallyHidden>{said}</VisuallyHidden>
    </span>
  );
}

/** The readiness fact: the meter while live, the word Draft for a draft, nothing once ended. */
function ReadinessFact({
  change,
  nameOf,
  me,
}: {
  change: CodeChangeSummary;
  nameOf: (h: string) => string;
  me: string;
}) {
  if (change.state === "draft") {
    const draft = change.readiness?.preconditions.find((p) => p.kind === "draft");
    const said = draft ? preconditionSentence(draft, { change, nameOf, me }) : null;
    return (
      <span className="code-change-draft" title={said ?? "A draft"}>
        Draft
      </span>
    );
  }
  return <ReadinessMeter change={change} nameOf={nameOf} me={me} />;
}

/** The comments fact: how many, opening the change's conversation; none is said, not linked. */
function CommentsFact({ change, comments }: { change: CodeChangeSummary; comments: number }) {
  if (comments === 0) {
    return (
      <span className="code-change-comments" title="No comments">
        <MessageSquareGlyph size="sm" aria-hidden />
        <span aria-hidden="true">0</span>
        <VisuallyHidden>No comments</VisuallyHidden>
      </span>
    );
  }
  const said = `${plural(comments, "comment")} on ${change.ref}`;
  return (
    <a
      className="code-change-comments"
      href={href(changePath(change.repo, change.number))}
      aria-label={said}
      title={said}
    >
      <MessageSquareGlyph size="sm" aria-hidden />
      {comments}
    </a>
  );
}

/** When it last moved, in words, its instant the title. */
function UpdatedFact({ at, now }: { at: string; now: number }) {
  return (
    <span className="code-change-updated">
      updated{" "}
      <time dateTime={at} title={fmtDateTime(at)}>
        {ago(at, now)}
      </time>
    </span>
  );
}

const NOT_ANSWERED = <EmptyValue label="Not answered" />;

/** A row's facts in slots that line up down the list: checks, readiness, reviewers, comments, updated. */
export function ChangeFacts({
  change,
  now,
  me,
  nameOf,
}: {
  change: CodeChangeSummary;
  now: number;
  me: string;
  nameOf: (handle: string) => string;
}) {
  const card = change.card;
  return (
    <span className="code-change-facts">
      <span className="code-change-fact">
        {card ? <ChecksFact rollup={card.checks} /> : NOT_ANSWERED}
      </span>
      <span className="code-change-fact">
        <ReadinessFact change={change} nameOf={nameOf} me={me} />
      </span>
      <span className="code-change-fact">
        {card ? <ReviewerFaces change={change} me={me} nameOf={nameOf} /> : NOT_ANSWERED}
      </span>
      <span className="code-change-fact">
        {card ? <CommentsFact change={change} comments={card.comments} /> : NOT_ANSWERED}
      </span>
      <span className="code-change-fact code-change-fact--updated">
        <UpdatedFact at={change.updated_at} now={now} />
      </span>
    </span>
  );
}

const changeKey = (c: CodeChangeSummary) => c.id;
const changeTitle = (c: CodeChangeSummary) => c.title;
const changePeek = (c: CodeChangeSummary): ObjectRef => changeObject(c.repo, c.number);
const changeLead = (c: CodeChangeSummary) => <ChangeStateGlyph state={c.state} />;

/** Changes as Gitlet's rows on CodeList, each peeking its change. */
export function ChangeRows({
  rows,
  label,
  omit,
  ...list
}: {
  rows: readonly CodeChangeSummary[];
  label: string;
  omit?: readonly ChangeFact[];
} & Pick<
  CodeListProps<CodeChangeSummary>,
  "density" | "head" | "placeholder" | "paged" | "moreLabel" | "foot" | "neighbours"
>) {
  const now = useNow();
  const me = useViewer().handle;
  const badge = useSeatBadgeOf();
  const nameOf = (handle: string) => badge(handle).name || handle;
  return (
    <CodeList<CodeChangeSummary>
      rows={rows}
      rowKey={changeKey}
      aria-label={label}
      peek={changePeek}
      lead={changeLead}
      title={changeTitle}
      titleAfter={(c) => <ChangeTags change={c} me={me} nameOf={nameOf} now={now} />}
      meta={(c) => <ChangeMeta change={c} now={now} omit={omit} />}
      facts={(c) => <ChangeFacts change={c} now={now} me={me} nameOf={nameOf} />}
      moreLabel="Load more changes"
      {...list}
    />
  );
}
