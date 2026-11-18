/** A diff's files' tree in its own scroller beside the stack, and the file the address names
 *  opened once it is listed: shared by a change's Files changed, a commit and a comparison. */

import { useEffect, useMemo, useRef, type ReactNode } from "react";
import type { CodeDiffFile } from "~/protocol/index.ts";
import { sourceParams, useSourceRead, type StackSource } from "./FileStack.tsx";
import { FileTree, type FileBadges } from "./FileTree.tsx";
import "~/styles/gitlet.css";

/** The page's width from which the tree sits beside the stack (the sheet's 720 px fold). */
export const TREE_BESIDE_FROM = 720;

/** The files' tree in its own scroller beside the stack, its current row kept in the list's view. */
export function FilesPane({
  files,
  current,
  onOpen,
  badges,
  label,
  head,
  foot,
  children,
}: {
  files: readonly CodeDiffFile[];
  current: string;
  onOpen: (f: CodeDiffFile) => void;
  badges?: ((f: CodeDiffFile) => FileBadges | null) | undefined;
  label: string;
  head?: ReactNode;
  foot: ReactNode;
  children?: ReactNode;
}) {
  const scroller = useRef<HTMLDivElement>(null);
  // THE CURRENT ROW INTO THE LIST'S VIEW by the list's own scroll, never the page's.
  useEffect(() => {
    const box = scroller.current;
    const row = box?.querySelector<HTMLElement>('[aria-current="true"]');
    if (!box || !row) return;
    const at = row.getBoundingClientRect();
    const view = box.getBoundingClientRect();
    if (at.top < view.top || at.bottom > view.bottom) {
      box.scrollTop += at.top - view.top - (view.height - at.height) / 2;
    }
  }, [current]);
  return (
    <aside className="code-tree code-files__pane" aria-label={label}>
      {head && <div className="code-tree__head">{head}</div>}
      <div ref={scroller} className="code-tree__scroll">
        <FileTree files={files} current={current} onOpen={onOpen} badges={badges} />
        {children}
      </div>
      <div className="code-tree__foot">{foot}</div>
    </aside>
  );
}

/** The file the address names, opened once listed. Answers whether the diff does not change it. */
export function useFileArrival({
  source,
  file,
  listed,
  loaded,
  more,
  paging,
  pageError,
  next,
  onArrive,
}: {
  source: StackSource;
  file: string;
  /** The files the pages read so far list, and whether the first page has arrived. */
  listed: readonly CodeDiffFile[];
  loaded: boolean;
  more: boolean;
  paging: boolean;
  pageError: string | null;
  next: () => void;
  onArrive: (f: CodeDiffFile) => void;
}): { unchanged: boolean } {
  const arrived = useRef(false);
  const listsIt = listed.some((one) => one.path === file);
  // A FILE THE PAGES LISTED LACK is asked alone first, so a path the diff does
  // not change pages nothing; one it changes is listed by the pages after.
  const params = useMemo(() => sourceParams(source, { file }), [source, file]);
  const { first: named } = useSourceRead(
    source,
    params,
    file !== "" && !listsIt && more && !arrived.current,
  );
  const namedEntry = named.data?.diff?.files?.find((one) => one.path === file);
  const unchanged =
    file !== "" &&
    !listsIt &&
    !arrived.current &&
    ((named.data !== undefined && !namedEntry) || (loaded && !more));
  useEffect(() => {
    if (arrived.current || !file) return;
    const f = listed.find((one) => one.path === file);
    if (!f) {
      if (namedEntry && more && !paging && !pageError) next();
      return;
    }
    arrived.current = true;
    onArrive(f);
  }, [file, listed, namedEntry, more, paging, pageError, next, onArrive]);
  return { unchanged };
}
