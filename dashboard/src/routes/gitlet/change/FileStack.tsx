/**
 * Every file a revision changes as one stack: each a section under a head that
 * sticks below the files' toolbar, its patch in its own `DiffView`. Patches are
 * read as the reader reaches them, by the engine's walk of every file's patch
 * a page at a time (`part=diff&patches=true`); a file jumped to is read alone.
 */

import {
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  Button,
  Disclosure,
  IconButton,
  Input,
  Menu,
  Select,
  Skeleton,
  Stack,
  Tag,
  Textarea,
  VisuallyHidden,
  useClipboard,
} from "@crewlethq/ui";
import {
  ChevronDownGlyph,
  Columns2Glyph,
  CopyGlyph,
  EllipsisVerticalGlyph,
  Rows2Glyph,
  UnfoldVerticalGlyph,
} from "@crewlethq/icons/glyphs";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { useDraftState } from "~/app/assistant/draft.ts";
import {
  DiffView,
  ROW_ESTIMATE_PX,
  type DiffGap,
  type DiffHunk,
  type DiffLine,
} from "~/components/DiffView.tsx";
import { PageFailed } from "~/components/common.tsx";
import { CATEGORY_CHOICES, SEVERITY_CHOICES, shortOID, severityWord } from "~/lib/code.ts";
import { EXPAND_STEP, expandHunks, gapsOf, type Revealed } from "~/lib/expand.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { lineSide, parsePatch } from "~/lib/patch.ts";
import { usePatchPages } from "~/lib/patchPages.ts";
import type { DraftFinding, ReviewDrafts } from "~/lib/reviewDrafts.ts";
import { scrollerOf } from "~/lib/scroller.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged, type Paged } from "~/lib/usePaged.ts";
import type {
  CodeChangeAnswer,
  CodeChangeDetail,
  CodeCommentRow,
  CodeDiffFile,
  CodeFilePatch,
  CodeFindingPlace,
  CodeFindingRow,
} from "~/protocol/index.ts";
import { Diffstat } from "../Diffstat.tsx";
import { statusWord } from "../FileTree.tsx";
import { PatchPager } from "../PatchPager.tsx";
import { changeHref, repoHref } from "../paths.ts";
import { useDiffSides, useDiffTokens, useExpandLines } from "../syntax.tsx";
import { Thread } from "./Conversation.tsx";
import { FindingCard, ownLine } from "./Finding.tsx";
import { changePartParams, refusedWords, useAskAgainWhenMoved } from "./reads.ts";
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

/**
 * The query's characters in order over a path, case-insensitive, as matched
 * runs `[start, end)` of code points, or null where they are not all there.
 */
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

const walkPagesOf = (a: CodeChangeAnswer): readonly WalkPage[] =>
  a.diff ? [{ next: a.next ?? "", patches: a.diff.patches ?? [] }] : [];
const walkPageId = (p: WalkPage): string => p.next;
const walkNext = (a: CodeChangeAnswer): string => a.next ?? "";

/** One file's patch as the walk holds it. */
export interface WalkFile {
  hunks: DiffHunk[];
  /** Its last fragment does not continue, or the rest was left out. */
  complete: boolean;
  restOmitted: boolean;
}

/** The walk of every file's patch, as far as the reader has reached. */
export interface StackPatches {
  paged: Paged<WalkPage, CodeChangeAnswer>;
  files: ReadonlyMap<number, WalkFile>;
  /** The first file the walk has not finished: Infinity once it has no page after. */
  frontier: number;
  /** The engine refused the walk: each file is read when its reader asks. */
  refused: boolean;
}

/** Follow the engine's walk of every file's patch of revision `rev`, read again when `moved` moves. */
export function useStackPatches(args: {
  change: Pick<CodeChangeDetail, "repo" | "number">;
  rev: number;
  since: number | null;
  moved: string;
}): StackPatches {
  const { change, rev, since, moved } = args;
  const params = useMemo(
    () =>
      ({
        ...changePartParams({ repo: change.repo, number: change.number }, "diff", {
          revision: rev,
          ...(since !== null ? { since_revision: since } : {}),
        }),
        patches: "true",
      }) as const,
    [change.repo, change.number, rev, since],
  );
  const first = useCodeQuery("code_change", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_change", { ...params, cursor }),
    walkPagesOf,
    walkPageId,
    walkNext,
  );
  useAskAgainWhenMoved(moved, true, paged.refresh);
  // PARSED ONCE A PAGE ARRIVES, each file's fragments joined in order.
  const files = useMemo(() => {
    const texts = new Map<number, { text: string; complete: boolean; restOmitted: boolean }>();
    for (const page of paged.items) {
      for (const p of page.patches) {
        const was = texts.get(p.file) ?? { text: "", complete: false, restOmitted: false };
        texts.set(p.file, {
          text: was.text + p.patch,
          complete: !p.continues || Boolean(p.rest_omitted),
          restOmitted: Boolean(p.rest_omitted),
        });
      }
    }
    const out = new Map<number, WalkFile>();
    for (const [at, t] of texts) {
      out.set(at, { hunks: parsePatch(t.text), complete: t.complete, restOmitted: t.restOmitted });
    }
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
  return { paged, files, frontier, refused };
}

/** A line's number on one side of the diff: its newer one, or its older on a line the newer lacks. */
function numberOn(line: DiffLine, side: "old" | "new"): number | null {
  if (side === "new") return line.after;
  return line.kind === "add" ? null : line.before;
}

/** The lines a reviewer has picked: one side's run, from its first line to its last. */
interface LinePick {
  side: "old" | "new";
  start: number;
  end: number;
}

/** A change's threads on lines, each by its first comment with its replies. */
function threadsOf(comments: readonly CodeCommentRow[]) {
  const threads = new Map<string, { root: CodeCommentRow; replies: CodeCommentRow[] }>();
  for (const c of comments) {
    if (c.finding_id) continue;
    if (!c.parent_id) threads.set(c.id, { root: c, replies: [] });
    else threads.get(c.parent_id)?.replies.push(c);
  }
  return [...threads.values()].filter((t) => t.root.anchor);
}

/** What lies on a stack's lines: its findings, threads and drafts, shared by every section. */
export interface StackNotes {
  findings: readonly CodeFindingRow[];
  placed: Readonly<Record<string, CodeFindingPlace>>;
  comments: readonly CodeCommentRow[];
  /** The reader's drafts on the head, or null where none is drafted. */
  drafts: ReviewDrafts | null;
  thread: string;
}

/** A jump to a file: its key, and a count so the same file is jumped to again. */
export interface StackJump {
  key: string;
  seq: number;
}

export function FileStack({
  change,
  head,
  rev,
  since,
  moved,
  files,
  indexOf,
  layout,
  notes,
  jump,
  onCurrent,
  foot,
}: {
  change: CodeChangeDetail;
  /** The revision's head, whole, where the file itself is read; "" where unknown. */
  head: string;
  rev: number;
  since: number | null;
  moved: string;
  /** The files drawn, in the listing's order. */
  files: readonly CodeDiffFile[];
  /** A file's place in the whole listing, which the walk names it by. */
  indexOf: (f: CodeDiffFile) => number;
  layout: "unified" | "split";
  notes: StackNotes;
  jump: StackJump | null;
  onCurrent: (path: string) => void;
  foot?: ReactNode;
}) {
  const walk = useStackPatches({ change, rev, since, moved });
  const root = useRef<HTMLDivElement>(null);
  const [near, setNear] = useState<ReadonlySet<string>>(() => new Set());
  const [alone, setAlone] = useState<ReadonlySet<string>>(() => new Set());
  const [split, setSplit] = useState<ReadonlyMap<string, "unified" | "split">>(() => new Map());
  const [folded, setFolded] = useState<ReadonlySet<string>>(() => new Set());
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
  const frontierKey = useMemo(() => {
    const f = files.find((one) => indexOf(one) === walk.frontier);
    return f ? fileKey(f) : null;
  }, [files, indexOf, walk.frontier]);
  useEffect(() => {
    if (walk.refused) return;
    if (frontierKey !== null && near.has(frontierKey) && walk.paged.more && !walk.paged.paging) {
      walk.paged.next();
    }
    for (const f of files) {
      const key = fileKey(f);
      if (!near.has(key) || alone.has(key) || !hasLines(f)) continue;
      if (f.added + f.deleted > FOLD_FROM_LINES) continue;
      const at = indexOf(f);
      if (walk.files.get(at)?.complete) continue;
      const past = at > walk.frontier && (frontierKey === null || !near.has(frontierKey));
      // THE WALK ENDED WITHOUT IT: a listing and a walk of different reads.
      const missed = walk.frontier === Infinity;
      if (past || missed) readAlone(key);
    }
  }, [near, files, indexOf, walk, frontierKey, alone, readAlone]);
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
    if (f && hasLines(f) && !walk.files.get(indexOf(f))?.complete) {
      if (f.added + f.deleted <= FOLD_FROM_LINES) readAlone(jump.key);
    }
    section.scrollIntoView?.({ block: "start" });
    section.querySelector<HTMLElement>("h3")?.focus({ preventScroll: true });
  }, [jump, files, indexOf, walk.files, readAlone]);
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
  const toggle = (set: typeof setFolded, key: string) =>
    set((was) => {
      const next = new Set(was);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  return (
    <div ref={root} className="code-stack">
      <VisuallyHidden as="h2">Files</VisuallyHidden>
      {walk.refused && (
        <p className="muted" role="status">
          Every file's patch on one page is unavailable on this fleet yet: each file is read when
          you open it.
        </p>
      )}
      {files.map((f) => {
        const key = fileKey(f);
        const at = indexOf(f);
        const own = split.get(key);
        return (
          <FileSection
            key={key}
            change={change}
            head={head}
            rev={rev}
            since={since}
            moved={moved}
            entry={f}
            fromWalk={alone.has(key) ? undefined : walk.files.get(at)}
            walk={walk}
            alone={alone.has(key)}
            onReadAlone={() => readAlone(key)}
            layout={own ?? layout}
            ownLayout={own !== undefined && own !== layout}
            onLayout={(next) =>
              setSplit((was) => {
                const map = new Map(was);
                if (next === layout) map.delete(key);
                else map.set(key, next);
                return map;
              })
            }
            folded={folded.has(key)}
            onFold={() => toggle(setFolded, key)}
            opened={opened.has(key)}
            onOpen={() => {
              setOpened((was) => new Set([...was, key]));
              readAlone(key);
            }}
            notes={notes}
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
      {slash >= 0 && <span className="code-file__dir">{path.slice(0, slash + 1)}</span>}
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

/** One file of the stack: its sticky head, then its patch with what lies on its lines. */
function FileSection({
  change,
  head,
  rev,
  since,
  moved,
  entry,
  fromWalk,
  walk,
  alone,
  onReadAlone,
  layout,
  ownLayout,
  onLayout,
  folded,
  onFold,
  opened,
  onOpen,
  notes,
}: {
  change: CodeChangeDetail;
  head: string;
  rev: number;
  since: number | null;
  moved: string;
  entry: CodeDiffFile;
  fromWalk: WalkFile | undefined;
  walk: StackPatches;
  alone: boolean;
  onReadAlone: () => void;
  layout: "unified" | "split";
  ownLayout: boolean;
  onLayout: (layout: "unified" | "split") => void;
  folded: boolean;
  onFold: () => void;
  opened: boolean;
  onOpen: () => void;
  notes: StackNotes;
}) {
  const path = entry.path;
  const headId = useId();
  const clip = useClipboard();
  const lines = hasLines(entry);
  const large = entry.added + entry.deleted > FOLD_FROM_LINES;
  const params = useMemo(
    () =>
      changePartParams({ repo: change.repo, number: change.number }, "diff", {
        revision: rev,
        ...(entry.path_b64 ? { file_b64: entry.path_b64 } : { file: path }),
        ...(since !== null ? { since_revision: since } : {}),
      }),
    [change.repo, change.number, rev, path, entry.path_b64, since],
  );
  const reading = alone && lines;
  const patch = useCodeQuery("code_change", params, { enabled: reading });
  const { paged, hunks: aloneHunks } = usePatchPages(patch, params, (cursor) =>
    codeRest.query("code_change", { ...params, cursor }),
  );
  useAskAgainWhenMoved(moved, reading, paged.refresh);
  const hunks = alone ? (patch.data ? aloneHunks : null) : (fromWalk?.hunks ?? null);
  const drawn = hunks !== null && !folded && (!large || opened);
  const sides = useDiffSides({ repo: change.repo, file: drawn ? entry : undefined });
  // EXPANDED LINES ARE THE REVISION'S: a moved diff forgets them.
  const at = `${rev}/${since ?? ""}/${moved}`;
  const [shown, setShown] = useState<{ at: string; revealed: Revealed }>({
    at,
    revealed: new Map(),
  });
  const revealed = shown.at === at ? shown.revealed : (new Map() as Revealed);
  const [gapFailed, setGapFailed] = useState<ReadonlyMap<number, string>>(new Map());
  const expandable = drawn && entry.status !== "D" && Boolean(entry.new_oid);
  const blob = useExpandLines(
    change.repo,
    expandable ? (entry.new_oid ?? null) : null,
    sides.after.lines,
  );
  const expanded = useMemo(
    () => expandHunks(hunks ?? [], revealed, blob.line),
    [hunks, revealed, blob.line],
  );
  const gaps = useMemo(() => gapsOf(hunks ?? [], blob.total), [hunks, blob.total]);
  const trailingLeft =
    gaps.trailing === null
      ? null
      : Math.max(0, gaps.trailing - (revealed.get(hunks?.length ?? 0) ?? 0));
  const tokens = useDiffTokens({ sides, file: drawn ? entry : undefined, hunks: expanded.hunks });
  const [pick, setPick] = useState<LinePick | null>(null);

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
    const failed = await blob.read(from, to);
    if (failed !== null) {
      setGapFailed((was) => new Map(was).set(gap, failed));
      return;
    }
    setShown((was) => {
      const base = was.at === at ? was.revealed : new Map<number, number>();
      const next = new Map(base);
      next.set(gap, (base.get(gap) ?? 0) + (to - from + 1));
      return { at, revealed: next };
    });
  };

  const expand = (gap: DiffGap): ReactNode => {
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
    const press = (take: number, words: string, name: string) => (
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
    );
    if (gap.skipped === null) {
      return press(EXPAND_STEP, "Expand lines below", `Expand hidden lines ${where}`);
    }
    if (gap.skipped <= EXPAND_STEP) {
      return press(
        gap.skipped,
        `Expand ${plural(gap.skipped, "line")}`,
        `Expand ${fmtCount(gap.skipped)} hidden lines ${where}`,
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
        )}
      </>
    );
  };

  const { findings, placed, comments, drafts, thread } = notes;
  const ofFile = findings
    .filter((f) => f.path === path)
    .map((f) => ({ finding: f, place: placed[f.id] }));
  const placedHere = ofFile.filter((p) => p.place && !p.place.outdated && !p.place.gone);
  const outdated = ofFile.filter((p) => !p.place || p.place.outdated || p.place.gone);
  const threads = threadsOf(comments).filter((t) => t.root.anchor?.path === path);
  const threadsHere = threads.filter((t) => t.root.anchor?.revision === rev);
  const threadsElsewhere = threads.filter((t) => t.root.anchor?.revision !== rev);
  const repliesTo = (finding: string) => comments.filter((c) => c.finding_id === finding);
  // A FINDING'S OWN LINES AS THE PATCH ON SCREEN DRAWS THEM, all of them or none.
  const ownOf = (finding: CodeFindingRow): DiffLine[] => {
    const own = expanded.hunks.flatMap((h) => h.lines.filter((l) => ownLine(finding, l)));
    return own.length === finding.line_end - finding.line_start + 1 ? own : [];
  };
  const threadRoot = threadsHere.find((t) => t.root.id === thread)?.root;
  const revealLine = useCallback(
    (line: DiffLine) => {
      const anchor = threadRoot?.anchor;
      return anchor ? numberOn(line, anchor.side === "old" ? "old" : "new") === anchor.line : false;
    },
    [threadRoot],
  );

  const lineNotes = (line: DiffLine) => {
    const out: ReactNode[] = [];
    for (const side of ["new", "old"] as const) {
      const n = numberOn(line, side);
      if (n === null) continue;
      for (const p of placedHere) {
        if (p.finding.side === side && (p.place?.end ?? p.place?.start) === n) {
          out.push(
            <FindingCard
              key={p.finding.id}
              change={change}
              finding={p.finding}
              place={p.place}
              replies={repliesTo(p.finding.id)}
              applicable={rev === change.revision && since === null}
              own={ownOf(p.finding)}
              where="line"
            />,
          );
        }
      }
      for (const t of threadsHere) {
        if (t.root.anchor?.side === side && t.root.anchor.line === n) {
          out.push(
            <div key={t.root.id} data-current={t.root.id === thread || undefined}>
              <Thread root={t.root} replies={t.replies} change={change} />
            </div>,
          );
        }
      }
      for (const d of drafts?.drafts ?? []) {
        if (d.path === path && d.side === side && d.line_end === n) {
          out.push(<DraftNote key={d.id} draft={d} remove={() => drafts!.remove(d.id)} />);
        }
      }
      if (drafts && pick && pick.side === side && pick.end === n) {
        out.push(
          <FindingComposer
            key="composer"
            pick={pick}
            onAdd={(draft) => {
              drafts.add({
                ...draft,
                path,
                side: pick.side,
                line_start: pick.start,
                line_end: pick.end,
              });
              setPick(null);
            }}
            onCancel={() => setPick(null)}
          />,
        );
      }
    }
    return out.length > 0 ? <>{out}</> : null;
  };

  const viewHref =
    head && entry.status !== "D" ? repoHref(change.repo, { ref: head, path }) : undefined;
  const fileLink = changeHref(change.repo, change.number, {
    tab: "files",
    ...(rev !== change.revision ? { rev } : {}),
    file: path,
  });
  const word = entry.status === "R" ? "Renamed" : statusWord(entry);
  const unread = entry.path_b64 !== undefined;

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
        <Button size="small" variant="secondary" onClick={onOpen}>
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
        <DiffView
          hue="code"
          tokens={tokens}
          hunks={expanded.hunks}
          layout={layout}
          aria-label={`Changes to ${path}`}
          notes={lineNotes}
          expand={expandable ? expand : undefined}
          trailing={trailingLeft}
          reveal={threadRoot ? revealLine : undefined}
          onPick={
            drafts
              ? (line, extend) => {
                  const where = lineSide(line);
                  if (!where) return;
                  setPick((was) =>
                    extend && was && was.side === where.side
                      ? {
                          side: where.side,
                          start: Math.min(was.start, where.n),
                          end: Math.max(was.end, where.n),
                        }
                      : { side: where.side, start: where.n, end: where.n },
                  );
                }
              : undefined
          }
          picked={(line) => {
            if (pick) {
              const n = numberOn(line, pick.side);
              if (n !== null && n >= pick.start && n <= pick.end) return true;
            }
            // A FINDING HANGS FROM ITS LAST LINE, which wears the accent bar.
            return placedHere.some((p) => {
              const n = numberOn(line, p.finding.side === "old" ? "old" : "new");
              return n !== null && n === (p.place?.end ?? p.place?.start);
            });
          }}
        />
        {alone ? (
          <PatchPager paged={paged} />
        ) : (
          fromWalk &&
          !fromWalk.complete && (
            <div className="code-file__pager">
              <Button
                size="small"
                variant="ghost"
                leadingIcon={<UnfoldVerticalGlyph size="sm" />}
                onClick={walk.paged.next}
                disabled={walk.paged.paging}
                loading={walk.paged.paging}
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
              {walk.paged.pageError && <PageFailed error={walk.paged.pageError} />}
            </div>
          )
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
  } else if (walk.refused && !alone) {
    body = (
      <p className="code-file__note">
        <Button size="small" variant="secondary" onClick={onReadAlone} disabled={unread}>
          Show its changes
        </Button>
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
        <Skeleton variant="text" rows={3} label={`Reading the changes to ${path}`} />
      </div>
    );
  }

  return (
    <section
      className="code-file"
      aria-label={`Changes to ${path}`}
      data-key={fileKey(entry)}
      data-path={path}
    >
      <div className="code-file__head">
        <IconButton
          label={folded ? `Unfold ${path}` : `Fold ${path}`}
          icon={<ChevronDownGlyph />}
          size="sm"
          variant="ghost"
          aria-expanded={!folded}
          className="code-file__fold"
          onClick={onFold}
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
            label="Copy the path"
            icon={<CopyGlyph />}
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
        <span className="code-file__end">
          {entry.binary ? (
            <span className="code-file__binary">binary</span>
          ) : (
            <Diffstat added={entry.added} deleted={entry.deleted} label={`in ${path}`} />
          )}
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
                      label: layout === "split" ? "View this file unified" : "View this file split",
                      onSelect: () => onLayout(layout === "split" ? "unified" : "split"),
                    },
                  ]
                : []),
              {
                key: "link",
                label: "Copy a link to this file",
                onSelect: () => void clip.copy(new URL(fileLink, location.href).href),
              },
            ]}
          />
        </span>
      </div>
      {!folded && (
        <div className="code-file__body">
          {(outdated.length > 0 || threadsElsewhere.length > 0) && (
            <Disclosure
              title={`${outdated.length + threadsElsewhere.length} outdated on this file`}
            >
              <Stack gap={2}>
                {outdated.map((p) => (
                  <FindingCard
                    key={p.finding.id}
                    change={change}
                    finding={p.finding}
                    place={p.place}
                    replies={repliesTo(p.finding.id)}
                    applicable={false}
                    where="line"
                  />
                ))}
                {threadsElsewhere.map((t) => (
                  <Thread key={t.root.id} root={t.root} replies={t.replies} change={change} />
                ))}
              </Stack>
            </Disclosure>
          )}
          {body}
        </div>
      )}
    </section>
  );
}

/** A drafted finding, marked as one, with the press that drops it. */
function DraftNote({ draft, remove }: { draft: DraftFinding; remove: () => void }) {
  return (
    <article className="code-finding" aria-label={`Drafted finding: ${draft.title}`}>
      <p className="row gap-1 wrap">
        <Tag size="sm" appearance="outline">
          Draft
        </Tag>
        <Tag size="sm" variant={draft.severity === "blocker" ? "danger" : "neutral"}>
          {severityWord(draft.severity)}
        </Tag>
        <strong>{draft.title}</strong>
        <Button size="small" variant="ghost" onClick={remove}>
          Drop
        </Button>
      </p>
      {draft.body && <div className="prose md">{renderMarkdown(draft.body)}</div>}
    </article>
  );
}

/**
 * The composer under a pick: a finding drafted on the picked lines. It writes
 * nothing to the engine: a draft is the tab's until a review sends it.
 */
function FindingComposer({
  pick,
  onAdd,
  onCancel,
}: {
  pick: LinePick;
  onAdd: (draft: Omit<DraftFinding, "id" | "path" | "side" | "line_start" | "line_end">) => void;
  onCancel: () => void;
}) {
  const [severity, setSeverity] = useState("major");
  const [category, setCategory] = useState("correctness");
  const [title, setTitle] = useDraftState("");
  const [body, setBody] = useDraftState("");
  const [suggesting, setSuggesting] = useState(false);
  const [suggestion, setSuggestion] = useDraftState("");
  const lines = pick.start === pick.end ? `line ${pick.start}` : `lines ${pick.start}–${pick.end}`;
  // THE FINDING'S BOXES AS FIELDS of the change page's one draft, so the
  // person's assistant reads and can fill what is being written here.
  useDraftBox({
    path: "finding.title",
    label: `Finding on ${lines}: title`,
    hint: "One line",
    rank: 10,
    value: title,
    set: setTitle,
    refuses: (next) => (next.includes("\n") ? "A finding's title is one line." : null),
  });
  useDraftBox({
    path: "finding.body",
    label: `Finding on ${lines}: why, and what to do`,
    hint: "Plain text",
    rank: 11,
    value: body,
    set: setBody,
    refuses: () => null,
  });
  useDraftBox(
    suggesting
      ? {
          path: "finding.suggestion",
          label: `Finding on ${lines}: the lines to put in their place`,
          hint: "The replacement lines, exactly",
          rank: 12,
          value: suggestion,
          set: setSuggestion,
          refuses: () => null,
        }
      : null,
  );
  return (
    <form
      className="code-finding"
      aria-label={`A finding on ${lines}`}
      onSubmit={(event) => {
        event.preventDefault();
        if (!title.trim()) return;
        onAdd({ severity, category, title, body, ...(suggesting ? { suggestion } : {}) });
      }}
    >
      <p className="muted">A finding on {lines}, sent with your review.</p>
      <div className="row gap-2 wrap">
        <Select
          width="auto"
          value={severity}
          onChange={(value) => setSeverity(String(value))}
          options={SEVERITY_CHOICES.map((c) => ({ value: c.value, label: c.label }))}
          ariaLabel="Weight"
        />
        <Select
          width="auto"
          value={category}
          onChange={(value) => setCategory(String(value))}
          options={CATEGORY_CHOICES.map((c) => ({ value: c.value, label: c.label }))}
          ariaLabel="About"
        />
      </div>
      <Input
        aria-label="Title"
        placeholder="What is wrong, in a line"
        value={title}
        onChange={(event) => setTitle(event.target.value)}
      />
      <Textarea
        aria-label="Body"
        placeholder="Why, and what to do (optional)"
        value={body}
        rows={3}
        onChange={(event) => setBody(event.target.value)}
      />
      {pick.side === "new" && (
        <label className="row gap-1">
          <input
            type="checkbox"
            checked={suggesting}
            onChange={(event) => setSuggesting(event.target.checked)}
          />
          Suggest the lines to put in their place
        </label>
      )}
      {suggesting && (
        <Textarea
          aria-label="Suggestion"
          className="mono"
          value={suggestion}
          rows={4}
          onChange={(event) => setSuggestion(event.target.value)}
        />
      )}
      <div className="row gap-2">
        <Button type="submit" size="small" variant="primary" disabled={!title.trim()}>
          Add to review
        </Button>
        <Button size="small" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  );
}
