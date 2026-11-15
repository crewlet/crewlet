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
 * NO HUE: an added or a removed line is not a state (`docs/reference/
 * dashboard-design.md`, "Diffs carry no hue"), so the counts are words in
 * the tertiary ink and the status a letter, each with its word in the title.
 */

import { useMemo } from "react";
import { FolderGlyph } from "@crewlethq/icons/glyphs";
import { baseName } from "~/lib/code.ts";
import { fmtCount } from "~/lib/format.ts";
import type { CodeDiffFile } from "~/protocol/index.ts";
import "~/styles/gitlet.css";

/** What git's status letter says of a file, in a word. */
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
      <span className="code-ftree__status" aria-hidden="true">
        {file.status}
      </span>
      <span className="code-ftree__name">{baseName(file.path)}</span>
      <span className="code-ftree__counts" aria-hidden="true">
        {counts}
      </span>
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
          {!file.binary && ` · +${fmtCount(file.added)} −${fmtCount(file.deleted)}`}
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
