/**
 * A repository's branches and its history.
 *
 * BRANCHES are the engine's listing of its live refs under `refs/heads/`
 * (`code_refs`), in name order a page at a time — the rows, read at the tab's
 * floor, never git's — each opening its files.
 *
 * THE HISTORY is git's (`code_log`): the commits that changed the path, or any
 * path, newest first from the revision, a page at a time, each with the push
 * that introduced it where the answering git node's index holds it — who
 * pushed it, which the commit's own author line is not — each opening the
 * commit's own page, and each branch but the default offering a comparison
 * with it. A page is what the
 * walk covered, so one can be short with more to come, and the first page is
 * held still from the press of Load older (`lib/usePaged.ts`).
 */

import { useMemo } from "react";
import { Button, Skeleton, TableDateCell, Tag } from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { refName, shortOID } from "~/lib/code.ts";
import { plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import type {
  CodeGitAnswer,
  CodeLogCommit,
  CodePushedBy,
  CodeRefRow,
  CodeRefsAnswer,
  CodeRepoDetail,
} from "~/protocol/index.ts";
import { commitHref, compareHref, repoHref } from "../paths.ts";
import { RefPicker, revisionWord } from "./refs.tsx";

/** The repository's branches, the default first in name order as the engine lists them, each opening its files. */
export function Branches({ repo }: { repo: CodeRepoDetail }) {
  const params = useMemo(() => ({ name: repo.name }), [repo.name]);
  const hold = usePageHold<CodeRefsAnswer>(params);
  const first = useCodeQuery("code_refs", params, { hold: hold.holding });
  const paged = usePaged(
    hold,
    first,
    (cursor) => codeRest.query("code_refs", { ...params, cursor }),
    (a: CodeRefsAnswer) => a.refs ?? [],
    (r: CodeRefRow) => r.ref,
    (a: CodeRefsAnswer) => a.next ?? "",
  );
  const rows = paged.items;
  return (
    <>
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
          rowHref={(r) => repoHref(repo.name, { tab: "files", ref: branchRev(repo, r) })}
          onLoadMore={paged.more ? paged.older : undefined}
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
                <span className="row gap-1 nowrap">
                  <span className="mono truncate">{refName(r.ref).name}</span>
                  {refName(r.ref).name === repo.default_branch && (
                    <Tag appearance="outline">default</Tag>
                  )}
                </span>
              ),
            },
            {
              key: "head",
              header: "Head",
              shrink: true,
              cell: (r) =>
                r.oid ? (
                  <span className="mono muted" title={r.oid}>
                    {shortOID(r.oid)}
                  </span>
                ) : null,
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
          ]}
        />
      </QueryState>
      {paged.pageError && <PageFailed error={paged.pageError} />}
    </>
  );
}

/** A branch as an address names it: none for the default branch, its name for any other. */
function branchRev(repo: CodeRepoDetail, r: CodeRefRow): string {
  const name = refName(r.ref).name;
  return name === repo.default_branch ? "" : name;
}

/** Who pushed a commit, by its push: the seat, or the credential and the seat it acts as. */
function PushedByCell({ by }: { by: CodePushedBy | undefined }) {
  const seatBadge = useSeatBadgeOf();
  // NOTHING WHERE THE PAGE NAMES NO PUSH: one read by a whole commit id is
  // answered with no stamp, and a node's index may not cover an old push.
  if (!by) return null;
  const seat = by.principal.seat;
  const name = seat ? seatBadge(seat).name : (by.principal.operator_id ?? "");
  return (
    <span title={by.principal.operator_id ? `through ${by.principal.operator_id}` : undefined}>
      {name}
    </span>
  );
}

/** A commit's subject, or what git left of one past its bound. */
export function subjectOf(c: { subject?: string; subject_bytes?: number }): string {
  if (c.subject) return c.subject;
  return c.subject_bytes ? `(a subject of ${c.subject_bytes} bytes)` : "(no message)";
}

/**
 * The history at the revision, of the path the address names or of every
 * path, newest first.
 */
export function Commits({ repo }: { repo: CodeRepoDetail }) {
  const now = useNow();
  const [rev, setRev] = useParam("ref", "", "filter");
  const [path, setPath] = useParam("path", "", "filter");
  const params = useMemo(
    () => ({ name: repo.name, ...(rev ? { ref: rev } : {}), ...(path ? { path } : {}) }),
    [repo.name, rev, path],
  );
  const hold = usePageHold<CodeGitAnswer>(params);
  const first = useCodeQuery("code_log", params, {
    enabled: Boolean(repo.head) || rev !== "",
    hold: hold.holding,
  });
  const paged = usePaged(
    hold,
    first,
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
  if (!repo.head && rev === "") {
    return (
      <p className="muted">Nobody has pushed a branch yet: {repo.default_branch} has no history.</p>
    );
  }
  return (
    <>
      <div className="toolbar">
        <RefPicker repo={repo} value={rev} onChoose={setRev} />
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
      </div>
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
          rows={rows}
          rowKey={(c) => c.oid}
          // GIT'S ORDER: every parent after every child.
          serverSorted
          rowHref={(c) => commitHref(repo.name, c.oid)}
          onLoadMore={paged.more ? paged.older : undefined}
          loadedNote={paged.more ? `${plural(rows.length, "commit")} loaded` : undefined}
          aria-label={`History of ${revisionWord(repo, rev)}`}
          columns={[
            {
              key: "subject",
              header: "Commit",
              floor: "16rem",
              phoneLead: true,
              cell: (c) => (
                <span className="row gap-1 nowrap">
                  <span className="mono muted" title={c.oid}>
                    {shortOID(c.oid)}
                  </span>
                  <span className="truncate">{subjectOf(c)}</span>
                </span>
              ),
            },
            {
              key: "author",
              header: "Author",
              shrink: true,
              cell: (c) => (
                <span title={c.author_email}>{c.author ?? `(${c.author_bytes ?? 0} bytes)`}</span>
              ),
            },
            {
              key: "authored",
              header: "Authored",
              shrink: true,
              cell: (c) => <TableDateCell value={c.authored_at} now={now} />,
            },
            {
              key: "pushed",
              header: "Pushed by",
              shrink: true,
              drop: 1,
              cell: (c) => <PushedByCell by={pushedBy[c.oid]} />,
            },
          ]}
        />
      </QueryState>
      {paged.pageError && <PageFailed error={paged.pageError} />}
    </>
  );
}
