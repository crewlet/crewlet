/**
 * One change's page, `#/gitlet/repos/{name}/changes/{n}` — `web!42` in full,
 * laid out as a code host lays one out: a header saying who wants to merge
 * which revision of which branch into which, then the conversation about it
 * with where its merge stands (its merge box) and a rail of whom it asked, its
 * diff with every finding and thread on the lines they are about, its
 * revisions, where it came from and whom its records notified.
 *
 * # Read a part at a time, at the tab's floor
 *
 * `code_change` answers the change a part at a time: its detail rides the
 * first page of its revisions, and each tab asks the part it draws — the
 * conversation its reviews and comments, the diff a revision's files, one
 * file's patch and the findings placed in that revision. Every one is a code
 * question read at this tab's floor on the code log (`lib/useCodeQuery.ts`),
 * so a press here is followed by reads that wait for it.
 *
 * # Every write is the engine's answer, never a guess
 *
 * Approve, Request changes and Comment are ONE review each
 * (`code_review_change`), carrying the findings drafted on the diff's lines;
 * Request review asks reviewers; Close and Reopen move the change's life
 * against the version on screen; Apply suggestion commits a finding's
 * suggestion on the change's branch against the head on screen; a comment, a
 * reply and Resolve go through `code_comment_on_change`; Merge (or Add to
 * queue), Override and Re-run checks are the merge controls in the
 * conversation's merge box (`Merge.tsx`), Merge sending the head on screen.
 * Each goes through `useAct` and a `WriteButton`: drawn for every reader the
 * change's state leaves it to, disabled with the reason where this reader
 * cannot act, and NEVER OPTIMISTIC: nothing on screen moves until the engine
 * answers, and the answer's position is the floor the page is read at next.
 *
 * # What the change's state rules out is not drawn
 *
 * A merged change takes no review, no ask and no close, from anybody, ever;
 * a closed one takes only its reopen. Drawn disabled, each read as something
 * this reader lacks rather than something the change is done with, so the
 * bar of a finished change offers only what still applies: its Reopen, and a
 * merged change's Revert (`ChangeWrites.tsx`). What THIS reader cannot do on
 * a live change stays drawn, held with its sentence: an author's own verdicts
 * above all, since an author's review counts for nothing and the engine takes
 * none (`own_change`), where Request review is the author's step and leads the
 * bar instead.
 */

import { useId, useMemo, useState, type ReactNode } from "react";
import {
  Button,
  EmptyState,
  FormField,
  Modal,
  Skeleton,
  Stack,
  Tabs,
  Textarea,
  tabId,
} from "@crewlethq/ui";
import {
  BellGlyph,
  DiffGlyph,
  ListChecksGlyph,
  MessagesSquareGlyph,
  RotateCcwClockGlyph,
  ScrollTextGlyph,
} from "@crewlethq/icons/glyphs";
import { DraftBoxes, DraftBoxesContext, boxesDraft, useOpenBoxes } from "~/app/assistant/boxes.ts";
import { useAssistantDraft } from "~/app/assistant/draft.ts";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePageMenu } from "~/app/Shell.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { QueryState } from "~/components/common.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import { authoredBy, changeLive, changeRef, isRepoName, refName } from "~/lib/code.ts";
import { useNow } from "~/lib/clock.ts";
import { draftedRevisions, findingOf, forgetDrafts, readDrafts } from "~/lib/reviewDrafts.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useAct } from "~/lib/useAct.ts";
import { menuHold, type WriteAccess } from "~/lib/useWriteAccess.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeChangeDetail, CodeVerdict } from "~/protocol/index.ts";
import { ChangeStateTag, ReadinessMeter } from "../Changes.tsx";
import { CodeNotFound } from "../NotFound.tsx";
import { ReviewerPicker } from "../ReviewerPicker.tsx";
import { changesHref, repoHref } from "../paths.ts";
import { Byline } from "../ui.tsx";
import { Checks } from "./Checks.tsx";
import { Conversation } from "./Conversation.tsx";
import { Files } from "./Files.tsx";
import { Commits, Notified, Provenance } from "./Facts.tsx";
import { CHANGE_POLL_MS, useStandingFindings } from "./reads.ts";
import "~/styles/gitlet.css";

/** The page's tabs, the conversation first: what a reader opens a change for. */
const CHANGE_TABS = [
  "conversation",
  "files",
  "commits",
  "checks",
  "provenance",
  "notified",
] as const;
type ChangeTab = (typeof CHANGE_TABS)[number];

/** Each tab's glyph: decorative, its label names it. */
const CHANGE_TAB_GLYPHS: Readonly<Record<ChangeTab, ReactNode>> = {
  conversation: <MessagesSquareGlyph size="sm" />,
  files: <DiffGlyph size="sm" />,
  commits: <RotateCcwClockGlyph size="sm" />,
  checks: <ListChecksGlyph size="sm" />,
  provenance: <ScrollTextGlyph size="sm" />,
  notified: <BellGlyph size="sm" />,
};

const CHANGE_TAB_LABELS: Readonly<Record<ChangeTab, string>> = {
  conversation: "Conversation",
  files: "Files",
  commits: "Commits",
  checks: "Checks",
  provenance: "Provenance",
  // WHOM ITS RECORDS NOTIFIED, named for what the tab lists.
  notified: "Notified",
};

export function ChangePage({ repo, number }: { repo: string; number: number }) {
  const named = isRepoName(repo);
  const read = useCodeQuery(
    "code_change",
    { name: repo, change: number },
    { enabled: named, pollMs: CHANGE_POLL_MS },
  );
  const [tab, setTab] = useTab<ChangeTab>("tab", CHANGE_TABS);
  const panelId = useId();
  const [boxes] = useState(() => new DraftBoxes());
  if (!named) {
    return (
      <EmptyState
        title="Not a repository"
        description={`“${repo}” is not a name a repository can have.`}
      />
    );
  }
  const change = read.data?.change;
  if (read.loading && !change)
    return <Skeleton variant="text" rows={8} label="Loading the change" />;
  if (!change) {
    if (read.error === "not_found") {
      return (
        <CodeNotFound
          title={`${repo} has no change ${number}`}
          description={read.detail ?? "No change has that number: it may not have been opened yet."}
          repo={repo}
          changes
        />
      );
    }
    return (
      <QueryState error={read.error} refusal={read.refusal} detail={read.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  return (
    <DraftBoxesContext.Provider value={boxes}>
      <ChangeDraft boxes={boxes} change={change} />
      <PageActions>
        <ChangeActions change={change} />
      </PageActions>
      <header>
        <ChangeHeader change={change} />
        <Tabs
          overflow="menu"
          variant="underline"
          ariaLabel="The change's sections"
          value={tab}
          onValueChange={(next) => setTab(next as ChangeTab)}
          panelId={panelId}
          items={CHANGE_TABS.map((value) => ({
            value,
            label: CHANGE_TAB_LABELS[value],
            icon: CHANGE_TAB_GLYPHS[value],
            ...(value === "conversation" && change.conversation
              ? { count: change.conversation.reviews + change.conversation.comments }
              : {}),
            ...(value === "commits" ? { count: change.revision } : {}),
          }))}
        />
      </header>
      <div
        className="tabpanel"
        role="tabpanel"
        id={panelId}
        aria-labelledby={tabId(panelId, tab)}
        tabIndex={0}
      >
        {tab === "conversation" && (
          <Conversation change={change} revisions={read.data?.revisions ?? []} />
        )}
        {tab === "files" && <Files change={change} revisions={read.data?.revisions ?? []} />}
        {tab === "commits" && <Commits change={change} revisions={read.data?.revisions ?? []} />}
        {tab === "checks" && <Checks change={change} />}
        {tab === "provenance" && <Provenance change={change} />}
        {tab === "notified" && <Notified change={change} />}
      </div>
    </DraftBoxesContext.Provider>
  );
}

/**
 * The change page's ONE DRAFT for the person's own assistant: every box open
 * on it (`app/assistant/boxes.ts`), the comment on the change, each reply box
 * of a thread or a finding, and a finding being written on picked lines.
 * The review, ask and close dialogs are modal, and the drawer is inert while
 * one holds the page, so none of their fields is among them.
 */
function ChangeDraft({ boxes, change }: { boxes: DraftBoxes; change: CodeChangeDetail }) {
  const open = useOpenBoxes(boxes);
  useAssistantDraft(
    boxesDraft(open, {
      id: change.ref,
      revision: change.revision,
      scope: `What you are writing on ${change.ref} (not sent)`,
      help:
        `Every box open on ${change.ref} is a field: the comment on the change, the reply ` +
        "box of each thread and finding, and a finding being written on picked lines. You " +
        "can propose text for any of them; commenting, replying and adding a finding to " +
        "the review stay the person's.",
    }),
  );
  return null;
}

/**
 * What the change's byline says it does, as a code host's does: who wants to
 * merge which revision of which branch into which — or merged it, or closed
 * it — and when.
 */
function bylineVerb(change: CodeChangeDetail): ReactNode {
  const source = refName(change.source_ref).name;
  const target = refName(change.target_ref).name;
  // EACH BRANCH A LINK IN THE SENTENCE, so it takes `.prose-link`'s rule
  // under its words as every link inside a sentence does, on its chip.
  const branches = (
    <>
      <a className="code-branch-chip prose-link" href={repoHref(change.repo, { ref: source })}>
        {source}
      </a>{" "}
      into{" "}
      <a className="code-branch-chip prose-link" href={repoHref(change.repo)}>
        {target}
      </a>
    </>
  );
  switch (change.state) {
    case "merged":
      return (
        <>
          merged revision {change.revision} of {branches}
        </>
      );
    case "closed":
      return <>closed its proposal of {branches}</>;
    case "draft":
      return (
        <>
          drafts revision {change.revision} of {branches}
        </>
      );
  }
  return (
    <>
      wants to merge revision {change.revision} of {branches}
    </>
  );
}

/** When the byline's act happened: the merge, the close, or the opening. */
function bylineAt(change: CodeChangeDetail): string | undefined {
  if (change.state === "merged") return change.merged_at ?? change.opened_at;
  if (change.state === "closed") return change.closed_at ?? change.opened_at;
  return change.opened_at;
}

/**
 * The change's header: its reference, title and state — beside a live
 * change's readiness, the meter every listing draws — and its byline, which
 * says what it proposes. Where it stands with its reviewers, its work item
 * and its head are the conversation's rail, beside it.
 */
function ChangeHeader({ change }: { change: CodeChangeDetail }) {
  const now = useNow();
  const seatBadge = useSeatBadgeOf();
  const actor = change.state === "merged" && change.merged_by ? change.merged_by : change.owner;
  const work = change.work_item;
  return (
    <ObjectHeader
      kind="Change"
      icon="diff"
      identifier={change.ref}
      title={change.title}
      status={
        <>
          <ChangeStateTag state={change.state} />
          {changeLive(change.state) && change.readiness && (
            <ReadinessMeter change={change} nameOf={(h) => seatBadge(h).name} />
          )}
        </>
      }
      description={
        <span className="code-change-byline">
          <Byline who={actor} verb={bylineVerb(change)} at={bylineAt(change)} now={now}>
            <a className="t-link" href={repoHref(change.repo)}>
              {change.repo}
            </a>
            {work && (
              <a
                className="t-link mono"
                href={changesHref({ work: `${work.backend}:${work.id}`, state: "all" })}
              >
                {work.key || work.id}
              </a>
            )}
          </Byline>
        </span>
      }
    />
  );
}

/**
 * Why the reader's verdict on a change they wrote is held: an author's review
 * counts for nothing, so the engine takes none (`own_change`), and the step
 * that is theirs is asking somebody else.
 */
export const OWN_CHANGE_HELD =
  "You wrote this change, so your review would not count. Ask somebody else to review it.";

/**
 * The page bar's writes, as the change's state leaves them: on a LIVE change a
 * draft's Ready for review, the three verdicts, an ask for review and Close;
 * on a closed one its Reopen alone; on a merged one none, its Revert standing
 * where the merge controls were. A verdict the reader cannot give is drawn
 * held with its reason: the change's own author's, above all.
 *
 * ONE PRIMARY ACTION STAYS IN VIEW ON A PHONE: Ready for review on a draft,
 * Request review for the change's author, Approve otherwise. Every other write
 * is marked `page-action-folds`, which the bar hides at a phone's width, and
 * published to its More menu (`usePageMenu`), held in the same words as the
 * inline control.
 */
function ChangeActions({ change }: { change: CodeChangeDetail }) {
  const [verdict, setVerdict] = useState<CodeVerdict | null>(null);
  const [asking, setAsking] = useState(false);
  const [closing, setClosing] = useState(false);
  const review = useAct("code_review_change");
  const ask = useAct("code_request_review");
  const close = useAct("code_close_change");
  const update = useAct("code_update_change");
  const viewer = useViewer();
  const live = changeLive(change.state);
  const draft = change.state === "draft";
  const reopenable = change.state === "closed";
  const own = authoredBy(change, viewer);
  const verdictHeld = own ? OWN_CHANGE_HELD : undefined;
  const markReady = () =>
    void update.run(
      { change: change.ref, expected_version: change.version, draft: false },
      { done: `Marked ${change.ref} ready for review` },
    );
  const held = (access: WriteAccess, blocked: string | undefined) =>
    access.can
      ? blocked === undefined
        ? {}
        : { disabled: true, description: blocked }
      : menuHold(access);
  // WHICH WRITE LEADS THE BAR, the one kept in view on a phone.
  const lead: "ready" | "ask" | "approve" = draft ? "ready" : own ? "ask" : "approve";
  // THE LEAD STAYS IN VIEW on a phone; every other write folds into the menu.
  const FOLDS = "page-action-folds";
  const askFolds = lead === "ask" ? undefined : FOLDS;
  const approveFolds = lead === "approve" ? undefined : FOLDS;
  usePageMenu(
    live
      ? [
          ...(lead !== "approve"
            ? [
                {
                  key: "approve",
                  label: "Approve",
                  onSelect: () => setVerdict("approve"),
                  ...held(review.access, verdictHeld),
                },
              ]
            : []),
          {
            key: "request_changes",
            label: "Request changes",
            onSelect: () => setVerdict("request_changes"),
            ...held(review.access, verdictHeld),
          },
          {
            key: "comment",
            label: "Comment",
            onSelect: () => setVerdict("comment"),
            ...held(review.access, verdictHeld),
          },
          ...(lead !== "ask"
            ? [
                {
                  key: "ask",
                  label: "Request review",
                  onSelect: () => setAsking(true),
                  ...held(ask.access, undefined),
                },
              ]
            : []),
          {
            key: "close",
            label: "Close",
            onSelect: () => setClosing(true),
            ...held(close.access, undefined),
          },
        ]
      : reopenable
        ? [
            {
              key: "close",
              label: "Reopen",
              onSelect: () => setClosing(true),
              ...held(close.access, undefined),
            },
          ]
        : [],
  );
  if (!live) {
    // A FINISHED CHANGE: a closed one is reopened, and a merged one is
    // reverted from where its merge controls were.
    return reopenable ? (
      <>
        <WriteButton
          write={close}
          size="small"
          variant="secondary"
          showRefusal={false}
          onPress={() => setClosing(true)}
        >
          Reopen
        </WriteButton>
        {closing && <CloseDialog change={change} reopen onClose={() => setClosing(false)} />}
      </>
    ) : null;
  }
  // REQUEST REVIEW leads an author's bar, and follows the verdicts on anybody
  // else's.
  const askButton = (
    <span className={askFolds}>
      <WriteButton
        write={ask}
        size="small"
        variant={lead === "ask" ? "primary" : "secondary"}
        showRefusal={false}
        onPress={() => setAsking(true)}
      >
        Request review
      </WriteButton>
    </span>
  );
  return (
    <>
      {draft && (
        <WriteButton write={update} size="small" variant="primary" onPress={markReady}>
          Ready for review
        </WriteButton>
      )}
      {lead === "ask" && askButton}
      <span className={approveFolds}>
        <WriteButton
          write={review}
          size="small"
          // A DRAFT'S STEP IS BEING MARKED READY, AND AN AUTHOR'S IS ASKING:
          // a verdict there is offered, never the page's primary action.
          variant={lead === "approve" ? "primary" : "secondary"}
          blocked={verdictHeld}
          showRefusal={false}
          onPress={() => setVerdict("approve")}
        >
          Approve
        </WriteButton>
      </span>
      <span className="page-action-folds">
        <WriteButton
          write={review}
          size="small"
          variant="secondary"
          blocked={verdictHeld}
          showRefusal={false}
          onPress={() => setVerdict("request_changes")}
        >
          Request changes
        </WriteButton>
      </span>
      <span className="page-action-folds">
        <WriteButton
          write={review}
          size="small"
          variant="secondary"
          blocked={verdictHeld}
          showRefusal={false}
          onPress={() => setVerdict("comment")}
        >
          Comment
        </WriteButton>
      </span>
      {lead !== "ask" && askButton}
      <span className="page-action-folds">
        <WriteButton
          write={close}
          size="small"
          variant="ghost"
          showRefusal={false}
          onPress={() => setClosing(true)}
        >
          Close
        </WriteButton>
      </span>
      {verdict && (
        <ReviewDialog change={change} verdict={verdict} onClose={() => setVerdict(null)} />
      )}
      {asking && <AskDialog change={change} onClose={() => setAsking(false)} />}
      {closing && <CloseDialog change={change} reopen={false} onClose={() => setClosing(false)} />}
    </>
  );
}

/** What each verdict's dialog is called, and what its press says it did. */
const VERDICT_TITLES: Readonly<Record<string, { title: string; done: string }>> = {
  approve: { title: "Approve", done: "Approved" },
  request_changes: { title: "Request changes", done: "Asked for changes on" },
  comment: { title: "Comment", done: "Reviewed" },
};

/**
 * One review of the change's head revision: its verdict, a summary, every
 * finding drafted on that revision's lines (`lib/reviewDrafts.ts`), and the
 * reader's own standing findings it CARRIES — checked by default, since a
 * review of a later revision supersedes every standing finding of the
 * reviewer's it does not carry (S40), and that is a choice made by unchecking
 * one, never by forgetting it. So the press is HELD until the dialog holds
 * every one of them (`useStandingFindings`, read to its last page): a review
 * sent while that read is out, or after it was refused, would carry none it
 * had not read and supersede the rest unseen. Sent as ONE
 * `code_review_change`; the drafts are forgotten only once the engine has
 * taken the review.
 */
function ReviewDialog({
  change,
  verdict,
  onClose,
}: {
  change: CodeChangeDetail;
  verdict: CodeVerdict;
  onClose: () => void;
}) {
  const write = useAct("code_review_change");
  const viewer = useViewer();
  const [summary, setSummary] = useState("");
  const drafts = readDrafts(change.id, change.revision);
  const elsewhere = draftedRevisions(change.id).filter((d) => d.revision !== change.revision);
  const read = useStandingFindings(change, viewer.owner);
  const standing = read.findings ?? [];
  const [dropped, setDropped] = useState<ReadonlySet<string>>(new Set());
  const carries = standing.filter((f) => !dropped.has(f.id)).map((f) => f.id);
  const words = VERDICT_TITLES[verdict] ?? { title: "Review", done: "Reviewed" };
  const held =
    read.findings !== null
      ? undefined
      : read.error === null
        ? "Reading your standing findings…"
        : "Your standing findings were not read: a review sent now would supersede those it leaves out.";
  const submit = async () => {
    // HELD HERE TOO, where Enter reaches it past the button.
    if (held !== undefined) return;
    const result = await write.run(
      {
        change: change.ref,
        revision: change.revision,
        verdict,
        ...(summary.trim() ? { summary } : {}),
        ...(drafts.length > 0 ? { findings: drafts.map(findingOf) } : {}),
        ...(carries.length > 0 ? { carries } : {}),
      },
      { done: `${words.done} ${change.ref}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      // FORGOTTEN ONCE TAKEN: an unknown answer keeps them, for the Retry
      // the toast offers sends the same review again.
      forgetDrafts(change.id, change.revision);
      onClose();
    }
  };
  return (
    <Modal
      open
      size="md"
      title={`${words.title} ${change.ref}`}
      subtitle={`Revision ${change.revision}`}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton write={write} variant="primary" blocked={held} onPress={() => void submit()}>
            {words.title}
          </WriteButton>
        </>
      }
    >
      <Textarea
        aria-label="Summary"
        placeholder="What you found, in a sentence or two"
        value={summary}
        rows={4}
        onChange={(event) => setSummary(event.target.value)}
      />
      <p className="muted">
        {drafts.length === 0
          ? "No finding is drafted on this revision's lines."
          : `${drafts.length} drafted finding${drafts.length === 1 ? "" : "s"} on revision ${change.revision} will be raised with it.`}
        {elsewhere.length > 0 &&
          ` Drafts on revision ${elsewhere.map((d) => d.revision).join(", ")} stay in this tab: a review raises findings on the revision it judges.`}
      </p>
      {read.error !== null ? (
        <Stack gap={2}>
          <QueryState
            error={read.error}
            refusal={read.refusal}
            {...(read.detail ? { detail: read.detail } : {})}
            loading={false}
          >
            {null}
          </QueryState>
          <p className="muted">
            {held}{" "}
            <Button size="small" variant="secondary" onClick={read.retry}>
              Read them again
            </Button>
          </p>
        </Stack>
      ) : (
        held !== undefined && <p className="muted">{held}</p>
      )}
      {standing.length > 0 && (
        <fieldset className="code-carries">
          <legend>Your standing findings it keeps standing</legend>
          {standing.map((f) => (
            <label key={f.id} className="row gap-1">
              <input
                type="checkbox"
                checked={!dropped.has(f.id)}
                onChange={(event) =>
                  setDropped((was) => {
                    const next = new Set(was);
                    if (event.target.checked) next.delete(f.id);
                    else next.add(f.id);
                    return next;
                  })
                }
              />
              {f.title} <span className="muted mono">{f.path}</span>
            </label>
          ))}
        </fieldset>
      )}
    </Modal>
  );
}

/**
 * Ask reviewers, picked from the chart (`ReviewerPicker`), with a note. The
 * change's authors are never offered: an author's review counts for nothing,
 * and the engine refuses asking one.
 */
function AskDialog({ change, onClose }: { change: CodeChangeDetail; onClose: () => void }) {
  const write = useAct("code_request_review");
  const [reviewers, setReviewers] = useState<string[]>([]);
  const [note, setNote] = useState("");
  const authors = useMemo(() => change.authors.map((a) => a.author), [change.authors]);
  const submit = async () => {
    const result = await write.run(
      { change: change.ref, reviewers, ...(note.trim() ? { note } : {}) },
      { done: `Asked for a review of ${change.ref}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Request review of ${change.ref}`}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            blocked={reviewers.length === 0 ? "Pick a reviewer to ask." : undefined}
            onPress={() => void submit()}
          >
            Ask
          </WriteButton>
        </>
      }
    >
      <Stack gap={3}>
        <FormField label="Reviewers">
          {(field) => (
            <ReviewerPicker
              id={field.id}
              describedBy={field.describedBy}
              value={reviewers}
              onChange={setReviewers}
              exclude={authors}
            />
          )}
        </FormField>
        <FormField label="Note" optional helper="What to look at.">
          {(field) => (
            <Textarea
              id={field.id}
              aria-describedby={field.describedBy}
              value={note}
              rows={3}
              onChange={(event) => setNote(event.target.value)}
            />
          )}
        </FormField>
      </Stack>
    </Modal>
  );
}

/** Close the change, or reopen a closed one, against the version on screen. */
function CloseDialog({
  change,
  reopen,
  onClose,
}: {
  change: CodeChangeDetail;
  reopen: boolean;
  onClose: () => void;
}) {
  const write = useAct("code_close_change");
  const [reason, setReason] = useState("");
  const submit = async () => {
    const result = await write.run(
      {
        change: change.ref,
        expected_version: change.version,
        ...(reopen ? { reopen: true } : reason.trim() ? { reason } : {}),
      },
      { done: `${reopen ? "Reopened" : "Closed"} ${changeRef(change.repo, change.number)}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`${reopen ? "Reopen" : "Close"} ${change.ref}`}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton write={write} variant="primary" onPress={() => void submit()}>
            {reopen ? "Reopen" : "Close"}
          </WriteButton>
        </>
      }
    >
      {reopen ? (
        <p>Its source branch must still be live, and no other change open on it.</p>
      ) : (
        <Textarea
          aria-label="Reason"
          placeholder="Why it is closed (optional)"
          value={reason}
          rows={3}
          onChange={(event) => setReason(event.target.value)}
        />
      )}
    </Modal>
  );
}
