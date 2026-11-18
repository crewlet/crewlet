/**
 * One change's page, `#/gitlet/repos/{name}/changes/{n}` — `web!42` in full,
 * laid out as a code host lays one out: a header saying who wants to merge
 * which revision of which branch into which, then the conversation about it
 * with where its merge stands (its merge box) and a rail of whom it asked, its
 * revisions, its checks, its diff with every finding and thread on the lines
 * they are about, and More: where it came from and whom its records notified.
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
 * none (`own_change`), where Request review is the author's step.
 */

import { useId, useMemo, useState, type ReactNode } from "react";
import {
  Button,
  EmptyState,
  FormField,
  Menu,
  Skeleton,
  Stack,
  Tabs,
  Tag,
  type TabsMarker,
  Textarea,
  tabId,
} from "@crewlethq/ui";
import {
  ChevronDownGlyph,
  EllipsisGlyph,
  FileDiffGlyph,
  GitCommitVerticalGlyph,
  GitPullRequestClosedGlyph,
  ListChecksGlyph,
  MessageSquareGlyph,
  SquareKanbanGlyph,
  UserPlusGlyph,
} from "@crewlethq/icons/glyphs";
import { Modal } from "~/components/Modal.tsx";
import { DraftBoxes, DraftBoxesContext, boxesDraft, useOpenBoxes } from "~/app/assistant/boxes.ts";
import { useAssistantDraft } from "~/app/assistant/draft.ts";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePageMenu } from "~/app/Shell.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { QueryState } from "~/components/common.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import { ago } from "~/lib/ago.ts";
import { changeLive, changeRef, isRepoName, refName } from "~/lib/code.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, plural } from "~/lib/format.ts";
import { useAct } from "~/lib/useAct.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type {
  CodeChangeDetail,
  CodeCheckRow,
  CodeRevisionRow,
  CodeVerdict,
} from "~/protocol/index.ts";
import { ChangeStateTag } from "../ChangeRow.tsx";
import { Diffstat } from "../Diffstat.tsx";
import { CodeNotFound } from "../NotFound.tsx";
import { CHANGE_TABS, changesHref, repoHref, type ChangeTab } from "../paths.ts";
import { BranchChip, Byline } from "../ui.tsx";
import { AskDialog } from "./AskDialog.tsx";
import { Checks } from "./Checks.tsx";
import { CopyButton } from "./CopyButton.tsx";
import { Conversation } from "./Conversation.tsx";
import { Files } from "./Files.tsx";
import { Notified, Provenance, Revisions } from "./Facts.tsx";
import { ReviewDialog, heldItem, verdictItems } from "./ReviewDialog.tsx";
import {
  CHANGE_POLL_MS,
  changePartParams,
  revisionInputs,
  useAskAgainWhenMoved,
  useLatestChecks,
  type LatestChecks,
} from "./reads.ts";
import "~/styles/gitlet.css";

/** Each tab's glyph: decorative, its label names it. */
const CHANGE_TAB_GLYPHS: Readonly<Record<ChangeTab, ReactNode>> = {
  conversation: <MessageSquareGlyph size="sm" />,
  revisions: <GitCommitVerticalGlyph size="sm" />,
  checks: <ListChecksGlyph size="sm" />,
  files: <FileDiffGlyph size="sm" />,
  more: <EllipsisGlyph size="sm" />,
};

const CHANGE_TAB_LABELS: Readonly<Record<ChangeTab, string>> = {
  conversation: "Conversation",
  revisions: "Revisions",
  checks: "Checks",
  files: "Files changed",
  more: "More",
};

/** The phone's fold of the tabs, worded apart from the More tab. */
const OVERFLOW_LABEL = "Other tabs";

/** More, folded into the phone's Other tabs menu, says what it holds. */
const MORE_MENU_LABEL = "Where it came from and who was notified";

/**
 * The head revision's diff totals, the Files changed tab's own first question,
 * asked again on the tab's own key: the version, the head and its facts' base.
 */
function useHeadDiff(change: CodeChangeDetail, revisions: readonly CodeRevisionRow[]) {
  const { repo, number, revision } = change;
  const params = useMemo(
    () => changePartParams({ repo, number }, "files", { revision }),
    [repo, number, revision],
  );
  const read = useCodeQuery("code_change", params);
  useAskAgainWhenMoved(
    `${change.version}/${revisionInputs(revisions, revision)}`,
    true,
    read.refetch,
  );
  return read.data?.diff;
}

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
  // ONE READ of the head's latest checks: the merge box, the rail and the Checks tab's count.
  const checks = useLatestChecks(read.data?.change);
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
  const revisions = read.data?.revisions ?? [];
  return (
    <DraftBoxesContext.Provider value={boxes}>
      <ChangeDraft boxes={boxes} change={change} />
      <PageActions>
        <ChangeActions change={change} />
      </PageActions>
      <header>
        <ChangeHeader change={change} />
        <ChangeTabs
          change={change}
          revisions={revisions}
          checks={checks}
          tab={tab}
          setTab={setTab}
          panelId={panelId}
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
          <Conversation change={change} revisions={revisions} checks={checks} />
        )}
        {tab === "revisions" && <Revisions change={change} revisions={revisions} />}
        {tab === "checks" && <Checks change={change} />}
        {tab === "files" && <Files change={change} revisions={revisions} />}
        {tab === "more" && (
          <Stack gap={5}>
            <section aria-labelledby={`${panelId}-provenance`} className="code-more">
              <h2 id={`${panelId}-provenance`} className="code-more__title">
                Where it came from
              </h2>
              <Provenance change={change} />
            </section>
            <section aria-labelledby={`${panelId}-notified`} className="code-more">
              <h2 id={`${panelId}-notified`} className="code-more__title">
                Whom its records notified
              </h2>
              <Notified change={change} />
            </section>
          </Stack>
        )}
      </div>
    </DraftBoxesContext.Provider>
  );
}

/** The Checks tab's marker: a count of the failed checks, or that every one passed. */
export function checksMarker(rows: readonly CodeCheckRow[]): TabsMarker | undefined {
  const failed = rows.filter((r) => r.state === "failed" || r.state === "errored").length;
  if (failed > 0)
    return { tone: "danger", count: failed, label: `${plural(failed, "check")} failed` };
  if (rows.every((r) => r.state === "passed" || r.state === "skipped")) {
    return { tone: "success", label: "Every check passed" };
  }
  return undefined;
}

/** The tab row: each tab's count where one is known, and the head's diffstat at its end. */
function ChangeTabs({
  change,
  revisions,
  checks,
  tab,
  setTab,
  panelId,
}: {
  change: CodeChangeDetail;
  revisions: readonly CodeRevisionRow[];
  checks: LatestChecks;
  tab: ChangeTab;
  setTab: (next: ChangeTab) => void;
  panelId: string;
}) {
  const diff = useHeadDiff(change, revisions);
  const checked = checks.rows !== null && checks.rows.length > 0 ? checks.rows : null;
  const counts: Partial<Record<ChangeTab, number>> = {
    ...(change.conversation ? { conversation: change.conversation.comments } : {}),
    revisions: change.revision,
    ...(checked ? { checks: checked.length } : {}),
    ...(diff ? { files: diff.total } : {}),
  };
  const marker = checked ? checksMarker(checked) : undefined;
  return (
    <Tabs
      overflow="menu"
      // THE FOLD'S TRIGGER is never named as the More tab beside it.
      moreLabel={OVERFLOW_LABEL}
      variant="underline"
      ariaLabel="The change's sections"
      value={tab}
      onValueChange={(next) => setTab(next as ChangeTab)}
      panelId={panelId}
      note={
        diff ? (
          <Diffstat
            added={diff.added}
            deleted={diff.deleted}
            label={`in revision ${change.revision}`}
          />
        ) : undefined
      }
      items={CHANGE_TABS.map((value) => ({
        value,
        label: value === "more" ? <span>{CHANGE_TAB_LABELS.more}</span> : CHANGE_TAB_LABELS[value],
        ...(value === "more" ? { menuLabel: MORE_MENU_LABEL } : {}),
        icon: CHANGE_TAB_GLYPHS[value],
        ...(counts[value] !== undefined ? { count: counts[value] } : {}),
        ...(value === "checks" && marker ? { marker } : {}),
      }))}
    />
  );
}

/**
 * The change page's ONE DRAFT for the person's own assistant: every box open
 * on it (`app/assistant/boxes.ts`), the comment on the change, each reply box
 * of a thread or a finding, its title and description being edited, and a
 * finding being written on picked lines. The review, ask and close dialogs
 * are modal, and the drawer is inert while one holds the page, so none of
 * their fields is among them.
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
        "box of each thread and finding, its title and description being edited, and a " +
        "finding being written on picked lines. You can propose text for any of them; " +
        "sending, saving and adding a finding to the review stay the person's.",
    }),
  );
  return null;
}

/**
 * What the change's byline says it does, as a code host's does: who wants to
 * merge which revision of which branch into which — or merged it, or closed
 * it.
 */
function bylineVerb(change: CodeChangeDetail): ReactNode {
  const source = refName(change.source_ref).name;
  const target = refName(change.target_ref).name;
  // EACH BRANCH A LINK IN THE SENTENCE, the source's name one press from the clipboard.
  const branches = (
    <>
      <span className="code-change-byline__source">
        <BranchChip name={source} href={repoHref(change.repo, { ref: source })} />
        <CopyButton text={source} label="Copy the branch name" />
      </span>{" "}
      into <BranchChip name={target} href={repoHref(change.repo)} />
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

/** When the byline's act happened, and the word before it on a live change. */
function bylineAt(change: CodeChangeDetail): { at: string; opened: boolean } {
  if (change.state === "merged") return { at: change.merged_at ?? change.opened_at, opened: false };
  if (change.state === "closed") return { at: change.closed_at ?? change.opened_at, opened: false };
  return { at: change.opened_at, opened: true };
}

/**
 * The change's header: its reference and title, then one sentence: its state pill, who wants to merge which revision of
 * which branch into which, when, and its work item.
 */
function ChangeHeader({ change }: { change: CodeChangeDetail }) {
  const now = useNow();
  const actor = change.state === "merged" && change.merged_by ? change.merged_by : change.owner;
  const work = change.work_item;
  const when = bylineAt(change);
  return (
    <ObjectHeader
      kind="Change"
      icon="diff"
      identifier={change.ref}
      title={change.title}
      description={
        <span className="code-change-byline">
          <span className="code-change-byline__state">
            <ChangeStateTag state={change.state} />
          </span>
          <Byline who={actor} verb={bylineVerb(change)} now={now}>
            <span className="code-change-byline__at">
              {when.opened ? "opened " : ""}
              <time className="code-byline__when" dateTime={when.at} title={fmtDateTime(when.at)}>
                {ago(when.at, now, { clock: true })}
              </time>
            </span>
            {work && (
              <a
                className="code-change-byline__work"
                href={changesHref({ work: `${work.backend}:${work.id}`, state: "all" })}
              >
                <Tag appearance="outline" size="sm" leadingIcon={<SquareKanbanGlyph size="xs" />}>
                  {work.key || work.id}
                </Tag>
              </a>
            )}
          </Byline>
        </span>
      }
    />
  );
}

/**
 * The page bar, as the change's state leaves it: on a LIVE change Close change
 * and a Review menu (the verdicts and Request review, each held with its
 * reason), a draft's Ready for review before them; a closed one's Reopen; none
 * on a merged one. Every control is a default button: the merge box holds the
 * page's one primary. Close change folds into the bar's More on a phone.
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
  const markReady = () =>
    void update.run(
      { change: change.ref, expected_version: change.version, draft: false },
      { done: `Marked ${change.ref} ready for review` },
    );
  usePageMenu(
    live
      ? [
          {
            key: "close",
            label: "Close change",
            onSelect: () => setClosing(true),
            ...heldItem(close.access, undefined),
          },
        ]
      : [],
  );
  if (!live) {
    // A FINISHED CHANGE: a closed one is reopened, a merged one reverted from its merge box.
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
  return (
    <>
      {draft && (
        <WriteButton write={update} size="small" variant="secondary" onPress={markReady}>
          Ready for review
        </WriteButton>
      )}
      <span className="page-action-folds">
        <WriteButton
          write={close}
          size="small"
          variant="secondary"
          leadingIcon={<GitPullRequestClosedGlyph size="sm" />}
          showRefusal={false}
          onPress={() => setClosing(true)}
        >
          Close change
        </WriteButton>
      </span>
      <Menu
        label={`Review ${change.ref}`}
        trigger="Review"
        trailingIcon={<ChevronDownGlyph size="sm" />}
        triggerVariant="secondary"
        align="end"
        items={[
          ...verdictItems(change, review.access, viewer, setVerdict),
          { kind: "separator", key: "asks" },
          {
            key: "ask",
            label: "Request review",
            icon: <UserPlusGlyph size="sm" />,
            onSelect: () => setAsking(true),
            ...heldItem(ask.access, undefined),
          },
        ]}
      />
      {verdict && (
        <ReviewDialog change={change} verdict={verdict} onClose={() => setVerdict(null)} />
      )}
      {asking && <AskDialog change={change} onClose={() => setAsking(false)} />}
      {closing && <CloseDialog change={change} reopen={false} onClose={() => setClosing(false)} />}
    </>
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
        <FormField label="Reason" optional helper="Kept with the close, and shown on the change.">
          {(field) => (
            <Textarea
              id={field.id}
              aria-describedby={field.describedBy}
              value={reason}
              rows={3}
              onChange={(event) => setReason(event.target.value)}
            />
          )}
        </FormField>
      )}
    </Modal>
  );
}
