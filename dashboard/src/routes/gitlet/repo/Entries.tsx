/**
 * A directory's entries as a code host lists them: each one's name, the commit
 * that last changed it and when, directories first once the whole directory is here.
 */
import { useMemo, useRef } from "react";
import { Button, Skeleton, VisuallyHidden } from "@crewlethq/ui";
import { baseName, shortOID } from "~/lib/code.ts";
import { fmtCount, fmtDateTime, plural, relTime } from "~/lib/format.ts";
import type { CodeLogCommit, CodeRepoDetail, CodeTreeEntry } from "~/protocol/index.ts";
import { useListKeys } from "../CodeList.tsx";
import { commitHref, repoHref } from "../paths.ts";
import { EntryGlyph, entryKind, useWhoName } from "../ui.tsx";
import { sortEntries, useTreeCommits, type TreeCommits } from "./entries.ts";
import { subjectOf } from "./History.tsx";
import { REFUSED_CODES } from "./marks.ts";
import { revisionWord } from "./refs.tsx";

/** Gitlet wrote a commit, by its own fields alone: the page holds no push or marks for an entry's. */
const byGitlet = (c: CodeLogCommit) => (c.parent_count ?? 0) >= 2 && c.engine_committed === true;

export function Entries({
  repo,
  rev,
  path,
  commit,
  entries,
  total,
  now,
}: {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  /** The commit the listing resolved, whole, or none while it is read. */
  commit: string;
  entries: readonly CodeTreeEntry[];
  total: number;
  now: number;
}) {
  const table = useRef<HTMLTableElement>(null);
  const whole = entries.length >= total;
  const rows = useMemo(() => (whole ? sortEntries(entries) : entries), [whole, entries]);
  const keys = useMemo(() => rows.map((e) => e.path), [rows]);
  useListKeys({ root: table, keys });
  const last = useTreeCommits(repo.name, commit, path, entries);
  const where = revisionWord(repo, rev);
  return (
    <>
      <table ref={table} className="code-entries">
        <caption className="sr-only">
          {path ? `Files in ${path} at ${where}` : `Files in ${where}`}
        </caption>
        {/* The widths live on the columns: a fixed table takes them from here, not the cells. */}
        <colgroup>
          <col className="code-entries__col-name" />
          <col />
          <col className="code-entries__col-when" />
        </colgroup>
        <thead className="sr-only">
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Last commit</th>
            <th scope="col">Updated</th>
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td colSpan={3} className="code-entries__empty">
                This directory is empty
              </td>
            </tr>
          ) : (
            rows.map((e) => (
              <EntryRow key={e.path} repo={repo.name} rev={rev} entry={e} last={last} now={now} />
            ))
          )}
        </tbody>
      </table>
      <EntriesFoot last={last} loaded={entries.length} total={total} />
    </>
  );
}

function EntryRow({
  repo,
  rev,
  entry,
  last,
  now,
}: {
  repo: string;
  rev: string;
  entry: CodeTreeEntry;
  last: TreeCommits;
  now: number;
}) {
  const whoName = useWhoName();
  const name = baseName(entry.path);
  const answered = last.of(entry.path);
  const c = answered?.commit ? last.commit(answered.commit) : undefined;
  const gitlet = c ? byGitlet(c) : false;
  const at = c ? (gitlet ? c.committed_at : c.authored_at) : "";
  const submodule = entry.type === "commit";
  return (
    <tr data-key={entry.path} tabIndex={submodule ? -1 : undefined}>
      <td className="code-entries__name">
        <span className="code-tree-entry">
          <EntryGlyph entry={entry} />
          {submodule ? (
            // A SUBMODULE IS ANOTHER REPOSITORY'S COMMIT: nothing of it is here to open.
            <span className="code-tree-entry__name" title={`${name} @ ${entry.oid}`}>
              {name} <span className="muted mono">@ {shortOID(entry.oid)}</span>
            </span>
          ) : (
            <a
              className="code-tree-entry__name"
              data-list-link
              href={repoHref(repo, { ref: rev, path: entry.path })}
            >
              {name}
            </a>
          )}
          <VisuallyHidden>{entryKind(entry)}</VisuallyHidden>
        </span>
      </td>
      <td className="code-entries__commit">
        {c ? (
          <a
            className="code-entries__subject"
            href={commitHref(repo, c.oid)}
            title={`${subjectOf(c)} by ${gitlet ? "Gitlet" : whoName(c.author ?? "")}`}
          >
            {subjectOf(c)}
          </a>
        ) : answered?.out_of_time ? (
          <span
            className="code-entries__note"
            title="Finding its last commit ran past one read's time. Reload to look again."
          >
            Not found in one read's time
          </span>
        ) : answered ? (
          <span className="code-entries__note">No file in it was ever committed</span>
        ) : last.done || last.failed ? null : (
          <Skeleton variant="text" rows={1} label={`Reading ${name}'s last commit`} />
        )}
      </td>
      <td className="code-entries__when">
        {c && (
          <time dateTime={at} title={fmtDateTime(at)}>
            {relTime(at, now)}
          </time>
        )}
      </td>
    </tr>
  );
}

/** Under the entries: how many are loaded of a paged directory, and why the last commits are missing. */
function EntriesFoot({
  last,
  loaded,
  total,
}: {
  last: TreeCommits;
  loaded: number;
  total: number;
}) {
  const refused = last.failed !== null && REFUSED_CODES.has(last.failed.code);
  if (loaded >= total && !last.failed) return null;
  return (
    <div className="code-entries__foot" role="status">
      {loaded < total && (
        <span>{`${fmtCount(loaded)} of ${plural(total, "entry", "entries")} loaded`}</span>
      )}
      {last.failed &&
        (refused ? (
          <span>
            {`Each entry's last commit is unavailable${last.failed.sentence ? `: ${last.failed.sentence}` : ""}.`}
          </span>
        ) : (
          <span>
            Each entry's last commit could not be read.{" "}
            <Button size="small" variant="ghost" onClick={last.retry}>
              Retry
            </Button>
          </span>
        ))}
    </div>
  );
}
