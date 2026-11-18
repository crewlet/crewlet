/**
 * A change's Files changed tab: every file a revision changes on one page, a
 * stack of sticky-headed diffs (`../FileStack.tsx`) beside the files' tree in
 * its own scroller, with what lies on each line drawn under it (`StackNotes.tsx`).
 *
 * # Read by object id, placed by the engine
 *
 * A revision's files, their patches and where each finding sits are the
 * engine's reads (`code_change` parts `files`, `diff` and `findings`): the
 * diff from the base the revision's facts record, or with `since=` what
 * changed since an earlier revision, and each finding placed by the engine's
 * one rule (`gitlet.PlaceFindings`), which a seat's `code_get_change` reads by
 * too. The page compares and moves nothing itself.
 *
 * # Outdated is said, never hidden
 *
 * A finding whose own lines changed since it was raised, one whose file is
 * gone, and a thread on another revision are drawn above their file's diff
 * under a Disclosure that counts them; findings and comments past one page
 * are said above the stack, since the pages are oldest first.
 *
 * # A finding is drafted on its lines, and sent with a review
 *
 * On the head, pressing a line's number picks it and a finding is drafted
 * under the pick; a draft is the tab's (`lib/reviewDrafts.ts`) until Finish
 * review sends every draft of the head in ONE `code_review_change`.
 */

import { useCallback, useMemo, useRef, useState } from "react";
import {
  Button,
  Checkbox,
  Combobox,
  IconButton,
  Input,
  Kbd,
  Popover,
  SegmentedControl,
  Select,
  Skeleton,
  Stack,
  useToast,
  type ComboboxOption,
} from "@crewlethq/ui";
import {
  Columns2Glyph,
  FlagGlyph,
  ListFilterGlyph,
  PanelLeftGlyph,
  PencilGlyph,
  Rows2Glyph,
  SearchGlyph,
  Settings2Glyph,
} from "@crewlethq/icons/glyphs";
import { capsOf, keyRow, useKeymap } from "~/app/keymap.ts";
import { useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { changeLive, refName, shortOID } from "~/lib/code.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { useReviewDrafts } from "~/lib/reviewDrafts.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type {
  CodeChangeAnswer,
  CodeChangeDetail,
  CodeDiffFile,
  CodeRevisionRow,
} from "~/protocol/index.ts";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { type FileBadges } from "../FileTree.tsx";
import { FIND_LIMIT, Matched } from "../repo/GoToFile.tsx";
import { FileStack, fileKey, matchPath, type StackJump, type StackSource } from "../FileStack.tsx";
import { FilesPane, TREE_BESIDE_FROM, useFileArrival } from "../FilesPane.tsx";
import { changeHref } from "../paths.ts";
import { ChangeLines, StackNotesContext } from "./StackNotes.tsx";
import { findingStands } from "./FindingAnswer.tsx";
import { FinishReview } from "./ReviewDialog.tsx";
import {
  changePartParams,
  partMovedBy,
  revisionInputs,
  useAskAgainWhenMoved,
  partRefusal,
  refusedWords,
  usePagedComments,
  usePagedFindingPaths,
  usePagedFindings,
} from "./reads.ts";
import "~/styles/gitlet.css";

/** A whole number from 1 off the address, or null. */
function wholeParam(value: string): number | null {
  if (!/^[1-9][0-9]{0,15}$/.test(value)) return null;
  const n = Number(value);
  return Number.isSafeInteger(n) ? n : null;
}

/** A file's extension, "" where its name has none. */
function extOf(path: string): string {
  const name = path.slice(path.lastIndexOf("/") + 1);
  const dot = name.lastIndexOf(".");
  return dot > 0 ? name.slice(dot + 1) : "";
}

/** A list in the address: "a,b" as its items, none for "". */
const listParam = (value: string): string[] => (value ? value.split(",").filter(Boolean) : []);

/** An extension as `ext=` writes it: "." for none, which no extension can be, and commas escaped. */
const extToken = (ext: string): string =>
  ext === "" ? "." : ext.replace(/%/g, "%25").replace(/,/g, "%2C");
const tokenExt = (token: string): string =>
  token === "." ? "" : token.replace(/%2C/gi, ",").replace(/%25/g, "%");

// A PAGE OF A REVISION'S FILES, as `usePaged` reads it. MODULE CONSTANTS, so
// the list is the same list until a page arrives.
const filesOf = (a: CodeChangeAnswer): readonly CodeDiffFile[] => a.diff?.files ?? [];
const nextOf = (a: CodeChangeAnswer): string => a.next ?? "";

export function Files({
  change,
  revisions: rows,
}: {
  change: CodeChangeDetail;
  /** The detail's page of revisions, newest first: what each diff is read from. */
  revisions: readonly CodeRevisionRow[];
}) {
  const [revParam, setRev] = useParam("rev", "", "filter");
  const [sinceParam, setSince] = useParam("since", "", "filter");
  const [fileParam, setFile] = useParam("file", "", "filter");
  const [layout, setLayout] = useParam("layout", "unified", "filter");
  const [thread] = useParam("thread", "", "filter");
  const [q, setQ] = useParam("q", "", "filter");
  const [extParam, setExt] = useParam("ext", "", "filter");
  const [onlyParam, setOnly] = useParam("only", "", "filter");
  const comments = usePagedComments(change);
  const threadRoot = comments.items.find((c) => c.id === thread && c.anchor);
  // A THREAD NAMED BY THE ADDRESS opens the revision and the file it is on.
  const rev = Math.min(
    wholeParam(revParam) ?? threadRoot?.anchor?.revision ?? change.revision,
    change.revision,
  );
  const sinceAsked = wholeParam(sinceParam);
  const since = sinceAsked !== null && sinceAsked < rev ? sinceAsked : null;
  const file = fileParam || threadRoot?.anchor?.path || "";
  // THE DIFF IS ASKED AGAIN ONLY WHEN WHAT IT IS READ FROM MOVES (reads.ts).
  const diffMoved = `${change.version}/${revisionInputs(rows, rev)}/${
    since !== null ? revisionInputs(rows, since) : ""
  }`;
  const filesParams = useMemo(
    () =>
      changePartParams({ repo: change.repo, number: change.number }, "files", {
        revision: rev,
        ...(since !== null ? { since_revision: since } : {}),
      }),
    [change.repo, change.number, rev, since],
  );
  const files = useCodeQuery("code_change", filesParams);
  const pagedFiles = usePaged(
    files,
    filesParams,
    (cursor) => codeRest.query("code_change", { ...filesParams, cursor }),
    filesOf,
    fileKey,
    nextOf,
  );
  useAskAgainWhenMoved(diffMoved, true, pagedFiles.refresh);
  const findings = usePagedFindings(
    change,
    rev,
    `${partMovedBy(change, "findings")}/${revisionInputs(rows, rev)}`,
  );
  const placed = useMemo(
    () => Object.assign({}, ...findings.answers.map((a) => a.placed ?? {})),
    [findings.answers],
  );
  // THE NEXT PAGE OF EACH PART THAT HAS ONE, at one press: a line's findings
  // and its threads are drawn together.
  const loadMoreSaid = () => {
    if (findings.more) findings.next();
    if (comments.more) comments.next();
  };
  const pagingSaid = findings.paging || comments.paging;
  const saidPageError = findings.pageError ?? comments.pageError;
  // A FINDING IS DRAFTED ON THE HEAD, read whole: never an interdiff's or an earlier revision's.
  const live = changeLive(change.state);
  const drafting = rev === change.revision && since === null && live;
  const drafts = useReviewDrafts(change.id, change.revision);
  // WHAT LIES ON THE LINES, one object until it moves: each section is drawn
  // again only when what it shows does.
  const { drafts: drafted, add, remove, update, clear } = drafts;
  const notesOf = useMemo(
    () => ({
      change,
      rev,
      since,
      notes: {
        findings: findings.items,
        placed,
        comments: comments.items,
        drafts: drafting ? { drafts: drafted, add, remove, update, clear } : null,
        thread,
      },
    }),
    [
      change,
      rev,
      since,
      findings.items,
      placed,
      comments.items,
      drafting,
      drafted,
      add,
      remove,
      update,
      clear,
      thread,
    ],
  );
  const listed = pagedFiles.items;
  // WHAT THE FILTERS KEEP: the words a path holds, the extensions shown, and
  // the files with a standing finding.
  const findingPaths = usePagedFindingPaths(change);
  const standing = useMemo(() => {
    const at = new Map<string, number>();
    for (const row of findingPaths.items) {
      if (findingStands(row.state)) at.set(row.path, (at.get(row.path) ?? 0) + row.findings);
    }
    return at;
  }, [findingPaths.items]);
  const pathsRefused = partRefusal(findingPaths);
  // WITH FINDINGS IS HELD until every page of the counts is read: a partial
  // count would hide files without saying so.
  const pathsRead =
    pathsRefused === null && !findingPaths.loading && !findingPaths.more && !findingPaths.error;
  const exts = useMemo(() => {
    const at = new Map<string, number>();
    for (const f of listed) at.set(extOf(f.path), (at.get(extOf(f.path)) ?? 0) + 1);
    return at;
  }, [listed]);
  const shownExts = listParam(extParam).map(tokenExt);
  const only = listParam(onlyParam);
  const byFindings = only.includes("findings") && pathsRead;
  const words = q.toLowerCase().split(/\s+/).filter(Boolean);
  const keeps = (f: CodeDiffFile) =>
    words.every((w) => f.path.toLowerCase().includes(w)) &&
    (shownExts.length === 0 || shownExts.includes(extOf(f.path))) &&
    (!byFindings || standing.has(f.path));
  const filtering = words.length > 0 || shownExts.length > 0 || byFindings;
  const visible = filtering ? listed.filter(keeps) : listed;
  const clearFilters = () => {
    setQ("");
    setExt("");
    setOnly("");
  };
  const indexOf = useMemo(() => {
    const at = new Map(listed.map((f, i) => [fileKey(f), i]));
    return (f: CodeDiffFile) => at.get(fileKey(f)) ?? -1;
  }, [listed]);
  // THE TREE BESIDE THE STACK on a wide page; on a narrow one, once asked for.
  const box = useRef<HTMLDivElement>(null);
  const narrow = useNarrowerThan(box, TREE_BESIDE_FROM);
  const [treeChoice, setTreeChoice] = useState<boolean | null>(null);
  const treeShown = treeChoice ?? !narrow;
  const [current, setCurrent] = useState("");
  const [jump, setJump] = useState<StackJump | null>(null);
  const toast = useToast();
  const go = useCallback(
    (f: CodeDiffFile) => {
      // A FILE A FILTER HIDES is shown by clearing the filters first.
      if (!visible.includes(f)) {
        setQ("");
        setExt("");
        setOnly("");
        toast.ok(`Filters cleared to show ${f.path}`);
      }
      if (!f.path_b64) setFile(f.path);
      setCurrent(f.path);
      setJump((was) => ({ key: fileKey(f), seq: (was?.seq ?? 0) + 1 }));
    },
    [setFile, visible, setQ, setExt, setOnly, toast],
  );
  // THE FILE THE ADDRESS NAMES, once it is listed: the stack opens at it.
  const source = useMemo<StackSource>(
    () => ({ kind: "change", change: { repo: change.repo, number: change.number }, rev, since }),
    [change.repo, change.number, rev, since],
  );
  const onArrive = useCallback((f: CodeDiffFile) => {
    setCurrent(f.path);
    setJump({ key: fileKey(f), seq: 1 });
  }, []);
  const { unchanged } = useFileArrival({
    source,
    file,
    listed,
    loaded: files.data !== undefined,
    more: pagedFiles.more,
    paging: pagedFiles.paging,
    pageError: pagedFiles.pageError,
    next: pagedFiles.next,
    onArrive,
  });
  const linkOf = useCallback(
    (f: CodeDiffFile) =>
      changeHref(change.repo, change.number, {
        tab: "files",
        ...(rev !== change.revision ? { rev } : {}),
        file: f.path,
      }),
    [change.repo, change.number, change.revision, rev],
  );
  const draftsOn = new Map<string, number>();
  if (drafting)
    for (const d of drafts.drafts) draftsOn.set(d.path, (draftsOn.get(d.path) ?? 0) + 1);
  const badges = (f: CodeDiffFile): FileBadges | null => {
    const found = standing.get(f.path) ?? 0;
    const mine = draftsOn.get(f.path) ?? 0;
    if (found + mine === 0) return null;
    return {
      node: (
        <>
          {found > 0 && (
            <span
              className="code-ftree__badge"
              title={`${plural(found, "open finding")} raised on this path`}
            >
              <FlagGlyph size="xs" aria-hidden="true" />
              {found}
            </span>
          )}
          {mine > 0 && (
            <span
              className="code-ftree__badge"
              title={`${plural(mine, "draft")} of yours on this file`}
            >
              <PencilGlyph size="xs" aria-hidden="true" />
              {mine}
            </span>
          )}
        </>
      ),
      words: [found > 0 && plural(found, "open finding"), mine > 0 && plural(mine, "draft")]
        .filter(Boolean)
        .join(", "),
    };
  };
  const openFindings = [...standing.values()].reduce((n, k) => n + k, 0);
  const showing = filtering && (
    <p className="code-files__showing">
      {visible.length === 0
        ? "No file matches these filters."
        : pagedFiles.more
          ? `Showing ${fmtCount(visible.length)} of the ${plural(listed.length, "file")} loaded`
          : `Showing ${fmtCount(visible.length)} of ${plural(listed.length, "file")}`}{" "}
      <Button size="small" variant="ghost" onClick={clearFilters}>
        Clear filters
      </Button>
    </p>
  );
  const head = rows.find((r) => r.n === rev)?.head ?? (rev === change.revision ? change.head : "");
  const revisions = Array.from({ length: change.revision }, (_, i) => change.revision - i);
  const target = refName(change.target_ref).name;
  const loadMoreFiles = pagedFiles.more && (
    <Button
      size="small"
      variant="secondary"
      onClick={pagedFiles.next}
      disabled={pagedFiles.paging}
      loading={pagedFiles.paging}
    >
      Load more files
    </Button>
  );
  return (
    <div ref={box}>
      <Stack gap={3}>
        <PageToolbar className="code-files-bar">
          <IconButton
            label={treeShown ? "Hide the file tree" : "Show the file tree"}
            icon={<PanelLeftGlyph />}
            size="sm"
            variant="ghost"
            aria-pressed={treeShown}
            onClick={() => setTreeChoice(!treeShown)}
          />
          <Select
            size="sm"
            width="auto"
            value={String(rev)}
            onChange={(value) =>
              setRev(String(value) === String(change.revision) ? "" : String(value))
            }
            options={revisions.map((n) => ({
              value: String(n),
              label: n === change.revision ? `Revision ${n} · head` : `Revision ${n}`,
            }))}
            ariaLabel="Revision"
          />
          <span className="code-files-bar__words">compared with</span>
          <Select
            size="sm"
            width="auto"
            value={since === null ? "" : String(since)}
            onChange={(value) => setSince(String(value))}
            options={[
              { value: "", label: `${target} · base` },
              ...revisions
                .filter((n) => n < rev)
                .map((n) => ({ value: String(n), label: `Revision ${n}` })),
            ]}
            ariaLabel="Compared with"
            active={since !== null}
          />
          <GoToChanged files={listed} onGo={go} />
          <span className="code-files-bar__end">
            <span className="code-files-bar__layout">
              <SegmentedControl
                label="Layout"
                semantics="radio"
                size="sm"
                value={layout === "split" ? "split" : "unified"}
                onValueChange={(value) => setLayout(value)}
                options={[
                  { value: "unified", label: "Unified", icon: <Rows2Glyph size="sm" /> },
                  { value: "split", label: "Split", icon: <Columns2Glyph size="sm" /> },
                ]}
              />
            </span>
            {live && <FinishReview change={change} drafts={drafts.drafts.length} />}
          </span>
        </PageToolbar>
        {(findings.more || comments.more) && (
          <Button
            variant="secondary"
            onClick={loadMoreSaid}
            disabled={pagingSaid}
            loading={pagingSaid}
          >
            Load more findings and comments
          </Button>
        )}
        {saidPageError && <PageFailed error={saidPageError} />}
        {files.loading && !files.data && (
          <Skeleton variant="text" rows={4} label="Loading the files" />
        )}
        <QueryState
          error={files.error}
          refusal={files.refusal}
          detail={files.detail}
          loading={files.loading && !files.data}
          empty={
            files.data && listed.length === 0 && !pagedFiles.more
              ? {
                  title: "No file changed",
                  hint: "This revision changes nothing against what it is compared with.",
                }
              : undefined
          }
        >
          <div className="code-files" data-tree={treeShown ? "shown" : "hidden"}>
            {treeShown && (
              <FilesPane
                files={visible}
                current={current || file}
                onOpen={go}
                badges={badges}
                label={`Files changed in revision ${rev}`}
                head={
                  <div className="code-files__filter">
                    <Input
                      inputSize="sm"
                      aria-label="Filter files"
                      placeholder="Filter files"
                      leading={<ListFilterGlyph size="sm" />}
                      value={q}
                      onChange={(event) => setQ(event.target.value)}
                      containerClassName="code-files__words"
                    />
                    <FilterOptions
                      exts={exts}
                      shown={shownExts}
                      onExts={(next) =>
                        setExt(next.length === exts.size ? "" : next.map(extToken).join(","))
                      }
                      only={only}
                      onOnly={(next) => setOnly(next.join(","))}
                      withFindings={listed.filter((f) => standing.has(f.path)).length}
                      findingsHeld={
                        pathsRefused !== null
                          ? refusedWords("Findings by file are unavailable", pathsRefused)
                          : !pathsRead
                            ? "Findings by file are still being read."
                            : undefined
                      }
                      narrowing={filtering}
                      onClear={clearFilters}
                    />
                  </div>
                }
                foot={
                  <>
                    <ComparedWith
                      change={change}
                      rows={rows}
                      rev={rev}
                      since={since}
                      target={target}
                    />
                    {pathsRefused !== null ? (
                      <p className="code-tree__said">
                        Findings by file are unavailable on this fleet yet.
                      </p>
                    ) : (
                      (openFindings > 0 || drafts.drafts.length > 0) && (
                        <p className="code-tree__said code-files__counts">
                          {openFindings > 0 && (
                            <span>
                              <FlagGlyph size="xs" aria-hidden="true" />
                              {plural(openFindings, "open finding")}
                            </span>
                          )}
                          {openFindings > 0 && drafts.drafts.length > 0 && " · "}
                          {drafts.drafts.length > 0 && (
                            <span>
                              <PencilGlyph size="xs" aria-hidden="true" />
                              {`your ${plural(drafts.drafts.length, "draft")}${drafting ? "" : ` on revision ${change.revision}`}`}
                            </span>
                          )}
                        </p>
                      )
                    )}
                    {showing}
                  </>
                }
              >
                {loadMoreFiles && <span>{loadMoreFiles}</span>}
                {pagedFiles.pageError && <PageFailed error={pagedFiles.pageError} />}
              </FilesPane>
            )}
            <StackNotesContext.Provider value={notesOf}>
              <FileStack
                source={source}
                repo={change.repo}
                head={head}
                moved={diffMoved}
                files={visible}
                lead={
                  <>
                    {unchanged && (
                      <p className="muted" role="status">
                        {since !== null
                          ? `${file} is not among the files changed since revision ${since}.`
                          : `${file} is not among the files revision ${rev} changes.`}
                      </p>
                    )}
                    {showing}
                  </>
                }
                indexOf={indexOf}
                layout={layout === "split" ? "split" : "unified"}
                linkOf={linkOf}
                lines={ChangeLines}
                jump={jump}
                onCurrent={setCurrent}
                foot={loadMoreFiles && <span>{loadMoreFiles}</span>}
              />
            </StackNotesContext.Provider>
          </div>
        </QueryState>
      </Stack>
    </div>
  );
}

/** "Compared with main at c71d0e9", or the revision a comparison reads since. */
function ComparedWith({
  change,
  rows,
  rev,
  since,
  target,
}: {
  change: CodeChangeDetail;
  rows: readonly CodeRevisionRow[];
  rev: number;
  since: number | null;
  target: string;
}) {
  if (since !== null) {
    const was = rows.find((r) => r.n === since)?.head;
    return (
      <p className="code-tree__said">
        Compared with <strong>revision {since}</strong>
        {was && (
          <>
            {" "}
            at <code>{shortOID(was)}</code>
          </>
        )}
      </p>
    );
  }
  const base = rows.find((r) => r.n === rev)?.facts?.base_oid;
  if (!base) {
    return <p className="code-tree__said">Compared with its base once it is recorded</p>;
  }
  return (
    <p className="code-tree__said">
      Compared with <strong>{change.target_ref ? target : "its base"}</strong> at{" "}
      <code>{shortOID(base)}</code>
    </p>
  );
}

/** Go to file over a diff's own listed files: `t` focuses it, a choice brings its head to the top. */
function GoToChanged({
  files,
  onGo,
}: {
  files: readonly CodeDiffFile[];
  onGo: (f: CodeDiffFile) => void;
}) {
  const field = useRef<HTMLInputElement>(null);
  const [typed, setTyped] = useState("");
  const [open, setOpen] = useState(false);
  useKeymap({ "code.find": { run: () => field.current?.focus(), when: true } });
  const ranked = useMemo(() => {
    if (typed.trim() === "") return [];
    return files
      .filter((f) => !f.path_b64)
      .flatMap((f) => {
        const spans = matchPath(typed, f.path);
        return spans ? [{ f, spans, width: spans.at(-1)![1] - spans[0]![0] }] : [];
      })
      .sort((a, b) => a.width - b.width || a.f.path.length - b.f.path.length);
  }, [files, typed]);
  const options: ComboboxOption[] = ranked.slice(0, FIND_LIMIT).map(({ f, spans }) => ({
    value: f.path,
    label: (
      <span className="code-find__row">
        <Matched path={f.path} spans={spans} />
      </span>
    ),
  }));
  if (ranked.length > FIND_LIMIT) {
    options.push({
      value: "",
      disabled: true,
      label: `${fmtCount(ranked.length - FIND_LIMIT)} more match: keep typing`,
    });
  }
  const cap = capsOf(keyRow("code.find").presses[0]!);
  return (
    <Combobox
      ref={field}
      label="Files"
      aria-label="Go to file"
      placeholder="Go to file"
      inputSize="sm"
      containerClassName="code-find code-files-bar__find"
      leading={<SearchGlyph size="sm" />}
      trailing={typed === "" ? <Kbd subtle keys={cap} /> : undefined}
      mono
      value={typed}
      onValueChange={(next) => {
        setTyped(next);
        setOpen(next !== "");
      }}
      open={open && typed.trim() !== ""}
      onOpenChange={setOpen}
      options={options}
      emptyMessage={`No changed file's path holds “${typed}”`}
      onCommit={(option) => {
        const f = files.find((one) => one.path === option.value);
        if (!f) return;
        setTyped("");
        setOpen(false);
        onGo(f);
      }}
    />
  );
}

/** Filter options: the diff's own extensions and With findings, each a checkbox. */
function FilterOptions({
  exts,
  shown,
  onExts,
  only,
  onOnly,
  withFindings,
  findingsHeld,
  narrowing,
  onClear,
}: {
  exts: ReadonlyMap<string, number>;
  /** The extensions shown; none is every one. */
  shown: readonly string[];
  onExts: (next: string[]) => void;
  only: readonly string[];
  onOnly: (next: string[]) => void;
  withFindings: number;
  /** Why With findings is held, where finding_paths was refused. */
  findingsHeld: string | undefined;
  narrowing: boolean;
  onClear: () => void;
}) {
  const all = [...exts.keys()].sort();
  const isShown = (ext: string) => shown.length === 0 || shown.includes(ext);
  // THE LAST BOX CHECKED STAYS: no extension at all would show no file.
  const lastShown = all.filter(isShown).length === 1;
  const toggleOnly = (key: string, on: boolean) =>
    onOnly(on ? [...only.filter((k) => k !== key), key] : only.filter((k) => k !== key));
  return (
    <Popover
      label="Filter options"
      align="end"
      trigger={(open, toggle) => (
        <span className="code-files__options">
          <IconButton
            label="Filter options"
            icon={<Settings2Glyph />}
            size="sm"
            variant="ghost"
            aria-expanded={open}
            onClick={toggle}
          />
          {narrowing && <span className="code-files__dot" aria-hidden="true" />}
        </span>
      )}
    >
      <div className="code-files__popover">
        <fieldset className="code-files__set">
          <legend>File extensions</legend>
          {all.map((ext) => (
            <Checkbox
              key={ext || "none"}
              size="sm"
              label={`${ext ? `.${ext}` : "No extension"} ${fmtCount(exts.get(ext) ?? 0)}`}
              checked={isShown(ext)}
              disabled={lastShown && isShown(ext)}
              onCheckedChange={(on) => onExts(all.filter((e) => (e === ext ? on : isShown(e))))}
            />
          ))}
        </fieldset>
        <fieldset className="code-files__set">
          <legend>Show only</legend>
          <Checkbox
            size="sm"
            label={`With findings ${fmtCount(withFindings)}`}
            description={findingsHeld}
            disabled={findingsHeld !== undefined}
            checked={only.includes("findings") && findingsHeld === undefined}
            onCheckedChange={(on) => toggleOnly("findings", on)}
          />
        </fieldset>
        <Button size="small" variant="ghost" onClick={onClear} disabled={!narrowing}>
          Clear filters
        </Button>
      </div>
    </Popover>
  );
}
