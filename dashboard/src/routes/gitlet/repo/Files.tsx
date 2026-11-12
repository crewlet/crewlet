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
 * file (`code_blob`), which the Gitlet node answers with the directory's listing
 * where the path names one, so a path is one read whatever it turns out to be.
 * A listing and a file are read a page at a time, each page joined to the one
 * before it (`lib/usePaged.ts`), and a file's pages
 * concatenate to its bytes, a line longer than a page continued on the next.
 *
 * # A file opens at the line its address picks
 *
 * A page is at most gitlet.MaxGitAnswerBytes, about fifteen hundred lines of
 * ordinary source, so a link to a line further in — a search hit, ⌘K, a
 * Permalink — opened on lines that did not hold it, and the reader paged
 * forward to find what the link had named. A path the address picks a line of
 * is now read, the FIRST time, from PICK_CONTEXT lines before that line
 * (`start_line`, [openingLine]), decided before any read: every read of a
 * file streams the whole of it from git, and every page answers the file's
 * line count, so a read from line 1 to learn where to read next would be a
 * second stream nothing draws. It is paged forward from there as before, and
 * "Load earlier lines" reads back towards line 1 a page's worth at a time
 * (`start_line` and `end_line`). A line past the file's end, which the engine
 * refuses as a `start_line`, reads the file again from line 1 and says so.
 * A blame is asked a range of at most CODE_MAX_BLAME_LINES lines at a time —
 * the engine's whole range for one — so it is opened at the picked line the
 * same way, its first range the one that holds the line, read range after
 * range forward, and back the same way.
 *
 * # A Permalink names the commit
 *
 * A revision named by a branch moves with the branch. Its read answers the
 * commit the branch resolved to (`resolved`), and the Permalink is the same
 * place at that commit's whole id — the address that says the same thing
 * tomorrow, and an answer the engine marks immutable.
 */

import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  EmptyValue,
  Skeleton,
  Stack,
  TableKeyCell,
  TableNumberCell,
  TableTextCell,
} from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { CODE_MAX_BLAME_LINES } from "~/contract/code.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import {
  EARLIER_FILE_LINES,
  baseName,
  isWholeOID,
  lineToOpenAt,
  linesValue,
  openingLine,
  parseLines,
  pathSteps,
  shortOID,
  type LineRun,
} from "~/lib/code.ts";
import { fmtBytes, fmtCount, fmtDateTime, plural } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { codeRest, useCodeQuery, type CodeParams } from "~/lib/useCodeQuery.ts";
import { usePaged, type Paged } from "~/lib/usePaged.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import {
  queryErrorCode,
  type CodeBlameCommit,
  type CodeBlameLine,
  type CodeBlobAnswer,
  type CodeGitAnswer,
  type CodeRepoDetail,
  type CodeTreeEntry,
} from "~/protocol/index.ts";
import { commitHref, repoHref } from "../paths.ts";
import { RefPicker, revisionWord, type Revisions } from "./refs.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";

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

export function Files({ repo, revisions }: { repo: CodeRepoDetail; revisions: Revisions }) {
  const [ref, setRef] = useParam("ref", "", "filter");
  const [path] = useParam("path", "", "filter");
  const [lens, setLens] = useParam("lens", "", "filter");
  const [lines, setLines] = useParam("lines", "", "filter");
  if (!repo.head && ref === "") {
    return (
      <p className="muted">Nobody has pushed a branch yet: {repo.default_branch} holds no file.</p>
    );
  }
  const place = { repo, revisions, rev: ref, path, onRev: setRef };
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
  /** The repository's branches and tags, read once by its page ([useRevisions]). */
  revisions: Revisions;
  rev: string;
  path: string;
  onRev: (rev: string) => void;
}

/** The repository's root at the revision: its listing. */
function RootListing({ repo, revisions, rev, path, onRev }: Place) {
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
        revisions={revisions}
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
  revisions,
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
  // THE LINE THE PATH IS READ FROM, taken once per revision and path from the
  // address it was opened with ([openingLine]): a link to line 2400 asks from
  // line 2350 in its one read, and a later press asks nothing.
  const [start, open] = useOpened(`${rev}\n${path}`, openingLine(picked));
  const base = useMemo(
    () => ({ name: repo.name, path, ...(rev ? { ref: rev } : {}) }),
    [repo.name, path, rev],
  );
  const params = useMemo(() => (start > 1 ? { ...base, start_line: start } : base), [base, start]);
  const first = useCodeQuery("code_blob", params);
  // A LINE PAST THE FILE'S END — a stale link — is refused as a `start_line`
  // (the engine's `bad_params`, naming the file's lines), and the file is read
  // again from line 1, where its caption says it has no such line. The line
  // is the one parameter the address added, so a refusal it did not cause is
  // refused again from line 1, and said there. React draws again at once with
  // the line moved, before anything of the refusal is shown.
  if (start > 1 && first.error === "bad_params") open(1);
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
        revisions={revisions}
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
        <Blame
          repo={repo}
          rev={commit || rev}
          path={path}
          total={file.lines}
          picked={picked}
          onPick={onPick}
        />
      ) : file && !file.binary && file.lines > 0 ? (
        <FileLines
          rev={rev}
          path={path}
          params={base}
          opened={start}
          opening={first}
          paged={paged}
          total={file.lines}
          picked={picked}
          onPick={onPick}
        />
      ) : (
        <PathBody repo={repo} rev={rev} path={path} first={first} paged={paged} />
      )}
    </Stack>
  );
}

/** The place a file's lines (or its blame) begin at, under the reading it was decided for. */
interface Opened {
  /** The file and the lens it was decided for: another starts again from its own opening line. */
  place: string;
  /** The first line read forward from: the opening line, or where the address's line needed them. */
  from: number;
}

/**
 * Where a file's lines (or its blame) begin: `initial` for a place first
 * opened — taken ONCE, as the place opens, from the address it was opened
 * with, so a press that picks another line later re-takes nothing — and
 * wherever `open` moves them after.
 */
function useOpened(place: string, initial: number): [number, (from: number) => void] {
  const [opened, setOpened] = useState<Opened>({ place, from: initial });
  let now = opened;
  if (opened.place !== place) {
    // A PLACE OPENED SINCE THE LAST DRAWING keeps the line decided for it
    // now: React draws again at once with it held, before anything is shown,
    // so a pick that moves later is never read as where the place opened.
    now = { place, from: initial };
    setOpened(now);
  }
  const open = useCallback((at: number) => setOpened({ place, from: at }), [place]);
  return [now.from, open];
}

/**
 * Move a file's lines (or its blame) to where the line the address picks is
 * drawn, once the lines read are SETTLED — the answer on screen is the one
 * asked from `from` — and do not draw it ([lineToOpenAt]). A press picks a
 * line on screen and so never moves them; an address that names one off
 * screen does.
 */
function useOpenAt(
  open: (from: number) => void,
  picked: LineRun | null,
  from: number,
  drawn: LineRun | null,
  settled: boolean,
  total: number,
) {
  const next = settled ? lineToOpenAt(picked, from, drawn, total) : null;
  useEffect(() => {
    if (next !== null) open(next);
  }, [open, next]);
}

/** What "Load earlier lines" holds: every answer read before the lines read forward, oldest first. */
interface Earlier {
  answers: readonly CodeGitAnswer[];
  /** The first line held: where the lines read forward begin until a step back is read. */
  from: number;
  reading: boolean;
  error: QueryErrorCode | null;
  read: () => void;
}

interface EarlierState {
  place: string;
  answers: readonly CodeGitAnswer[];
  from: number;
  reading: boolean;
  error: QueryErrorCode | null;
}

const NO_ANSWERS: readonly CodeGitAnswer[] = [];

/**
 * Read a file's lines (or its blame) BACKWARDS from `from`, `step` lines at a
 * time — EARLIER_FILE_LINES for a file, a page's worth, and
 * CODE_MAX_BLAME_LINES for a blame, the most one names — each step a range
 * (`start_line`, `end_line`) followed through its pages to its end, so it
 * meets the lines after it with no line between them. What it holds belongs
 * to `place`, the file, the lens and the line read forward from: another
 * starts with none, and a step still on its way for one that ended is dropped.
 */
function useEarlier(
  place: string,
  from: number,
  step: number,
  ask: (start: number, end: number, cursor: string) => Promise<CodeGitAnswer>,
): Earlier {
  const [state, setState] = useState<EarlierState>({
    place,
    answers: NO_ANSWERS,
    from,
    reading: false,
    error: null,
  });
  const now: EarlierState =
    state.place === place
      ? state
      : { place, answers: NO_ANSWERS, from, reading: false, error: null };
  const held = useRef({ now, step, ask });
  held.current = { now, step, ask };
  const read = useCallback(() => {
    const { now: at, step, ask } = held.current;
    if (at.reading || at.from <= 1) return;
    const start = Math.max(1, at.from - step);
    const end = at.from - 1;
    setState({ ...at, reading: true, error: null });
    void (async () => {
      const got: CodeGitAnswer[] = [];
      try {
        let cursor = "";
        do {
          const answer = await ask(start, end, cursor);
          got.push(answer);
          cursor = answer.next ?? "";
        } while (cursor);
      } catch (err) {
        const error = queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed";
        setState((s) => (s.place === at.place ? { ...s, reading: false, error } : s));
        return;
      }
      setState((s) =>
        s.place === at.place
          ? { ...s, answers: [...got, ...s.answers], from: start, reading: false, error: null }
          : s,
      );
    })();
  }, []);
  return { answers: now.answers, from: now.from, reading: now.reading, error: now.error, read };
}

/** Read the lines before those on screen, while there are any. */
function EarlierLines({ earlier }: { earlier: Earlier }) {
  if (earlier.from <= 1) return null;
  return (
    <div className="row gap-2 wrap">
      <Button
        size="small"
        variant="secondary"
        onClick={earlier.read}
        disabled={earlier.reading}
        loading={earlier.reading}
      >
        Load earlier lines
      </Button>
      {earlier.error && <PageFailed error={earlier.error} />}
    </div>
  );
}

const blobOf = (a: CodeGitAnswer): readonly CodeBlobAnswer[] => (a.blob ? [a.blob] : []);
const pageId = (p: CodeBlobAnswer): string => `page:${p.start_line ?? 0}:${p.start_byte ?? 0}`;

/**
 * A file's lines: from `opened`, the line the path's own read began at — 1,
 * or PICK_CONTEXT lines before the line the address picked as the path opened
 * — and drawn from that read and its `paged` pages, so a file is read once
 * wherever its link points. Only where those do not draw the picked line —
 * a line longer than a page above it, or an address moved to another line
 * since — are the lines read again from where it needs them ([useOpenAt]), a
 * second question read a page at a time forward as the first is; the lines
 * before them are a press away.
 */
function FileLines({
  rev,
  path,
  params,
  opened,
  opening,
  paged: fromOpening,
  total,
  picked,
  onPick,
}: {
  rev: string;
  path: string;
  /** The path's read without a line: what every other read of its lines adds its own to. */
  params: CodeParams;
  opened: number;
  opening: QueryResult<CodeGitAnswer>;
  paged: Paged<PathPart, CodeGitAnswer>;
  total: number;
  picked: LineRun | null;
  onPick: (run: LineRun | null) => void;
}) {
  const place = `${rev}\n${path}\n${opened}`;
  const [from, open] = useOpened(place, opened);
  const moved = from !== opened;
  const atParams = useMemo(() => ({ ...params, start_line: from }), [params, from]);
  const atLine = useCodeQuery("code_blob", atParams, { enabled: moved });
  const fromLine = usePaged(
    atLine,
    atParams,
    (cursor) => codeRest.query("code_blob", { ...atParams, cursor }),
    blobOf,
    pageId,
    nextOf,
  );
  const first = moved ? atLine : opening;
  const earlier = useEarlier(`${place}\n${from}`, from, EARLIER_FILE_LINES, (start, end, cursor) =>
    codeRest.query("code_blob", {
      ...params,
      start_line: start,
      end_line: end,
      ...(cursor ? { cursor } : {}),
    }),
  );
  // EACH LIST OF PAGES KEPT for as long as its pages are — `items` changes
  // only when a page arrives — so the text is joined and split once a page,
  // never on a render that brought none: a line pressed, or the repository's
  // row read again every minute.
  const openedPages = useMemo(
    () => fromOpening.items.filter((p): p is CodeBlobAnswer => !("mode" in p)),
    [fromOpening.items],
  );
  const forward = moved ? fromLine.items : openedPages;
  const pages = useMemo(
    () => (earlier.answers.length ? [...earlier.answers.flatMap(blobOf), ...forward] : forward),
    [earlier.answers, forward],
  );
  const head = pages[0];
  const foot = pages[pages.length - 1];
  const settled = (first.data?.blob?.start_line ?? 1) === from && forward.length > 0;
  const drawn = head && foot ? { start: head.start_line ?? 1, end: foot.end_line ?? total } : null;
  useOpenAt(open, picked, from, drawn, settled, total);
  if (!first.data) {
    return first.loading ? (
      <Skeleton variant="text" rows={8} label={`Loading ${path} from line ${from}`} />
    ) : (
      <QueryState error={first.error} refusal={first.refusal} detail={first.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  const more = moved ? fromLine : fromOpening;
  return (
    <>
      <EarlierLines earlier={earlier} />
      <Source
        path={path}
        place={`${place}\n${from}`}
        pages={pages}
        picked={picked}
        onPick={onPick}
      />
      <Pager paged={more} what="lines" />
    </>
  );
}

/**
 * The bar above the files: the revision, the path back up to the root, the
 * lens of a file, its history, and the Permalink.
 */
function FilesBar({
  repo,
  revisions,
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
    <PageToolbar>
      <RefPicker repo={repo} revisions={revisions} value={rev} onChoose={onRev} />
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
    </PageToolbar>
  );
}

/**
 * What a path read answered where it is not a file's lines ([FileLines]): a
 * directory's listing, or a file that has none — empty, or binary — said so.
 */
function PathBody({
  repo,
  rev,
  path,
  first,
  paged,
}: {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  first: QueryResult<CodeGitAnswer>;
  paged: Paged<PathPart, CodeGitAnswer>;
}) {
  // A FILE'S PAGES AND A LISTING'S ENTRIES, kept for as long as the pages
  // read are — `paged.items` changes only when a page arrives — so the file's
  // text is joined and split once a page, never on a render that brought
  // none: a line pressed, or the repository's row read again every minute.
  const pages = useMemo(
    () => paged.items.filter((p): p is CodeBlobAnswer => !("mode" in p)),
    [paged.items],
  );
  const entries = useMemo(
    () => paged.items.filter((p): p is CodeTreeEntry => "mode" in p),
    [paged.items],
  );
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
  if (first.data.blob) return <Source path={path} place={path} pages={pages} picked={null} />;
  return (
    <>
      <Listing
        repo={repo}
        rev={rev}
        entries={entries}
        total={first.data.tree?.total ?? entries.length}
      />
      <Pager paged={paged} what="entries" />
    </>
  );
}

/** Load more of a listing, a file or a blame, while there is more. */
function Pager({
  paged,
  what,
}: {
  paged: Pick<Paged<unknown, unknown>, "more" | "next" | "paging" | "pageError">;
  what: string;
}) {
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

/**
 * A directory's entries, each opening its directory or its file, on the
 * grid every list is drawn with.
 *
 * IN GIT'S ORDER, AND SORTED BY ITS HEADS ONLY ONCE THE WHOLE DIRECTORY IS
 * HERE: the engine pages a long directory, and a sort of the pages loaded
 * would answer a different question from the one the heads ask.
 */
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
  const whole = entries.length >= total;
  const sorts = <K,>(by: (e: CodeTreeEntry) => K) => (whole ? by : undefined);
  return (
    <DataGrid<CodeTreeEntry>
      name="files"
      rows={[...entries]}
      rowKey={(e) => e.path}
      serverSorted={!whole}
      aria-label="Files"
      loadedNote={
        whole ? undefined : `${fmtCount(entries.length)} of ${plural(total, "entry", "entries")}`
      }
      empty={{ title: "This directory is empty", icon: "folder" }}
      columns={[
        {
          key: "name",
          header: "Name",
          floor: "12rem",
          phoneLead: true,
          sortValue: sorts((e) => baseName(e.path).toLowerCase()),
          cell: (e) =>
            e.type === "commit" ? (
              // A SUBMODULE IS ANOTHER REPOSITORY'S COMMIT: nothing of it is
              // here to open.
              <TableKeyCell
                value={`${baseName(e.path)} @ ${e.oid}`}
                text={`${baseName(e.path)} @ ${shortOID(e.oid)}`}
              />
            ) : (
              <TableKeyCell
                value={`${baseName(e.path)}${e.type === "tree" ? "/" : ""}`}
                href={repoHref(repo.name, { tab: "files", ref: rev, path: e.path })}
              />
            ),
        },
        {
          key: "kind",
          header: "Kind",
          shrink: true,
          drop: 1,
          sortValue: sorts(entryKind),
          cell: (e) => <TableTextCell>{entryKind(e)}</TableTextCell>,
        },
        {
          key: "size",
          header: "Size",
          shrink: true,
          align: "right",
          sortValue: sorts((e) => (e.type === "blob" ? (e.size ?? 0) : null)),
          cell: (e) =>
            e.type === "blob" ? (
              <TableNumberCell value={e.size ?? 0} format={fmtBytes} />
            ) : (
              <EmptyValue label="Not a file" />
            ),
        },
      ]}
    />
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
  place,
  pages,
  picked,
  onPick,
}: {
  path: string;
  /** Where the lines were opened ([usePickInView]): the file and the line read forward from. */
  place: string;
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
  usePickInView(place, picked, lines.length);
  // ONE PRESS FOR EVERY LINE, the same function whatever is picked, reading
  // the pick as it is when pressed: a line is drawn again only when its own
  // number, text or pick changes ([SourceLine]), so a press redraws the lines
  // whose pick it moved rather than every line of the file.
  const now = useRef({ picked, onPick });
  now.current = { picked, onPick };
  const press = useCallback((n: number, extend: boolean) => {
    const { picked, onPick } = now.current;
    if (!onPick) return;
    if (extend && picked) {
      onPick({ start: Math.min(picked.start, n), end: Math.max(picked.end, n) });
    } else if (picked && picked.start === n && picked.end === n) {
      onPick(null);
    } else {
      onPick({ start: n, end: n });
    }
  }, []);
  if (!head) return null;
  if (head.binary) {
    return (
      <p className="muted">{`A binary file of ${fmtBytes(head.size)}: git draws no lines of it.`}</p>
    );
  }
  if (head.lines === 0) return <p className="muted">This file is empty.</p>;
  const shown = from + lines.length - 1;
  return (
    <Stack gap={2}>
      <p className="t-caption">
        {`${plural(head.lines, "line")} · ${fmtBytes(head.size)}`}
        {picked &&
          (picked.start > head.lines
            ? ` · it has no line ${picked.start}`
            : (picked.start < from || picked.start > shown) &&
              ` · line ${picked.start} is not among the lines loaded`)}
      </p>
      <div className="code-source" role="region" aria-label={`The lines of ${path}`}>
        {lines.map((text, i) => {
          const n = from + i;
          return (
            <SourceLine
              key={n}
              n={n}
              text={text}
              on={picked !== null && n >= picked.start && n <= picked.end}
              press={press}
            />
          );
        })}
      </div>
    </Stack>
  );
}

/**
 * One line of a file: its number, which picks it, and its text. MEMOIZED on
 * what it draws, so a file of thousands of lines redraws, on a press, the
 * lines whose pick changed — each line is three elements, and a file read to
 * its end is tens of thousands of them.
 */
const SourceLine = memo(function SourceLine({
  n,
  text,
  on,
  press,
}: {
  n: number;
  text: string;
  on: boolean;
  press: (n: number, extend: boolean) => void;
}) {
  return (
    <div id={`L${n}`} className="code-source__line" data-picked={on || undefined}>
      <button
        type="button"
        className="code-source__number"
        aria-label={`Line ${n}`}
        aria-pressed={on}
        onClick={(event) => press(n, event.shiftKey)}
      >
        {n}
      </button>
      <span className="code-source__text">{text}</span>
    </div>
  );
});

/**
 * Scroll the first line an address picks into view, ONCE for each place the
 * lines are opened at (`place`: the file and the line read forward from), and
 * only once that line is DRAWN — `drawn` counts the lines on screen, so the
 * lines a move to the picked line brings ask again, where the lines drawn
 * first did not hold it. An address that names a line is opened at
 * it, and a press that picks another moves nothing under the reader's
 * pointer: it moves no lines, so their place has been scrolled already.
 */
function usePickInView(place: string, picked: LineRun | null, drawn: number) {
  const done = useRef("");
  useEffect(() => {
    if (!picked || drawn === 0 || done.current === place) return;
    const line = document.getElementById(`L${picked.start}`);
    if (!line) return;
    done.current = place;
    line.scrollIntoView?.({ block: "center" });
  }, [place, picked, drawn]);
}

/**
 * The line each page of a blame was asked from, by the page's own answer:
 * the first line of the range it is a page of, which the page after it inside
 * that range is asked from again — the engine refuses a cursor outside the
 * range it is read with. A page the question itself answered is the range's
 * the lines were opened at, and is in none of these.
 */
const blameAskedFrom = new WeakMap<CodeGitAnswer, number>();

/**
 * The cursor to the page of a blame after `a`: inside its range, the range's
 * first line and the engine's cursor; after a range's last page, the next
 * range's first line alone; after the file's last line, none.
 */
function rangeCursor(a: CodeGitAnswer, from: number, total: number): string {
  const b = a.blame;
  if (!b) return "";
  if (a.next) return `${blameAskedFrom.get(a) ?? from}:${a.next}`;
  return b.end_line < total ? `${b.end_line + 1}:` : "";
}

/** A [rangeCursor] read back: the line its range begins at, and the engine's cursor inside it, if any. */
function parseRangeCursor(cursor: string): { line: number; at: string } {
  const colon = cursor.indexOf(":");
  return { line: Number(cursor.slice(0, colon)), at: cursor.slice(colon + 1) };
}

/**
 * Who last changed each line of a file: each run of lines one commit last
 * changed is headed by that commit — its id, its author and when — a page at a
 * time, as git's blame attributes it.
 *
 * A BLAME NAMES A RANGE of at most CODE_MAX_BLAME_LINES lines, paged within
 * it, so the lines are read range after range: the cursor after a range's
 * last page is the next range's first line, and the cursor inside one carries
 * the line its range was asked from, which the engine holds every page of it
 * to. They begin PICK_CONTEXT lines before the line the address picks as the
 * blame opens ([openingLine]), or at line 1, and move only where those do not
 * draw it ([useOpenAt]), with the lines before them a press away, as a
 * file's are. `total` is the file's lines, from its own read.
 */
function Blame({
  repo,
  rev,
  path,
  total,
  picked,
  onPick,
}: {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  total: number;
  picked: LineRun | null;
  onPick: (run: LineRun | null) => void;
}) {
  const place = `${rev}\n${path}\nblame`;
  // THE FIRST RANGE ASKED HOLDS THE PICKED LINE, decided from the address
  // and the file's lines before any blame is asked: a blame walks the file's
  // history for every line it names, the costliest read of a file, and a
  // range from line 1 that only said where to ask next would be one walk
  // more that nothing draws.
  const [from, open] = useOpened(place, openingLine(picked, total));
  const params = useMemo(
    () => ({ name: repo.name, path, ...(rev ? { ref: rev } : {}) }),
    [repo.name, path, rev],
  );
  const atParams = useMemo(
    () => (from > 1 ? { ...params, start_line: from } : params),
    [params, from],
  );
  const first = useCodeQuery("code_blame", atParams);
  const paged = usePaged(
    first,
    atParams,
    async (cursor) => {
      const { line, at } = parseRangeCursor(cursor);
      const answer = await codeRest.query("code_blame", {
        ...params,
        start_line: line,
        ...(at ? { cursor: at } : {}),
      });
      blameAskedFrom.set(answer, line);
      return answer;
    },
    (a: CodeGitAnswer): readonly CodeBlameLine[] => a.blame?.lines ?? [],
    (l: CodeBlameLine) => String(l.line),
    (a: CodeGitAnswer) => rangeCursor(a, from, total),
  );
  const earlier = useEarlier(
    `${place}\n${from}`,
    from,
    CODE_MAX_BLAME_LINES,
    (start, end, cursor) =>
      codeRest.query("code_blame", {
        ...params,
        start_line: start,
        end_line: end,
        ...(cursor ? { cursor } : {}),
      }),
  );
  const commits = useMemo(() => {
    const all: Record<string, CodeBlameCommit> = {};
    for (const a of [...earlier.answers, ...paged.answers])
      Object.assign(all, a.blame?.commits ?? {});
    return all;
  }, [earlier.answers, paged.answers]);
  const rows = useMemo(
    () =>
      earlier.answers.length
        ? [...earlier.answers.flatMap((a) => a.blame?.lines ?? []), ...paged.items]
        : paged.items,
    [earlier.answers, paged.items],
  );
  const head = rows[0];
  const foot = rows[rows.length - 1];
  const settled = (first.data?.blame?.start_line ?? 1) === from && paged.items.length > 0;
  useOpenAt(
    open,
    picked,
    from,
    head && foot ? { start: head.line, end: foot.line } : null,
    settled,
    total,
  );
  usePickInView(`${place}\n${from}`, picked, rows.length);
  if (first.loading && !first.data)
    return <Skeleton variant="text" rows={8} label={`Blaming ${path}`} />;
  if (!first.data) {
    return (
      <QueryState error={first.error} refusal={first.refusal} detail={first.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  if (rows.length === 0) return <p className="muted">This file is empty: no line to blame.</p>;
  return (
    <Stack gap={2}>
      <EarlierLines earlier={earlier} />
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
      <Pager paged={paged} what="lines" />
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
      {/* A CAPTION, NOT A LABEL: it names a file and a revision, which the
          label register's capitals would rewrite. */}
      <p className="t-caption">
        <a
          className="prose-link mono"
          href={repoHref(repo.name, { tab: "files", ref: commit, path: entry.path })}
        >
          {baseName(entry.path)}
        </a>{" "}
        at <span className="mono">{revisionWord(repo, commit)}</span>
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
