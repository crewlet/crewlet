/**
 * A repository's files at a revision: a directory's listing, a file's lines
 * with the ones an address picks marked, and the blame of a file — drawn as a
 * code host draws them. A directory is one card: the latest commit that
 * touched it across its head, then its entries, each with the commit that last
 * changed it and when; at the root its README and LICENSE follow as tabs of
 * one card under the files. A file is read beside the repository's tree
 * (`FileView.tsx`): over it, its crumbs and its latest commit; then its card,
 * its lines or its blame under a head of what a reader does with it.
 *
 * # The address is the place
 *
 * `ref=` is the revision (none is the default branch), `path=` the directory
 * or the file from the root, `lens=blame` who last changed each line, and
 * `lines=` the lines picked — `12`, or `12-20` — which pressing a line's
 * number sets (Shift extends the run), so a link to a line is the address on
 * screen. Opening a directory or a file is a new place in the history; picking
 * lines and changing the revision or the lens replace it. A picked run takes the
 * selected tint, and its first line holds its actions ([LineActions]).
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
 *
 * # Coloured by kind
 *
 * A file under SYNTAX_MAX_BYTES is coloured by its tokens' kinds, its state walked
 * from line 1 (`routes/gitlet/syntax.tsx`); a larger one is drawn plain.
 */

import {
  memo,
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
} from "react";
import {
  Button,
  ButtonLink,
  Callout,
  Card,
  CodeBlock,
  IconButton,
  IconButtonLink,
  SegmentedControl,
  Skeleton,
  Stack,
  Tabs,
  tabId,
} from "@crewlethq/ui";
import {
  BookOpenGlyph,
  ScaleGlyph,
  GitBranchGlyph,
  LinkGlyph,
  RotateCcwClockGlyph,
  TagGlyph,
} from "@crewlethq/icons/glyphs";
import { useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { CODE_MAX_BLAME_LINES } from "~/contract/code.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { useNow } from "~/lib/clock.ts";
import { CHECK_HISTORY_DAYS, rollupFor, rollupTitle } from "~/lib/checks.ts";
import {
  EARLIER_FILE_LINES,
  baseName,
  isWholeOID,
  lineToOpenAt,
  linesValue,
  noDefaultHead,
  openingLine,
  parseLines,
  pathSteps,
  shortOID,
  type LineRun,
} from "~/lib/code.ts";
import { fmtBytes, fmtCount, fmtDateTime, plural, relTime } from "~/lib/format.ts";
import { imageSources, renderMarkdown } from "~/lib/markdown.ts";
import { codeRest, useCodeQuery, type CodeParams } from "~/lib/useCodeQuery.ts";
import type { Grammar, State } from "~/lib/syntax/types.ts";
import { usePaged, type Paged } from "~/lib/usePaged.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import {
  queryErrorCode,
  type CodeBlameCommit,
  type CodeBlameLine,
  type CodeBlobAnswer,
  type CodeCommitMark,
  type CodeGitAnswer,
  type CodePushedBy,
  type CodeRepoDetail,
  type CodeTreeEntry,
} from "~/protocol/index.ts";
import { commitHref, repoHref } from "../paths.ts";
import {
  CheckRollupMark,
  Face,
  GITLET,
  LICENSE_NAMES,
  README_NAMES,
  ShaChip,
  useWhoName,
} from "../ui.tsx";
import { Painted, paintFence, tokensOf, useGrammar, useLineStates } from "../syntax.tsx";
import { gitletWrote, mergeTitle, subjectOf } from "./History.tsx";
import { useReportDocs } from "./About.tsx";
import { Entries } from "./Entries.tsx";
import { useCommitMarks } from "./marks.ts";
import { RefPicker, revisionWord } from "./refs.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import {
  EditFileButton,
  FileMoreMenu,
  FirstFileButton,
  NewFileButton,
  targetOf,
  type CommitTarget,
} from "./FileWrites.tsx";
import { CloneAddress, CodeMenu, NO_CLONE_ADDRESS } from "./CodeMenu.tsx";
import { GoToFile } from "./GoToFile.tsx";
import { FileView } from "./FileView.tsx";
import { FileBytes, WAITING_FOR_COMMIT } from "./FileActions.tsx";
import { LineActions } from "./LineActions.tsx";
import { FilePicture, picturePath, pictureType, usePictures } from "./pictures.tsx";
import "~/styles/gitlet.css";

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

export function Files({
  repo,
  cloneURL,
}: {
  repo: CodeRepoDetail;
  /** The engine's clone address, absent where the deployment names none. */
  cloneURL: string | undefined;
}) {
  const [ref, setRef] = useParam("ref", "", "filter");
  const [path] = useParam("path", "", "filter");
  const [lens, setLens] = useParam("lens", "", "filter");
  const [lines, setLines] = useParam("lines", "", "filter");
  if (!repo.head && ref === "") {
    // EMPTY ONLY WITH NO BRANCH AT ALL: `head` is the default branch's tip,
    // and a repository whose other branches are live while its default
    // branch holds nothing is read from them.
    return repo.branches > 0 ? (
      <NoDefaultBranch repo={repo} onRev={setRef} />
    ) : (
      <EmptyRepository repo={repo} cloneURL={cloneURL} />
    );
  }
  const place = { repo, cloneURL, rev: ref, path, onRev: setRef };
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
  cloneURL: string | undefined;
  rev: string;
  path: string;
  onRev: (rev: string) => void;
}

/** The repository's root at the revision: its listing. */
function RootListing({ repo, cloneURL, rev, path, onRev }: Place) {
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
  const commit = commitOf(rev, first.data);
  return (
    <Stack gap={4}>
      <FilesBar
        repo={repo}
        cloneURL={cloneURL}
        rev={rev}
        path={path}
        onRev={onRev}
        target={targetOf(rev, first.data)}
        commit={commit}
        unread={Boolean(first.error)}
        dir
      />
      <Card padding="none" className="code-browser">
        <LatestCommit
          repo={repo}
          rev={rev}
          path={path}
          commit={commit}
          resolved={first.data?.resolved?.ref?.ref ?? ""}
          by={first.data?.pushed_by?.[commit]}
        />
        <PathBody repo={repo} rev={rev} path={path} commit={commit} first={first} paged={paged} />
      </Card>
      {/* THE README OF THE REVISION ON SCREEN, read at the commit the
          listing was: the two are one revision. */}
      <RepoDocs
        repo={repo}
        entries={paged.items}
        commit={commit}
        target={targetOf(rev, first.data)}
      />
    </Stack>
  );
}

/** A path under the root: a directory's listing, or a file's lines or its blame. */
function PathView({
  repo,
  cloneURL,
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
  const target = targetOf(rev, first.data);
  const dir = Boolean(first.data?.tree);
  // THE WHOLE TEXT WHERE THE PATH'S OWN PAGES HOLD IT: read from line 1 to its last page.
  const loaded = useMemo(() => {
    if (start > 1 || paged.more || !file || file.binary) return null;
    return paged.items.map((p) => ("mode" in p ? "" : (p.text ?? ""))).join("");
  }, [start, paged.more, paged.items, file]);
  // A PATH NOT ANSWERED YET keeps the last kind drawn, so moving between files keeps
  // the pane; a first one is a file where the address picks lines or the blame.
  const [kind, setKind] = useState<"file" | "dir" | null>(null);
  const now = file ? "file" : dir ? "dir" : null;
  if (now && now !== kind) setKind(now);
  const guess = kind ?? (picked || blame ? "file" : null);
  if (now === "file" || (now === null && guess === "file")) {
    return (
      <FileView
        repo={repo}
        rev={rev}
        path={path}
        commit={commit}
        target={target}
        onRev={onRev}
        latest={
          <Card padding="none" className="code-latest--card">
            <LatestCommit
              repo={repo}
              rev={rev}
              path={path}
              commit={commit}
              resolved={first.data?.resolved?.ref?.ref ?? ""}
              by={first.data?.pushed_by?.[commit]}
              history={false}
            />
          </Card>
        }
      >
        {file ? (
          <FileCard
            repo={repo}
            rev={rev}
            path={path}
            commit={commit}
            file={file}
            loaded={loaded}
            blame={blame}
            onLens={onLens}
            picked={picked}
            target={target}
          >
            {blame ? (
              // THE BLAME IS READ AT THE COMMIT THE FILE WAS, where the file's
              // read resolved one: the lines it attributes are the lines on
              // screen.
              <Blame
                repo={repo}
                rev={commit || rev}
                path={path}
                commit={commit}
                blob={file}
                total={file.lines}
                picked={picked}
                onPick={onPick}
                onLens={onLens}
              />
            ) : !file.binary && file.lines > 0 ? (
              <FileLines
                repo={repo.name}
                rev={rev}
                path={path}
                params={base}
                opened={start}
                opening={first}
                paged={paged}
                total={file.lines}
                picked={picked}
                onPick={onPick}
                commit={commit}
                onLens={onLens}
              />
            ) : (
              <PathBody
                repo={repo}
                rev={rev}
                path={path}
                commit={commit}
                first={first}
                paged={paged}
              />
            )}
          </FileCard>
        ) : (
          <Card padding="none" className="code-blob">
            <PathBody
              repo={repo}
              rev={rev}
              path={path}
              commit={commit}
              first={first}
              paged={paged}
            />
          </Card>
        )}
      </FileView>
    );
  }
  return (
    <Stack gap={4}>
      <FilesBar
        repo={repo}
        cloneURL={cloneURL}
        rev={rev}
        path={path}
        onRev={onRev}
        target={target}
        commit={commit}
        dir={dir}
      />
      <Card padding="none" className="code-browser">
        <LatestCommit
          repo={repo}
          rev={rev}
          path={path}
          commit={commit}
          resolved={first.data?.resolved?.ref?.ref ?? ""}
          by={first.data?.pushed_by?.[commit]}
        />
        <PathBody repo={repo} rev={rev} path={path} commit={commit} first={first} paged={paged} />
      </Card>
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
    <div className="code-pager">
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
  repo,
  rev,
  path,
  params,
  opened,
  opening,
  paged: fromOpening,
  total,
  picked,
  onPick,
  commit,
  onLens,
}: {
  /** The repository's name, which a read of the blob's lines above them names. */
  repo: string;
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
  /** The commit the file was read at, whole, which the run's link names. */
  commit: string;
  onLens: (blame: boolean) => void;
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
        repo={repo}
        path={path}
        place={`${place}\n${from}`}
        pages={pages}
        picked={picked}
        onPick={onPick}
        commit={commit}
        onLens={onLens}
      />
      <Pager paged={more} what="lines" />
    </>
  );
}

/**
 * The bar above the code: the revision, the path back up to the root (the
 * repository's name first, every directory a link) and, at a directory, Go
 * to file and a new file in it and, at the root, the Code menu. What a reader
 * does with a file is on the file's own card ([FileCard]).
 */
function FilesBar({
  repo,
  cloneURL,
  rev,
  path,
  onRev,
  target,
  commit,
  dir = false,
  unread = false,
}: Place & {
  /** Where a commit from here lands ([targetOf]). */
  target: CommitTarget;
  /** The commit the path's read resolved, whole, or none while it is read. */
  commit: string;
  /** The path is a directory (or the root), which takes a new file. */
  dir?: boolean;
  /** The path's read failed, so its commit is not known. */
  unread?: boolean;
}) {
  const at = (p: string) => repoHref(repo.name, { ref: rev, path: p });
  return (
    <PageToolbar>
      <RefPicker repo={repo} value={rev} onChoose={onRev} />
      {path === "" ? (
        // AT THE ROOT the path is the repository the header names, so the bar
        // says what the picker picks among instead, each a link to its list.
        <RefCounts repo={repo} />
      ) : (
        <nav aria-label="Path" className="code-crumbs">
          <a className="code-crumbs__root" href={at("")}>
            {repo.name}
          </a>
          {pathSteps(path).map((step) => (
            <span key={step.path} className="code-crumbs__step">
              <span className="code-crumbs__slash" aria-hidden="true">
                /
              </span>
              {step.path === path ? (
                <span className="code-crumbs__here" aria-current="location">
                  {step.name}
                </span>
              ) : (
                <a className="code-crumbs__link" href={at(step.path)}>
                  {step.name}
                </a>
              )}
            </span>
          ))}
        </nav>
      )}
      <span className="code-bar-end">
        {dir && <GoToFile repo={repo.name} rev={rev} commit={commit} />}
        {dir && <NewFileButton repo={repo} target={target} dir={path} />}
        {path === "" && (
          <CodeMenu repo={repo} cloneURL={cloneURL} commit={commit} failed={unread} />
        )}
      </span>
    </PageToolbar>
  );
}

/** The branches and tags the picker picks among, each a link to its list on Branches. */
function RefCounts({ repo, tags = true }: { repo: CodeRepoDetail; tags?: boolean }) {
  return (
    <span className="code-ref-counts">
      <ButtonLink
        size="small"
        variant="ghost"
        href={repoHref(repo.name, { tab: "branches" })}
        leadingIcon={<GitBranchGlyph size="sm" />}
      >
        <span>
          <strong>{fmtCount(repo.branches)}</strong> {repo.branches === 1 ? "Branch" : "Branches"}
        </span>
      </ButtonLink>
      {tags && (
        <ButtonLink
          size="small"
          variant="ghost"
          href={repoHref(repo.name, { tab: "branches", view: "tags" })}
          leadingIcon={<TagGlyph size="sm" />}
        >
          <span>
            <strong>{fmtCount(repo.tags)}</strong> {repo.tags === 1 ? "Tag" : "Tags"}
          </span>
        </ButtonLink>
      )}
    </span>
  );
}

/**
 * The latest commit that touched a directory, across the head of its card, as
 * a code host draws it: who wrote it (Gitlet where it wrote the merge), its
 * subject as the link to the commit, its check, its id, when, and the history.
 *
 * ONE COMMIT OF THE PATH'S HISTORY (`code_log`, `limit` 1) at the commit the
 * listing was read at, and that commit's marks (`code_commit_marks`), so the
 * row and the entries under it are one revision.
 */
function LatestCommit({
  repo,
  rev,
  path,
  commit,
  resolved,
  by,
  history = true,
}: {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  /** The commit the listing resolved, whole, or none while it is read. */
  commit: string;
  /** The whole ref the listing's revision resolved, "" for a commit's id. */
  resolved: string;
  /** The push that introduced `commit`, where the listing's read names one. */
  by: CodePushedBy | undefined;
  /** Its History link; a file's is in its crumbs' row. */
  history?: boolean;
}) {
  const now = useNow();
  const whoName = useWhoName();
  const params = useMemo(
    () => ({ name: repo.name, ref: commit, limit: 1, ...(path ? { path } : {}) }),
    [repo.name, commit, path],
  );
  const read = useCodeQuery("code_log", params, { enabled: commit !== "" });
  const latest = read.data?.log?.commits?.[0];
  const pages = useMemo(() => (latest ? [[latest.oid]] : []), [latest]);
  const marks = useCommitMarks(repo.name, pages);
  const historyHref = repoHref(repo.name, { tab: "commits", ref: rev, path });
  if (!latest) {
    return (
      <div className="code-latest">
        {read.loading || commit === "" ? (
          <Skeleton variant="text" rows={1} label="Reading the latest commit" />
        ) : read.error ? (
          <PageFailed error={read.error} />
        ) : (
          <span className="muted">
            No commit of {revisionWord(repo, rev)} changed {path || "it"}.
          </span>
        )}
      </div>
    );
  }
  // THE LATEST'S OWN `by` only: the listing's push decorates the commit it resolved.
  const pushed = latest.oid === commit ? by : undefined;
  const mark = marks.failed ? null : marks.of(latest.oid);
  const mayBeGitlet = (latest.parent_count ?? 0) >= 2 && latest.engine_committed === true;
  const gitlet = gitletWrote(latest, pushed, mark);
  const author = latest.author ?? `(${latest.author_bytes ?? 0} bytes)`;
  const at = gitlet ? latest.committed_at : latest.authored_at;
  const defaultRef = `refs/heads/${repo.default_branch}`;
  const branch = resolved.startsWith("refs/heads/") ? resolved : defaultRef;
  return (
    <div className="code-latest">
      {mayBeGitlet && mark === undefined ? (
        // THE WRITER WAITS FOR THE MARKS where it may be Gitlet, so the name never changes.
        <span className="code-latest__who">
          <Skeleton variant="text" rows={1} label="Reading who wrote it" />
        </span>
      ) : (
        <span
          className="code-latest__who"
          title={gitlet ? mergeTitle(latest, pushed, mark, whoName) : latest.author_email}
        >
          <Face who={gitlet ? GITLET : author} />
          <span className="code-latest__author">{whoName(gitlet ? GITLET : author)}</span>
        </span>
      )}
      <a
        className="code-latest__subject"
        href={commitHref(repo.name, latest.oid)}
        title={subjectOf(latest)}
      >
        {subjectOf(latest)}
      </a>
      <span className="code-latest__end">
        <span className="code-latest__check">
          <LatestCheck
            mark={mark}
            failed={marks.failed !== null}
            by={pushed}
            branch={branch}
            defaultRef={defaultRef}
            now={now}
          />
        </span>
        <ShaChip oid={latest.oid} />
        <time className="code-latest__when" dateTime={at} title={fmtDateTime(at)}>
          {relTime(at, now)}
        </time>
        {history && (
          <ButtonLink
            size="small"
            variant="ghost"
            href={historyHref}
            leadingIcon={<RotateCcwClockGlyph size="sm" />}
          >
            History
          </ButtonLink>
        )}
      </span>
    </div>
  );
}

/**
 * The latest commit's check as its glyph, or that checks are no longer kept;
 * nothing until known, and nothing where the marks were refused or failed.
 */
function LatestCheck({
  mark,
  failed,
  by,
  branch,
  defaultRef,
  now,
}: {
  mark: CodeCommitMark | null | undefined;
  /** The marks read was refused or failed: the engine said nothing of the checks. */
  failed: boolean;
  by: CodePushedBy | undefined;
  branch: string;
  defaultRef: string;
  now: number;
}) {
  if (mark === undefined || failed) return null;
  const rollup = rollupFor(mark, branch, defaultRef);
  if (rollup) {
    return <CheckRollupMark bare rollup={rollup} title={rollupTitle(rollup, branch, now)} />;
  }
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

/**
 * A file's card: across its head the switch between its lines and its blame, its
 * lines and size, then Raw, Copy and Download, Edit, the Permalink and More.
 */
function FileCard({
  repo,
  rev,
  path,
  commit,
  file,
  loaded,
  blame,
  onLens,
  picked,
  target,
  children,
}: {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  commit: string;
  file: CodeBlobAnswer;
  /** The whole text where every page is loaded, else null. */
  loaded: string | null;
  blame: boolean;
  onLens: (blame: boolean) => void;
  picked: LineRun | null;
  target: CommitTarget;
  children: React.ReactNode;
}) {
  const facts = file.binary
    ? `${pictureType(path) ? "A picture" : "A binary file"} of ${fmtBytes(file.size)}`
    : `${plural(file.lines, "line")} · ${fmtBytes(file.size)}`;
  // A PICK THE FILE CANNOT DRAW is said beside its size: a stale link to a
  // line past its end.
  const beyond = picked && !file.binary && picked.start > file.lines;
  const link = <LinkGlyph />;
  const permalink = `Permalink: this file at ${commit ? shortOID(commit) : revisionWord(repo, rev)}`;
  return (
    <Card padding="none" className="code-blob">
      <div className="code-blob__head">
        <span className="code-blob__lens">
          {!file.binary && file.lines > 0 && (
            <SegmentedControl
              semantics="radio"
              activate="manual"
              size="sm"
              label="How to read the file"
              value={blame ? "blame" : "code"}
              onValueChange={(next) => onLens(next === "blame")}
              options={[
                { value: "code", label: "Code", title: "The file's lines" },
                { value: "blame", label: "Blame", title: "Who last changed each line" },
              ]}
            />
          )}
          <span className="code-blob__facts">
            {facts}
            {beyond ? ` · it has no line ${picked.start}` : ""}
          </span>
        </span>
        <span className="code-blob__actions">
          <FileBytes repo={repo.name} path={path} commit={commit} file={file} loaded={loaded} />
          <EditFileButton repo={repo} target={target} path={path} file={file} />
          <span className="code-blob__rule" aria-hidden="true" />
          {commit ? (
            <IconButtonLink
              label={permalink}
              icon={link}
              size="sm"
              variant="ghost"
              href={repoHref(repo.name, {
                ref: commit,
                path,
                ...(blame ? { lens: "blame" as const } : {}),
                ...(picked ? { lines: linesValue(picked) } : {}),
              })}
            />
          ) : (
            <IconButton
              label={permalink}
              icon={link}
              size="sm"
              variant="ghost"
              disabledReason={WAITING_FOR_COMMIT}
            />
          )}
          <FileMoreMenu repo={repo} target={target} path={path} />
        </span>
      </div>
      <div className="code-blob__body">{children}</div>
    </Card>
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
  commit,
  first,
  paged,
}: {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  /** The commit the read resolved, whole, or none while it is read. */
  commit: string;
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
  const now = useNow();
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
  if (first.data.blob)
    return (
      <Source
        repo={repo.name}
        path={path}
        place={path}
        pages={pages}
        picked={null}
        commit={commit}
      />
    );
  return (
    <>
      <Entries
        repo={repo}
        rev={rev}
        path={path}
        commit={commit}
        entries={entries}
        total={first.data.tree?.total ?? entries.length}
        now={now}
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
    <div className="code-pager">
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

/**
 * A file's lines, numbered, the ones the address picks tinted, the run's first
 * line holding its actions. Pressing a number picks its line, Shift extends the
 * run from the one picked, and pressing the one line picked again lets it go.
 */
function Source({
  repo,
  path,
  place,
  pages,
  picked,
  onPick,
  commit = "",
  onLens,
}: {
  repo: string;
  path: string;
  /** Where the lines were opened ([usePickInView]): the file and the line read forward from. */
  place: string;
  pages: readonly CodeBlobAnswer[];
  picked: LineRun | null;
  onPick?: (run: LineRun | null) => void;
  commit?: string;
  /** Where a run can be blamed: the gutter's actions are drawn only with it and `onPick`. */
  onLens?: (blame: boolean) => void;
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
  const drawnLines = useMemo(() => ({ from, texts: lines }), [from, lines]);
  const grammar = useGrammar(path);
  const stateOf = useLineStates({ repo, blob: head ?? null, grammar, from, lines });
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
    return pictureType(path) ? (
      <FilePicture repo={repo} commit={commit} path={path} size={head.size} />
    ) : (
      <p className="code-blob__note">Git draws no lines of a binary file.</p>
    );
  }
  if (head.lines === 0) return <p className="code-blob__note">This file is empty.</p>;
  const shown = from + lines.length - 1;
  // A PICKED LINE THE FILE HAS BUT THE LINES LOADED DO NOT HOLD is said above
  // them; one it does not have at all is said in the card's head.
  const offLoaded =
    picked !== null && picked.start <= head.lines && (picked.start < from || picked.start > shown);
  const gutter = Boolean(onPick && onLens);
  const menuAt = gutter && picked && !offLoaded && picked.start <= shown ? picked.start : 0;
  return (
    <>
      {offLoaded && (
        <p className="code-blob__note">{`Line ${picked.start} is not among the lines loaded.`}</p>
      )}
      <div
        className="code-source"
        role="region"
        aria-label={`The lines of ${path}`}
        data-gutter={gutter || undefined}
      >
        {lines.map((text, i) => {
          const n = from + i;
          return (
            <SourceLine
              key={n}
              n={n}
              text={text}
              grammar={grammar}
              state={stateOf(n)}
              on={picked !== null && n >= picked.start && n <= picked.end}
              press={press}
              gutter={gutter}
              menu={
                n === menuAt && picked && onLens ? (
                  <LineActions
                    repo={repo}
                    path={path}
                    commit={commit}
                    picked={picked}
                    drawn={drawnLines}
                    blame={false}
                    onLens={onLens}
                  />
                ) : undefined
              }
            />
          );
        })}
      </div>
    </>
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
  grammar,
  state,
  on,
  press,
  gutter,
  menu,
}: {
  n: number;
  text: string;
  grammar: Grammar | null;
  /** The state the line opens in, or null until it is known. */
  state: State | null;
  on: boolean;
  press: (n: number, extend: boolean) => void;
  /** A track before the number, holding `menu` on the run's first line. */
  gutter: boolean;
  menu: React.ReactNode;
}) {
  // A press redraws the line but tokenizes nothing: its tokens follow its text and state.
  const tokens = useMemo(() => tokensOf(grammar, text, state), [grammar, text, state]);
  return (
    <div id={`L${n}`} className="code-source__line" data-picked={on || undefined}>
      {gutter && <span className="code-source__gutter">{menu}</span>}
      <button
        type="button"
        className="code-source__number"
        aria-label={`Line ${n}`}
        aria-pressed={on}
        onClick={(event) => press(n, event.shiftKey)}
      >
        {n}
      </button>
      <span className="code-source__text">
        <Painted text={text} tokens={tokens} />
      </span>
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

// A blame's lines and their ids, kept across renders so its rows change only
// when a page arrives.
const blameLinesOf = (a: CodeGitAnswer): readonly CodeBlameLine[] => a.blame?.lines ?? [];
const blameLineId = (l: CodeBlameLine): string => String(l.line);

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
/** A blame line's text, or undefined where it was too long to send: the wire leaves an empty line's out too. */
function blameText(l: CodeBlameLine): string | undefined {
  return l.text_bytes !== undefined ? undefined : (l.text ?? "");
}

function Blame({
  repo,
  rev,
  path,
  commit,
  blob,
  total,
  picked,
  onPick,
  onLens,
}: {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  /** The commit the file was read at, whole, which the run's link names. */
  commit: string;
  /** The file's first page: its blob's id and size, which colouring its lines reads. */
  blob: CodeBlobAnswer;
  total: number;
  picked: LineRun | null;
  onPick: (run: LineRun | null) => void;
  onLens: (blame: boolean) => void;
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
    blameLinesOf,
    blameLineId,
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
  // COLOURED AS THE FILE IS: a line past its bound counts as empty for the walk.
  const grammar = useGrammar(path);
  const texts = useMemo(() => rows.map((l) => blameText(l) ?? ""), [rows]);
  const drawnLines = useMemo(
    () => ({ from: rows[0]?.line ?? 1, texts: rows.map(blameText) }),
    [rows],
  );
  const stateOf = useLineStates({
    repo: repo.name,
    blob,
    grammar,
    from: head?.line ?? 1,
    lines: texts,
  });
  const tokens = useMemo(
    () =>
      rows.map((l) => {
        const text = blameText(l);
        return text === undefined ? null : tokensOf(grammar, text, stateOf(l.line));
      }),
    [rows, grammar, stateOf],
  );
  const now = useNow();
  if (first.loading && !first.data)
    return <Skeleton variant="text" rows={8} label={`Blaming ${path}`} />;
  if (!first.data) {
    return (
      <QueryState error={first.error} refusal={first.refusal} detail={first.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  if (rows.length === 0)
    return <p className="code-blob__note">This file is empty: no line to blame.</p>;
  // THE WHEN COLUMN FITS ITS LONGEST TIME: past thirty days relTime names the date.
  const whenWidth = Math.max(
    7,
    ...Object.values(commits).map((c) => relTime(c.authored_at, now).length),
  );
  return (
    <>
      <EarlierLines earlier={earlier} />
      <table
        className="code-blame"
        aria-label={`Who last changed each line of ${path}`}
        style={{ "--code-blame-when": `${whenWidth}ch` } as CSSProperties}
      >
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
                    // EACH RUN HEADED BY ITS COMMIT, as a code host's blame is:
                    // when, who and what, the subject the link to the commit.
                    <span className="code-blame__run">
                      {c && (
                        <time
                          className="code-blame__when"
                          dateTime={c.authored_at}
                          title={fmtDateTime(c.authored_at)}
                        >
                          {relTime(c.authored_at, now)}
                        </time>
                      )}
                      {c?.author && <Face who={c.author} size="text" />}
                      <a
                        className="code-blame__subject"
                        href={commitHref(repo.name, l.commit)}
                        title={`${shortOID(l.commit)} ${c?.author ?? ""}: ${c?.subject ?? ""}`}
                      >
                        {c?.subject ?? shortOID(l.commit)}
                      </a>
                    </span>
                  )}
                </td>
                <td className="code-blame__number">
                  {picked && l.line === picked.start && (
                    <span className="code-source__gutter">
                      <LineActions
                        repo={repo.name}
                        path={path}
                        commit={commit}
                        picked={picked}
                        drawn={drawnLines}
                        blame
                        onLens={onLens}
                      />
                    </span>
                  )}
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
                  {l.text_bytes !== undefined ? (
                    `(a line of ${fmtBytes(l.text_bytes)})`
                  ) : (
                    <Painted text={l.text ?? ""} tokens={tokens[i] ?? null} />
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      <Pager paged={paged} what="lines" />
    </>
  );
}

/** A document's own pictures, read at the document's commit: each source a path from its directory
 *  (or the root), mapped to its object URL once read; any other source is left to the renderer. */
function useDocPictures(
  repo: string,
  commit: string,
  docPath: string,
  text: string | undefined,
): (src: string) => string | null | undefined {
  const dir = docPath.includes("/") ? docPath.slice(0, docPath.lastIndexOf("/")) : "";
  const places = useMemo(() => {
    const out = new Map<string, string>();
    for (const src of imageSources(text ?? "")) {
      const path = picturePath(src, dir);
      if (path !== null && pictureType(path)) out.set(src, path);
    }
    return out;
  }, [text, dir]);
  const paths = useMemo(() => [...new Set(places.values())], [places]);
  const pictures = usePictures(repo, commit, paths);
  return useCallback(
    (src: string) => {
      const path = places.get(src);
      if (path === undefined) return undefined;
      const shown = pictures.get(path);
      return shown && "url" in shown ? shown.url : null;
    },
    [places, pictures],
  );
}

/**
 * The README and LICENSE at the root, tabs of one card under the files
 * (`doc=license`), read at the listing's commit, markdown by the dashboard's
 * own renderer with its fences coloured. A root with neither draws nothing.
 */
function RepoDocs({
  repo,
  entries,
  commit,
  target,
}: {
  repo: CodeRepoDetail;
  /** What the root's listing holds, every page of it read so far. */
  entries: readonly PathPart[];
  /** The commit the listing resolved, whole. */
  commit: string;
  target: CommitTarget;
}) {
  const [doc, setDoc] = useParam("doc", "", "filter");
  const panelId = useId();
  const files = entries.filter((e): e is CodeTreeEntry => "mode" in e && e.type === "blob");
  const firstOf = (names: readonly string[]) =>
    names.map((name) => files.find((e) => baseName(e.path).toLowerCase() === name)).find(Boolean);
  const docs = [
    {
      value: "readme",
      label: "README",
      icon: <BookOpenGlyph size="sm" />,
      entry: firstOf(README_NAMES),
    },
    {
      value: "license",
      label: "LICENSE",
      icon: <ScaleGlyph size="sm" />,
      entry: firstOf(LICENSE_NAMES),
    },
  ].filter((d) => d.entry !== undefined);
  useReportDocs(docs.map((d) => d.value).join(","));
  const shown = docs.find((d) => d.value === (doc === "license" ? "license" : "readme")) ?? docs[0];
  const entry = shown?.entry;
  const file = useCodeQuery(
    "code_blob",
    { name: repo.name, path: entry?.path ?? "", ...(commit ? { ref: commit } : {}) },
    { enabled: Boolean(entry) && commit !== "" },
  );
  const blob = file.data?.blob;
  const markdown = entry !== undefined && /\.(md|markdown)$/i.test(entry.path);
  const image = useDocPictures(repo.name, commit, entry?.path ?? "", markdown ? blob?.text : "");
  if (!shown || !entry) return null;
  return (
    <Card padding="none" className="code-readme" as="section" aria-label={baseName(entry.path)}>
      <div className="code-docs__head">
        <Tabs
          variant="chip"
          size="sm"
          ariaLabel="The repository's documents"
          value={shown.value}
          onValueChange={(next) => setDoc(next === "license" ? "license" : "")}
          panelId={panelId}
          items={docs.map(({ value, label, icon }) => ({ value, label, icon }))}
        />
        <EditFileButton repo={repo} target={target} path={entry.path} file={blob} />
      </div>
      <div
        className="code-docs__body"
        role="tabpanel"
        id={panelId}
        aria-labelledby={tabId(panelId, shown.value)}
      >
        {(file.loading && !blob) || commit === "" ? (
          <Skeleton variant="text" rows={4} label={`Loading ${baseName(entry.path)}`} />
        ) : file.error ? (
          <QueryState
            error={file.error}
            refusal={file.refusal}
            detail={file.detail}
            loading={false}
          >
            {null}
          </QueryState>
        ) : blob?.binary ? (
          <p className="muted">It is a binary file.</p>
        ) : markdown ? (
          <div className="prose md">
            {renderMarkdown(blob?.text ?? "", { fence: paintFence, image })}
          </div>
        ) : (
          <pre className="code-readme__text">{blob?.text ?? ""}</pre>
        )}
        {file.data?.next && (
          <Callout variant="neutral">It runs past one page: open it to read the rest.</Callout>
        )}
      </div>
    </Card>
  );
}

/**
 * A repository whose default branch holds nothing while other branches are
 * live: its first push went to another branch, or its default branch was
 * deleted before the engine kept it. Not empty, so it is never told nobody
 * pushed: the picker reaches its branches, and its Settings make one of them
 * the default.
 */
function NoDefaultBranch({ repo, onRev }: { repo: CodeRepoDetail; onRev: (rev: string) => void }) {
  return (
    <Stack gap={3}>
      <PageToolbar>
        <RefPicker repo={repo} value="" onChoose={onRev} />
        <RefCounts repo={repo} tags={false} />
      </PageToolbar>
      <Card className="code-setup">
        <Card.Header divided>
          <Card.Title as="h2">{`${repo.default_branch} holds no commit`}</Card.Title>
        </Card.Header>
        <Card.Body padding="lg">
          <Stack gap={3}>
            <p className="muted">{noDefaultHead(repo)}</p>
            <span>
              <a className="t-link" href={repoHref(repo.name, { tab: "settings" })}>
                Change the default branch →
              </a>
            </span>
          </Stack>
        </Card.Body>
      </Card>
    </Stack>
  );
}

/**
 * A repository nobody has pushed to: its first file is a press away — the
 * root commit that makes its default branch (`FileWrites.tsx`) — and the
 * commands that clone it or push a repository a reader already has, each
 * with its copy, as a code host's empty repository offers them.
 */
function EmptyRepository({
  repo,
  cloneURL,
}: {
  repo: CodeRepoDetail;
  cloneURL: string | undefined;
}) {
  return (
    <Card className="code-setup">
      <Card.Header divided>
        <Card.Title as="h2">This repository is empty</Card.Title>
      </Card.Header>
      <Card.Body padding="lg">
        <Stack gap={4}>
          <p className="muted">
            Nobody has pushed a branch yet: {repo.default_branch} holds no file. Add its first file
            here, or push a branch from a clone.
          </p>
          <span>
            <FirstFileButton repo={repo} />
          </span>
          <Stack gap={2}>
            <h3 className="t-label">Clone it</h3>
            <CloneAddress url={cloneURL} />
          </Stack>
          <Stack gap={2}>
            <h3 className="t-label">Or push a repository you already have</h3>
            {cloneURL ? (
              <CodeBlock
                copyable
                copyLabel="Copy the commands"
                code={`git remote add origin ${cloneURL}\ngit push -u origin ${repo.default_branch}`}
              />
            ) : (
              <p className="muted">{NO_CLONE_ADDRESS}</p>
            )}
          </Stack>
        </Stack>
      </Card.Body>
    </Card>
  );
}
