/**
 * Two revisions of a repository compared, `#/code/repos/{name}/compare?base=&head=`:
 * how they relate, what merging head into base would do, what head changed
 * since the two parted — and Open change, which proposes head into base.
 *
 * # One comparison, the merge its home would make
 *
 * `code_compare` resolves both revisions in one read at the caller's
 * freshness and answers how the two commits relate — their merge base, which
 * contains which, how many commits each has that the other lacks — and the
 * merge preview, asked of the repository's home git node BY THE TWO WHOLE IDS
 * the comparison answered, so the preview merges exactly the commits compared,
 * by the one merge a change's mergeability is recorded by. What head changed
 * is read from the merge base to head, both whole, so it is immutable: the
 * three-dot reading every code host shows.
 *
 * # Open change, prefilled and never guessed at
 *
 * A change proposes a BRANCH into a branch, so Open change is offered where
 * head is a branch with a commit base lacks and base is a branch — held, with
 * the reason, everywhere else, and where a live change already proposes the
 * commit head stands at. Its title is head's subject and its work item the
 * one the engine stamped on the push that introduced head's commit, where a
 * seat's turn was charged to one: the work the commit was made for, which the
 * reader may change or clear before the one `code_open_change` is sent.
 * Nothing moves until the engine answers, and an applied open leads to the
 * change it opened.
 */

import { useState } from "react";
import {
  Button,
  Callout,
  EmptyState,
  Input,
  Modal,
  Skeleton,
  Stack,
  Tag,
  Textarea,
} from "@crewlethq/ui";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import { changeLive, changePath, isRepoName, parseChangeRef, shortOID } from "~/lib/code.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { useAct } from "~/lib/useAct.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type {
  CodeCommitAnswer,
  CodeCompareFacts,
  CodeComparisonAnswer,
  CodeMergePreview,
  CodeRepoDetail,
} from "~/protocol/index.ts";
import { commitHref, repoHref } from "../paths.ts";
import { subjectOf } from "../repo/History.tsx";
import { RefPicker, revisionWord } from "../repo/refs.tsx";
import { DiffFiles } from "./DiffFiles.tsx";

/** The default branch, as a revision a comparison asks the engine for. */
const DEFAULT_REV = "HEAD";

/** Where a branch lives, whole: what a resolved ref is told a branch by. */
const BRANCH_PREFIX = "refs/heads/";

export function ComparePage({ repo }: { repo: string }) {
  const named = isRepoName(repo);
  const read = useCodeQuery("code_repo", { name: repo }, { enabled: named });
  const [base, setBase] = useParam("base", "", "filter");
  const [head, setHead] = useParam("head", "", "filter");
  if (!named) {
    return (
      <EmptyState
        title="Not a repository"
        description={`“${repo}” is not a name a repository can have.`}
      />
    );
  }
  const detail = read.data?.repo;
  if (read.loading && !detail)
    return <Skeleton variant="text" rows={6} label={`Loading ${repo}`} />;
  if (!detail) {
    return (
      <QueryState error={read.error} refusal={read.refusal} detail={read.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  return (
    <Stack gap={4}>
      <div className="toolbar">
        <RefPicker repo={detail} value={base} onChoose={setBase} label="Base" />
        <span className="muted" aria-hidden="true">
          ←
        </span>
        <RefPicker
          repo={detail}
          value={head}
          onChoose={setHead}
          label="Compared"
          unset="Choose what to compare"
        />
        <Button
          size="small"
          variant="ghost"
          disabled={head === ""}
          onClick={() => {
            // A SWAP NAMES THE DEFAULT BRANCH WHERE IT WAS LEFT UNNAMED: the
            // head cannot be left unset, which no comparison asks.
            setBase(head);
            setHead(base || detail.default_branch);
          }}
        >
          Swap
        </Button>
      </div>
      {head === "" ? (
        <EmptyState
          title="Choose what to compare"
          description={`Pick a branch, a tag or a commit to compare with ${revisionWord(detail, base)}: what it changed since the two parted, and whether it merges.`}
        />
      ) : (
        <Comparison repo={detail} base={base} head={head} />
      )}
    </Stack>
  );
}

/** One comparison, read and drawn. */
function Comparison({ repo, base, head }: { repo: CodeRepoDetail; base: string; head: string }) {
  const read = useCodeQuery("code_compare", {
    name: repo.name,
    base: base || DEFAULT_REV,
    head,
  });
  const facts = read.data?.compare;
  // THE COMMIT HEAD STANDS AT, read for the push that introduced it — the
  // work item a change of it is opened for — and the changes it already
  // heads a revision of.
  const tip = useCodeQuery(
    "code_commit",
    { name: repo.name, sha: facts?.head ?? "" },
    { enabled: Boolean(facts?.head) },
  );
  if (read.loading && !read.data) return <Skeleton variant="text" rows={6} label="Comparing" />;
  if (!read.data || !facts) {
    return (
      <QueryState error={read.error} refusal={read.refusal} detail={read.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  const baseWord = revisionWord(repo, base);
  const headWord = revisionWord(repo, head);
  const headFacts: Fact[] = [
    {
      label: "Base",
      value: <span className="mono">{shortOID(facts.base)}</span>,
      path: ["code", "repos", repo.name, "commits", facts.base],
      token: true,
    },
    {
      label: "Compared",
      value: <span className="mono">{shortOID(facts.head)}</span>,
      path: ["code", "repos", repo.name, "commits", facts.head],
      token: true,
    },
    { label: "Ahead", value: plural(facts.ahead, "commit") },
    { label: "Behind", value: plural(facts.behind, "commit") },
    {
      label: "Merge base",
      value: facts.merge_base ? (
        <span className="mono">{shortOID(facts.merge_base)}</span>
      ) : (
        "None: the histories share no commit"
      ),
      ...(facts.merge_base
        ? { path: ["code", "repos", repo.name, "commits", facts.merge_base] }
        : {}),
      token: true,
    },
  ];
  return (
    <Stack gap={4}>
      <PageActions>
        <OpenChange
          repo={repo}
          answer={read.data}
          facts={facts}
          tip={tip.data}
          reading={tip.loading}
        />
      </PageActions>
      <ObjectHeader
        kind="Comparison"
        icon="diff"
        identifier={repo.name}
        title={`${headWord} into ${baseWord}`}
        status={<PreviewTag preview={read.data.preview} />}
        facts={headFacts}
      />
      <Relation
        repo={repo}
        base={baseWord}
        head={headWord}
        headRev={head}
        facts={facts}
        preview={read.data.preview}
      />
      {facts.merge_base ? (
        facts.ahead === 0 ? (
          <p className="muted">{`${headWord} has no commit ${baseWord} lacks: it changed nothing since the two parted.`}</p>
        ) : (
          <section aria-label="What it changed">
            <p className="t-label">
              {`What ${headWord} changed since ${shortOID(facts.merge_base)}: ${plural(facts.total, "file")}, +${fmtCount(facts.added)} −${fmtCount(facts.deleted)}`}
            </p>
            <DiffFiles repo={repo.name} base={facts.merge_base} head={facts.head} />
          </section>
        )
      ) : (
        <p className="muted">
          The two histories share no commit, so nothing is a change of one from the other.
        </p>
      )}
    </Stack>
  );
}

/** What merging head into base would do, as a state: the one place this page wears a tone. */
function PreviewTag({ preview }: { preview: CodeMergePreview }) {
  switch (preview.state) {
    case "clean":
      return <Tag variant="success">Merges cleanly</Tag>;
    case "conflict":
      return <Tag variant="danger">Conflicts</Tag>;
    case "unknown":
      return <Tag appearance="outline">Merge not decided</Tag>;
  }
  return <Tag appearance="outline">{preview.state}</Tag>;
}

/** How the two relate, in words, and the paths a merge of them conflicts on. */
function Relation({
  repo,
  base,
  head,
  headRev,
  facts,
  preview,
}: {
  repo: CodeRepoDetail;
  base: string;
  head: string;
  headRev: string;
  facts: CodeCompareFacts;
  preview: CodeMergePreview;
}) {
  const relation = facts.base_is_ancestor
    ? `${head} is ${plural(facts.ahead, "commit")} ahead of ${base}: a fast-forward.`
    : facts.head_is_ancestor
      ? `${head} is already in ${base}, which is ${plural(facts.behind, "commit")} ahead of it.`
      : `${head} is ${plural(facts.ahead, "commit")} ahead of ${base} and ${plural(facts.behind, "commit")} behind it.`;
  return (
    <Stack gap={2}>
      <p>
        {relation}{" "}
        <a className="prose-link" href={repoHref(repo.name, { tab: "commits", ref: headRev })}>
          {`History of ${head}`}
        </a>
      </p>
      {preview.state === "conflict" ? (
        <Callout variant="neutral">
          <Stack gap={1}>
            <span>{`Merging ${head} into ${base} conflicts in:`}</span>
            <ul className="code-rules">
              {(preview.conflicts ?? []).map((p) => (
                <li key={p} className="mono">
                  {p}
                </li>
              ))}
            </ul>
            {(preview.conflicts_omitted ?? 0) > 0 && (
              <span className="t-caption">{`and ${plural(preview.conflicts_omitted ?? 0, "more path")}`}</span>
            )}
          </Stack>
        </Callout>
      ) : preview.state === "unknown" ? (
        <p className="muted">
          Its home git node could not decide the merge within a read’s time; reading the comparison
          again may.
        </p>
      ) : null}
    </Stack>
  );
}

/** A branch a resolved revision names, by its name, or "" where it named none. */
function branchOf(answer: CodeComparisonAnswer, param: "base" | "head"): string {
  const ref = answer.resolved?.[param]?.ref ?? "";
  return ref.startsWith(BRANCH_PREFIX) ? ref.slice(BRANCH_PREFIX.length) : "";
}

/**
 * The work item the push that introduced a commit was charged to, as
 * `code_open_change` takes one (`<backend>:<id>`, the tracker's own key as the
 * id), or "" for a push charged to none.
 */
function workItemOf(tip: CodeCommitAnswer | null): string {
  const run = tip?.push?.run ?? tip?.pushed_by?.principal.run;
  if (!run?.work_key) return "";
  return run.work_backend ? `${run.work_backend}:${run.work_key}` : run.work_key;
}

/**
 * Open change: drawn for every reader and held, with the reason, wherever a
 * change of head into base cannot be opened.
 */
function OpenChange({
  repo,
  answer,
  facts,
  tip,
  reading,
}: {
  repo: CodeRepoDetail;
  answer: CodeComparisonAnswer;
  facts: CodeCompareFacts;
  tip: CodeCommitAnswer | null;
  /** The commit head stands at is still being read: what the open is prefilled from. */
  reading: boolean;
}) {
  const [open, setOpen] = useState(false);
  const write = useAct("code_open_change");
  const branch = branchOf(answer, "head");
  const target = branchOf(answer, "base");
  const proposing = (tip?.revisions ?? []).find((r) => changeLive(r.change.state));
  const proposed = proposing ? parseChangeRef(proposing.change.ref) : null;
  const blocked = !branch
    ? "A change proposes a branch: compare a branch rather than a tag or a commit."
    : !target
      ? "A change is proposed into a branch: choose a branch as the base."
      : branch === target
        ? "A branch is compared with itself."
        : facts.ahead === 0
          ? `${branch} has no commit ${target} lacks: there is nothing to propose.`
          : reading && !tip
            ? `Reading the commit ${branch} stands at.`
            : proposing
              ? `${proposing.change.ref} already proposes the commit ${branch} stands at.`
              : undefined;
  return (
    <>
      {proposing && proposed && (
        <a className="t-link" href={href(changePath(proposed.repo, proposed.number))}>
          {`Open ${proposing.change.ref} →`}
        </a>
      )}
      <WriteButton
        write={write}
        size="small"
        variant="primary"
        blocked={blocked}
        showRefusal={false}
        onPress={() => setOpen(true)}
      >
        Open change
      </WriteButton>
      {open && (
        <OpenChangeDialog
          repo={repo}
          branch={branch}
          target={target}
          title={tip ? subjectOf(tip.commit) : ""}
          workItem={workItemOf(tip)}
          head={facts.head}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  );
}

/**
 * The change, before it is opened: its title (head's subject), a description,
 * the work item (the one head's push was charged to), reviewers, and whether
 * it opens as a draft — sent as ONE `code_open_change`.
 */
function OpenChangeDialog({
  repo,
  branch,
  target,
  title: subject,
  workItem,
  head,
  onClose,
}: {
  repo: CodeRepoDetail;
  branch: string;
  target: string;
  title: string;
  workItem: string;
  head: string;
  onClose: () => void;
}) {
  const write = useAct("code_open_change");
  const nav = useNavigator();
  const [title, setTitle] = useState(subject);
  const [body, setBody] = useState("");
  const [work, setWork] = useState(workItem);
  const [who, setWho] = useState("");
  const [draft, setDraft] = useState(false);
  const reviewers = who
    .split(/[\s,]+/)
    .map((h) => h.trim().replace(/^@/, ""))
    .filter(Boolean);
  const submit = async () => {
    const result = await write.run(
      {
        repo: repo.name,
        branch,
        target,
        title: title.trim(),
        ...(body.trim() ? { body } : {}),
        ...(draft ? { draft: true } : {}),
        ...(reviewers.length > 0 ? { reviewers } : {}),
        ...(work.trim() ? { work_item: work.trim() } : {}),
      },
      { done: `Opened a change of ${branch} into ${target}` },
    );
    if (!result || (result.kind !== "applied" && result.kind !== "pending")) return;
    onClose();
    // AN APPLIED OPEN NAMES ITS CHANGE, and the reader is taken to it; a
    // pending one is numbered by the apply that opens it, which has not run.
    const ref = (result.receipt as { change?: unknown } | null)?.change;
    const at = typeof ref === "string" ? parseChangeRef(ref) : null;
    if (at) nav.to(changePath(at.repo, at.number));
  };
  return (
    <Modal
      open
      size="md"
      title={`Open a change of ${branch}`}
      subtitle={`Into ${target}, at ${shortOID(head)}`}
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
            blocked={title.trim() === "" ? "A change needs a title." : undefined}
            onPress={() => void submit()}
          >
            Open change
          </WriteButton>
        </>
      }
    >
      <Input
        aria-label="Title"
        placeholder="What the change does"
        value={title}
        onChange={(event) => setTitle(event.target.value)}
      />
      <Textarea
        aria-label="Description"
        placeholder="Why, and what to look at (optional)"
        value={body}
        rows={5}
        onChange={(event) => setBody(event.target.value)}
      />
      <Input
        aria-label="Work item"
        placeholder="ENG-42, or <tracker>:<id> (optional)"
        value={work}
        onChange={(event) => setWork(event.target.value)}
      />
      {workItem && work === workItem && (
        <p className="t-caption">
          The work item the push that brought {shortOID(head)} was charged to.
        </p>
      )}
      <Input
        aria-label="Reviewers"
        placeholder="Handles to ask, separated by commas (optional)"
        value={who}
        onChange={(event) => setWho(event.target.value)}
      />
      <label className="row gap-1">
        <input
          type="checkbox"
          checked={draft}
          onChange={(event) => setDraft(event.target.checked)}
        />
        Open it as a draft
      </label>
      <p className="t-caption">
        <a className="prose-link" href={commitHref(repo.name, head)}>
          {`Read ${shortOID(head)}`}
        </a>
      </p>
    </Modal>
  );
}
