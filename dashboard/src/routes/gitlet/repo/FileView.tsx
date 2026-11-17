/**
 * A file's view: the repository's tree in a pane of its own beside the file's column,
 * which holds the path's crumbs, the file's latest commit and the file's card.
 */
import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { ButtonLink, IconButton, useClipboard } from "@crewlethq/ui";
import {
  CheckGlyph,
  CopyGlyph,
  PanelLeftGlyph,
  RotateCcwClockGlyph,
} from "@crewlethq/icons/glyphs";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { useKeymap } from "~/app/keymap.ts";
import { pathSteps } from "~/lib/code.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import type { CodeRepoDetail } from "~/protocol/index.ts";
import { repoHref } from "../paths.ts";
import type { CommitTarget } from "./FileWrites.tsx";
import { TreePane } from "./RepoTree.tsx";
import { RefPicker } from "./refs.tsx";

// The pane's choice lasts the visit; nothing is stored. Null is the default: shown unless a phone.
let treeShown: boolean | null = null;

/** Forget the pane's choice: a test's fresh page load. */
export function forgetTreeChoice(): void {
  treeShown = null;
}

export function FileView({
  repo,
  rev,
  path,
  commit,
  target,
  onRev,
  latest,
  children,
}: {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  /** The commit the file was read at, whole, or "" while it is read. */
  commit: string;
  target: CommitTarget;
  onRev: (rev: string) => void;
  /** The file's latest commit, a card of its own. */
  latest: ReactNode;
  /** The file's card. */
  children: ReactNode;
}) {
  const view = useRef<HTMLDivElement>(null);
  const narrow = useNarrowerThan(view, PHONE_BREAKPOINT);
  // NO PANE BEFORE THE VIEW IS MEASURED, so a phone's hidden pane never mounts and reads.
  const [measured, setMeasured] = useState(false);
  useLayoutEffect(() => setMeasured(true), []);
  const [shown, setShownNow] = useState(treeShown);
  const setShown = (next: boolean) => {
    treeShown = next;
    setShownNow(next);
  };
  const drawn = shown ?? (measured && !narrow);
  // THE LAST WHOLE COMMIT of this repository and revision, held while another path reads.
  const key = `${repo.name}\n${rev}`;
  const [held, setHeld] = useState({ key, commit });
  if (commit && (held.key !== key || held.commit !== commit)) setHeld({ key, commit });
  const treeCommit = commit || (held.key === key ? held.commit : "");
  const collapse = useRef<HTMLButtonElement>(null);
  const show = useRef<HTMLButtonElement>(null);
  const focusNext = useRef<"collapse" | "show" | "find" | null>(null);
  useEffect(() => {
    const next = focusNext.current;
    focusNext.current = null;
    if (next === "collapse") collapse.current?.focus();
    else if (next === "show") show.current?.focus();
    else if (next === "find") findField(view.current)?.focus();
  }, [drawn]);
  // `t` FOCUSES GO TO FILE IN THE PANE, showing a hidden pane first.
  useKeymap({
    "code.find": () => {
      const field = findField(view.current);
      if (drawn && field) {
        field.focus();
      } else {
        focusNext.current = "find";
        setShown(true);
      }
    },
  });
  return (
    <div ref={view} className="code-file-view" data-tree={drawn ? "shown" : "hidden"}>
      {drawn && (
        <TreePane
          repo={repo}
          rev={rev}
          commit={treeCommit}
          path={path}
          target={target}
          onRev={onRev}
          collapseRef={collapse}
          onCollapse={() => {
            focusNext.current = "show";
            setShown(false);
          }}
        />
      )}
      <div className="code-file-col">
        <FileCrumbs
          repo={repo}
          rev={rev}
          path={path}
          lead={
            drawn || (shown === null && !measured) ? null : (
              <>
                <IconButton
                  ref={show}
                  label="Show the file tree"
                  icon={<PanelLeftGlyph />}
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    focusNext.current = "collapse";
                    setShown(true);
                  }}
                />
                <RefPicker repo={repo} value={rev} onChoose={onRev} />
              </>
            )
          }
        />
        {latest}
        {children}
      </div>
    </div>
  );
}

const findField = (view: HTMLElement | null) =>
  view?.querySelector<HTMLInputElement>('.code-tree [role="combobox"]') ?? null;

/** The path back up to the root, each directory a link; the path copied; the file's history. */
function FileCrumbs({
  repo,
  rev,
  path,
  lead,
}: {
  repo: CodeRepoDetail;
  rev: string;
  path: string;
  /** What leads the row where the pane is hidden: its Show, and the picker. */
  lead: ReactNode;
}) {
  const at = (p: string) => repoHref(repo.name, { ref: rev, path: p });
  return (
    <div className="code-file-crumbs">
      {lead && <span className="code-file-crumbs__lead">{lead}</span>}
      <span className="code-file-crumbs__where">
        <nav aria-label="Path of the file" className="code-file-crumbs__path">
          <a className="code-file-crumbs__link" href={at("")}>
            {repo.name}
          </a>
          {pathSteps(path).map((step) => (
            <span key={step.path} className="code-file-crumbs__step">
              <span className="code-file-crumbs__slash" aria-hidden="true">
                /
              </span>
              {step.path === path ? (
                <strong className="code-file-crumbs__here" aria-current="location">
                  {step.name}
                </strong>
              ) : (
                <a className="code-file-crumbs__link" href={at(step.path)}>
                  {step.name}
                </a>
              )}
            </span>
          ))}
        </nav>
        <CopyPath path={path} />
      </span>
      <ButtonLink
        size="small"
        variant="secondary"
        className="code-file-crumbs__history"
        href={repoHref(repo.name, { tab: "commits", ref: rev, path })}
        leadingIcon={<RotateCcwClockGlyph size="sm" />}
      >
        History
      </ButtonLink>
    </div>
  );
}

/** Copy the path from the root, never a URL. */
function CopyPath({ path }: { path: string }) {
  const { state, copy } = useClipboard({ failedResetMs: null });
  return (
    <IconButton
      label={
        state === "copied"
          ? `Copied ${path}`
          : state === "failed"
            ? "The browser refused the clipboard"
            : `Copy the path ${path}`
      }
      icon={state === "copied" ? <CheckGlyph /> : <CopyGlyph />}
      size="sm"
      variant="ghost"
      onClick={() => void copy(path)}
    />
  );
}
