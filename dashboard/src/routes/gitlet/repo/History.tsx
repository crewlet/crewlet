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
 * BANDED BY THE READER'S DAY each commit was AUTHORED, in git's order: a band
 * is a run of consecutive commits on one day, so a history git orders by
 * generation — where a merged branch's older commit can follow a newer one —
 * is never re-sorted to fit its bands; a day met again after another opens
 * a band of its own.
 */

import { useMemo } from "react";
import { Button, Skeleton, Tag, VisuallyHidden, type DataGridBand } from "@crewlethq/ui";
import { BookmarkGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { noDefaultHead, refName } from "~/lib/code.ts";
import { ago } from "~/lib/ago.ts";
import { fmtDate, fmtDateTime, plural, readerDay } from "~/lib/format.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type {
  CodeGitAnswer,
  CodeLogCommit,
  CodePushedBy,
  CodeRefRow,
  CodeRefsAnswer,
  CodeRepoDetail,
} from "~/protocol/index.ts";
import { commitHref, compareHref, repoHref } from "../paths.ts";
import { BranchChip, Face, ShaChip, useWhoName } from "../ui.tsx";
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

/** Who pushed a commit, by its push, in a reader's words: the seat, or the credential's own name. */
function pusherOf(by: CodePushedBy | undefined): string {
  if (!by) return "";
  return by.principal.seat ?? by.principal.operator_id ?? "";
}

/**
 * One commit of the history: its subject, then who wrote it and when — and,
 * where the push that introduced it is known, who pushed it where that is
 * somebody else, and the work item a seat's push was charged to.
 *
 * NO LINK INSIDE: the row is the link to the commit's page.
 */
function CommitCell({
  commit,
  by,
  now,
}: {
  commit: CodeLogCommit;
  by: CodePushedBy | undefined;
  now: number;
}) {
  const whoName = useWhoName();
  const author = commit.author ?? `(${commit.author_bytes ?? 0} bytes)`;
  const pusher = pusherOf(by);
  // SAID ONLY WHERE IT IS NEWS: a commit pushed by the seat or person who
  // wrote it says who wrote it once.
  const pushedBySomebodyElse = pusher !== "" && whoName(pusher) !== whoName(author);
  const work = by?.principal.run?.work_key;
  return (
    <span className="code-log-row">
      <span className="code-log-row__subject">{subjectOf(commit)}</span>
      <span className="code-log-row__by">
        <Face who={author} />
        <span className="code-log-row__author" title={commit.author_email}>
          {whoName(author)}
        </span>
        <span>
          authored{" "}
          <time dateTime={commit.authored_at} title={fmtDateTime(commit.authored_at)}>
            {ago(commit.authored_at, now)}
          </time>
        </span>
        {pushedBySomebodyElse && (
          <span
            title={by?.principal.operator_id ? `through ${by.principal.operator_id}` : undefined}
          >
            pushed by <span className="code-log-row__author">{whoName(pusher)}</span>
          </span>
        )}
        {work && <span className="code-log-row__work mono">{work}</span>}
      </span>
    </span>
  );
}

/** The history's bands: each run of consecutive commits authored on one of the reader's days. */
function byDay(rows: readonly CodeLogCommit[]): DataGridBand<CodeLogCommit>[] {
  const bands: DataGridBand<CodeLogCommit>[] = [];
  for (const commit of rows) {
    const at = Date.parse(commit.authored_at);
    const day = Number.isNaN(at) ? "" : readerDay(at);
    const last = bands[bands.length - 1];
    if (last && last.key.startsWith(`${day}#`)) {
      last.rows.push(commit);
    } else {
      // A DAY MET AGAIN after another keeps its band apart: the key carries
      // the run's place in the history.
      bands.push({
        key: `${day}#${bands.length}`,
        label: day ? `Commits on ${fmtDate(commit.authored_at)}` : "Commits",
        rows: [commit],
      });
    }
  }
  return bands;
}

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
  const bands = useMemo(() => byDay(rows), [rows]);
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
        <DataGrid<CodeLogCommit>
          bands={bands}
          rowKey={(c) => c.oid}
          // GIT'S ORDER: every parent after every child.
          serverSorted
          rowHref={(c) => commitHref(repo.name, c.oid)}
          onLoadMore={paged.more ? paged.next : undefined}
          loadedNote={paged.more ? `${plural(rows.length, "commit")} loaded` : undefined}
          aria-label={`History of ${revisionWord(repo, rev)}`}
          columns={[
            {
              key: "commit",
              header: "Commit",
              floor: "16rem",
              phoneLead: true,
              cell: (c) => <CommitCell commit={c} by={pushedBy[c.oid]} now={now} />,
            },
            {
              key: "id",
              header: <VisuallyHidden>Commit id</VisuallyHidden>,
              label: "Commit id",
              shrink: true,
              action: true,
              cell: (c) => <ShaChip oid={c.oid} />,
            },
          ]}
        />
      </QueryState>
      {paged.pageError && <PageFailed error={paged.pageError} />}
    </>
  );
}
