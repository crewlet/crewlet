/**
 * The files a diff changes, as a code host lists them beside the diff: a tree
 * of their directories — a run of directories holding nothing but one more
 * directory folded into one row, `internal/render` — each file its status
 * letter, its name and how many lines it added and removed, the one on screen
 * marked. Pressing a file opens its patch.
 *
 * GROUPED, NEVER RE-SORTED: the files arrive a page at a time in the order git
 * diffed them, and the tree keeps that order inside each directory, its
 * directories in the order their first file arrived — so a page loaded later
 * lands each file in its own directory and moves no row a reader has read.
 *
 * THE STATUS IS A LETTER AND THE COUNTS ARE WORDS, in the tertiary ink, each
 * with its word in the title (`dashboard-design.md#the-gitlet-workspace`).
 *
 * Its row ([TreeRow]) is drawn by both trees, the diff's and the repository's
 * (`repo/RepoTree.tsx`).
 */

import { useMemo, type CSSProperties, type ReactNode } from "react";
import { FolderGlyph } from "@crewlethq/icons/glyphs";
import { baseName } from "~/lib/code.ts";
import { fmtCount } from "~/lib/format.ts";
import type { CodeDiffFile } from "~/protocol/index.ts";
import "~/styles/gitlet.css";

/** What git's status letter says of a file, in a word. */
/** `+207 −51`: lines added and removed, each figure in its diff ink. */
export function DiffCounts({ added, deleted }: { added: number; deleted: number }) {
  return (
    <>
      <span className="code-diffstat__added">+{fmtCount(added)}</span>{" "}
      <span className="code-diffstat__removed">−{fmtCount(deleted)}</span>
    </>
  );
}

export function statusWord(f: Pick<CodeDiffFile, "status" | "old_path">): string {
  switch (f.status) {
    case "A":
      return "Added";
    case "D":
      return "Deleted";
    case "R":
      return f.old_path ? `Renamed from ${f.old_path}` : "Renamed";
    case "T":
      return "Its type changed";
  }
  return "Modified";
}

/** One directory of the tree: the directories under it and its files, each in arrival order. */
interface Dir {
  /** Its name as the row draws it: one segment, or a folded run of them. */
  name: string;
  dirs: Map<string, Dir>;
  files: CodeDiffFile[];
}

const newDir = (name: string): Dir => ({ name, dirs: new Map(), files: [] });

/** The tree of `files`' directories, each file under its own. */
function treeOf(files: readonly CodeDiffFile[]): Dir {
  const root = newDir("");
  for (const f of files) {
    const steps = f.path.split("/");
    steps.pop();
    let at = root;
    for (const step of steps) {
      let next = at.dirs.get(step);
      if (!next) {
        next = newDir(step);
        at.dirs.set(step, next);
      }
      at = next;
    }
    at.files.push(f);
  }
  return fold(root);
}

/** Fold every directory that holds one directory and no file into that one. */
function fold(dir: Dir): Dir {
  const dirs = new Map<string, Dir>();
  for (const [key, child] of dir.dirs) {
    let folded = fold(child);
    while (folded.files.length === 0 && folded.dirs.size === 1) {
      const [only] = folded.dirs.values();
      folded = { ...only!, name: `${folded.name}/${only!.name}` };
    }
    dirs.set(key, folded);
  }
  return { ...dir, dirs };
}

/** One row of a tree: its depth, an optional chevron, its glyph, its name and what ends it. */
export function TreeRow({
  depth = 0,
  chevron,
  glyph,
  name,
  end,
}: {
  depth?: number;
  chevron?: ReactNode;
  glyph: ReactNode;
  name: ReactNode;
  end?: ReactNode;
}) {
  return (
    <span
      className="code-ftree__row"
      style={depth ? ({ "--code-depth": depth } as CSSProperties) : undefined}
    >
      {chevron}
      {glyph}
      <span className="code-ftree__name">{name}</span>
      {end !== undefined && <span className="code-ftree__end">{end}</span>}
    </span>
  );
}

/** A file's row: its status letter, its name and its counts, a press that opens it. */
function FileRow({
  file,
  current,
  onOpen,
}: {
  file: CodeDiffFile;
  current: boolean;
  onOpen: (file: CodeDiffFile) => void;
}) {
  const said = statusWord(file);
  const counts = file.binary ? "binary" : `+${fmtCount(file.added)} −${fmtCount(file.deleted)}`;
  return (
    <button
      type="button"
      className="code-ftree__file"
      aria-current={current || undefined}
      aria-label={`${file.path}, ${said.toLowerCase()}, ${counts}`}
      title={`${file.path} · ${said}`}
      // A PATH GIT CANNOT SPELL AS TEXT names no file an address can carry:
      // listed, and not opened.
      disabled={Boolean(file.path_b64)}
      onClick={() => onOpen(file)}
    >
      <TreeRow
        glyph={
          <span className="code-ftree__status" aria-hidden="true">
            {file.status}
          </span>
        }
        name={baseName(file.path)}
        end={
          <span className="code-ftree__counts" aria-hidden="true">
            {file.binary ? counts : <DiffCounts added={file.added} deleted={file.deleted} />}
          </span>
        }
      />
    </button>
  );
}

/** A directory's rows: its directories first, then its files, each nested a step in. */
function Rows({
  dir,
  current,
  onOpen,
}: {
  dir: Dir;
  current: string;
  onOpen: (file: CodeDiffFile) => void;
}) {
  return (
    <>
      {[...dir.dirs.values()].map((child) => (
        <li key={`dir:${child.name}`} className="code-ftree__dir">
          <span className="code-ftree__dirname">
            <FolderGlyph size="sm" className="code-glyph code-glyph--dir" />
            <span>{child.name}</span>
          </span>
          <ul className="code-ftree__nested">
            <Rows dir={child} current={current} onOpen={onOpen} />
          </ul>
        </li>
      ))}
      {dir.files.map((f) => (
        <li key={f.path_b64 ?? f.path}>
          <FileRow file={f} current={f.path === current} onOpen={onOpen} />
        </li>
      ))}
    </>
  );
}

export function FileTree({
  files,
  current,
  onOpen,
}: {
  files: readonly CodeDiffFile[];
  /** The path whose patch is on screen. */
  current: string;
  onOpen: (file: CodeDiffFile) => void;
}) {
  const root = useMemo(() => treeOf(files), [files]);
  return (
    <ul className="code-ftree" aria-label="Files changed">
      <Rows dir={root} current={current} onOpen={onOpen} />
    </ul>
  );
}

/**
 * The head of one file's diff, as a code host draws it across the diff: the
 * file's path, what became of it, how many lines it added and removed, and
 * the way to the file itself at the revision the diff reads to.
 */
export function DiffHead({
  path,
  file,
  viewHref,
}: {
  path: string;
  /** The file as the listing or its patch describes it, where either does. */
  file: CodeDiffFile | undefined;
  /** The file at the newer revision, where it still exists there. */
  viewHref: string | undefined;
}) {
  return (
    <div className="code-diff-head">
      <span className="code-diff-head__path mono">{path}</span>
      {file && (
        <span className="code-diff-head__facts">
          {statusWord(file)}
          {!file.binary && (
            <>
              {" · "}
              <DiffCounts added={file.added} deleted={file.deleted} />
            </>
          )}
        </span>
      )}
      {viewHref && file?.status !== "D" && (
        <a className="t-link code-diff-head__view" href={viewHref}>
          View the file
        </a>
      )}
    </div>
  );
}
