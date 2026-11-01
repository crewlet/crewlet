/**
 * One change's page, `#/code/repos/{name}/changes/{n}` — `web!42` in full:
 * what it proposes, where its merge stands, the conversation about it, its
 * diff with every finding and thread on the lines they are about, its
 * revisions, where it came from and whom its records woke.
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
 * reply and Resolve go through `code_comment_on_change`. Each goes through
 * `useAct` and a `WriteButton`: drawn for every reader, disabled with the
 * reason where it cannot act, and NEVER OPTIMISTIC — nothing on screen moves
 * until the engine answers, and the answer's position is the floor the page
 * is read at next.
 */

import { useId, useState } from "react";
import { Button, EmptyState, Input, Modal, Skeleton, Tabs, Textarea, tabId } from "@crewlethq/ui";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { QueryState } from "~/components/common.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import { changeLive, changeRef, isRepoName, refName, repoPath } from "~/lib/code.ts";
import { draftedRevisions, findingOf, forgetDrafts, readDrafts } from "~/lib/reviewDrafts.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useAct } from "~/lib/useAct.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeChangeDetail, CodeVerdict } from "~/protocol/index.ts";
import { ChangeStateTag } from "../Changes.tsx";
import { Checks } from "./Checks.tsx";
import { Conversation } from "./Conversation.tsx";
import { Files } from "./Files.tsx";
import { Commits, Provenance, Woke } from "./Facts.tsx";
import { CHANGE_POLL_MS, useStandingFindings } from "./reads.ts";

/** The page's tabs, the conversation first: what a reader opens a change for. */
const CHANGE_TABS = ["conversation", "files", "commits", "checks", "provenance", "woke"] as const;
type ChangeTab = (typeof CHANGE_TABS)[number];

const CHANGE_TAB_LABELS: Readonly<Record<ChangeTab, string>> = {
  conversation: "Conversation",
  files: "Files",
  commits: "Commits",
  checks: "Checks",
  provenance: "Provenance",
  woke: "Woke",
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
    return (
      <QueryState error={read.error} refusal={read.refusal} detail={read.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  return (
    <>
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
        {tab === "files" && <Files change={change} />}
        {tab === "commits" && <Commits change={change} revisions={read.data?.revisions ?? []} />}
        {tab === "checks" && <Checks change={change} />}
        {tab === "provenance" && <Provenance change={change} />}
        {tab === "woke" && <Woke change={change} />}
      </div>
    </>
  );
}

/** The change's header: its reference, title, state and the facts a reader places it by. */
function ChangeHeader({ change }: { change: CodeChangeDetail }) {
  const seatBadge = useSeatBadgeOf();
  const work = change.work_item;
  const facts: Fact[] = [
    {
      label: "Repository",
      value: change.repo,
      path: repoPath(change.repo),
    },
    {
      label: "Branches",
      value: (
        <span className="mono">
          {refName(change.source_ref).name} → {refName(change.target_ref).name}
        </span>
      ),
      token: true,
    },
    {
      label: "Owner",
      value: seatBadge(change.owner).name,
      path: ["code"],
      query: { owner: change.owner },
    },
    { label: "Revisions", value: String(change.revision) },
    ...(work
      ? [
          {
            label: "Work item",
            value: work.key || work.id,
            path: ["code"],
            query: { work: `${work.backend}:${work.id}`, state: "all" },
          },
        ]
      : []),
  ];
  return (
    <ObjectHeader
      kind="Change"
      icon="diff"
      identifier={change.ref}
      title={change.title}
      status={<ChangeStateTag state={change.state} />}
      facts={facts}
    />
  );
}

/**
 * The page bar's writes: the three verdicts, an ask for review, and the
 * change's close or reopen. A change no longer live is reviewed by nobody and
 * asked of nobody; each button stays drawn, held with that reason.
 */
function ChangeActions({ change }: { change: CodeChangeDetail }) {
  const [verdict, setVerdict] = useState<CodeVerdict | null>(null);
  const [asking, setAsking] = useState(false);
  const [closing, setClosing] = useState(false);
  const review = useAct("code_review_change");
  const ask = useAct("code_request_review");
  const close = useAct("code_close_change");
  const live = changeLive(change.state);
  const ended = live ? undefined : `This change is ${change.state}: it takes no more reviews.`;
  const reopenable = change.state === "closed";
  return (
    <>
      <WriteButton
        write={review}
        size="small"
        variant="primary"
        blocked={ended}
        showRefusal={false}
        onPress={() => setVerdict("approve")}
      >
        Approve
      </WriteButton>
      <WriteButton
        write={review}
        size="small"
        variant="secondary"
        blocked={ended}
        showRefusal={false}
        onPress={() => setVerdict("request_changes")}
      >
        Request changes
      </WriteButton>
      <WriteButton
        write={review}
        size="small"
        variant="secondary"
        blocked={ended}
        showRefusal={false}
        onPress={() => setVerdict("comment")}
      >
        Comment
      </WriteButton>
      <WriteButton
        write={ask}
        size="small"
        variant="secondary"
        blocked={live ? undefined : `This change is ${change.state}: nobody is asked to review it.`}
        showRefusal={false}
        onPress={() => setAsking(true)}
      >
        Request review
      </WriteButton>
      <WriteButton
        write={close}
        size="small"
        variant="ghost"
        blocked={
          live || reopenable
            ? undefined
            : "A merged change is final: it is neither closed nor reopened."
        }
        showRefusal={false}
        onPress={() => setClosing(true)}
      >
        {reopenable ? "Reopen" : "Close"}
      </WriteButton>
      {verdict && (
        <ReviewDialog change={change} verdict={verdict} onClose={() => setVerdict(null)} />
      )}
      {asking && <AskDialog change={change} onClose={() => setAsking(false)} />}
      {closing && (
        <CloseDialog change={change} reopen={reopenable} onClose={() => setClosing(false)} />
      )}
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
 * one, never by forgetting it. Sent as ONE `code_review_change`; the drafts
 * are forgotten only once the engine has taken the review.
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
  const standing = useStandingFindings(change, viewer.owner);
  const [dropped, setDropped] = useState<ReadonlySet<string>>(new Set());
  const carries = standing.filter((f) => !dropped.has(f.id)).map((f) => f.id);
  const words = VERDICT_TITLES[verdict] ?? { title: "Review", done: "Reviewed" };
  const submit = async () => {
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
          <WriteButton write={write} variant="primary" onPress={() => void submit()}>
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

/** Ask reviewers — seats by their handles, or the human seat a person holds — with a note. */
function AskDialog({ change, onClose }: { change: CodeChangeDetail; onClose: () => void }) {
  const write = useAct("code_request_review");
  const [who, setWho] = useState("");
  const [note, setNote] = useState("");
  const reviewers = who
    .split(/[\s,]+/)
    .map((h) => h.trim().replace(/^@/, ""))
    .filter(Boolean);
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
            blocked={reviewers.length === 0 ? "Name a reviewer to ask." : undefined}
            onPress={() => void submit()}
          >
            Ask
          </WriteButton>
        </>
      }
    >
      <Input
        aria-label="Reviewers"
        placeholder="Handles, separated by commas"
        value={who}
        onChange={(event) => setWho(event.target.value)}
      />
      <Textarea
        aria-label="Note"
        placeholder="What to look at (optional)"
        value={note}
        rows={3}
        onChange={(event) => setNote(event.target.value)}
      />
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
