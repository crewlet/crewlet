/**
 * A change's merge box: a section per question its gate asks, each on a disc in where it
 * stands, with what acts on it; then the method, Merge or Add to queue, Override, and what
 * the press will do.
 *
 * # The engine decides, and the page says what it decided
 *
 * Every press is ONE act through `useAct` and a `WriteButton`, never
 * optimistic. Merge sends the head ON SCREEN as `expected_head`, so a push
 * that landed after the page was read is refused `head_moved` rather than
 * merged unseen. The controls are drawn for every reader and held, with a
 * sentence, where the engine would refuse: Merge with the gate's own first
 * unmet precondition (`readiness.first`, judged for THIS reader as the
 * merger, so a reader without the authority to merge is told so), Override
 * for anybody but a person.
 *
 * # A refusal in its own words
 *
 * Several of Gitlet's refusals share one class — a head that moved and
 * a train whose base moved are both `stale_version` — so the controls answer
 * by the host's own word (`hostRefusal`, `contract/merge.ts`), and draw the
 * engine's words where only the engine knows what to say: a gate the change
 * does not meet yet (`not_ready`, saying what it still waits for and whom), a
 * branch the merge may not delete, a repository at its size, and a merge
 * asked for somebody who may not make it. Those are a person's words from the
 * engine's refusal catalog (ADR-0060), never the sentence it writes for a
 * model, and a refusal nobody has worded yet answers its code's own sentence.
 *
 * WHAT THIS IS NOT: a landing-policy choice or an auto-revert consent. A
 * gated target's queue lands each change tested merged on the branch, the
 * one way the engine lands one.
 */

import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  Card,
  Checkbox,
  SegmentedControl,
  SegmentedMeter,
  Tag,
  Textarea,
} from "@crewlethq/ui";
import {
  ArrowRightGlyph,
  ChevronDownGlyph,
  ChevronUpGlyph,
  CircleDashedGlyph,
  GitCommitVerticalGlyph,
  GitMergeGlyph,
  GitPullRequestClosedGlyph,
  ListChecksGlyph,
  ListOrderedGlyph,
  PencilGlyph,
  RotateCwGlyph,
  ScrollTextGlyph,
  ShieldCheckGlyph,
  ShieldUserGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { Modal } from "~/components/Modal.tsx";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import { MERGE_CONFLICT, MERGE_METHODS, MERGE_REFUSALS } from "~/contract/merge.ts";
import { ago } from "~/lib/ago.ts";
import { changeLive, preconditionWord, refName, shortOID } from "~/lib/code.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, plural } from "~/lib/format.ts";
import { useAct, type Act } from "~/lib/useAct.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { ActionTool } from "~/protocol/act.ts";
import type {
  CodeChangeDetail,
  CodeCheckRow,
  CodePrecondition,
  CodeRevisionRow,
} from "~/protocol/index.ts";
import { ORDINALS, preconditionTone } from "../ChangeRow.tsx";
import { CheckPane, attemptKey } from "../checks/CheckList.tsx";
import { CheckMark, CheckPhrase } from "../checks/marks.tsx";
import { namesSaid } from "../owners.ts";
import { commitHref } from "../paths.ts";
import {
  checkWaits,
  checksWords,
  findingsWords,
  metSentence,
  preconditionSentence,
  readinessNext,
  readinessWaits,
  reviewsWords,
} from "../readinessWords.ts";
import { Byline, Face, GITLET, useWhoName } from "../ui.tsx";
import { DequeueButton, RebaseButton, RevertChangePanel } from "./ChangeWrites.tsx";
import {
  boxTone,
  checksKept,
  mergeSections,
  sectionTone,
  type MergeSection,
  type Standing,
} from "./mergeSections.ts";
import type { LatestChecks } from "./reads.ts";
import { Disc } from "./Timeline.tsx";

type MergeMethod = (typeof MERGE_METHODS)[number];
type MergeRefusal = (typeof MERGE_REFUSALS)[number];

/** How each method is offered. */
const METHOD_LABELS: Readonly<Record<MergeMethod, { label: string; title: string }>> = {
  merge: { label: "Merge commit", title: "Its head merged with a merge commit" },
  squash: { label: "Squash", title: "Its changes landed as one commit" },
  fast_forward: {
    label: "Fast-forward",
    title: "Its target moved to its head, which must already hold the target's tip",
  },
};

/**
 * What the controls say for each of Gitlet's refusals, or `null` for
 * the engine's words: a person's words the engine writes for a refusal whose
 * sentence needs what only the engine knows (the conditions a gate still
 * waits for, and whom), and its code's own sentence for a refusal nobody has
 * worded yet (ADR-0060). COMPLETE OVER `MERGE_REFUSALS` by
 * the compiler, so a refusal the engine grows is a type error here before
 * it is a sentence in the general words of its class.
 */
export const MERGE_REFUSAL_SENTENCES: Readonly<Record<MergeRefusal, string | null>> = {
  repo_deleted: "Its repository is deleted, so nothing merges into it.",
  repo_archived: "Its repository is archived and read-only, so nothing merges into it.",
  repo_importing: "Its repository is still being imported. Try again once the import is done.",
  repo_mirrored:
    "Its repository is a pull mirror: it moves only by syncs from its source, and takes no merge.",
  expunging: "An expunge began on its repository while the merge was built. Merge it again.",
  change_not_found: "The repository holds no such change any more.",
  change_not_open: "It is no longer open: somebody merged or closed it since you loaded it.",
  change_draft: "It is a draft, and a draft merges nowhere. Mark it ready for review first.",
  stale: "Its target changed since you loaded it. Look again, then merge it.",
  head_moved:
    "Somebody pushed to it since you loaded it. Read what the push changed, then merge the head you read.",
  target_moved: "Its target moved while the merge was built. Merge it again.",
  target_moving:
    "Its target moved under every rebuild of the merge: the branch moves faster than a merge is built onto it. Add it to the queue, or try again at a quieter moment.",
  not_person: "Only a person overrides a merge gate.",
  queue_target:
    "Its target's gate requires a check, so its merges land through the queue: add it to the queue, or override as a person.",
  no_queue: "Its target has no queue: its gate requires no check, so it merges directly.",
  already_queued: "It is in its target's queue already.",
  queue_full: "Its target's queue is full. Try again once a change has landed.",
  gate_unmet: null,
  source_refused: null,
  upload_expired: "The merge took too long to record. Merge it again.",
  repo_size: null,
  compaction_behind:
    "Its repository is waiting for a compaction before it takes more. Try again in a moment.",
  not_queued: "It is not in its target's queue.",
  not_permitted: null,
  not_green: "Its target's required checks have not all passed on what it would land on.",
  not_red: "None of the checks named failed on that landing.",
  no_landing: "There is no landing above its target's green one.",
  record_full: "The landing holds more than one record may. Its queue lands fewer at once.",
  busy: "Its repository's committer is busy. Try again in a moment.",
  not_applied: "The merge was recorded and dropped on every node. Look again before you retry.",
  unavailable: "The code log refused it on every node. Try again in a moment.",
};

/** What the controls say when the merge's build met a conflict with its target's tip. */
const CONFLICT_SENTENCE =
  "It does not merge cleanly onto its target's tip: rebase it, or resolve the conflict on its branch.";

/** The controls' own words for a refusal, or null for the engine's. */
export function mergeRefusalSentence(hostRefusal: string): string | null {
  if (hostRefusal === MERGE_CONFLICT) return CONFLICT_SENTENCE;
  return (MERGE_REFUSALS as readonly string[]).includes(hostRefusal)
    ? MERGE_REFUSAL_SENTENCES[hostRefusal as MergeRefusal]
    : null;
}

/** A merge write's refusal, drawn in the controls' words where they have some. */
function MergeRefusalNote<T extends ActionTool>({ write }: { write: Act<T> }) {
  const said = write.refusal ? mergeRefusalSentence(write.refusal.hostRefusal) : null;
  return (
    <RefusalNote
      write={
        said && write.refusal ? { ...write, refusal: { ...write.refusal, sentence: said } } : write
      }
    />
  );
}

/** Where a live change's gate stands, as the controls read it. */
export function mergeStanding(
  change: CodeChangeDetail,
  nameOf: (handle: string) => string,
): {
  /** Its target's gate requires a check, so it is ADDED TO THE QUEUE rather than merged. */
  viaQueue: boolean;
  /** Its place in the queue, where it holds one. */
  queued: CodePrecondition | undefined;
  /** Why a Merge would be refused now, or undefined where the gate holds. */
  held: string | undefined;
} {
  const pre = change.readiness?.preconditions ?? [];
  const viaQueue = pre.some((p) => p.kind === "check");
  const queued = pre.find((p) => p.kind === "queue");
  const readiness = change.readiness;
  const subject = { change, nameOf };
  let held: string | undefined;
  if (!changeLive(change.state)) held = `This change is ${change.state}: it merges no more.`;
  else if (change.state === "draft") held = DRAFT_HELD;
  else if (queued) held = preconditionSentence(queued, subject) ?? undefined;
  else if (!readiness) held = "Its gate has not been judged on this read yet.";
  else if (readiness.met < readiness.total) held = readinessNext(readiness, subject);
  return { viaQueue, queued, held };
}

/** Why a draft is not merged: marking it ready, in the page bar, is the step. */
const DRAFT_HELD = "A draft merges nowhere. Mark it ready for review first.";

/** Each method's glyph on the method control. */
const METHOD_GLYPHS: Readonly<Record<MergeMethod, ReactNode>> = {
  merge: <GitMergeGlyph size="sm" />,
  squash: <GitCommitVerticalGlyph size="sm" />,
  fast_forward: <ArrowRightGlyph size="sm" />,
};

/** What a press will do, in words: the method, then the branch, then the work item. */
export function mergeDetails(
  change: CodeChangeDetail,
  method: MergeMethod,
  deleteBranch: boolean | null,
  closeTask: boolean,
  viaQueue: boolean,
): string {
  const target = refName(change.target_ref).name;
  const writes =
    method === "merge"
      ? `Gitlet writes one merge commit on ${target}.`
      : method === "squash"
        ? `Gitlet writes its changes as one commit on ${target}.`
        : `${target} moves to its head, ${shortOID(change.head)}, and no commit is written.`;
  const said = viaQueue ? `The queue tests it merged onto ${target}, then ${writes}` : writes;
  const branch =
    deleteBranch === true
      ? "Its branch is deleted."
      : deleteBranch === null && change.owner_kind === "agent"
        ? `Its branch is deleted unless ${target}'s rule keeps it.`
        : "Its branch is kept.";
  const work = change.work_item;
  const closes = closeTask && work ? ` ${work.key || work.id} closes once it merges.` : "";
  return `${said} ${branch}${closes}`;
}

/**
 * The box's foot: how it merges, whether its branch goes and its work item
 * closes, the count and what it waits on, then Merge (or Add to queue) and
 * Override, and what the press will do.
 */
export function MergePanel({
  change,
  checks,
}: {
  change: CodeChangeDetail;
  checks: readonly CodeCheckRow[] | null;
}) {
  const merge = useAct("code_merge_change");
  const viewer = useViewer();
  const [method, setMethod] = useState<MergeMethod>(MERGE_METHODS[0]);
  // UNSAID UNTIL CHOSEN: the engine's own default stands until the person ticks it
  // either way; the details line says which way it goes.
  const [deleteBranch, setDeleteBranch] = useState<boolean | null>(null);
  const [closeTask, setCloseTask] = useState(false);
  const [overriding, setOverriding] = useState(false);
  const seatBadge = useSeatBadgeOf();
  if (!changeLive(change.state)) return null;
  const nameOf = (h: string) => seatBadge(h).name;
  const readiness = change.readiness;
  const sum = readiness && (
    <div className="code-merge-box__sum">
      <SegmentedMeter
        size="compact"
        decorative
        total={readiness.total}
        segments={readiness.preconditions.map((p, i) => ({
          id: `${i}-${p.kind}`,
          value: 1,
          tone: preconditionTone(p),
          label: p.state,
        }))}
      />
      <span>
        <strong>
          {readiness.met} of {plural(readiness.total, "condition")} met.
        </strong>{" "}
        {readinessWaits(readiness, { change, nameOf, me: viewer.handle }, checks)}
      </span>
    </div>
  );
  // A DRAFT IS NOT OFFERED A MERGE: marking it ready is the page bar's step.
  if (change.state === "draft") {
    return (
      <div className="code-merge-box__controls">
        {sum}
        <p className="code-merge-box__note">
          <Face who={GITLET} size="xs" />
          <span>A draft merges nowhere: mark it ready for review in the page bar.</span>
        </p>
      </div>
    );
  }
  const { viaQueue, held } = mergeStanding(change, nameOf);
  const work = change.work_item;
  const person = viewer.kind === "human";
  const overrideHeld = !person
    ? "Only a person overrides a merge gate."
    : !held
      ? `Its gate holds: ${viaQueue ? "add it to the queue" : "merge it"} instead.`
      : undefined;
  const submit = async () => {
    await merge.run(
      {
        change: change.ref,
        expected_head: change.head,
        method,
        ...(deleteBranch === null ? {} : { delete_branch: deleteBranch }),
        ...(closeTask ? { close_task: true } : {}),
      },
      { done: viaQueue ? `Added ${change.ref} to the queue` : `Merged ${change.ref}` },
    );
  };
  return (
    <div className="code-merge-box__controls">
      <div className="code-merge-box__how">
        <SegmentedControl<MergeMethod>
          semantics="radio"
          activate="manual"
          size="sm"
          label="How it merges"
          value={method}
          onValueChange={setMethod}
          options={MERGE_METHODS.map((value) => ({
            value,
            icon: METHOD_GLYPHS[value],
            ...METHOD_LABELS[value],
          }))}
        />
        <span className="code-merge-box__ticks">
          <Checkbox
            size="sm"
            label="Delete its branch"
            checked={deleteBranch === true}
            onCheckedChange={setDeleteBranch}
          />
          {work && (
            <Checkbox
              size="sm"
              label={`Close ${work.key || work.id} once it merges`}
              checked={closeTask}
              onCheckedChange={setCloseTask}
            />
          )}
        </span>
      </div>
      {sum}
      <div className="code-merge-box__act">
        <WriteButton
          write={merge}
          size="small"
          variant="primary"
          leadingIcon={viaQueue ? <ListOrderedGlyph size="sm" /> : <GitMergeGlyph size="sm" />}
          blocked={held}
          showRefusal={false}
          onPress={() => void submit()}
        >
          {viaQueue ? "Add to queue" : "Merge"}
        </WriteButton>
        <WriteButton
          write={merge}
          size="small"
          variant="ghost"
          leadingIcon={<ShieldUserGlyph size="sm" />}
          blocked={overrideHeld}
          showRefusal={false}
          onPress={() => setOverriding(true)}
        >
          Override
        </WriteButton>
        <p className="code-merge-box__note">
          <Face who={GITLET} size="xs" />
          <span>{mergeDetails(change, method, deleteBranch, closeTask, viaQueue)}</span>
        </p>
      </div>
      {!overriding && <MergeRefusalNote write={merge} />}
      {overriding && (
        <OverrideDialog
          change={change}
          method={method}
          deleteBranch={deleteBranch}
          closeTask={closeTask}
          onClose={() => setOverriding(false)}
        />
      )}
    </div>
  );
}

/**
 * How a merged change came to be merged, in words, by the engine's
 * `merged_via` (`gitlet.MergedVias`): a push that brought its target to its
 * head, a merge record held to its gate (or a person's override of it), or
 * its target's queue. A way a newer engine names is said as the gate's.
 */
function mergedVia(change: CodeChangeDetail): string {
  switch (change.merged_via) {
    case "push":
      return "brought its target to its head by a push";
    case "queue":
      return "landed it through its target's queue";
  }
  return change.override_reason ? "merged it past its gate" : "merged it through its gate";
}

/** What the merge box reads beside the change: its checks, reviewers' standing and findings. */
export interface MergeFacts {
  revisions: readonly CodeRevisionRow[];
  checks: LatestChecks;
  standings: readonly Standing[];
  /** Whom a re-request asks: every standing verdict not an approval of the head, not asked now. */
  again: readonly string[];
  /** The open findings counted by weight, null where not every open one is loaded. */
  severities: ReadonlyMap<string, number> | null;
}

/** The box's disc on the timeline's spine: where the change stands, named. */
export function MergeDisc({
  change,
  checks,
}: {
  change: CodeChangeDetail;
  checks: readonly CodeCheckRow[] | null;
}) {
  const seatBadge = useSeatBadgeOf();
  const viewer = useViewer();
  if (change.state === "merged") {
    return <Disc glyph={<GitMergeGlyph size="md" />} tone="success" size="large" label="Merged" />;
  }
  if (change.state === "closed") {
    return (
      <Disc
        glyph={<GitPullRequestClosedGlyph size="md" />}
        tone="neutral"
        size="large"
        label="Closed"
      />
    );
  }
  const readiness = change.readiness;
  const label = readiness
    ? readinessWaits(
        readiness,
        { change, nameOf: (h) => seatBadge(h).name, me: viewer.handle },
        checks,
        "Waits on",
      )
    : "Not judged yet";
  return (
    <Disc
      glyph={<GitMergeGlyph size="md" />}
      tone={boxTone(readiness)}
      size="large"
      label={label}
    />
  );
}

/**
 * Whether the change may merge: a section per question its gate asks, each on its disc, then the
 * controls. A merged change says into what, by whom and how; a closed one says why it closed.
 */
export function MergeBox({ change, facts }: { change: CodeChangeDetail; facts: MergeFacts }) {
  const now = useNow();
  const target = refName(change.target_ref).name;
  if (change.state === "merged") {
    return (
      <Card as="section" padding="none" className="code-merge-box" aria-label="Merge">
        <article className="code-merge-box__section" aria-label={`Merged into ${target}`}>
          <Disc glyph={<GitMergeGlyph size="sm" />} tone="success" />
          <span className="code-merge-box__words">
            <strong className="code-merge-box__title">Merged into {target}</strong>
            <span className="code-merge-box__said">
              {change.merged_by && (
                <Byline
                  who={change.merged_by}
                  verb={mergedVia(change)}
                  at={change.merged_at}
                  now={now}
                />
              )}
              {!change.merged_by && <span>{mergedSentence(change)}</span>}
              {change.merge_oid && (
                <>
                  {" "}
                  as{" "}
                  <a className="prose-link mono" href={commitHref(change.repo, change.merge_oid)}>
                    {shortOID(change.merge_oid)}
                  </a>
                </>
              )}
            </span>
          </span>
        </article>
        <RevertChangePanel change={change} />
      </Card>
    );
  }
  if (change.state === "closed") {
    return (
      <Card as="section" padding="none" className="code-merge-box" aria-label="Merge">
        <article className="code-merge-box__section" aria-label="Closed without being merged">
          <Disc glyph={<GitPullRequestClosedGlyph size="sm" />} tone="neutral" />
          <span className="code-merge-box__words">
            <strong className="code-merge-box__title">Closed without being merged</strong>
            <span className="code-merge-box__said">
              {change.close_reason ?? "Nobody said why."} Reopen it from the page bar.
            </span>
          </span>
        </article>
      </Card>
    );
  }
  return (
    <Card as="section" padding="none" className="code-merge-box" aria-label="Merge">
      {!change.readiness && (
        <article className="code-merge-box__section" aria-label="Not judged yet">
          <Disc glyph={<CircleDashedGlyph size="sm" />} />
          <span className="code-merge-box__words">
            <strong className="code-merge-box__title">Not judged yet</strong>
            <span className="code-merge-box__said">
              Its gate has not been judged on this read yet.
            </span>
          </span>
        </article>
      )}
      {change.readiness &&
        mergeSections(change.readiness).map((section, i) => (
          <SectionRow
            key={`${section.key}-${i}`}
            change={change}
            section={section}
            facts={facts}
            now={now}
          />
        ))}
      <MergePanel change={change} checks={facts.checks.rows} />
    </Card>
  );
}

/** One section of the box: its disc, its title and sentence, what it offers, and what hangs under it. */
function Section({
  section,
  glyph,
  title,
  said,
  saidTitle,
  end,
  children,
}: {
  section: Pick<MergeSection, "preconditions">;
  glyph: ReactNode;
  title: string;
  said: ReactNode;
  saidTitle?: string | undefined;
  end?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <article className="code-merge-box__section" aria-label={title}>
      <Disc glyph={glyph} tone={sectionTone(section)} />
      <span className="code-merge-box__words">
        <strong className="code-merge-box__title">{title}</strong>
        <span className="code-merge-box__said" title={saidTitle}>
          {said}
        </span>
      </span>
      {end && <span className="code-merge-box__end">{end}</span>}
      {children}
    </article>
  );
}

/** A section by its key, with what it reads. */
function SectionRow({
  change,
  section,
  facts,
  now,
}: {
  change: CodeChangeDetail;
  section: MergeSection;
  facts: MergeFacts;
  now: number;
}) {
  const seatBadge = useSeatBadgeOf();
  const viewer = useViewer();
  const subject = { change, nameOf: (h: string) => seatBadge(h).name, me: viewer.handle };
  const target = refName(change.target_ref).name;
  switch (section.key) {
    case "reviews": {
      const words = reviewsWords(
        section.preconditions,
        subject,
        facts.standings,
        change.authors.map((a) => a.author),
      );
      return (
        <Section
          section={section}
          glyph={<UsersGlyph size="sm" />}
          title={words.title}
          said={words.sentence}
          end={
            <>
              <Faces change={change} standings={facts.standings} />
              <ReRequest change={change} handles={facts.again} />
            </>
          }
        />
      );
    }
    case "checks":
      return (
        <ChecksSection
          change={change}
          section={section}
          checks={facts.checks}
          kept={checksKept(change, facts.revisions, now)}
        />
      );
    case "findings": {
      const words = findingsWords(
        section.preconditions,
        subject,
        change.conversation?.open_findings ?? 0,
        facts.severities,
      );
      return (
        <Section
          section={section}
          glyph={<ShieldCheckGlyph size="sm" />}
          title={words.title}
          said={words.sentence}
        />
      );
    }
    case "facts": {
      const head = facts.revisions.find((r) => r.n === change.revision)?.facts;
      const measuring = section.state === "computing" || section.state === "unmet";
      if (measuring || (!head && section.state !== "met")) {
        return (
          <Section
            section={section}
            glyph={<ScrollTextGlyph size="sm" />}
            title="Its facts are being measured"
            said={`Revision ${change.revision} is still being measured against ${target}.`}
          />
        );
      }
      if (!head) {
        return (
          <Section
            section={section}
            glyph={<ScrollTextGlyph size="sm" />}
            title="Its facts are recorded"
            said={`Revision ${change.revision}'s facts are recorded.`}
          />
        );
      }
      const touches = head.touches_all
        ? "touches more files than its facts list"
        : `touches ${plural(head.touched.length, "file")}`;
      return (
        <Section
          section={section}
          glyph={<ScrollTextGlyph size="sm" />}
          title="Its facts are recorded"
          saidTitle={head.measured_at ? undefined : NOT_KEPT_MEASURED}
          said={
            head.measured_at ? (
              <>
                Revision {change.revision} {touches}, measured{" "}
                <time dateTime={head.measured_at} title={fmtDateTime(head.measured_at)}>
                  {ago(head.measured_at, now, { clock: true })}
                </time>
                .
              </>
            ) : (
              `Revision ${change.revision} ${touches}.`
            )
          }
        />
      );
    }
    case "mergeable":
      return <MergesSection change={change} section={section} now={now} />;
    case "other":
      return <OtherSection change={change} section={section} />;
  }
}

/** Why a revision's facts carry no time: they were recorded before the engine kept it. */
export const NOT_KEPT_MEASURED =
  "When they were measured is not kept for facts recorded before it was.";

/** Up to five reviewers' faces, the reader's ringed, and how many more. */
function Faces({
  change,
  standings,
}: {
  change: CodeChangeDetail;
  standings: readonly Standing[];
}) {
  const viewer = useViewer();
  const whoName = useWhoName();
  const waiting = (change.readiness?.preconditions ?? [])
    .filter((p) => APPROVALS.has(p.kind))
    .flatMap((p) => (p.waits_on ?? []).map((w) => w.handle));
  const handles = [...new Set([...standings.map((s) => s.handle), ...waiting])];
  if (handles.length === 0) return null;
  const shown = handles.slice(0, 5);
  const names = namesSaid(handles.map((h) => whoName(h)));
  return (
    <span className="code-merge-box__faces" role="img" aria-label={`Reviewers: ${names}`}>
      {shown.map((h) => (
        <Face key={h} who={h} ring={h === viewer.handle ? "brand" : undefined} />
      ))}
      {handles.length > 5 && <span className="code-merge-box__more">+{handles.length - 5}</span>}
    </span>
  );
}

const APPROVALS = new Set(["approval", "human_approval", "owners_approval"]);

/** Re-request: one ask of everyone whose standing verdict is not an approval of the head. */
export function ReRequest({
  change,
  handles,
  icon = false,
}: {
  change: CodeChangeDetail;
  handles: readonly string[];
  /** The rail's one-reviewer form: a ghost icon button. */
  icon?: boolean;
}) {
  const ask = useAct("code_request_review");
  const whoName = useWhoName();
  if (handles.length === 0 || !changeLive(change.state)) return null;
  const names = namesSaid(handles.map((h) => whoName(h)));
  const press = () =>
    void ask.run(
      { change: change.ref, reviewers: [...handles] },
      { done: `Asked ${names} to review ${change.ref} again` },
    );
  return icon ? (
    <WriteButton
      write={ask}
      size="small"
      variant="ghost"
      aria-label={`Ask ${names} to review again`}
      onPress={press}
    >
      <RotateCwGlyph size="sm" />
    </WriteButton>
  ) : (
    <WriteButton
      write={ask}
      size="small"
      variant="secondary"
      leadingIcon={<RotateCwGlyph size="sm" />}
      title={`Ask ${names} to review again`}
      onPress={press}
    >
      Re-request
    </WriteButton>
  );
}

/** The Checks section: each check's latest attempt, Re-run where one failed, and Hide or Show. */
function ChecksSection({
  change,
  section,
  checks,
  kept,
}: {
  change: CodeChangeDetail;
  section: MergeSection;
  checks: LatestChecks;
  /** Whether its head's checks are still kept (`checksKept`). */
  kept: boolean;
}) {
  const rerun = useAct("code_rerun_checks");
  const rows = checks.rows ?? [];
  const allPassed = rows.every((r) => !checkWaits(r));
  // SHOWN WHILE ONE HAS NOT PASSED; the reader's press holds for the visit.
  const [shown, setShown] = useState<boolean | null>(null);
  const open = shown ?? !allPassed;
  const [pane, setPane] = useState<CodeCheckRow | null>(null);
  const details = useRef(new Map<string, HTMLButtonElement>());
  const listId = useId();
  const target = refName(change.target_ref).name;
  const sectioned = { preconditions: section.preconditions };
  if (checks.refused !== null) {
    return (
      <Section
        section={{ preconditions: [] }}
        glyph={<ListChecksGlyph size="sm" />}
        title="Its checks are unavailable"
        said={
          checks.refused
            ? `${checks.refused.charAt(0).toUpperCase()}${checks.refused.slice(1).replace(/\.?$/, ".")}`
            : "Its read was refused."
        }
      />
    );
  }
  if (checks.rows === null) {
    return (
      <Section
        section={sectioned}
        glyph={<ListChecksGlyph size="sm" />}
        title="Reading its checks"
        said="Its latest checks are being read."
      />
    );
  }
  const words = checksWords(rows, target, kept);
  const failed = rows.some((r) => ["failed", "errored", "cancelled"].includes(r.state));
  const close = () => {
    const key = pane ? attemptKey(pane) : "";
    setPane(null);
    queueMicrotask(() => details.current.get(key)?.focus());
  };
  return (
    <Section
      section={sectioned}
      glyph={<ListChecksGlyph size="sm" />}
      title={words.title}
      said={words.sentence}
      end={
        <>
          {failed && changeLive(change.state) && (
            <WriteButton
              write={rerun}
              size="small"
              variant="ghost"
              leadingIcon={<RotateCwGlyph size="sm" />}
              onPress={() =>
                void rerun.run(
                  { change: change.ref, all_failed: true },
                  { done: `Ran ${change.ref}'s failed checks again` },
                )
              }
            >
              Re-run failed checks
            </WriteButton>
          )}
          {rows.length > 0 && (
            <Button
              size="small"
              variant="ghost"
              aria-expanded={open}
              aria-controls={listId}
              trailingIcon={open ? <ChevronUpGlyph size="sm" /> : <ChevronDownGlyph size="sm" />}
              onClick={() => setShown(!open)}
            >
              {open ? "Hide" : "Show"}
            </Button>
          )}
        </>
      }
    >
      <ul
        id={listId}
        className="code-merge-checks"
        aria-label={`Checks of ${shortOID(change.head)}`}
        hidden={!open || rows.length === 0}
      >
        {rows.map((r) => (
          <li key={attemptKey(r)} className="code-merge-check">
            <CheckMark state={r.state} />
            <span className="code-merge-check__said">
              <strong>{r.name}</strong> · <CheckPhrase row={r} />
            </span>
            <span className="code-merge-check__end">
              {r.required && (
                <Tag size="xs" appearance="outline">
                  Required
                </Tag>
              )}
              <button
                type="button"
                className="t-link code-merge-check__details"
                ref={(b) => {
                  if (b) details.current.set(attemptKey(r), b);
                }}
                aria-label={`Details of ${r.name}`}
                onClick={() => setPane(pane && attemptKey(pane) === attemptKey(r) ? null : r)}
              >
                Details
              </button>
            </span>
          </li>
        ))}
      </ul>
      {pane && pane.repo && (
        <PaneAt key={attemptKey(pane)} onEscape={close}>
          <CheckPane row={pane} repo={pane.repo} onClose={close} />
        </PaneAt>
      )}
    </Section>
  );
}

/** A check's pane, focused as it opens; Escape closes it. */
function PaneAt({ onEscape, children }: { onEscape: () => void; children: ReactNode }) {
  const at = useRef<HTMLDivElement>(null);
  useEffect(() => at.current?.focus(), []);
  return (
    <div
      ref={at}
      tabIndex={-1}
      className="code-merge-checks__pane"
      onKeyDown={(event) => {
        if (event.key === "Escape") {
          event.stopPropagation();
          onEscape();
        }
      }}
    >
      {children}
    </div>
  );
}

/** The Merges section: whether its revision merges into its target, measured when, and Rebase. */
function MergesSection({
  change,
  section,
  now,
}: {
  change: CodeChangeDetail;
  section: MergeSection;
  now: number;
}) {
  const m = change.mergeability;
  const target = refName(change.target_ref).name;
  // A DRAFT REBASES TOO: the engine rebases any live change.
  const rebase = changeLive(change.state) ? <RebaseButton change={change} /> : undefined;
  const glyph = <GitMergeGlyph size="sm" />;
  if (!m || m.target_ref !== change.target_ref) {
    // AFTER A RETARGET the measurement on hand is the old target's: named as its own.
    const old = m && refName(m.target_ref).name;
    return (
      <Section
        section={section}
        glyph={glyph}
        title={`Not measured against ${target} yet`}
        said={
          old
            ? `Revision ${m.revision} was measured against ${old}, its earlier target; ${target} is still being measured by its home.`
            : `Revision ${change.revision} is still being merged onto ${target} by its home.`
        }
        end={rebase}
      />
    );
  }
  const which = m.revision === change.revision ? "measured" : `revision ${m.revision} measured`;
  // A SENTENCE'S START, or a clause after the conflict's paths.
  const measured = (start: boolean) => (
    <>
      {start ? `${which.charAt(0).toUpperCase()}${which.slice(1)}` : which} against {target} at{" "}
      <a className="prose-link mono" href={commitHref(change.repo, m.target_oid)}>
        {shortOID(m.target_oid)}
      </a>
      ,{" "}
      <time dateTime={m.at} title={fmtDateTime(m.at)}>
        {ago(m.at, now, { clock: true })}
      </time>
      .
    </>
  );
  if (m.state === "clean") {
    return (
      <Section
        section={section}
        glyph={glyph}
        title={`Merges cleanly into ${target}`}
        said={measured(true)}
        end={rebase}
      />
    );
  }
  if (m.state === "conflict") {
    const paths = m.conflicts ?? [];
    const count = paths.length + (m.conflicts_omitted ?? 0);
    return (
      <Section
        section={section}
        glyph={glyph}
        title={`Conflicts with ${target}`}
        said={
          <>
            In {plural(count, "path")}, {measured(false)}
          </>
        }
        end={rebase}
      >
        {paths.length > 0 && (
          <ul className="code-merge-paths" aria-label="Paths in conflict">
            {paths.map((path) => (
              <li key={path} className="mono">
                {path}
              </li>
            ))}
            {(m.conflicts_omitted ?? 0) > 0 && <li>and {m.conflicts_omitted} more</li>}
          </ul>
        )}
      </Section>
    );
  }
  return (
    <Section
      section={section}
      glyph={glyph}
      title={`Not decided against ${target}`}
      said={`Its home could not decide whether revision ${m.revision} merges into ${target} in a read’s time.`}
      end={rebase}
    />
  );
}

/** A condition none of the five holds: a draft, merge authority, the queue, or a newer kind. */
function OtherSection({ change, section }: { change: CodeChangeDetail; section: MergeSection }) {
  const seatBadge = useSeatBadgeOf();
  const viewer = useViewer();
  const p = section.preconditions[0]!;
  const subject = { change, nameOf: (h: string) => seatBadge(h).name, me: viewer.handle };
  const target = refName(change.target_ref).name;
  const said = preconditionSentence(p, subject) ?? metSentence(p, subject);
  switch (p.kind) {
    case "draft":
      return (
        <Section
          section={section}
          glyph={<PencilGlyph size="sm" />}
          title={p.state === "met" ? "It is ready for review" : "It is a draft"}
          said={said.replace(/^It is a draft\. /, "")}
        />
      );
    case "merge_authority":
      return (
        <Section
          section={section}
          glyph={<ShieldUserGlyph size="sm" />}
          title={
            p.state === "met" ? `You may merge into ${target}` : `You may not merge into ${target}`
          }
          said={said}
        />
      );
    case "queue": {
      const q = p.queue;
      const place = q ? (ORDINALS[q.place - 1] ?? `#${q.place}`) : "";
      return (
        <Section
          section={section}
          glyph={<ListOrderedGlyph size="sm" />}
          title={
            q?.state === "testing"
              ? "Testing on a train"
              : q
                ? `${place} in ${target}'s queue`
                : `In ${target}'s queue`
          }
          said={said}
          end={<DequeueButton changeRef={change.ref} target={target} queued />}
        />
      );
    }
  }
  return (
    <Section
      section={section}
      glyph={<CircleDashedGlyph size="sm" />}
      title={preconditionWord(p.kind)}
      said={said}
    />
  );
}

/** How a merged change came to be merged where no record names who merged it. */
function mergedSentence(change: CodeChangeDetail): string {
  switch (change.merged_via) {
    case "push":
      return "Its target came to hold its head by a push.";
    case "queue":
      return "Its target's queue landed it.";
  }
  return "It merged through its gate.";
}

/**
 * A person's merge past the gate: merged directly — even where the target's
 * merges land through its queue — and audited, with the reason they give,
 * which the change keeps beside its merge.
 */
function OverrideDialog({
  change,
  method,
  deleteBranch,
  closeTask,
  onClose,
}: {
  change: CodeChangeDetail;
  method: MergeMethod;
  deleteBranch: boolean | null;
  closeTask: boolean;
  onClose: () => void;
}) {
  const write = useAct("code_merge_change");
  const [reason, setReason] = useState("");
  const submit = async () => {
    const result = await write.run(
      {
        change: change.ref,
        expected_head: change.head,
        method,
        override_reason: reason.trim(),
        ...(deleteBranch === null ? {} : { delete_branch: deleteBranch }),
        ...(closeTask ? { close_task: true } : {}),
      },
      { done: `Merged ${change.ref} past its gate` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  const seatBadge = useSeatBadgeOf();
  const { held } = mergeStanding(change, (h) => seatBadge(h).name);
  return (
    <Modal
      open
      size="sm"
      title={`Override ${change.ref}'s gate`}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="danger"
            showRefusal={false}
            blocked={reason.trim() ? undefined : "Say why it merges past its gate."}
            onPress={() => void submit()}
          >
            Merge past the gate
          </WriteButton>
        </>
      }
    >
      <Callout variant="warning">
        It merges now, directly, whatever its gate still waits on.{held ? ` ${held}` : ""} Your
        reason is kept with the merge, and the override is audited.
      </Callout>
      <Textarea
        aria-label="Reason"
        placeholder="Why it merges past its gate"
        value={reason}
        rows={3}
        onChange={(event) => setReason(event.target.value)}
      />
      <MergeRefusalNote write={write} />
    </Modal>
  );
}
