/** One commit's page by its whole id: its header, its provenance in sentences (`Provenance.tsx`),
 *  and every file it changed as one stack, against its first parent (a root commit's against git's
 *  empty tree). Its page bar holds Browse files, Revert and Cherry-pick (`CommitWrites.tsx`), and
 *  Tag (`repo/TagWrites.tsx`). */

import {
  ButtonLink,
  Copyable,
  EmptyState,
  EmptyValue,
  Skeleton,
  Stack,
  Tag,
  VisuallyHidden,
} from "@crewlethq/ui";
import { BotGlyph, FileCodeGlyph, GitMergeGlyph, UploadGlyph } from "@crewlethq/icons/glyphs";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { EMPTY_TREE, commitPath, isRepoName, repoPath, shortOID } from "~/lib/code.ts";
import { fmtDateTime, plural } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeCommitAnswer } from "~/protocol/index.ts";
import { mergeTitle, provenanceOf, subjectOf } from "../repo/History.tsx";
import { CodeNotFound } from "../NotFound.tsx";
import { repoHref } from "../paths.ts";
import { Byline, useWhoName } from "../ui.tsx";
import { TagCommitButton } from "../repo/TagWrites.tsx";
import { CommitWrites } from "./CommitWrites.tsx";
import { DiffFiles, type DiffAgainst } from "./DiffFiles.tsx";
import { Provenance } from "./Provenance.tsx";
import "~/styles/gitlet.css";

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
      <PageActions>
        <ButtonLink
          size="small"
          variant="secondary"
          leadingIcon={<FileCodeGlyph size="sm" />}
          href={repoHref(repo, { ref: c.oid })}
          aria-label={`Browse the files at ${shortOID(c.oid)}`}
        >
          Browse files
        </ButtonLink>
        <CommitWrites repo={repo} sha={c.oid} subject={subjectOf(c)} />
        <TagCommitButton repo={repo} sha={c.oid} />
      </PageActions>
      <CommitHeader repo={repo} answer={answer} />
      <Provenance answer={answer} />
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
  const whoName = useWhoName();
  const c = answer.commit;
  const parents = c.parents ?? [];
  const author = c.author ?? `(a name of ${c.author_bytes ?? 0} bytes)`;
  // THE CHIP A HISTORY ROW DRAWS, so the row and the page say it the same way.
  const by = answer.pushed_by;
  const prov = provenanceOf(by, author, whoName);
  const Glyph =
    prov?.glyph === "run" ? BotGlyph : prov?.glyph === "merge" ? GitMergeGlyph : UploadGlyph;
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
          {prov && (
            <Tag
              size="xs"
              appearance="outline"
              leadingIcon={<Glyph size="xs" />}
              title={
                prov.glyph === "merge" && by
                  ? mergeTitle(c, by, { commit: c.oid, merged: answer.merged }, whoName)
                  : undefined
              }
            >
              {prov.words}
            </Tag>
          )}
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
