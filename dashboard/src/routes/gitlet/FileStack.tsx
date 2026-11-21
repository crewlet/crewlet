/** Every file a diff changes as one stack of sticky-headed `DiffView`s, read as the reader
 *  reaches them by the engine's walk of every patch (`patches=true`): a change's revision
 *  (`code_change` parts) or two whole commits (`code_diff`). */

import {
  memo,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ComponentType,
  type CSSProperties,
  type ReactNode,
} from "react";
import {
  Button,
  IconButton,
  Menu,
  Skeleton,
  Tag,
  VisuallyHidden,
  cx,
  useClipboard,
  useToast,
} from "@crewlethq/ui";
import {
  CheckGlyph,
  ChevronDownGlyph,
  ChevronsUpDownGlyph,
  Columns2Glyph,
  CopyGlyph,
  EllipsisVerticalGlyph,
  Rows2Glyph,
  UnfoldVerticalGlyph,
} from "@crewlethq/icons/glyphs";
import {
  DiffView,
  ROW_ESTIMATE_PX,
  type DiffGap,
  type DiffHunk,
  type DiffLine,
} from "~/components/DiffView.tsx";
import { PageFailed } from "~/components/common.tsx";
import { RefusalsTo, useRefusalPlace } from "~/components/WriteButton.tsx";
import { shortOID } from "~/lib/code.ts";
import { EXPAND_STEP, expandHunks, gapsOf, type Revealed } from "~/lib/expand.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { copyLink } from "~/lib/pageLink.ts";
import { parsePatch } from "~/lib/patch.ts";
import { usePatchPages, type PatchAnswer } from "~/lib/patchPages.ts";
import { scrollerOf } from "~/lib/scroller.ts";
import { codeRest, useCodeQuery, type CodeParams } from "~/lib/useCodeQuery.ts";
import { usePaged, type Paged } from "~/lib/usePaged.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { CodeChangeDetail, CodeDiffFile, CodeFilePatch } from "~/protocol/index.ts";
import { Diffstat } from "./Diffstat.tsx";
import { statusWord } from "./FileTree.tsx";
import { PatchPager } from "./PatchPager.tsx";
import { repoHref } from "./paths.ts";
import { useDiffSides, useDiffTokens, useExpandLines } from "./syntax.tsx";
import { SlashBreaks } from "./ui.tsx";
import { changePartParams, refusedWords, useAskAgainWhenMoved } from "./change/reads.ts";
import "~/styles/gitlet.css";

/** Changed lines past which a file starts folded and is read only when asked. */
export const FOLD_FROM_LINES = 400;

/** A file's key in the stack: its path, base64 where git cannot spell it as text. */
export const fileKey = (f: Pick<CodeDiffFile, "path" | "path_b64">): string => f.path_b64 ?? f.path;

/** Whether a file has lines to draw: no binary, submodule, pure rename or mode change. */
function hasLines(f: CodeDiffFile): boolean {
  return !f.binary && !isSubmodule(f) && f.added + f.deleted > 0;
}

const isSubmodule = (f: CodeDiffFile) => f.old_mode === "160000" || f.new_mode === "160000";

/** The query's characters in order over a path, case-insensitive, as matched runs `[start, end)` of
 *  code points, or null where they are not all there. */
export function matchPath(query: string, path: string): [number, number][] | null {
  const want = Array.from(query.toLowerCase()).filter((c) => c.trim() !== "");
  if (want.length === 0) return null;
  const points = Array.from(path.toLowerCase());
  // THE BASE NAME FIRST: a match there reads as the file meant.
  const slash = path.lastIndexOf("/");
  const base = slash < 0 ? 0 : Array.from(path.slice(0, slash + 1)).length;
  const from = (start: number) => {
    const spans: [number, number][] = [];
    let k = 0;
    for (let i = start; i < points.length && k < want.length; i++) {
      if (points[i] !== want[k]) continue;
      const last = spans[spans.length - 1];
      if (last && last[1] === i) last[1] = i + 1;
      else spans.push([i, i + 1]);
      k++;
    }
    return k === want.length ? spans : null;
  };
  return from(base) ?? from(0);
}

/** A page of the walk: its fragments of the files' patches. */
interface WalkPage {
  next: string;
  patches: readonly CodeFilePatch[];
}

const walkPagesOf = (a: PatchAnswer): readonly WalkPage[] =>
  a.diff ? [{ next: a.next ?? "", patches: a.diff.patches ?? [] }] : [];
const walkPageId = (p: WalkPage): string => p.next;
const walkNext = (a: PatchAnswer): string => a.next ?? "";

/** Where a stack's diff is read: a change's revision, or two whole commits; held stable by its host. */
export type StackSource =
  | {
      kind: "change";
      change: Pick<CodeChangeDetail, "repo" | "number">;
      rev: number;
      since: number | null;
    }
  | { kind: "diff"; repo: string; base: string; head: string };

/** A source's question with `extra` beside it: the change's `diff` part, or `code_diff`. */
export function sourceParams(source: StackSource, extra: Readonly<Record<string, string>>) {
  if (source.kind === "diff") {
    return { name: source.repo, base: source.base, head: source.head, ...extra };
  }
  return changePartParams(source.change, "diff", {
    revision: source.rev,
    ...(source.since !== null ? { since_revision: source.since } : {}),
    ...extra,
  });
}

/** A source's read, each kind named at its own call (`app/source.test.ts` reads only literals). */
export function useSourceRead(
  source: StackSource,
  params: CodeParams,
  enabled: boolean,
): { first: QueryResult<PatchAnswer>; page: (cursor: string) => Promise<PatchAnswer> } {
  const byChange = useCodeQuery("code_change", params, {
    enabled: enabled && source.kind === "change",
  });
  const byDiff = useCodeQuery("code_diff", params, { enabled: enabled && source.kind === "diff" });
  const kind = source.kind;
  const page = useCallback(
    (cursor: string): Promise<PatchAnswer> =>
      kind === "change"
        ? codeRest.query("code_change", { ...params, cursor })
        : codeRest.query("code_diff", { ...params, cursor }),
    [kind, params],
  );
  return { first: kind === "change" ? byChange : byDiff, page };
}

/** The key a source's reads are kept under: a new one is a new diff. */
const sourceKey = (s: StackSource): string =>
  s.kind === "diff"
    ? `diff/${s.repo}/${s.base}/${s.head}`
    : `change/${s.change.repo}/${s.change.number}/${s.rev}/${s.since ?? ""}`;

/** One file's patch as the walk holds it, with the path it was walked under. */
export interface WalkFile {
  path: string;
  pathB64: string | undefined;
  hunks: DiffHunk[];
  /** Its last fragment does not continue, or the rest was left out. */
  complete: boolean;
  restOmitted: boolean;
}

/** Whether the walk's patch is of the listed file `f`: a moved diff can list another there first. */
export const walkedAs = (w: WalkFile, f: CodeDiffFile): boolean =>
  w.path === f.path && w.pathB64 === f.path_b64;

/** A file's fragments as the walk holds them, and what was made of them. */
interface Parsed {
  fragments: readonly CodeFilePatch[];
  file: WalkFile;
}

/** The walk of every file's patch, as far as the reader has reached. */
export interface StackPatches {
  paged: Paged<WalkPage, PatchAnswer>;
  files: ReadonlyMap<number, WalkFile>;
  /** The first file the walk has not finished: Infinity once it has no page after. */
  frontier: number;
  /** The engine refused the walk: each file is read when its reader asks. */
  refused: boolean;
}

/** Follow the engine's walk of every file's patch of `source`, read again when `moved` moves. */
export function useStackPatches(args: { source: StackSource; moved: string }): StackPatches {
  const { source, moved } = args;
  const params = useMemo(() => sourceParams(source, { patches: "true" }), [source]);
  const { first, page } = useSourceRead(source, params, true);
  const paged = usePaged(first, params, page, walkPagesOf, walkPageId, walkNext);
  useAskAgainWhenMoved(moved, true, paged.refresh);
  // A FILE IS PARSED AGAIN ONLY WHEN ITS FRAGMENTS CHANGE: an arriving page
  // leaves every file it does not hold as it was.
  const parsed = useRef<ReadonlyMap<number, Parsed>>(new Map());
  const files = useMemo(() => {
    const fragments = new Map<number, CodeFilePatch[]>();
    for (const page of paged.items) {
      for (const p of page.patches) {
        const was = fragments.get(p.file);
        if (was) was.push(p);
        else fragments.set(p.file, [p]);
      }
    }
    const kept = new Map<number, Parsed>();
    const out = new Map<number, WalkFile>();
    for (const [at, parts] of fragments) {
      const was = parsed.current.get(at);
      const same =
        was !== undefined &&
        was.fragments.length === parts.length &&
        was.fragments.every((p, i) => p === parts[i]);
      const end = parts.at(-1)!;
      const one: Parsed = same
        ? was
        : {
            fragments: parts,
            file: {
              path: parts[0]!.path,
              pathB64: parts[0]!.path_b64,
              hunks: parsePatch(parts.map((p) => p.patch).join("")),
              complete: !end.continues || Boolean(end.rest_omitted),
              restOmitted: Boolean(end.rest_omitted),
            },
          };
      kept.set(at, one);
      out.set(at, one.file);
    }
    parsed.current = kept;
    return out;
  }, [paged.items]);
  const last = paged.items.at(-1)?.patches.at(-1);
  const frontier =
    paged.items.length > 0 && !paged.more
      ? Infinity
      : last
        ? last.continues && !last.rest_omitted
          ? last.file
          : last.file + 1
        : 0;
  const refused = paged.error !== null && paged.items.length === 0 && !paged.loading;
  return useMemo(() => ({ paged, files, frontier, refused }), [paged, files, frontier, refused]);
}

/** Where read hunks stand to a newer line: whether one draws it, and the furthest newer line
 *  reached. Git writes hunks in the newer side's order, so a line reached past is outside. */
function placeOf(hunks: readonly DiffHunk[], line: number): { drawn: boolean; reached: number } {
  let drawn = false;
  let reached = 0;
  for (const hunk of hunks) {
    for (const l of hunk.lines) {
      if (l.after === null) continue;
      if (l.after === line) drawn = true;
      if (l.after > reached) reached = l.after;
    }
  }
  return { drawn: line > 0 && drawn, reached };
}

/** What a host adds to each head: a control before More actions, and whether the file starts folded. */
export interface StackHeadSlot {
  /** Kept stable: a new one draws every section again, so the control reads its state itself. */
  control: (f: CodeDiffFile) => ReactNode;
  /** Folded unless the reader unfolded it; its answer moving drops the reader's own choice. */
  foldedByHost: (f: CodeDiffFile) => boolean;
}

/** A jump to a file: its key, and a count so the same file is jumped to again. */
export interface StackJump {
  key: string;
  seq: number;
}

/** What a host puts on one section's lines, given the hunks it draws. */
export interface SectionLines {
  notes?: ((line: DiffLine) => ReactNode) | undefined;
  picked?: ((line: DiffLine) => boolean) | undefined;
  onPick?: ((line: DiffLine, extend: boolean) => void) | undefined;
  reveal?: ((line: DiffLine) => boolean) | undefined;
  /** Drawn at the top of the section's body, and just above its diff. */
  above?: ReactNode;
  aboveDiff?: ReactNode;
}

export interface StackLinesProps {
  file: CodeDiffFile;
  hunks: readonly DiffHunk[];
  children: (lines: SectionLines) => ReactNode;
}

const NO_LINES: SectionLines = {};

export function FileStack({
  source,
  repo,
  head,
  moved,
  files,
  indexOf,
  layout,
  linkOf,
  lines,
  target,
  jump,
  onCurrent,
  lead,
  foot,
  headSlot,
}: {
  source: StackSource;
  repo: string;
  /** The newer side's commit, whole, where the file itself is read; "" where unknown. */
  head: string;
  /** Moves when what the diff is read from moves; "" for a diff by ids. */
  moved: string;
  /** The files drawn, in the listing's order. */
  files: readonly CodeDiffFile[];
  /** A file's place in the whole listing, which the walk names it by. */
  indexOf: (f: CodeDiffFile) => number;
  layout: "unified" | "split";
  /** A copied link to one file. */
  linkOf: (f: CodeDiffFile) => string;
  /** What a host draws on a section's lines; none draws the diff alone. */
  lines?: ComponentType<StackLinesProps> | undefined;
  /** A file and line of its newer side the address names: read to, marked and revealed. */
  target?: { key: string; line: number } | undefined;
  jump: StackJump | null;
  onCurrent: (path: string) => void;
  /** What is said above the sections: how many a filter shows. */
  lead?: ReactNode;
  foot?: ReactNode;
  headSlot?: StackHeadSlot;
}) {
  const walk = useStackPatches({ source, moved });
  const root = useRef<HTMLDivElement>(null);
  const [near, setNear] = useState<ReadonlySet<string>>(() => new Set());
  const [alone, setAlone] = useState<ReadonlySet<string>>(() => new Set());
  const [split, setSplit] = useState<ReadonlyMap<string, "unified" | "split">>(() => new Map());
  // THE READER'S OWN FOLD per file, else the host's (a viewed file starts folded).
  const [own, setOwn] = useState<ReadonlyMap<string, boolean>>(() => new Map());
  const hostFolds = useMemo(
    () => new Map(files.map((f) => [fileKey(f), headSlot?.foldedByHost(f) ?? false])),
    [files, headSlot],
  );
  const foldedKeys = files
    .map(fileKey)
    .filter((k) => own.get(k) ?? hostFolds.get(k) ?? false)
    .join("\n");
  const folded = useMemo(
    () => new Set(foldedKeys === "" ? [] : foldedKeys.split("\n")),
    [foldedKeys],
  );
  const [opened, setOpened] = useState<ReadonlySet<string>>(() => new Set());
  const readAlone = useCallback(
    (key: string) => setAlone((was) => (was.has(key) ? was : new Set([...was, key]))),
    [],
  );
  const observing = typeof IntersectionObserver !== "undefined";
  const keys = useMemo(() => files.map(fileKey).join("\n"), [files]);
  // WHICH SECTIONS ARE WITHIN ONE VIEW: every one where the browser has no observer.
  useEffect(() => {
    const el = root.current;
    if (!el) return;
    if (!observing) {
      setNear(new Set(keys.split("\n")));
      return;
    }
    const watch = new IntersectionObserver(
      (entries) =>
        setNear((was) => {
          let now: Set<string> | null = null;
          for (const entry of entries) {
            const key = (entry.target as HTMLElement).dataset.key ?? "";
            if (entry.isIntersecting === (now ?? was).has(key)) continue;
            now ??= new Set(was);
            if (entry.isIntersecting) now.add(key);
            else now.delete(key);
          }
          return now ?? was;
        }),
      { root: scrollerOf(el), rootMargin: "100% 0px" },
    );
    for (const section of el.querySelectorAll<HTMLElement>("section[data-key]")) {
      watch.observe(section);
    }
    return () => watch.disconnect();
  }, [keys, observing]);
  // THE WALK GOES ON when its frontier's file comes near; a file near past it
  // while the frontier is far (a jump) is read alone.
  const { frontier, refused, files: walked } = walk;
  const { more, paging, pageError, next, refresh } = walk.paged;
  const frontierKey = useMemo(() => {
    const f = files.find((one) => indexOf(one) === frontier);
    return f ? fileKey(f) : null;
  }, [files, indexOf, frontier]);
  // A FAILED PAGE IS ASKED AGAIN ONLY BY ITS PRESS; a refused cursor reads the
  // held pages again first, for a cursor the engine resumes.
  const goOn = useCallback(() => {
    if (pageError === "bad_params") refresh();
    next();
  }, [pageError, refresh, next]);
  useEffect(() => {
    if (refused) return;
    if (frontierKey !== null && near.has(frontierKey) && more && !paging && pageError === null) {
      next();
    }
    for (const f of files) {
      const key = fileKey(f);
      if (!near.has(key) || alone.has(key) || folded.has(key) || !hasLines(f)) continue;
      if (f.added + f.deleted > FOLD_FROM_LINES) continue;
      const at = indexOf(f);
      const held = walked.get(at);
      if (held?.complete && walkedAs(held, f)) continue;
      const past = at > frontier && (frontierKey === null || !near.has(frontierKey));
      // THE WALK ENDED WITHOUT IT: a listing and a walk of different reads.
      const missed = frontier === Infinity;
      if (past || missed) readAlone(key);
    }
  }, [
    near,
    files,
    indexOf,
    walked,
    frontier,
    refused,
    more,
    paging,
    pageError,
    next,
    frontierKey,
    alone,
    folded,
    readAlone,
  ]);
  // A JUMP: read the file alone if the walk does not hold it, then bring its head to the top.
  const jumped = useRef(0);
  useLayoutEffect(() => {
    if (!jump || jumped.current === jump.seq) return;
    const section = root.current?.querySelector<HTMLElement>(
      `section[data-key="${CSS.escape(jump.key)}"]`,
    );
    if (!section) return;
    jumped.current = jump.seq;
    const f = files.find((one) => fileKey(one) === jump.key);
    const held = f ? walked.get(indexOf(f)) : undefined;
    if (f && hasLines(f) && !folded.has(jump.key) && !(held?.complete && walkedAs(held, f))) {
      if (f.added + f.deleted <= FOLD_FROM_LINES) readAlone(jump.key);
    }
    section.scrollIntoView?.({ block: "start" });
    section.querySelector<HTMLElement>("h3")?.focus({ preventScroll: true });
  }, [jump, files, indexOf, walked, folded, readAlone]);
  // THE FILE WHOSE HEAD IS STUCK at the top, read on the scroller's scroll once a frame.
  useEffect(() => {
    const el = root.current;
    const scroller = el ? scrollerOf(el) : null;
    if (!el || !scroller) return;
    let frame = 0;
    const read = () => {
      frame = 0;
      const top =
        scroller.getBoundingClientRect().top +
        (parseFloat(getComputedStyle(scroller).getPropertyValue("--sticky-top")) || 0);
      let current: string | null = null;
      for (const section of el.querySelectorAll<HTMLElement>("section[data-key]")) {
        if (section.getBoundingClientRect().top > top + 1) break;
        current = section.dataset.path ?? null;
      }
      if (current !== null) onCurrent(current);
    };
    const onScroll = () => {
      if (frame === 0) frame = requestAnimationFrame(read);
    };
    scroller.addEventListener("scroll", onScroll, { passive: true });
    return () => {
      scroller.removeEventListener("scroll", onScroll);
      if (frame !== 0) cancelAnimationFrame(frame);
    };
  }, [onCurrent]);
  // STABLE BY KEY, so a section draws again only when what it shows moves.
  const onFold = useCallback(
    (key: string, fold: boolean) => setOwn((was) => new Map(was).set(key, fold)),
    [],
  );
  // A HOST'S FOLD THAT MOVES drops the reader's own choice. A fold pressed from inside its
  // section (focus there) brings a stuck head to the top; a late read or another tab never scrolls.
  const hostWas = useRef(hostFolds);
  const keepPlace = useRef(new Set<string>());
  useLayoutEffect(() => {
    const was = hostWas.current;
    hostWas.current = hostFolds;
    const moved = [...hostFolds].filter(([k, fold]) => was.has(k) && was.get(k) !== fold);
    if (moved.length === 0) return;
    const at = document.activeElement?.closest<HTMLElement>("section[data-key]")?.dataset.key;
    for (const [k, fold] of moved) if (fold && k === at) keepPlace.current.add(k);
    setOwn((mine) => {
      if (!moved.some(([k]) => mine.has(k))) return mine;
      const now = new Map(mine);
      for (const [k] of moved) now.delete(k);
      return now;
    });
  }, [hostFolds]);
  useLayoutEffect(() => {
    const el = root.current;
    const scroller = el ? scrollerOf(el) : null;
    for (const k of keepPlace.current) {
      if (!folded.has(k)) continue;
      keepPlace.current.delete(k);
      const section = el?.querySelector<HTMLElement>(`section[data-key="${CSS.escape(k)}"]`);
      if (!section || !scroller) continue;
      const top =
        scroller.getBoundingClientRect().top +
        (parseFloat(getComputedStyle(scroller).getPropertyValue("--sticky-top")) || 0);
      if (section.getBoundingClientRect().top < top - 1)
        section.scrollIntoView?.({ block: "start" });
    }
  }, [folded]);
  // EACH HEAD'S CONTROL made once a file entry, until the host's control moves.
  const control = headSlot?.control;
  const controls = useMemo(() => new WeakMap<CodeDiffFile, ReactNode>(), [control]);
  const controlOf = (f: CodeDiffFile): ReactNode => {
    if (!control) return null;
    if (!controls.has(f)) controls.set(f, control(f));
    return controls.get(f);
  };
  const onOpen = useCallback(
    (key: string) => {
      setOpened((was) => new Set([...was, key]));
      readAlone(key);
    },
    [readAlone],
  );
  const onLayout = useCallback(
    (key: string, to: "unified" | "split") =>
      setSplit((was) => {
        const map = new Map(was);
        if (to === layout) map.delete(key);
        else map.set(key, to);
        return map;
      }),
    [layout],
  );
  return (
    <div ref={root} className="code-stack">
      <VisuallyHidden as="h2">Files</VisuallyHidden>
      {lead}
      {refused && (
        <p className="muted" role="status">
          Every file's patch on one page is unavailable on this fleet yet: each file is read when
          you open it.
        </p>
      )}
      {files.map((f) => {
        const key = fileKey(f);
        const at = indexOf(f);
        const own = split.get(key);
        const held = walked.get(at);
        const fromWalk = !alone.has(key) && held && walkedAs(held, f) ? held : undefined;
        // ONLY A SECTION THE WALK GOES ON FROM hears its paging.
        const paces =
          !refused && (at === frontier || (fromWalk !== undefined && !fromWalk.complete));
        return (
          <FileSection
            key={key}
            source={source}
            repo={repo}
            head={head}
            moved={moved}
            entry={f}
            fromWalk={fromWalk}
            refused={refused}
            walkPager={paces ? { goOn, paging, pageError } : null}
            alone={alone.has(key)}
            onReadAlone={readAlone}
            layout={own ?? layout}
            ownLayout={own !== undefined && own !== layout}
            onLayout={onLayout}
            folded={folded.has(key)}
            onFold={onFold}
            headEnd={controlOf(f)}
            opened={opened.has(key)}
            onOpen={onOpen}
            linkOf={linkOf}
            Lines={lines}
            line={target?.key === key ? target.line : 0}
          />
        );
      })}
      {foot}
    </div>
  );
}

/** The head's path: the directory in the secondary ink, the base name in the primary. */
function PathWords({ path }: { path: string }) {
  const slash = path.lastIndexOf("/");
  return (
    <>
      {slash >= 0 && (
        <span className="code-file__dir">
          <SlashBreaks text={path.slice(0, slash + 1)} />
        </span>
      )}
      <span>{path.slice(slash + 1)}</span>
    </>
  );
}

/** A file's mode change in words: its executable bit, or the two modes. */
function modeWords(f: CodeDiffFile): string {
  if (f.old_mode === "100644" && f.new_mode === "100755") return "It became executable.";
  if (f.old_mode === "100755" && f.new_mode === "100644") return "It is no longer executable.";
  return `Its mode changed from ${f.old_mode ?? "none"} to ${f.new_mode ?? "none"}.`;
}

/** The walk's paging, heard by the section it goes on from. */
interface WalkPager {
  goOn: () => void;
  paging: boolean;
  pageError: string | null;
}

/** One file of the stack: its sticky head, then its patch with what lies on its lines. */
const FileSection = memo(function FileSection({
  source,
  repo,
  head,
  moved,
  entry,
  fromWalk,
  refused,
  walkPager,
  alone,
  onReadAlone,
  layout,
  ownLayout,
  onLayout,
  folded,
  onFold,
  headEnd,
  opened,
  onOpen,
  linkOf,
  Lines,
  line,
}: {
  source: StackSource;
  repo: string;
  head: string;
  moved: string;
  entry: CodeDiffFile;
  fromWalk: WalkFile | undefined;
  /** The engine refused the walk: the file is read when its reader asks. */
  refused: boolean;
  walkPager: WalkPager | null;
  alone: boolean;
  onReadAlone: (key: string) => void;
  layout: "unified" | "split";
  ownLayout: boolean;
  onLayout: (key: string, layout: "unified" | "split") => void;
  folded: boolean;
  onFold: (key: string, fold: boolean) => void;
  /** The host's control at the head's end, before More actions. */
  headEnd: ReactNode;
  opened: boolean;
  onOpen: (key: string) => void;
  linkOf: (f: CodeDiffFile) => string;
  Lines: ComponentType<StackLinesProps> | undefined;
  /** The newer side's line the address names on this file, or 0. */
  line: number;
}) {
  const path = entry.path;
  const key = fileKey(entry);
  const headId = useId();
  // THE HEAD IS A ROW OF CONTROLS: its control's refusal is drawn under it, never in it.
  const refusals = useRefusalPlace("code-file__refusal");
  const clip = useClipboard({ failedResetMs: null });
  const toast = useToast();
  const lines = hasLines(entry);
  const large = entry.added + entry.deleted > FOLD_FROM_LINES;
  const params = useMemo(
    () => sourceParams(source, entry.path_b64 ? { file_b64: entry.path_b64 } : { file: path }),
    [source, path, entry.path_b64],
  );
  const reading = alone && lines;
  const { first: patch, page } = useSourceRead(source, params, reading);
  const { paged, hunks: aloneHunks } = usePatchPages(patch, params, page);
  useAskAgainWhenMoved(moved, reading, paged.refresh);
  const hunks = alone ? (patch.data ? aloneHunks : null) : (fromWalk?.hunks ?? null);
  // A NAMED LINE: its file's pages followed to it with no press, stopping at a failed page.
  const { drawn: lineDrawn, reached } = useMemo(() => placeOf(hunks ?? [], line), [hunks, line]);
  const short = line > 0 && !lineDrawn && reached < line;
  const follow = alone && short && paged.more && !paged.paging && !paged.pageError;
  const { next: nextPage } = paged;
  useEffect(() => {
    if (follow) nextPage();
  }, [follow, nextPage]);
  const complete = alone ? patch.data !== undefined && !paged.more : Boolean(fromWalk?.complete);
  const outside = line > 0 && hunks !== null && !lineDrawn && (reached >= line || complete);
  const isLine = useCallback((d: DiffLine) => d.after === line, [line]);
  const drawn = hunks !== null && !folded && (!large || opened);
  const sides = useDiffSides({ repo, file: drawn ? entry : undefined });
  // EXPANDED LINES ARE THE DIFF'S: a moved diff forgets them.
  const at = `${sourceKey(source)}/${moved}`;
  const [shown, setShown] = useState<{ at: string; revealed: Revealed }>({
    at,
    revealed: new Map(),
  });
  const revealed = shown.at === at ? shown.revealed : (new Map() as Revealed);
  const [gapFailed, setGapFailed] = useState<ReadonlyMap<number, string>>(new Map());
  const expandable = drawn && entry.status !== "D" && Boolean(entry.new_oid);
  const blob = useExpandLines(repo, expandable ? (entry.new_oid ?? null) : null, sides.after.lines);
  const expanded = useMemo(
    () => expandHunks(hunks ?? [], revealed, blob.line, blob.total),
    [hunks, revealed, blob.line, blob.total],
  );
  const gaps = useMemo(() => gapsOf(hunks ?? [], blob.total), [hunks, blob.total]);
  // AN ADDED FILE'S PATCH IS THE WHOLE FILE: nothing follows its last hunk.
  const trailingLeft =
    entry.status === "A"
      ? 0
      : gaps.trailing === null
        ? null
        : Math.max(0, gaps.trailing - (revealed.get(hunks?.length ?? 0) ?? 0));
  const tokens = useDiffTokens({ sides, file: drawn ? entry : undefined, hunks: expanded.hunks });

  const reveal = async (gap: number, take: number) => {
    const original = hunks ?? [];
    const r = revealed.get(gap) ?? 0;
    let from: number;
    let to: number;
    if (gap < original.length) {
      const hunk = original[gap]!;
      const first = hunk.lines.find((l) => l.after !== null)?.after ?? 1;
      const top = hunk.lines[0]!.kind === "remove" ? first : first;
      to = top - r - 1;
      from = Math.max(1, to - take + 1);
    } else {
      const last = original.at(-1)!;
      const afters = last.lines.filter((l) => l.after !== null);
      const end = (afters.at(-1)?.after ?? 0) + 1;
      from = end + r;
      to = from + take - 1;
      if (blob.total !== null) to = Math.min(to, blob.total);
    }
    if (to < from) return;
    // THE LINES AFTER THE LAST HUNK of a blob of unknown length are read from
    // the last line shown, which it holds, so the answer says its length.
    const unknown = gap === original.length && blob.total === null;
    const got = await blob.read(unknown ? Math.max(1, from - 1) : from, to);
    if (got.failed !== null) {
      const failed = got.failed;
      setGapFailed((was) => new Map(was).set(gap, failed));
      return;
    }
    const upTo = got.total !== null ? Math.min(to, got.total) : to;
    if (upTo < from) return;
    setShown((was) => {
      const base = was.at === at ? was.revealed : new Map<number, number>();
      const next = new Map(base);
      next.set(gap, (base.get(gap) ?? 0) + (upTo - from + 1));
      return { at, revealed: next };
    });
  };

  const gapControls = (gap: DiffGap): ReactNode => {
    const original =
      gap.hunk < expanded.gaps.length ? expanded.gaps[gap.hunk]! : (hunks?.length ?? 0);
    const failed = gapFailed.get(original);
    if (failed !== undefined) {
      return (
        <span className="code-gap__failed">
          {refusedWords("These lines are unavailable", failed)}
        </span>
      );
    }
    if (gap.skipped === 0) return null;
    const hunk = expanded.hunks[gap.hunk];
    const firstAfter = hunk?.lines.find((l) => l.after !== null)?.after;
    const where =
      gap.hunk < expanded.hunks.length
        ? `above line ${firstAfter ?? 1}`
        : `below line ${expanded.hunks.at(-1)?.lines.findLast((l) => l.after !== null)?.after ?? 0}`;
    // ABOVE A HUNK, glyphs in the numbers' gutter beside git's header; after
    // the last, words, as nothing else is on that row.
    const trailingGap = gap.hunk >= expanded.hunks.length;
    const press = (take: number, words: string, name: string, all = false) =>
      trailingGap ? (
        <Button
          key={words}
          size="small"
          variant="ghost"
          leadingIcon={<UnfoldVerticalGlyph size="sm" />}
          aria-label={name}
          onClick={() => void reveal(original, take)}
        >
          {words}
        </Button>
      ) : (
        <IconButton
          key={words}
          label={name}
          icon={all ? <ChevronsUpDownGlyph /> : <UnfoldVerticalGlyph />}
          size="sm"
          variant="ghost"
          onClick={() => void reveal(original, take)}
        />
      );
    if (gap.skipped === null) {
      return press(EXPAND_STEP, "Expand lines below", `Expand hidden lines ${where}`);
    }
    if (gap.skipped <= EXPAND_STEP) {
      return press(
        gap.skipped,
        `Expand ${plural(gap.skipped, "line")}`,
        `Expand ${plural(gap.skipped, "hidden line")} ${where}`,
      );
    }
    return (
      <>
        {press(
          EXPAND_STEP,
          `Expand ${EXPAND_STEP} lines`,
          `Expand ${EXPAND_STEP} hidden lines ${where}`,
        )}
        {press(
          gap.skipped,
          `Expand all ${fmtCount(gap.skipped)}`,
          `Expand all ${fmtCount(gap.skipped)} hidden lines ${where}`,
          true,
        )}
      </>
    );
  };
  // THE NUMBERS' GUTTER IS AS WIDE AS THE DIFF'S LONGEST NUMBER, as the kit draws it.
  const digits = useMemo(() => {
    let most = 1;
    for (const h of expanded.hunks)
      for (const l of h.lines) most = Math.max(most, l.before ?? 0, l.after ?? 0);
    return String(most).length;
  }, [expanded.hunks]);
  // A GAP'S ROW: its controls in the numbers' gutter, git's hunk line in the code's column.
  const expand = (gap: DiffGap): ReactNode => (
    <div
      className={cx(
        "code-gap",
        gap.hunk === 0 && "code-gap--lead",
        gap.hunk >= expanded.hunks.length && "code-gap--trail",
      )}
      data-gap=""
      data-split={gap.split || undefined}
      style={{ "--code-gap-digits": digits } as CSSProperties}
    >
      <span className="code-gap__controls">{gapControls(gap)}</span>
      {gap.header && <span className="code-gap__header">{gap.header}</span>}
    </div>
  );

  const viewHref = head && entry.status !== "D" ? repoHref(repo, { ref: head, path }) : undefined;
  const word = entry.status === "R" ? "Renamed" : statusWord(entry);
  const unread = entry.path_b64 !== undefined;

  // THE BODY AND THE SECTION ARE DRAWN WITH WHAT THE HOST PUTS ON THE LINES, held by the
  // host's own component so its state outlives a fold.
  const draw = (l: SectionLines): ReactNode => {
    let body: ReactNode;
    if (!lines) {
      body = entry.binary ? (
        <p className="code-file__note">
          A binary file: git draws no lines of it.{" "}
          {viewHref && (
            <a className="t-link" href={viewHref}>
              View the file
            </a>
          )}
        </p>
      ) : isSubmodule(entry) ? (
        <p className="code-file__note">
          A submodule moved from {shortOID(entry.old_oid ?? "")} to {shortOID(entry.new_oid ?? "")}.
        </p>
      ) : entry.status === "R" ? (
        <p className="code-file__note">Renamed from {entry.old_path}, its lines unchanged.</p>
      ) : entry.old_mode !== entry.new_mode ? (
        <p className="code-file__note">{modeWords(entry)}</p>
      ) : (
        <p className="code-file__note">No line changed.</p>
      );
    } else if (large && !opened) {
      body = (
        <p className="code-file__note">
          This file changes {fmtCount(entry.added + entry.deleted)} lines.{" "}
          <Button size="small" variant="secondary" onClick={() => onOpen(key)}>
            Show its changes
          </Button>
        </p>
      );
    } else if (alone && patch.error && !patch.data) {
      body = (
        <p className="code-file__note">
          {refusedWords("Its changes could not be read", patch.detail ?? "")}{" "}
          <Button size="small" variant="secondary" onClick={patch.refetch}>
            Retry
          </Button>
        </p>
      );
    } else if (hunks !== null) {
      body = (
        <>
          {l.aboveDiff}
          <DiffView
            className="code-file__diff"
            tokens={tokens}
            hunks={expanded.hunks}
            layout={layout}
            aria-label={`Changes to ${path}`}
            notes={l.notes}
            expand={expandable ? expand : undefined}
            trailing={trailingLeft}
            reveal={line > 0 ? isLine : l.reveal}
            onPick={l.onPick}
            picked={line > 0 ? isLine : l.picked}
          />
          {alone ? (
            <PatchPager paged={paged} />
          ) : (
            fromWalk &&
            !fromWalk.complete &&
            walkPager && (
              <div className="code-file__pager">
                <Button
                  size="small"
                  variant="ghost"
                  leadingIcon={<UnfoldVerticalGlyph size="sm" />}
                  onClick={walkPager.goOn}
                  disabled={walkPager.paging}
                  loading={walkPager.paging}
                >
                  {(() => {
                    const left =
                      entry.added +
                      entry.deleted -
                      fromWalk.hunks.reduce(
                        (n, h) => n + h.lines.filter((l) => l.kind !== "same").length,
                        0,
                      );
                    return left > 0 ? `Load ${plural(left, "more line")}` : "Load more lines";
                  })()}
                </Button>
                {walkPager.pageError && <PageFailed error={walkPager.pageError} />}
              </div>
            )
          )}
          {outside && (
            <p className="code-file__note">
              {`Line ${line} is outside what this diff changes. `}
              <a className="t-link" href={repoHref(repo, { ref: head, path, lines: String(line) })}>
                {`Open ${path} at line ${line}`}
              </a>
            </p>
          )}
          {fromWalk?.restOmitted && (
            <p className="code-file__note">
              The rest of this file's patch holds a line too long to show here.{" "}
              {viewHref && (
                <a className="t-link" href={viewHref}>
                  View the file
                </a>
              )}
            </p>
          )}
        </>
      );
    } else if (refused && !alone) {
      body = (
        <p className="code-file__note">
          <Button
            size="small"
            variant="secondary"
            onClick={() => onReadAlone(key)}
            disabled={unread}
          >
            Show its changes
          </Button>
        </p>
      );
    } else if (walkPager?.pageError && !alone) {
      // THE WALK'S PAGE FOR THIS FILE FAILED: asked again only by this press.
      body = (
        <p className="code-file__note">
          <Button
            size="small"
            variant="secondary"
            onClick={walkPager.goOn}
            disabled={walkPager.paging}
            loading={walkPager.paging}
          >
            Load its changes
          </Button>{" "}
          <PageFailed error={walkPager.pageError} />
        </p>
      );
    } else {
      body = (
        <div
          className="code-file__pending"
          aria-busy="true"
          style={{
            minBlockSize: `${Math.min(entry.added + entry.deleted + 3, FOLD_FROM_LINES) * ROW_ESTIMATE_PX}px`,
          }}
        >
          {/* NO LIVE REGION A FILE: a long stack's every unread file announced itself. */}
          <Skeleton variant="text" rows={3} />
          <VisuallyHidden>{`Reading the changes to ${path}`}</VisuallyHidden>
        </div>
      );
    }

    const copyWord =
      clip.state === "copied"
        ? `Copied ${path}`
        : clip.state === "failed"
          ? "The browser refused the clipboard"
          : "Copy the path";
    return (
      <section className="code-file" aria-labelledby={headId} data-key={key} data-path={path}>
        <div className="code-file__head">
          <span className="code-file__lead">
            <IconButton
              label={folded ? `Unfold ${path}` : `Fold ${path}`}
              icon={<ChevronDownGlyph />}
              size="sm"
              variant="ghost"
              aria-expanded={!folded}
              className="code-file__fold"
              onClick={() => onFold(key, !folded)}
            />
            <h3 id={headId} className="code-file__path" tabIndex={-1}>
              {unread ? (
                <em>a path that is not valid text</em>
              ) : entry.status === "R" && entry.old_path ? (
                <>
                  <PathWords path={entry.old_path} /> → <PathWords path={path} />
                </>
              ) : (
                <PathWords path={path} />
              )}
            </h3>
            {!unread && (
              <IconButton
                label={copyWord}
                icon={clip.state === "copied" ? <CheckGlyph /> : <CopyGlyph />}
                size="sm"
                variant="ghost"
                onClick={() => void clip.copy(path)}
              />
            )}
            <Tag size="xs" appearance="outline">
              {word}
            </Tag>
            {ownLayout && (
              <Tag
                size="xs"
                appearance="outline"
                leadingIcon={
                  layout === "split" ? <Columns2Glyph size="xs" /> : <Rows2Glyph size="xs" />
                }
              >
                {layout === "split" ? "Split" : "Unified"}
              </Tag>
            )}
          </span>
          <span className="code-file__end">
            {entry.binary ? (
              <span className="code-file__binary">binary</span>
            ) : (
              entry.added + entry.deleted > 0 && (
                <Diffstat added={entry.added} deleted={entry.deleted} label={`in ${path}`} />
              )
            )}
            {headEnd !== null && <RefusalsTo place={refusals.element}>{headEnd}</RefusalsTo>}
            <Menu
              label={`More actions on ${path}`}
              icon={<EllipsisVerticalGlyph />}
              triggerVariant="ghost"
              align="end"
              items={[
                ...(viewHref
                  ? [
                      {
                        key: "view",
                        label: "View the file",
                        onSelect: () => void (location.hash = viewHref.replace(/^#/, "")),
                      },
                    ]
                  : []),
                ...(lines
                  ? [
                      {
                        key: "layout",
                        label:
                          layout === "split" ? "View this file unified" : "View this file split",
                        onSelect: () => onLayout(key, layout === "split" ? "unified" : "split"),
                      },
                    ]
                  : []),
                {
                  key: "link",
                  label: "Copy a link to this file",
                  onSelect: () =>
                    void copyLink(linkOf(entry)).then((said) =>
                      said === "Copied"
                        ? toast.ok(`Copied a link to ${path}`)
                        : toast.show({
                            variant: "warning",
                            message: "The browser refused the clipboard",
                          }),
                    ),
                },
              ]}
            />
          </span>
        </div>
        {headEnd !== null && refusals.place}
        {!folded && (
          <div className="code-file__body">
            {l.above}
            {body}
          </div>
        )}
      </section>
    );
  };
  return Lines ? (
    <Lines file={entry} hunks={expanded.hunks}>
      {draw}
    </Lines>
  ) : (
    draw(NO_LINES)
  );
});
