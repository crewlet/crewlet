/**
 * The repository's tree beside a file: the kit's tree model over one `code_tree` read
 * per directory opened, at the file's commit, in a pane whose list scrolls alone.
 */
import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type Ref } from "react";
import {
  Button,
  IconButton,
  Skeleton,
  treeItemAction,
  treeLevel,
  treePosInSet,
  treeSetSize,
  treeStep,
  useTreeState,
} from "@crewlethq/ui";
import {
  ChevronRightGlyph,
  FolderGlyph,
  FolderOpenGlyph,
  PanelLeftGlyph,
} from "@crewlethq/icons/glyphs";
import { shortOID } from "~/lib/code.ts";
import type { CodeRepoDetail } from "~/protocol/index.ts";
import { repoHref } from "../paths.ts";
import { EntryGlyph } from "../ui.tsx";
import { TreeRow } from "../FileTree.tsx";
import { NewFileButton, type CommitTarget } from "./FileWrites.tsx";
import { GoToFile } from "./GoToFile.tsx";
import { RefPicker, revisionWord } from "./refs.tsx";
import { ancestorsOf, dirWord, treeForest, useRepoTree, type TreeNode } from "./tree.ts";

/** The pane: collapse, the title and a new file; the picker; Go to file; then the tree. */
export function TreePane({
  repo,
  rev,
  commit,
  path,
  target,
  onRev,
  onCollapse,
  collapseRef,
}: {
  repo: CodeRepoDetail;
  rev: string;
  /** The commit the file was read at, whole, or "" while it is read. */
  commit: string;
  /** The file on screen. */
  path: string;
  target: CommitTarget;
  onRev: (rev: string) => void;
  onCollapse: () => void;
  collapseRef: Ref<HTMLButtonElement>;
}) {
  const dir = ancestorsOf(path).at(-1) ?? "";
  const label = `Files of ${repo.name} at ${revisionWord(repo, rev)}`;
  return (
    <aside className="code-tree" aria-label={label}>
      <div className="code-tree__head">
        <div className="code-tree__title">
          <IconButton
            ref={collapseRef}
            label="Collapse the file tree"
            icon={<PanelLeftGlyph />}
            size="sm"
            variant="ghost"
            onClick={onCollapse}
          />
          <h2 className="code-tree__name">Files</h2>
          <NewFileButton repo={repo} target={target} dir={dir} compact />
        </div>
        <div className="code-tree__picker">
          <RefPicker repo={repo} value={rev} onChoose={onRev} />
        </div>
        <GoToFile repo={repo.name} rev={rev} commit={commit} keyed={false} />
      </div>
      <RepoTree repo={repo} rev={rev} commit={commit} path={path} label={label} />
    </aside>
  );
}

/** A directory's state row under it: reading, refused, failed, or Load more. */
function stateRow(node: TreeNode): { text: string; act: boolean; busy: boolean } {
  switch (node.kind) {
    case "loading":
      return { text: `Loading ${dirWord(node.dir)}…`, act: false, busy: true };
    case "refused":
      return {
        text: `This directory's listing is unavailable${node.refusal ? `: ${node.refusal}` : "."}`,
        act: false,
        busy: false,
      };
    case "failed":
      return { text: "This directory could not be read. Retry", act: true, busy: false };
    case "more":
      return node.level.pageFailed
        ? { text: `Loading more of ${dirWord(node.dir)} failed: Retry`, act: true, busy: false }
        : {
            text: `Load more in ${dirWord(node.dir)}`,
            act: !node.level.paging,
            busy: node.level.paging,
          };
  }
  return { text: "", act: false, busy: false };
}

/** The tree itself: a directory opens in place, a file is a link to its view at the address's revision. */
export function RepoTree({
  repo,
  rev,
  commit,
  path,
  label,
}: {
  repo: CodeRepoDetail;
  rev: string;
  commit: string;
  path: string;
  label: string;
}) {
  // THE FILE'S ANCESTORS OPEN ON ARRIVAL and whenever the file changes; another repository starts again.
  const [held, setHeld] = useState({ repo: repo.name, path, open: new Set(ancestorsOf(path)) });
  let open = held.open;
  if (held.repo !== repo.name || held.path !== path) {
    open = new Set(held.repo === repo.name ? held.open : []);
    for (const dir of ancestorsOf(path)) open.add(dir);
    setHeld({ repo: repo.name, path, open });
  }
  const dirs = useMemo(() => ["", ...open], [open]);
  const reads = useRepoTree(repo.name, commit, dirs);
  const forest = useMemo(() => treeForest(reads.levels), [reads.levels]);
  const tree = useTreeState(forest.roots, path, {
    defaultExpanded: "none",
    expanded: open,
    onExpandedChange: (next) => setHeld((h) => ({ ...h, open: new Set(next) })),
  });
  const list = useRef<HTMLUListElement>(null);
  const scroller = useRef<HTMLDivElement>(null);
  // THE CURRENT ROW INTO THE LIST'S VIEW once per path, by the list's own scroll: never the page's.
  const shown = useRef("");
  const drawn = tree.rows.includes(path);
  useEffect(() => {
    const box = scroller.current;
    if (!drawn || !box || shown.current === path) return;
    const row = box.querySelector<HTMLElement>('[aria-current="page"]');
    if (!row) return;
    shown.current = path;
    const at = row.getBoundingClientRect();
    const view = box.getBoundingClientRect();
    if (at.top < view.top || at.bottom > view.bottom) {
      box.scrollTop += at.top - view.top - (view.height - at.height) / 2;
    }
  }, [drawn, path]);
  const focusRow = (id: string) => {
    tree.setActive(id);
    const rows = list.current?.querySelectorAll<HTMLElement>("[data-id]") ?? [];
    [...rows].find((row) => row.dataset.id === id)?.focus();
  };
  const act = (node: TreeNode | undefined, id: string) => {
    if (!node) return;
    if (node.kind === "dir") tree.toggle(id);
    else if (node.kind === "more") reads.more(node.dir);
    if (node.kind === "failed" || (node.kind === "more" && node.level.pageFailed)) {
      reads.retry(node.dir);
    }
  };
  const onKeyDown = (event: KeyboardEvent<HTMLUListElement>) => {
    const id = (event.target as HTMLElement).closest<HTMLElement>("[data-id]")?.dataset.id;
    if (id === undefined) return;
    const node = forest.nodes.get(id);
    if (treeItemAction(event) === "activate" || (event.key === " " && node?.kind !== "file")) {
      // A FILE'S ENTER IS ITS LINK'S OWN, so a chord with it opens a tab.
      if (node?.kind === "file" && event.key === "Enter") return;
      event.preventDefault();
      act(node, id);
      return;
    }
    const step = treeStep(tree, id, { ...event, timeStamp: event.timeStamp });
    if (step === undefined) return;
    event.preventDefault();
    if (step === null) return;
    if ("focus" in step) focusRow(step.focus);
    else tree.toggle(step.toggle);
  };
  const root = reads.levels.get("");
  let body;
  if (!commit || !root || root.state === "reading") {
    body = <Skeleton variant="text" rows={4} label="Reading the files" />;
  } else if (root.state === "refused") {
    body = (
      <p className="code-tree__note" role="status">
        {`This directory's listing is unavailable${root.refusal ? `: ${root.refusal}` : "."}`}
      </p>
    );
  } else if (root.state === "failed") {
    body = (
      <p className="code-tree__note" role="status">
        The files could not be read.{" "}
        <Button size="small" variant="secondary" onClick={() => reads.retry("")}>
          Retry
        </Button>
      </p>
    );
  } else if (forest.roots.length === 0) {
    body = <p className="code-tree__note">No file at this commit.</p>;
  } else {
    body = (
      <ul ref={list} role="tree" aria-label={label} className="code-ftree" onKeyDown={onKeyDown}>
        {tree.rows.map((id) => {
          const node = forest.nodes.get(id);
          if (!node) return null;
          const facts = {
            "data-id": id,
            "aria-level": treeLevel(tree.model, id),
            "aria-setsize": treeSetSize(tree.model, id),
            "aria-posinset": treePosInSet(tree.model, id),
            tabIndex: tree.active === id ? 0 : -1,
            onFocus: () => tree.setActive(id),
          };
          const depth = treeLevel(tree.model, id) - 1;
          if (node.kind === "dir") {
            const isOpen = tree.expanded.has(id);
            return (
              <li
                key={id}
                role="treeitem"
                aria-expanded={isOpen}
                className="code-tree__item"
                title={node.path}
                onClick={() => act(node, id)}
                {...facts}
              >
                <TreeRow
                  depth={depth}
                  chevron={
                    <ChevronRightGlyph
                      size="sm"
                      className="code-tree__chevron"
                      aria-hidden="true"
                    />
                  }
                  glyph={
                    isOpen ? (
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
          if (node.kind === "file") {
            const current = node.path === path;
            return (
              <li key={id} role="none">
                <a
                  role="treeitem"
                  className="code-tree__item"
                  href={repoHref(repo.name, { ref: rev, path: node.path })}
                  aria-current={current ? "page" : undefined}
                  title={node.path}
                  {...facts}
                >
                  <TreeRow
                    depth={depth}
                    chevron={<span className="code-tree__gap" />}
                    glyph={<EntryGlyph entry={node.entry} />}
                    name={node.name}
                  />
                </a>
              </li>
            );
          }
          if (node.kind === "inert") {
            const sub = node.entry.mode === "160000";
            return (
              <li
                key={id}
                role="treeitem"
                className="code-tree__item"
                data-inert=""
                title={node.path}
                {...facts}
              >
                <TreeRow
                  depth={depth}
                  chevron={<span className="code-tree__gap" />}
                  glyph={<EntryGlyph entry={node.entry} />}
                  name={node.name}
                  end={sub ? `@ ${shortOID(node.entry.oid)}` : undefined}
                />
              </li>
            );
          }
          const said = stateRow(node);
          return (
            <li
              key={id}
              role="treeitem"
              className="code-tree__item code-tree__state"
              aria-busy={said.busy || undefined}
              data-act={said.act || undefined}
              onClick={said.act ? () => act(node, id) : undefined}
              {...facts}
            >
              <TreeRow
                depth={depth}
                chevron={<span className="code-tree__gap" />}
                glyph={null}
                name={said.text}
              />
            </li>
          );
        })}
      </ul>
    );
  }
  return (
    <div ref={scroller} className="code-tree__scroll">
      {body}
    </div>
  );
}
