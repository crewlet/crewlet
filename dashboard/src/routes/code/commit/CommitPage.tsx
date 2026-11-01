/**
 * One commit's page, `#/code/repos/{name}/commits/{sha}`, by its WHOLE id —
 * the one spelling no push can move: its facts as git keeps them, who pushed
 * it and from which turn and run, the changes it belongs to, and what it
 * changed.
 *
 * # Whose it is, from the rows; what it is, from git
 *
 * `code_commit` answers the commit as the repository's history lists it,
 * read of the home git node, beside what the ROWS say of it: the push that
 * introduced it — by the git node's index of pushes, and that push's row
 * where the history still holds it, which names who pushed, the run behind a
 * seat's push and the refs it moved — and every change it belongs to, read at
 * the caller's freshness: the changes whose merge it is and the revisions it
 * heads. A commit's author line is what its writer said about it; who pushed
 * it is what the engine recorded, and the page says both.
 *
 * # Its diff is against its first parent
 *
 * As git shows a commit: a merge's change is what it brought into the branch
 * it was made on, and a root commit's is every file it holds, measured from
 * git's empty tree. The diff is read by both whole ids, so it is immutable.
 */

import type { ReactNode } from "react";
import { EmptyState, EmptyValue, Skeleton, Stack, Tag } from "@crewlethq/ui";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import {
  EMPTY_TREE,
  changePath,
  changeStateWord,
  commitPath,
  isRepoName,
  parseChangeRef,
  refName,
  repoPath,
  shortOID,
} from "~/lib/code.ts";
import { fmtDateTime, plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeChangeName, CodeCommitAnswer, CodePushRun } from "~/protocol/index.ts";
import { subjectOf } from "../repo/History.tsx";
import { DiffFiles } from "./DiffFiles.tsx";

/** Every way a push's commits came to be, in words (`code.PushVias`). */
const VIA_WORDS: Readonly<Record<string, string>> = {
  git: "a git push",
  engine: "a commit the engine built from a tool's call",
  merge: "a change merged",
  expiry: "a ref expired",
  queue: "a merge queue landed it",
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
        <EmptyState
          title={`No commit ${shortOID(sha)} in ${repo}`}
          description={read.detail ?? "The repository holds no commit by that id."}
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
      <Pushed answer={answer} />
      <BelongsTo answer={answer} />
      <section aria-label="What it changed">
        <p className="t-label">
          {base === EMPTY_TREE
            ? "What it changed: every file of a root commit"
            : parents.length > 1
              ? `What it changed against its first parent, ${shortOID(base)}, of ${plural(parents.length, "parent")}`
              : "What it changed"}
        </p>
        {base ? (
          <DiffFiles repo={repo} base={base} head={c.oid} />
        ) : (
          <p className="muted">
            {`It names ${plural(c.parent_count ?? 0, "parent")}, more than a history lists: open it in git to read its diff.`}
          </p>
        )}
      </section>
    </Stack>
  );
}

/** The commit's header: its subject, its id, and the facts git keeps of it. */
function CommitHeader({ repo, answer }: { repo: string; answer: CodeCommitAnswer }) {
  const c = answer.commit;
  const parents = c.parents ?? [];
  const facts: Fact[] = [
    { label: "Repository", value: repo, path: repoPath(repo) },
    {
      label: "Author",
      value: c.author ?? `(a name of ${c.author_bytes ?? 0} bytes)`,
      ...(c.author_email ? { note: c.author_email } : {}),
    },
    { label: "Authored", value: fmtDateTime(c.authored_at) },
    ...(c.committed_at !== c.authored_at
      ? [{ label: "Committed", value: fmtDateTime(c.committed_at) }]
      : []),
    {
      label: parents.length > 1 ? "Parents" : "Parent",
      value:
        parents.length === 0 ? (
          <EmptyValue
            label={c.parent_count ? `${c.parent_count} parents` : "None: a root commit"}
          />
        ) : (
          <span className="row gap-1 mono">
            {parents.map((p) => (
              <a key={p} className="prose-link" href={href(commitPath(repo, p))} title={p}>
                {shortOID(p)}
              </a>
            ))}
          </span>
        ),
      token: true,
    },
  ];
  return (
    <ObjectHeader
      kind="Commit"
      icon="code"
      identifier={shortOID(c.oid)}
      title={subjectOf(c)}
      facts={facts}
    />
  );
}

/** A label and its value, as the page's fact lists draw them. */
function FactList({ label, rows }: { label: string; rows: { label: string; value: ReactNode }[] }) {
  return (
    <dl className="code-facts" aria-label={label}>
      {rows.map((r) => (
        <div key={r.label}>
          <dt>{r.label}</dt>
          <dd>{r.value}</dd>
        </div>
      ))}
    </dl>
  );
}

/**
 * Who pushed the commit, when and how, and the turn, coding run and work item
 * behind a seat's push — as the engine recorded them, never as the commit says
 * — or why the answering node cannot say.
 */
function Pushed({ answer }: { answer: CodeCommitAnswer }) {
  const seatBadge = useSeatBadgeOf();
  const by = answer.pushed_by;
  if (!by) {
    return (
      <section aria-label="Pushed">
        <p className="t-label">Pushed</p>
        <p className="muted">
          {answer.coverage?.complete
            ? "No push the index keeps introduced it: it came in a push that brought more commits than the index lists, or before the push history the repository keeps."
            : "The index of pushes on the node that answered has not read the push that introduced it yet."}
        </p>
      </section>
    );
  }
  const push = answer.push;
  const run: CodePushRun | undefined = push?.run ?? by.principal.run;
  const seat = by.principal.seat;
  const moved = (push?.commands ?? []).filter((cmd) => cmd.result === "ok");
  return (
    <section aria-label="Pushed">
      <p className="t-label">Pushed</p>
      <FactList
        label="Who pushed it"
        rows={[
          {
            label: "Pushed by",
            value: (
              <>
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
            label: "Coding run",
            value: run?.run_id ? (
              <a className="mono prose-link" href={href(["live", "runs", run.run_id])}>
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
                    <span className="mono">
                      {moved.map((cmd) => refName(cmd.ref).name).join(", ")}
                    </span>
                  ),
                },
              ]
            : []),
        ]}
      />
    </section>
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
      {change.title} <Tag appearance="outline">{changeStateWord(change.state)}</Tag>
    </>
  );
}

/** The changes the commit belongs to: those it merged, and those whose revision it heads. */
function BelongsTo({ answer }: { answer: CodeCommitAnswer }) {
  const merged = answer.merged ?? [];
  const revisions = answer.revisions ?? [];
  return (
    <section aria-label="Changes">
      <p className="t-label">Changes</p>
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
    </section>
  );
}
