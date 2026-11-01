/**
 * A repository's files at a revision: a directory's listing, a file's lines
 * with the ones an address picks marked, and the blame of a file.
 *
 * # The address is the place
 *
 * `ref=` is the revision (none is the default branch), `path=` the directory
 * or the file from the root, `lens=blame` who last changed each line, and
 * `lines=` the lines picked — `12`, or `12-20` — which pressing a line's
 * number sets (Shift extends the run), so a link to a line is the address on
 * screen. Opening a directory or a file is a new place in the history; picking
 * lines and changing the revision or the lens replace it.
 *
 * # One read for a directory or a file
 *
 * The root is a directory's listing (`code_tree`); any other path is asked as a
 * file (`code_blob`), which the git node answers with the directory's listing
 * where the path names one, so a path is one read whatever it turns out to be.
 * A listing and a file are read a page at a time, each page joined to the one
 * before it (`lib/usePaged.ts`), and a file's pages
 * concatenate to its bytes, a line longer than a page continued on the next.
 *
 * # A Permalink names the commit
 *
 * A revision named by a branch moves with the branch. Its read answers the
 * commit the branch resolved to (`resolved`), and the Permalink is the same
 * place at that commit's whole id — the address that says the same thing
 * tomorrow, and an answer the engine marks immutable.
 */

import { useEffect, useMemo, useRef } from "react";
import { Button, ButtonLink, Callout, Skeleton, Stack } from "@crewlethq/ui";
import { useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import {
  baseName,
  isWholeOID,
  linesValue,
  parseLines,
  pathSteps,
  shortOID,
  type LineRun,
} from "~/lib/code.ts";
import { fmtBytes, fmtCount, fmtDateTime, plural } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged, type Paged } from "~/lib/usePaged.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type {
  CodeBlameCommit,
  CodeBlameLine,
  CodeBlobAnswer,
  CodeGitAnswer,
  CodeRepoDetail,
  CodeTreeEntry,
} from "~/protocol/index.ts";
import { commitHref, repoHref } from "../paths.ts";
import { RefPicker, revisionWord } from "./refs.tsx";

/** What one page of a path read holds: a listing's entries, or one page of a file. */
type PathPart = CodeTreeEntry | CodeBlobAnswer;

const entriesOf = (a: CodeGitAnswer): readonly PathPart[] =>
  a.tree ? (a.tree.entries ?? []) : a.blob ? [a.blob] : [];
// AN ENTRY BY ITS PATH, a page of a file by where it begins: the line and the
// byte into it, which no two pages of one file share.
const partId = (p: PathPart): string =>
  "mode" in p ? `entry:${p.path}` : `page:${p.start_line ?? 0}:${p.start_byte ?? 0}`;
const nextOf = (a: CodeGitAnswer): string => a.next ?? "";

/** The commit a read's revision resolved to, whole: its own where it named one. */
function commitOf(rev: string, answer: CodeGitAnswer | null, param = "ref"): string {
  if (isWholeOID(rev)) return rev;
  return answer?.resolved?.[param]?.oid ?? "";
}

export function Files({ repo }: { repo: CodeRepoDetail }) {
  const [ref, setRef] = useParam("ref", "", "filter");
  const [path] = useParam("path", "", "filter");
  const [lens, setLens] = useParam("lens", "", "filter");
  const [lines, setLines] = useParam("lines", "", "filter");
  if (!repo.head && ref === "") {
    return (
      <p className="muted">Nobody has pushed a branch yet: {repo.default_branch} holds no file.</p>
    );
  }
  const place = { repo, rev: ref, path, onRev: setRef };
  return path === "" ? (
    <RootListing {...place} />
  ) : (
    <PathView
      {...place}
      blame={lens === "blame"}
      onLens={(blame) => setLens(blame ? "blame" : "")}
      picked={parseLines(lines)}
      onPick={(run) => setLines(run ? linesValue(run) : "")}
    />
  );
}

interface Place {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  onRev: (rev: string) => void;
}

/** The repository's root at the revision: its listing. */
function RootListing({ repo, rev, path, onRev }: Place) {
  const params = useMemo(
    () => ({ name: repo.name, ...(rev ? { ref: rev } : {}) }),
    [repo.name, rev],
  );
  const first = useCodeQuery("code_tree", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_tree", { ...params, cursor }),
    entriesOf,
    partId,
    nextOf,
  );
  return (
    <Stack gap={3}>
      <FilesBar
        repo={repo}
        rev={rev}
        path={path}
        onRev={onRev}
        commit={commitOf(rev, first.data)}
      />
      <PathBody repo={repo} rev={rev} path={path} first={first} paged={paged} />
    </Stack>
  );
}

/** A path under the root: a directory's listing, or a file's lines or its blame. */
function PathView({
  repo,
  rev,
  path,
  onRev,
  blame,
  onLens,
  picked,
  onPick,
}: Place & {
  blame: boolean;
  onLens: (blame: boolean) => void;
  picked: LineRun | null;
  onPick: (run: LineRun | null) => void;
}) {
  const params = useMemo(
    () => ({ name: repo.name, path, ...(rev ? { ref: rev } : {}) }),
    [repo.name, path, rev],
  );
  const first = useCodeQuery("code_blob", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_blob", { ...params, cursor }),
    entriesOf,
    partId,
    nextOf,
  );
  const file = first.data?.blob;
  const commit = commitOf(rev, first.data);
  return (
    <Stack gap={3}>
      <FilesBar
        repo={repo}
        rev={rev}
        path={path}
        onRev={onRev}
        commit={commit}
        lens={file ? { blame, onLens } : undefined}
        lines={picked}
      />
      {file && blame ? (
        // THE BLAME IS READ AT THE COMMIT THE FILE WAS, where the file's read
        // resolved one: the lines it attributes are the lines on screen.
        <Blame repo={repo} rev={commit || rev} path={path} picked={picked} onPick={onPick} />
      ) : (
        <PathBody
          repo={repo}
          rev={rev}
          path={path}
          first={first}
          paged={paged}
          picked={picked}
          onPick={onPick}
        />
      )}
    </Stack>
  );
}

/**
 * The bar above the files: the revision, the path back up to the root, the
 * lens of a file, its history, and the Permalink.
 */
function FilesBar({
  repo,
  rev,
  path,
  onRev,
  commit,
  lens,
  lines,
}: Place & {
  commit: string;
  lens?: { blame: boolean; onLens: (blame: boolean) => void };
  lines?: LineRun | null;
}) {
  const at = (p: string) => repoHref(repo.name, { tab: "files", ref: rev, path: p });
  return (
    <div className="toolbar">
      <RefPicker repo={repo} value={rev} onChoose={onRev} />
      <nav aria-label="Path" className="row gap-1 wrap mono">
        <a className="prose-link" href={at("")}>
          {repo.name}
        </a>
        {pathSteps(path).map((step) => (
          <span key={step.path} className="row gap-1">
            <span className="muted">/</span>
            {step.path === path ? (
              <span aria-current="location">{step.name}</span>
            ) : (
              <a className="prose-link" href={at(step.path)}>
                {step.name}
              </a>
            )}
          </span>
        ))}
      </nav>
      {lens && (
        <Button
          size="small"
          variant="ghost"
          aria-pressed={lens.blame}
          onClick={() => lens.onLens(!lens.blame)}
        >
          {lens.blame ? "Show the source" : "Blame"}
        </Button>
      )}
      <ButtonLink
        size="small"
        variant="ghost"
        href={repoHref(repo.name, { tab: "commits", ref: rev, path })}
      >
        History
      </ButtonLink>
      {commit && (
        <ButtonLink
          size="small"
          variant="ghost"
          title={`The same place at commit ${commit}`}
          href={repoHref(repo.name, {
            tab: "files",
            ref: commit,
            path,
            ...(lens?.blame ? { lens: "blame" as const } : {}),
            ...(lines ? { lines: linesValue(lines) } : {}),
          })}
        >
          Permalink
        </ButtonLink>
      )}
    </div>
  );
}

/** What a path read answered: a directory's listing, or a file's lines. */
function PathBody({
  repo,
  rev,
  path,
  first,
  paged,
  picked = null,
  onPick,
}: {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  first: QueryResult<CodeGitAnswer>;
  paged: Paged<PathPart, CodeGitAnswer>;
  picked?: LineRun | null;
  onPick?: (run: LineRun | null) => void;
}) {
  if (first.loading && !first.data) {
    return <Skeleton variant="text" rows={8} label={`Loading ${path || repo.name}`} />;
  }
  if (!first.data) {
    return (
      <QueryState error={first.error} refusal={first.refusal} detail={first.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  const more = <Pager paged={paged} what={first.data.blob ? "lines" : "entries"} />;
  if (first.data.blob) {
    const pages = paged.items.filter((p): p is CodeBlobAnswer => !("mode" in p));
    return (
      <>
        <Source path={path} pages={pages} picked={picked} onPick={onPick} />
        {more}
      </>
    );
  }
  const entries = paged.items.filter((p): p is CodeTreeEntry => "mode" in p);
  return (
    <>
      <Listing
        repo={repo}
        rev={rev}
        entries={entries}
        total={first.data.tree?.total ?? entries.length}
      />
      {more}
    </>
  );
}

/** Load more of a listing or a file, while there is more. */
function Pager({ paged, what }: { paged: Paged<PathPart, CodeGitAnswer>; what: string }) {
  if (!paged.more && !paged.pageError) return null;
  return (
    <div className="row gap-2 wrap">
      {paged.more && (
        <Button
          size="small"
          variant="secondary"
          onClick={paged.next}
          disabled={paged.paging}
          loading={paged.paging}
        >
          {`Load more ${what}`}
        </Button>
      )}
      {paged.pageError && <PageFailed error={paged.pageError} />}
    </div>
  );
}

/** What an entry is, in a word, by git's mode. */
function entryKind(e: CodeTreeEntry): string {
  switch (e.mode) {
    case "040000":
      return "Directory";
    case "100755":
      return "Executable";
    case "120000":
      return "Link";
    case "160000":
      return "Submodule";
  }
  return "File";
}

/** A directory's entries in git's own order, each opening its directory or its file. */
function Listing({
  repo,
  rev,
  entries,
  total,
}: {
  repo: CodeRepoDetail;
  rev: string;
  entries: readonly CodeTreeEntry[];
  total: number;
}) {
  if (entries.length === 0) return <p className="muted">This directory is empty.</p>;
  return (
    <table className="code-tree" aria-label="Files">
      <caption className="t-caption">
        {entries.length < total
          ? `${fmtCount(entries.length)} of ${plural(total, "entry", "entries")}`
          : plural(total, "entry", "entries")}
      </caption>
      <thead>
        <tr>
          <th scope="col">Name</th>
          <th scope="col">Kind</th>
          <th scope="col">Size</th>
        </tr>
      </thead>
      <tbody>
        {entries.map((e) => (
          <tr key={e.path}>
            <td className="mono">
              {e.type === "commit" ? (
                // A SUBMODULE IS ANOTHER REPOSITORY'S COMMIT: nothing of it is
                // here to open.
                <span title={`A submodule at ${e.oid}`}>
                  {baseName(e.path)} <span className="muted">@ {shortOID(e.oid)}</span>
                </span>
              ) : (
                <a
                  className="prose-link"
                  href={repoHref(repo.name, { tab: "files", ref: rev, path: e.path })}
                >
                  {baseName(e.path)}
                  {e.type === "tree" ? "/" : ""}
                </a>
              )}
            </td>
            <td className="muted">{entryKind(e)}</td>
            <td className="muted">{e.type === "blob" ? fmtBytes(e.size ?? 0) : ""}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/**
 * A file's lines, numbered, the ones the address picks ruled — never tinted:
 * a pick is the reader's place, not a state of the line. Pressing a number
 * picks its line, Shift extends the run from the one picked, and pressing the
 * one line picked again lets it go.
 */
function Source({
  path,
  pages,
  picked,
  onPick,
}: {
  path: string;
  pages: readonly CodeBlobAnswer[];
  picked: LineRun | null;
  onPick?: (run: LineRun | null) => void;
}) {
  const head = pages[0];
  const lines = useMemo(() => {
    const text = pages.map((p) => p.text ?? "").join("");
    const split = text.split("\n");
    // THE TEXT'S OWN END: the newline that closes the last line splits into
    // one empty string after it, which is no line.
    if (split.length > 0 && split[split.length - 1] === "") split.pop();
    return split;
  }, [pages]);
  const from = head?.start_line ?? 1;
  usePickInView(path, picked, lines.length > 0);
  if (!head) return null;
  if (head.binary) {
    return (
      <p className="muted">{`A binary file of ${fmtBytes(head.size)}: git draws no lines of it.`}</p>
    );
  }
  if (head.lines === 0) return <p className="muted">This file is empty.</p>;
  const shown = from + lines.length - 1;
  const pick = (n: number, extend: boolean) => {
    if (!onPick) return;
    if (extend && picked) {
      onPick({ start: Math.min(picked.start, n), end: Math.max(picked.end, n) });
    } else if (picked && picked.start === n && picked.end === n) {
      onPick(null);
    } else {
      onPick({ start: n, end: n });
    }
  };
  return (
    <Stack gap={2}>
      <p className="t-caption">
        {`${plural(head.lines, "line")} · ${fmtBytes(head.size)}`}
        {picked && picked.start > shown && ` · line ${picked.start} is past the lines loaded`}
      </p>
      <div className="code-source" role="region" aria-label={`The lines of ${path}`}>
        {lines.map((text, i) => {
          const n = from + i;
          const on = picked !== null && n >= picked.start && n <= picked.end;
          return (
            <div key={n} id={`L${n}`} className="code-source__line" data-picked={on || undefined}>
              <button
                type="button"
                className="code-source__number"
                aria-label={`Line ${n}`}
                aria-pressed={on}
                onClick={(event) => pick(n, event.shiftKey)}
              >
                {n}
              </button>
              <span className="code-source__text">{text}</span>
            </div>
          );
        })}
      </div>
    </Stack>
  );
}

/**
 * Scroll the first line an address picks into view, ONCE a file's lines have
 * drawn — an address that names a line is opened at it, and a press that
 * picks another moves nothing under the reader's pointer.
 */
function usePickInView(path: string, picked: LineRun | null, drawn: boolean) {
  const done = useRef("");
  useEffect(() => {
    if (!picked || !drawn || done.current === path) return;
    done.current = path;
    document.getElementById(`L${picked.start}`)?.scrollIntoView?.({ block: "center" });
  }, [path, picked, drawn]);
}

/**
 * Who last changed each line of a file: each run of lines one commit last
 * changed is headed by that commit — its id, its author and when — a page at a
 * time, as git's blame attributes it.
 */
function Blame({
  repo,
  rev,
  path,
  picked,
  onPick,
}: {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  picked: LineRun | null;
  onPick: (run: LineRun | null) => void;
}) {
  const params = useMemo(
    () => ({ name: repo.name, path, ...(rev ? { ref: rev } : {}) }),
    [repo.name, path, rev],
  );
  const first = useCodeQuery("code_blame", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_blame", { ...params, cursor }),
    (a: CodeGitAnswer): readonly CodeBlameLine[] => a.blame?.lines ?? [],
    (l: CodeBlameLine) => String(l.line),
    nextOf,
  );
  const commits = useMemo(() => {
    const all: Record<string, CodeBlameCommit> = {};
    for (const a of paged.answers) Object.assign(all, a.blame?.commits ?? {});
    return all;
  }, [paged.answers]);
  usePickInView(`${path}#blame`, picked, paged.items.length > 0);
  if (first.loading && !first.data)
    return <Skeleton variant="text" rows={8} label={`Blaming ${path}`} />;
  if (!first.data) {
    return (
      <QueryState error={first.error} refusal={first.refusal} detail={first.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  const rows = paged.items;
  if (rows.length === 0) return <p className="muted">This file is empty: no line to blame.</p>;
  return (
    <Stack gap={2}>
      <table className="code-blame" aria-label={`Who last changed each line of ${path}`}>
        <tbody>
          {rows.map((l, i) => {
            const c = commits[l.commit];
            const run = i === 0 || rows[i - 1]?.commit !== l.commit;
            const on = picked !== null && l.line >= picked.start && l.line <= picked.end;
            return (
              <tr
                key={l.line}
                id={`L${l.line}`}
                data-run={run || undefined}
                data-picked={on || undefined}
              >
                <td className="code-blame__commit">
                  {run && (
                    <span title={c?.subject ?? l.commit}>
                      <a className="mono prose-link" href={commitHref(repo.name, l.commit)}>
                        {shortOID(l.commit)}
                      </a>{" "}
                      {c?.author ?? ""}
                      <span className="muted"> {c ? fmtDateTime(c.authored_at) : ""}</span>
                    </span>
                  )}
                </td>
                <td className="code-blame__number">
                  <button
                    type="button"
                    className="code-source__number"
                    aria-label={`Line ${l.line}`}
                    aria-pressed={on}
                    onClick={(event) =>
                      onPick(
                        event.shiftKey && picked
                          ? {
                              start: Math.min(picked.start, l.line),
                              end: Math.max(picked.end, l.line),
                            }
                          : { start: l.line, end: l.line },
                      )
                    }
                  >
                    {l.line}
                  </button>
                </td>
                <td className="code-blame__text mono">
                  {l.text ?? `(a line of ${fmtBytes(l.text_bytes ?? 0)})`}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {(paged.more || paged.pageError) && (
        <div className="row gap-2 wrap">
          {paged.more && (
            <Button
              size="small"
              variant="secondary"
              onClick={paged.next}
              disabled={paged.paging}
              loading={paged.paging}
            >
              Load more lines
            </Button>
          )}
          {paged.pageError && <PageFailed error={paged.pageError} />}
        </div>
      )}
    </Stack>
  );
}

/** The names a README goes by, the one a reader writes first first. */
const README_NAMES = ["readme.md", "readme.markdown", "readme", "readme.txt"];

/**
 * The README at the default branch's root, as the overview draws it: the
 * root's listing names it, and its file is read at the commit that listing
 * resolved, so the two are one revision. Markdown is rendered by the
 * dashboard's own renderer, which builds every element itself, so text a
 * pusher wrote cannot introduce one; any other README is drawn as its text.
 */
export function Readme({ repo }: { repo: CodeRepoDetail }) {
  const root = useCodeQuery("code_tree", { name: repo.name });
  const entries = root.data?.tree?.entries ?? [];
  const entry = README_NAMES.map((name) =>
    entries.find((e) => e.type === "blob" && baseName(e.path).toLowerCase() === name),
  ).find(Boolean);
  const commit = commitOf("", root.data);
  const file = useCodeQuery(
    "code_blob",
    { name: repo.name, path: entry?.path ?? "", ...(commit ? { ref: commit } : {}) },
    { enabled: Boolean(entry) },
  );
  if (root.loading && !root.data)
    return <Skeleton variant="text" rows={4} label="Loading the README" />;
  if (root.error) {
    return (
      <QueryState error={root.error} refusal={root.refusal} detail={root.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  if (!entry) {
    return (
      <p className="muted">
        {`No README at the root of ${repo.default_branch}.`}{" "}
        <a className="prose-link" href={repoHref(repo.name, { tab: "files" })}>
          Browse its files
        </a>
      </p>
    );
  }
  const blob = file.data?.blob;
  return (
    <section aria-label={baseName(entry.path)} className="code-readme">
      <p className="t-label">
        <a
          className="prose-link"
          href={repoHref(repo.name, { tab: "files", ref: commit, path: entry.path })}
        >
          {baseName(entry.path)}
        </a>
        <span className="muted"> at {revisionWord(repo, commit)}</span>
      </p>
      {file.loading && !blob ? (
        <Skeleton variant="text" rows={4} label="Loading the README" />
      ) : file.error ? (
        <QueryState error={file.error} refusal={file.refusal} detail={file.detail} loading={false}>
          {null}
        </QueryState>
      ) : blob?.binary ? (
        <p className="muted">It is a binary file.</p>
      ) : /\.(md|markdown)$/i.test(entry.path) ? (
        <div className="prose md">{renderMarkdown(blob?.text ?? "")}</div>
      ) : (
        <pre className="code-readme__text">{blob?.text ?? ""}</pre>
      )}
      {file.data?.next && (
        <Callout variant="neutral">It runs past one page: open it to read the rest.</Callout>
      )}
    </section>
  );
}
