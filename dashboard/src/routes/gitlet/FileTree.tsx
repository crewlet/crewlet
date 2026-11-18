/**
 * A diff's files as a tree on the kit's tree model, in git's order and never re-sorted, each
 * file ending in its badges, counts and status square. Its row, [TreeRow], draws the repository's.
 */

import { useMemo, useRef, type CSSProperties, type KeyboardEvent, type ReactNode } from "react";
import {
  treeItemAction,
  treeLevel,
  treePosInSet,
  treeSetSize,
  treeStep,
  useTreeState,
  type TreeInput,
} from "@crewlethq/ui";
import {
  ChevronRightGlyph,
  FolderGlyph,
  FolderOpenGlyph,
  SquareArrowRightGlyph,
  SquareDotGlyph,
  SquareMinusGlyph,
  SquarePlusGlyph,
} from "@crewlethq/icons/glyphs";
import { baseName } from "~/lib/code.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import type { CodeDiffFile } from "~/protocol/index.ts";
import { EntryGlyph } from "./ui.tsx";
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

/** A file's status as a square glyph, a type change read as modified. */
function StatusSquare({ status }: { status: string }) {
  const Glyph =
    status === "A"
      ? SquarePlusGlyph
      : status === "D"
        ? SquareMinusGlyph
        : status === "R"
          ? SquareArrowRightGlyph
          : SquareDotGlyph;
  return (
    <span className="code-ftree__status" data-status={status === "T" ? "M" : status}>
      <Glyph size="sm" aria-hidden="true" />
    </span>
  );
}

/** What a file's row says beside its counts: its badges, and their words for its name. */
export interface FileBadges {
  node: ReactNode;
  words: string;
}

/** A tree node: a directory by its path, or a file. */
type Node = { kind: "dir"; name: string; path: string } | { kind: "file"; file: CodeDiffFile };

/** The kit's forest of `dir`, each node kept by its id in `nodes`. */
function forestOf(dir: Dir, at: string, nodes: Map<string, Node>): TreeInput[] {
  const out: TreeInput[] = [];
  for (const child of dir.dirs.values()) {
    const path = at ? `${at}/${child.name}` : child.name;
    const id = `d:${path}`;
    nodes.set(id, { kind: "dir", name: child.name, path });
    out.push({ id, label: child.name, children: forestOf(child, path, nodes) });
  }
  for (const file of dir.files) {
    const id = `f:${file.path_b64 ?? file.path}`;
    nodes.set(id, { kind: "file", file });
    out.push({ id, label: baseName(file.path) });
  }
  return out;
}

/** A file's counts in words, for its row's name. */
export function countsSaid(file: Pick<CodeDiffFile, "added" | "deleted" | "binary">): string {
  return file.binary
    ? "binary"
    : `${plural(file.added, "line")} added and ${fmtCount(file.deleted)} removed`;
}

export function FileTree({
  files,
  current,
  onOpen,
  badges,
  label = "Files changed",
}: {
  files: readonly CodeDiffFile[];
  /** The path whose patch is on screen. */
  current: string;
  onOpen: (file: CodeDiffFile) => void;
  badges?: ((file: CodeDiffFile) => FileBadges | null) | undefined;
  label?: string | undefined;
}) {
  const { forest, nodes } = useMemo(() => {
    const nodes = new Map<string, Node>();
    return { forest: forestOf(treeOf(files), "", nodes), nodes };
  }, [files]);
  const shown = files.find((f) => f.path === current);
  const tree = useTreeState(forest, shown ? `f:${shown.path_b64 ?? shown.path}` : null, {
    defaultExpanded: "all",
  });
  const list = useRef<HTMLUListElement>(null);
  const focusRow = (id: string) => {
    tree.setActive(id);
    const rows = list.current?.querySelectorAll<HTMLElement>("[data-id]") ?? [];
    [...rows].find((row) => row.dataset.id === id)?.focus();
  };
  const act = (id: string) => {
    const node = nodes.get(id);
    if (node?.kind === "dir") tree.toggle(id);
    // A PATH GIT CANNOT SPELL AS TEXT names no file an address can carry: listed, not opened.
    else if (node?.kind === "file" && !node.file.path_b64) onOpen(node.file);
  };
  const onKeyDown = (event: KeyboardEvent<HTMLUListElement>) => {
    const id = (event.target as HTMLElement).closest<HTMLElement>("[data-id]")?.dataset.id;
    if (id === undefined) return;
    if (treeItemAction(event) === "activate" || event.key === " ") {
      event.preventDefault();
      act(id);
      return;
    }
    const step = treeStep(tree, id, { ...event, timeStamp: event.timeStamp });
    if (step === undefined) return;
    event.preventDefault();
    if (step === null) return;
    if ("focus" in step) focusRow(step.focus);
    else tree.toggle(step.toggle);
  };
  return (
    <ul ref={list} role="tree" className="code-ftree" aria-label={label} onKeyDown={onKeyDown}>
      {tree.rows.map((id) => {
        const node = nodes.get(id);
        if (!node) return null;
        const facts = {
          "data-id": id,
          "aria-level": treeLevel(tree.model, id),
          "aria-setsize": treeSetSize(tree.model, id),
          "aria-posinset": treePosInSet(tree.model, id),
          tabIndex: tree.active === id ? 0 : -1,
          onFocus: () => tree.setActive(id),
          onClick: () => act(id),
        };
        const depth = treeLevel(tree.model, id) - 1;
        if (node.kind === "dir") {
          const open = tree.expanded.has(id);
          return (
            <li
              key={id}
              role="treeitem"
              aria-expanded={open}
              className="code-tree__item"
              title={node.path}
              {...facts}
            >
              <TreeRow
                depth={depth}
                chevron={
                  <ChevronRightGlyph size="sm" className="code-tree__chevron" aria-hidden="true" />
                }
                glyph={
                  open ? (
                    <FolderOpenGlyph size="sm" className="code-glyph code-glyph--dir" />
                  ) : (
                    <FolderGlyph size="sm" className="code-glyph code-glyph--dir" />
                  )
                }
                name={node.name}
              />
            </li>
          );
        }
        const f = node.file;
        const said = statusWord(f);
        const extra = badges?.(f) ?? null;
        return (
          <li
            key={id}
            role="treeitem"
            className="code-tree__item"
            aria-current={f.path === current ? "true" : undefined}
            aria-disabled={f.path_b64 ? true : undefined}
            aria-label={[f.path, said.toLowerCase(), countsSaid(f), extra?.words]
              .filter(Boolean)
              .join(", ")}
            title={`${f.path} · ${said}`}
            {...facts}
          >
            <TreeRow
              depth={depth}
              chevron={<span className="code-tree__gap" />}
              glyph={
                <EntryGlyph entry={{ mode: f.new_mode ?? f.old_mode ?? "100644", path: f.path }} />
              }
              name={baseName(f.path)}
              end={
                <span className="code-ftree__facts" aria-hidden="true">
                  {extra?.node}
                  <span className="code-ftree__counts">
                    {f.binary ? (
                      "binary"
                    ) : (
                      <>
                        {f.added > 0 && (
                          <span className="code-diffstat__added">+{fmtCount(f.added)}</span>
                        )}
                        {f.deleted > 0 && (
                          <span className="code-diffstat__removed">−{fmtCount(f.deleted)}</span>
                        )}
                      </>
                    )}
                  </span>
                  <StatusSquare status={f.status} />
                </span>
              }
            />
          </li>
        );
      })}
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
