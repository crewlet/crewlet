/**
 * A repository's branches, its tags and its history, drawn as a code host
 * draws them: a branch as its chip and its marks; the history by the day each
 * commit was written, each commit its subject, who wrote it and when, who
 * pushed it and for which work item, and its id with its copy.
 *
 * BRANCHES are the engine's listing of its live refs under `refs/heads/`
 * (`code_refs`), in name order a page at a time — the rows, read at the tab's
 * floor, never git's — each opening its files. TAGS are the same listing
 * under `refs/tags/`, below them, each opening its files at the tag.
 *
 * The tab makes a branch (New branch), and each row deletes its own or opens
 * a protection rule on it (`BranchWrites.tsx`, `Protect.tsx`), every one held
 * with its reason. A branch a rule names already leads to that rule instead.
 *
 * THE HISTORY is git's (`code_log`): the commits that changed the path, or any
 * path, newest first from the revision, a page at a time, each with the push
 * that introduced it where the answering Gitlet node's index holds it — who
 * pushed it, which the commit's own author line is not — each opening the
 * commit's own page, and each branch but the default offering a comparison
 * with it. A page is what the
 * walk covered, so one can be short with more to come, and each page is joined
 * to the one before it (`lib/usePaged.ts`).
 *
 * A TIMELINE OF THE READER'S DAYS each commit was AUTHORED, in git's order: a
 * day met again after another opens its own heading.
 *
 * Each commit carries its checks for the branch read, the change it merged or landed with,
 * and its tags (`code_commit_marks`, the page's `tagged`).
 */

import { Fragment, useMemo, type ReactNode } from "react";
import { Button, IconButtonLink, Skeleton, Tag, VisuallyHidden } from "@crewlethq/ui";
import {
  BookmarkGlyph,
  BotGlyph,
  CodeGlyph,
  GitCommitVerticalGlyph,
  GitMergeGlyph,
  UploadGlyph,
} from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { href, useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { changePath, noDefaultHead, parseChangeRef, refName, shortOID } from "~/lib/code.ts";
import { ago } from "~/lib/ago.ts";
import { CHECK_HISTORY_DAYS, rollupFor, rollupTitle } from "~/lib/checks.ts";
import { fmtDate, fmtDateTime, plural } from "~/lib/format.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type {
  CodeChangeLanding,
  CodeCommitMark,
  CodeCommitTags,
  CodeGitAnswer,
  CodeLogCommit,
  CodePushedBy,
  CodeRefRow,
  CodeRefsAnswer,
  CodeRepoDetail,
} from "~/protocol/index.ts";
import { CodeList, useDayGroups, type ListDay } from "../CodeList.tsx";
import { commitHref, compareHref, repoHref } from "../paths.ts";
import { ChangeRefPill } from "../Changes.tsx";
import {
  BranchChip,
  CheckRollupMark,
  Face,
  GITLET,
  ShaChip,
  TagChip,
  useWhoName,
  type Who,
} from "../ui.tsx";
import { useCommitMarks, type MarksFailure } from "./marks.ts";
import { DeleteBranchButton, NewBranchButton } from "./BranchWrites.tsx";
import { ProtectButton } from "./Protect.tsx";
import { RefPicker, revisionWord, type Revisions } from "./refs.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import "~/styles/gitlet.css";

/** Where a repository's tags are listed (`gitlet.ReadQuery.Prefix`). */
const TAG_PREFIX = "refs/tags/";

/** The repository's branches, the default first in name order as the engine lists them, each opening its files. */
export function Branches({ repo }: { repo: CodeRepoDetail }) {
  const params = useMemo(() => ({ name: repo.name }), [repo.name]);
  const first = useCodeQuery("code_refs", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_refs", { ...params, cursor }),
    (a: CodeRefsAnswer) => a.refs ?? [],
    (r: CodeRefRow) => r.ref,
    (a: CodeRefsAnswer) => a.next ?? "",
  );
  const rows = paged.items;
  // THE REFS A RULE NAMES EXACTLY, a branch's own rule; a glob's is drawn in
  // Settings beside the refs it matches.
  const protectedRefs = useMemo(
    () => new Set((repo.protections ?? []).map((p) => p.pattern)),
    [repo.protections],
  );
  return (
    <>
      <PageToolbar>
        <NewBranchButton repo={repo} />
      </PageToolbar>
      {paged.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={6} label={`Loading the branches of ${repo.name}`} />
      )}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={
          rows.length > 0 || paged.more
            ? undefined
            : {
                title: "No branch yet",
                hint: "A branch is listed once a push that creates it applies.",
              }
        }
      >
        <DataGrid<CodeRefRow>
          rows={rows}
          rowKey={(r) => r.ref}
          // THE ENGINE'S ORDER, by name.
          serverSorted
          rowHref={(r) => repoHref(repo.name, { ref: branchRev(repo, r) })}
          onLoadMore={paged.more ? paged.next : undefined}
          loadedNote={
            paged.more ? `${plural(rows.length, "branch", "branches")} loaded` : undefined
          }
          aria-label={`Branches of ${repo.name}`}
          columns={[
            {
              key: "name",
              header: "Branch",
              floor: "12rem",
              phoneLead: true,
              cell: (r) => (
                <span className="code-ref">
                  <BranchChip name={refName(r.ref).name} />
                  {refName(r.ref).name === repo.default_branch && (
                    <Tag appearance="outline">default</Tag>
                  )}
                  {protectedRefs.has(r.ref) && <Tag appearance="outline">protected</Tag>}
                </span>
              ),
            },
            {
              // A VALUE, not a row action, so a phone names it on a line of its own.
              key: "head",
              header: "Head",
              shrink: true,
              cell: (r) => (r.oid ? <ShaChip oid={r.oid} /> : null),
            },
            {
              key: "history",
              header: "History",
              shrink: true,
              drop: 1,
              cell: (r) => (
                <a
                  className="t-link"
                  href={repoHref(repo.name, { tab: "commits", ref: branchRev(repo, r) })}
                >
                  Commits →
                </a>
              ),
            },
            {
              key: "compare",
              header: "Compare",
              shrink: true,
              drop: 2,
              // WITH THE DEFAULT BRANCH, which a branch is proposed into
              // unless a change names another; nothing for the default
              // branch itself, which compares with nothing here.
              cell: (r) =>
                branchRev(repo, r) ? (
                  <a className="t-link" href={compareHref(repo.name, { head: branchRev(repo, r) })}>
                    Compare →
                  </a>
                ) : null,
            },
            {
              key: "actions",
              header: <VisuallyHidden>Actions</VisuallyHidden>,
              action: true,
              shrink: true,
              // A RULE ON THIS BRANCH ALONE, and its delete at the head this
              // row shows. A branch a rule names already is edited where its
              // rule is drawn whole, gate and all (Settings): a new-rule form
              // opened here would start from the engine's defaults rather
              // than from what the rule holds.
              cell: (r) => (
                <span className="row gap-1 nowrap">
                  {protectedRefs.has(r.ref) ? (
                    <a className="t-link" href={repoHref(repo.name, { tab: "settings" })}>
                      Protection →
                    </a>
                  ) : (
                    <ProtectButton repo={repo} pattern={r.ref} label="Protect" />
                  )}
                  <DeleteBranchButton repo={repo} row={r} />
                </span>
              ),
            },
          ]}
        />
      </QueryState>
      {paged.pageError && <PageFailed error={paged.pageError} />}
      <Tags repo={repo} />
    </>
  );
}

/**
 * The repository's tags, under its branches: the engine's listing under
 * `refs/tags/`, by name a page at a time, each opening its files at the tag —
 * named by its whole ref, which no branch of the same name can shadow.
 */
function Tags({ repo }: { repo: CodeRepoDetail }) {
  const params = useMemo(() => ({ name: repo.name, prefix: TAG_PREFIX }), [repo.name]);
  const first = useCodeQuery("code_refs", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_refs", { ...params, cursor }),
    (a: CodeRefsAnswer) => a.refs ?? [],
    (r: CodeRefRow) => r.ref,
    (a: CodeRefsAnswer) => a.next ?? "",
  );
  const rows = paged.items;
  return (
    <section className="code-section" aria-labelledby="code-tags-title">
      <h2 id="code-tags-title" className="code-section__title">
        Tags
      </h2>
      {paged.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={3} label={`Loading the tags of ${repo.name}`} />
      )}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={
          rows.length > 0 || paged.more
            ? undefined
            : { title: "No tag yet", hint: "A tag is listed once a push that creates it applies." }
        }
      >
        <DataGrid<CodeRefRow>
          name="tags"
          rows={rows}
          rowKey={(r) => r.ref}
          serverSorted
          rowHref={(r) => repoHref(repo.name, { ref: r.ref })}
          onLoadMore={paged.more ? paged.next : undefined}
          loadedNote={paged.more ? `${plural(rows.length, "tag")} loaded` : undefined}
          aria-label={`Tags of ${repo.name}`}
          columns={[
            {
              key: "name",
              header: "Tag",
              floor: "12rem",
              phoneLead: true,
              cell: (r) => (
                <span className="code-ref">
                  <BookmarkGlyph size="sm" className="code-glyph" />
                  <span className="code-ref__name">{refName(r.ref).name}</span>
                </span>
              ),
            },
            {
              key: "commit",
              header: "Commit",
              shrink: true,
              action: true,
              cell: (r) => (r.oid ? <ShaChip oid={r.oid} /> : null),
            },
          ]}
        />
      </QueryState>
      {paged.pageError && <PageFailed error={paged.pageError} />}
    </section>
  );
}

/** A branch as an address names it: none for the default branch, its name for any other. */
function branchRev(repo: CodeRepoDetail, r: CodeRefRow): string {
  const name = refName(r.ref).name;
  return name === repo.default_branch ? "" : name;
}

/** How a commit reached its branch, as its chip says it: a glyph and its words. */
export interface Provenance {
  glyph: "run" | "push" | "merge";
  words: ReactNode;
}

/**
 * How the push the index holds brought a commit: Gitlet merging or landing a
 * change for its merger, a run for its work item, the author with git, or who
 * pushed it. None where the index holds no push.
 */
export function provenanceOf(
  by: CodePushedBy | undefined,
  author: string,
  whoName: (who: Who) => string,
): Provenance | null {
  if (!by) return null;
  const pusher = by.principal.seat ?? by.principal.operator_id ?? "";
  const forWhom = pusher ? ` for ${whoName(pusher)}` : "";
  if (by.via === "merge") return { glyph: "merge", words: `merged by Gitlet${forWhom}` };
  if (by.via === "queue") {
    return { glyph: "merge", words: `landed by Gitlet from the queue${forWhom}` };
  }
  const work = by.principal.run?.work_key;
  const theAuthor = pusher !== "" && whoName(pusher) === whoName(author);
  if (work) {
    const key = <span className="code-commit-prov__key mono">{work}</span>;
    return {
      glyph: "run",
      words: theAuthor ? (
        <>pushed by a run for {key}</>
      ) : (
        <>
          pushed by a run of {whoName(pusher)} for {key}
        </>
      ),
    };
  }
  if (theAuthor && by.via === "git") return { glyph: "push", words: "pushed with git" };
  if (pusher === "") return null;
  return { glyph: "push", words: `pushed by ${whoName(pusher)}` };
}

/** How many parents a commit has, or null where the page says neither. */
const parentsOf = (c: CodeLogCommit): number | null => c.parent_count ?? c.parents?.length ?? null;

/** Gitlet wrote this merge commit: two parents or more, brought by a merge or a queue landing. */
export function gitletWrote(c: CodeLogCommit, by: CodePushedBy | undefined): boolean {
  const merged = by?.via === "merge" || by?.via === "queue";
  return merged && (parentsOf(c) ?? 0) >= 2 && c.engine_committed !== false;
}

/** The merge chip's title: which change Gitlet merged or landed, and for whom. */
function mergeTitle(
  c: CodeLogCommit,
  by: CodePushedBy,
  mark: CodeCommitMark | null | undefined,
  whoName: (who: Who) => string,
): string {
  const pusher = by.principal.seat ?? by.principal.operator_id ?? "";
  const who = pusher ? whoName(pusher) : "a person";
  const change = mark?.merged?.[0]?.ref ?? mark?.heads?.[0]?.change.ref ?? "a change";
  if (by.via === "queue") {
    return `Gitlet wrote this commit when ${change} landed from the merge queue, queued by ${who}`;
  }
  const what = (parentsOf(c) ?? 0) >= 2 ? "merge commit" : "commit";
  return `Gitlet wrote this ${what} when ${who} merged ${change} through its gate`;
}

/** A change a commit names, as a link to its page. */
function ChangeLink({ change }: { change: CodeChangeLanding }) {
  const at = parseChangeRef(change.ref);
  return at ? (
    <a className="prose-link" href={href(changePath(at.repo, at.number))} title={change.title}>
      {change.ref}
    </a>
  ) : (
    <span>{change.ref}</span>
  );
}

/** The changes whose revision this commit heads: "landed with web!41", "revision 3 of web!42". */
function Landed({ repo, commit, mark }: { repo: string; commit: string; mark: CodeCommitMark }) {
  const merged = new Set((mark.merged ?? []).map((m) => m.ref));
  const heads = (mark.heads ?? []).filter((h) => !merged.has(h.change.ref));
  if (heads.length === 0 && !mark.more) return null;
  return (
    <span className="code-commit-landed">
      {heads.map((h, i) => (
        <Fragment key={h.change.ref}>
          {i > 0 && ", "}
          {h.change.state === "merged" ? (
            <>
              landed with <ChangeLink change={h.change} />
            </>
          ) : (
            <>
              revision {h.n} of <ChangeLink change={h.change} />
              {h.change.state === "closed" && ", closed"}
            </>
          )}
        </Fragment>
      ))}
      {mark.more && (
        <>
          {heads.length > 0 && ", "}
          <a className="prose-link" href={commitHref(repo, commit)}>
            and more
          </a>
        </>
      )}
    </span>
  );
}

/** One commit's meta line: who wrote it and when, how it reached its branch, its change and its tags. */
function CommitMeta({
  repo,
  commit,
  by,
  mark,
  tags,
  now,
}: {
  repo: string;
  commit: CodeLogCommit;
  by: CodePushedBy | undefined;
  mark: CodeCommitMark | null | undefined;
  tags: CodeCommitTags | undefined;
  now: number;
}) {
  const whoName = useWhoName();
  const author = commit.author ?? `(${commit.author_bytes ?? 0} bytes)`;
  // GITLET IS THE WRITER of a merge commit it built, dated when it committed it.
  const byGitlet = gitletWrote(commit, by);
  const writer: Who = byGitlet ? GITLET : author;
  const at = byGitlet ? commit.committed_at : commit.authored_at;
  const prov = provenanceOf(by, author, whoName);
  const Glyph =
    prov?.glyph === "run" ? BotGlyph : prov?.glyph === "merge" ? GitMergeGlyph : UploadGlyph;
  const provTitle =
    prov?.glyph === "merge" && by
      ? mergeTitle(commit, by, mark, whoName)
      : by?.principal.operator_id
        ? `through ${by.principal.operator_id}`
        : undefined;
  return (
    <>
      <Face who={writer} />
      <span className="code-commit-meta__who" title={byGitlet ? undefined : commit.author_email}>
        {whoName(writer)}
      </span>
      <span>
        {byGitlet ? "committed" : "authored"}{" "}
        <time dateTime={at} title={fmtDateTime(at)}>
          {ago(at, now)}
        </time>
      </span>
      {prov && (
        <span className="code-commit-prov" title={provTitle}>
          <Glyph size="xs" aria-hidden />
          <span>{prov.words}</span>
        </span>
      )}
      {mark && <Landed repo={repo} commit={commit.oid} mark={mark} />}
      {tags?.refs.map((ref) => (
        <TagChip key={ref} repo={repo} gitRef={ref} />
      ))}
      {tags?.more ? <span>{`+${plural(tags.more, "more tag", "more tags")}`}</span> : null}
    </>
  );
}

/** The commit's checks for the branch read, or that they are no longer kept; nothing until known. */
function CommitCheck({
  mark,
  by,
  branch,
  defaultRef,
  now,
}: {
  mark: CodeCommitMark | null | undefined;
  by: CodePushedBy | undefined;
  branch: string;
  defaultRef: string;
  now: number;
}) {
  if (mark === undefined) return null;
  const rollup = rollupFor(mark, branch, defaultRef);
  if (rollup) return <CheckRollupMark rollup={rollup} title={rollupTitle(rollup, branch, now)} />;
  const pushed = Date.parse(by?.at ?? "");
  if (!Number.isNaN(pushed) && now - pushed > CHECK_HISTORY_DAYS * 86_400_000) {
    return (
      <span className="t-caption" title={`Checks are kept ${CHECK_HISTORY_DAYS} days.`}>
        Checks no longer kept
      </span>
    );
  }
  return null;
}

/** Why the marks are missing, said once above the rows. */
function MarksNote({ failed, retry }: { failed: MarksFailure; retry: () => void }) {
  const what = "Checks and the changes each commit landed with";
  const refused = REFUSED_CODES.has(failed.code);
  return (
    <p className="code-commit-note" role="status">
      {refused
        ? `${what} are unavailable${failed.sentence ? `: ${failed.sentence}` : ""}.`
        : `${what} could not be read.`}
      {!refused && (
        <Button size="small" variant="ghost" onClick={retry}>
          Retry
        </Button>
      )}
    </p>
  );
}

/** The refusals a retry does not change: the engine said no, or this fleet answers no marks yet. */
const REFUSED_CODES = new Set(["unavailable", "bad_params", "not_found", "unauthorized"]);

/** The commit's files, browsed at it. */
function BrowseAt({ repo, oid }: { repo: string; oid: string }) {
  return (
    <IconButtonLink
      href={repoHref(repo, { ref: oid })}
      icon={<CodeGlyph />}
      label={`Browse the files at ${shortOID(oid)}`}
      size="sm"
      variant="ghost"
    />
  );
}

/** When a commit was authored, the day its heading groups it by. */
const authoredAt = (c: CodeLogCommit) => c.authored_at;

/** A day's heading: the day its commits were authored, or "Commits" where the time does not parse. */
const dayHeading = (day: ListDay | null, c: CodeLogCommit) =>
  day ? `Commits on ${fmtDate(c.authored_at)}` : "Commits";

/** A commit's subject, or what git left of one past its bound. */
export function subjectOf(c: { subject?: string; subject_bytes?: number }): string {
  if (c.subject) return c.subject;
  return c.subject_bytes ? `(a subject of ${c.subject_bytes} bytes)` : "(no message)";
}

/**
 * The history at the revision, of the path the address names or of every
 * path, newest first. Its picker lists the branches and tags its page read
 * once ([useRevisions]).
 */
export function Commits({ repo, revisions }: { repo: CodeRepoDetail; revisions: Revisions }) {
  const now = useNow();
  const [rev, setRev] = useParam("ref", "", "filter");
  const [path, setPath] = useParam("path", "", "filter");
  const params = useMemo(
    () => ({ name: repo.name, ...(rev ? { ref: rev } : {}), ...(path ? { path } : {}) }),
    [repo.name, rev, path],
  );
  const first = useCodeQuery("code_log", params, {
    enabled: Boolean(repo.head) || rev !== "",
  });
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_log", { ...params, cursor }),
    (a: CodeGitAnswer) => a.log?.commits ?? [],
    (c: CodeLogCommit) => c.oid,
    (a: CodeGitAnswer) => a.next ?? "",
  );
  // WHO PUSHED EACH, from the page that named it: a commit is on one page.
  const pushedBy = useMemo(() => {
    const all: Record<string, CodePushedBy> = {};
    for (const a of paged.answers) Object.assign(all, a.pushed_by ?? {});
    return all;
  }, [paged.answers]);
  const rows = paged.items;
  const groups = useDayGroups(rows, authoredAt, dayHeading);
  // EACH PAGE'S MARKS, asked once per page by its whole ids; its tags ride the page itself.
  const pages = useMemo(
    () => paged.answers.map((a) => (a.log?.commits ?? []).map((c) => c.oid)),
    [paged.answers],
  );
  const marks = useCommitMarks(repo.name, pages);
  const tagged = useMemo(() => {
    const all: Record<string, CodeCommitTags> = {};
    for (const a of paged.answers) Object.assign(all, a.tagged?.commits ?? {});
    return all;
  }, [paged.answers]);
  // THE BRANCH READ, whose checks a row draws: a tag's or a commit's history reads the default's.
  const defaultRef = `refs/heads/${repo.default_branch}`;
  const resolvedRef = paged.answers[0]?.resolved?.ref?.ref ?? "";
  const branch = resolvedRef.startsWith("refs/heads/") ? resolvedRef : defaultRef;
  if (!repo.head && rev === "") {
    // THE DEFAULT BRANCH HOLDS NOTHING: with no branch at all there is no
    // history to read, and with others live the picker reaches theirs.
    return repo.branches === 0 ? (
      <p className="muted">Nobody has pushed a branch yet: {repo.default_branch} has no history.</p>
    ) : (
      <>
        <PageToolbar>
          <RefPicker repo={repo} revisions={revisions} value={rev} onChoose={setRev} />
        </PageToolbar>
        <p className="muted">{noDefaultHead(repo)}</p>
      </>
    );
  }
  return (
    <>
      <PageToolbar>
        <RefPicker repo={repo} revisions={revisions} value={rev} onChoose={setRev} />
        {path && (
          <span className="row gap-1">
            <span className="t-caption">
              History of <span className="mono">{path}</span>
            </span>
            <Button size="small" variant="ghost" onClick={() => setPath("")}>
              Clear
            </Button>
          </span>
        )}
      </PageToolbar>
      {marks.failed && rows.length > 0 && <MarksNote failed={marks.failed} retry={marks.retry} />}
      {paged.loading && rows.length === 0 && (
        <Skeleton
          variant="text"
          rows={6}
          label={`Loading the history of ${revisionWord(repo, rev)}`}
        />
      )}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={
          rows.length > 0 || paged.more
            ? undefined
            : {
                title: path ? `No commit changed ${path}` : "No commit",
                hint: `The history is read from ${revisionWord(repo, rev)}, newest first.`,
              }
        }
      >
        <CodeList<CodeLogCommit>
          groups={groups}
          look="timeline"
          spine={GitCommitVerticalGlyph}
          rowKey={(c) => c.oid}
          rowHref={(c) => commitHref(repo.name, c.oid)}
          aria-label={`History of ${revisionWord(repo, rev)}`}
          title={subjectOf}
          titleAfter={(c) =>
            (marks.of(c.oid)?.merged ?? [])
              .filter((m) => m.merge_oid === c.oid)
              .map((m) => <ChangeRefPill key={m.ref} change={m} />)
          }
          meta={(c) => (
            <CommitMeta
              repo={repo.name}
              commit={c}
              by={pushedBy[c.oid]}
              mark={marks.of(c.oid)}
              tags={tagged[c.oid]}
              now={now}
            />
          )}
          facts={(c) => (
            <>
              <span className="code-commit-check">
                <CommitCheck
                  mark={marks.of(c.oid)}
                  by={pushedBy[c.oid]}
                  branch={branch}
                  defaultRef={defaultRef}
                  now={now}
                />
              </span>
              <ShaChip oid={c.oid} />
              <BrowseAt repo={repo.name} oid={c.oid} />
            </>
          )}
          paged={paged}
          moreLabel="Load older commits"
          foot={
            rows.length > 0 &&
            `${plural(rows.length, "commit")} of ${revisionWord(repo, rev)} loaded, newest first`
          }
        />
      </QueryState>
    </>
  );
}
