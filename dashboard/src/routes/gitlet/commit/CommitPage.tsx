/**
 * One commit's page, `#/gitlet/repos/{name}/commits/{sha}`, by its WHOLE id —
 * the one spelling no push can move — laid out as a code host lays one out:
 * its subject with who wrote it and when, its parents and its whole id with
 * its copy; then how it arrived — who pushed it, how, and the turn, executor
 * and work item behind a seat's push — and the changes it belongs to, a card
 * each; then what it changed, the file tree beside each file's diff.
 *
 * # Whose it is, from the rows; what it is, from git
 *
 * `code_commit` answers the commit as the repository's history lists it,
 * read of the home Gitlet node, beside what the ROWS say of it: the push that
 * introduced it — by the Gitlet node's index of pushes, and that push's row
 * where the history still holds it, which names who pushed, the run behind a
 * seat's push and the refs it moved — and every change it belongs to, read at
 * the caller's freshness: the changes whose merge it is and the revisions it
 * heads. A commit's author line is what its writer said about it; who pushed
 * it is what the engine recorded, and the page says both.
 *
 * # Reverted, or picked onto a branch
 *
 * Under the header, Revert and Cherry-pick (`CommitWrites.tsx`): each one new
 * commit on a branch the person chooses, never a rewrite of this one.
 *
 * # Its diff is against its first parent
 *
 * As git shows a commit: a merge's change is what it brought into the branch
 * it was made on, and a root commit's is every file it holds, measured from
 * git's empty tree. The diff is read by both whole ids, so it is immutable.
 */

import { Fragment, type ReactNode } from "react";
import {
  Card,
  Copyable,
  EmptyState,
  EmptyValue,
  Skeleton,
  Stack,
  VisuallyHidden,
} from "@crewlethq/ui";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import {
  EMPTY_TREE,
  changePath,
  commitPath,
  isRepoName,
  parseChangeRef,
  repoPath,
  shortOID,
} from "~/lib/code.ts";
import { fmtDateTime, plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeChangeName, CodeCommitAnswer, CodePushRun } from "~/protocol/index.ts";
import { subjectOf } from "../repo/History.tsx";
import { ChangeStateTag } from "../ChangeRow.tsx";
import { CodeNotFound } from "../NotFound.tsx";
import { Byline, Face, RefName } from "../ui.tsx";
import { CommitWrites } from "./CommitWrites.tsx";
import { DiffFiles, type DiffAgainst } from "./DiffFiles.tsx";
import "~/styles/gitlet.css";

/** Every way a push's commits came to be, in words (`gitlet.PushVias`). */
const VIA_WORDS: Readonly<Record<string, string>> = {
  git: "a git push",
  engine: "a commit the engine built from a tool's call",
  merge: "a change merged",
  expiry: "a ref expired",
  queue: "a merge queue landed it",
  import: "imported from another host",
  mirror: "synced from the repository it mirrors",
};

export function CommitPage({ repo, sha }: { repo: string; sha: string }) {
  const named = isRepoName(repo);
  const read = useCodeQuery("code_commit", { name: repo, sha }, { enabled: named });
  if (!named) {
    return (
      <EmptyState
        title="Not a repository"
        description={`“${repo}” is not a name a repository can have.`}
      />
    );
  }
  const answer = read.data;
  if (read.loading && !answer) {
    return <Skeleton variant="text" rows={8} label={`Loading commit ${shortOID(sha)}`} />;
  }
  if (!answer) {
    if (read.error === "not_found" || read.error === "bad_params") {
      return (
        <CodeNotFound
          title={`No commit ${shortOID(sha)} in ${repo}`}
          description={read.detail ?? "The repository holds no commit by that id."}
          repo={repo}
        />
      );
    }
    return (
      <QueryState error={read.error} refusal={read.refusal} detail={read.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  const c = answer.commit;
  const parents = c.parents ?? [];
  // THE FIRST PARENT, or git's empty tree for a root commit; none where the
  // commit names more parents than a history lists (an octopus past its
  // bound), whose diff the page does not guess at.
  const base = parents[0] ?? (c.parent_count ? "" : EMPTY_TREE);
  return (
    <Stack gap={4}>
      <CommitHeader repo={repo} answer={answer} />
      <CommitWrites repo={repo} sha={c.oid} subject={subjectOf(c)} />
      <div className="code-commit-cards">
        <Pushed answer={answer} />
        <BelongsTo answer={answer} />
      </div>
      <section aria-label="What it changed" className="code-section">
        <VisuallyHidden as="h2">What it changed</VisuallyHidden>
        {base ? (
          <DiffFiles
            repo={repo}
            base={base}
            head={c.oid}
            against={againstOf(repo, base, parents)}
          />
        ) : (
          <p className="muted">
            {`It names ${plural(c.parent_count ?? 0, "parent")}, more than a history lists: open it in git to read its diff.`}
          </p>
        )}
      </section>
    </Stack>
  );
}

/** What the commit's diff is against, said after its totals and at the tree's foot. */
function againstOf(repo: string, base: string, parents: readonly string[]): DiffAgainst {
  if (base === EMPTY_TREE) {
    return {
      words: " of a root commit, every file it holds",
      foot: <p className="code-tree__said">Compared with git's empty tree</p>,
    };
  }
  const parent = (
    <a className="mono t-link" href={href(commitPath(repo, base))} title={base}>
      {shortOID(base)}
    </a>
  );
  return {
    words: parents.length > 1 && <> against its first parent {parent}</>,
    foot: (
      <p className="code-tree__said">
        Compared with its first parent at <code>{shortOID(base)}</code>
      </p>
    ),
  };
}

/**
 * The commit's header: its subject, and under it who wrote it and when — and
 * when git says it was committed apart from that — its repository, its
 * parents and its whole id, with its copy.
 */
function CommitHeader({ repo, answer }: { repo: string; answer: CodeCommitAnswer }) {
  const now = useNow();
  const c = answer.commit;
  const parents = c.parents ?? [];
  const author = c.author ?? `(a name of ${c.author_bytes ?? 0} bytes)`;
  return (
    <ObjectHeader
      kind="Commit"
      icon="code"
      identifier={shortOID(c.oid)}
      title={subjectOf(c)}
      description={
        <span className="code-commit-byline">
          <span title={c.author_email}>
            <Byline who={author} verb="authored it" at={c.authored_at} now={now} />
          </span>
          {c.committed_at !== c.authored_at && (
            <span className="code-commit-byline__part">
              committed{" "}
              <time dateTime={c.committed_at} title={fmtDateTime(c.committed_at)}>
                {fmtDateTime(c.committed_at)}
              </time>
            </span>
          )}
          <a className="t-link" href={href(repoPath(repo))}>
            {repo}
          </a>
          <span className="code-commit-byline__part">
            {parents.length === 0 ? (
              <EmptyValue
                label={c.parent_count ? `${c.parent_count} parents` : "None: a root commit"}
              />
            ) : (
              <>
                {parents.length > 1 ? "parents" : "parent"}{" "}
                {parents.map((p) => (
                  <a key={p} className="mono t-link" href={href(commitPath(repo, p))} title={p}>
                    {shortOID(p)}
                  </a>
                ))}
              </>
            )}
          </span>
          <Copyable
            className="code-sha"
            value={c.oid}
            monospace
            truncate={false}
            ariaLabel="Copy the commit's whole id"
            title={`Copy ${c.oid}`}
          />
        </span>
      }
    />
  );
}

/**
 * Who pushed the commit, when and how, and the turn, executor and work item
 * behind a seat's push — as the engine recorded them, never as the commit says
 * — or why the answering node cannot say.
 */
function Pushed({ answer }: { answer: CodeCommitAnswer }) {
  const seatBadge = useSeatBadgeOf();
  const by = answer.pushed_by;
  if (!by) {
    return (
      <CommitCard label="Pushed" title="Pushed">
        <p className="muted">
          {answer.coverage?.complete
            ? "No push the index keeps introduced it: it came in a push that brought more commits than the index lists, or before the push history the repository keeps."
            : "The index of pushes on the node that answered has not read the push that introduced it yet."}
        </p>
      </CommitCard>
    );
  }
  const push = answer.push;
  const run: CodePushRun | undefined = push?.run ?? by.principal.run;
  const seat = by.principal.seat;
  const moved = (push?.commands ?? []).filter((cmd) => cmd.result === "ok");
  return (
    <CommitCard label="Pushed" title="Pushed">
      <PropertiesRail
        headings="sentence"
        groups={[
          {
            properties: [
              {
                label: "Pushed by",
                value: (
                  <>
                    {seat && <Face who={seat} size="text" />}{" "}
                    {seat ? seatBadge(seat).name : (by.principal.operator_id ?? "")}
                    {by.principal.operator_id && seat && (
                      <span className="muted mono"> via {by.principal.operator_id}</span>
                    )}
                  </>
                ),
              },
              { label: "When", value: fmtDateTime(by.at) },
              { label: "How", value: VIA_WORDS[by.via] ?? by.via },
              {
                label: "Turn",
                value: run?.turn_id ? (
                  <a className="mono prose-link" href={href(["live", "turns", run.turn_id])}>
                    {run.turn_id}
                  </a>
                ) : (
                  <EmptyValue label="Pushed by a person" />
                ),
              },
              {
                // THE EXECUTORS BOARD IS KEYED BY THE TURN that launched the
                // executor: the launch's job id is shown, the turn is linked.
                label: "Executor",
                value:
                  run?.run_id && run.turn_id ? (
                    <a className="mono prose-link" href={href(["live", "executors", run.turn_id])}>
                      {run.run_id}
                    </a>
                  ) : (
                    <EmptyValue label="None" />
                  ),
              },
              {
                label: "Work item",
                value: run?.work_key ? (
                  <span className="mono">
                    {run.work_key}
                    {run.work_backend && <span className="muted"> ({run.work_backend})</span>}
                  </span>
                ) : (
                  <EmptyValue label="None" />
                ),
              },
              ...(moved.length > 0
                ? [
                    {
                      label: "Refs it moved",
                      value: (
                        <span>
                          {moved.map((cmd, i) => (
                            <Fragment key={cmd.ref}>
                              {i > 0 && ", "}
                              <RefName gitRef={cmd.ref} />
                            </Fragment>
                          ))}
                        </span>
                      ),
                    },
                  ]
                : []),
            ],
          },
        ]}
      />
    </CommitCard>
  );
}

/** One card under the commit's header: its title, then what it says. */
function CommitCard({
  label,
  title,
  children,
}: {
  label: string;
  title: string;
  children: ReactNode;
}) {
  return (
    <Card as="section" aria-label={label} className="code-commit-card">
      <Card.Header divided>
        <Card.Title as="h2">{title}</Card.Title>
      </Card.Header>
      <Card.Body>{children}</Card.Body>
    </Card>
  );
}

/** A change a commit belongs to, as a link to its page. */
function ChangeLink({ change }: { change: CodeChangeName }) {
  const at = parseChangeRef(change.ref);
  return (
    <>
      {at ? (
        <a className="mono prose-link" href={href(changePath(at.repo, at.number))}>
          {change.ref}
        </a>
      ) : (
        <span className="mono">{change.ref}</span>
      )}{" "}
      {change.title} <ChangeStateTag state={change.state} />
    </>
  );
}

/** The changes the commit belongs to: those it merged, and those whose revision it heads. */
function BelongsTo({ answer }: { answer: CodeCommitAnswer }) {
  const merged = answer.merged ?? [];
  const revisions = answer.revisions ?? [];
  return (
    <CommitCard label="Changes" title="Changes">
      {merged.length === 0 && revisions.length === 0 ? (
        <p className="muted">No change names this commit: it is no change's revision or merge.</p>
      ) : (
        <ul className="code-rules">
          {merged.map((m) => (
            <li key={`merged:${m.ref}`}>
              <span>Merged</span>
              <span>
                <ChangeLink change={m} />
              </span>
            </li>
          ))}
          {revisions.map((r) => (
            <li key={`rev:${r.change.ref}:${r.n}`}>
              <span>{`Revision ${r.n} of`}</span>
              <span>
                <ChangeLink change={r.change} />
              </span>
            </li>
          ))}
        </ul>
      )}
      {answer.more && <p className="t-caption">More changes name it than one page lists.</p>}
    </CommitCard>
  );
}
