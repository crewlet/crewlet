/**
 * A change's Files changed tab: every file a revision changes on one page, a
 * stack of sticky-headed diffs (`FileStack.tsx`) beside the files' tree in its
 * own scroller, with what lies on each line drawn under it.
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

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Button,
  Combobox,
  IconButton,
  Kbd,
  SegmentedControl,
  Select,
  Skeleton,
  Stack,
  type ComboboxOption,
} from "@crewlethq/ui";
import { Columns2Glyph, PanelLeftGlyph, Rows2Glyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import { capsOf, keyRow, useKeymap } from "~/app/keymap.ts";
import { useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { changeLive, refName, shortOID } from "~/lib/code.ts";
import { fmtCount } from "~/lib/format.ts";
import { useReviewDrafts } from "~/lib/reviewDrafts.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type {
  CodeChangeAnswer,
  CodeChangeDetail,
  CodeDiffFile,
  CodeRevisionRow,
} from "~/protocol/index.ts";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { FileTree } from "../FileTree.tsx";
import { FIND_LIMIT, Matched } from "../repo/GoToFile.tsx";
import { FileStack, fileKey, matchPath, type StackJump } from "./FileStack.tsx";
import { FinishReview } from "./ReviewDialog.tsx";
import {
  changePartParams,
  partMovedBy,
  revisionInputs,
  useAskAgainWhenMoved,
  usePagedComments,
  usePagedFindings,
} from "./reads.ts";
import "~/styles/gitlet.css";

/** A whole number from 1 off the address, or null. */
function wholeParam(value: string): number | null {
  if (!/^[1-9][0-9]{0,15}$/.test(value)) return null;
  const n = Number(value);
  return Number.isSafeInteger(n) ? n : null;
}

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
  const listed = pagedFiles.items;
  const indexOf = useMemo(() => {
    const at = new Map(listed.map((f, i) => [fileKey(f), i]));
    return (f: CodeDiffFile) => at.get(fileKey(f)) ?? -1;
  }, [listed]);
  const [treeShown, setTreeShown] = useState(true);
  const [current, setCurrent] = useState("");
  const [jump, setJump] = useState<StackJump | null>(null);
  const go = useCallback(
    (f: CodeDiffFile) => {
      if (!f.path_b64) setFile(f.path);
      setCurrent(f.path);
      setJump((was) => ({ key: fileKey(f), seq: (was?.seq ?? 0) + 1 }));
    },
    [setFile],
  );
  // THE FILE THE ADDRESS NAMES, once it is listed: the stack opens at it.
  const arrived = useRef(false);
  const { more, paging, pageError, next } = pagedFiles;
  useEffect(() => {
    if (arrived.current || !file) return;
    const f = listed.find((one) => one.path === file);
    // A FILE PAST THE PAGES LISTED is listed by the pages after them.
    if (!f) {
      if (more && !paging && !pageError) next();
      return;
    }
    arrived.current = true;
    setCurrent(f.path);
    setJump({ key: fileKey(f), seq: 1 });
  }, [file, listed, more, paging, pageError, next]);
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
    <Stack gap={3}>
      <PageToolbar className="code-files-bar">
        <IconButton
          label={treeShown ? "Hide the file tree" : "Show the file tree"}
          icon={<PanelLeftGlyph />}
          size="sm"
          variant="ghost"
          aria-pressed={treeShown}
          onClick={() => setTreeShown((was) => !was)}
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
              files={listed}
              current={current || file}
              onOpen={go}
              label={`Files changed in revision ${rev}`}
              foot={
                <ComparedWith change={change} rows={rows} rev={rev} since={since} target={target} />
              }
            >
              {loadMoreFiles && <span>{loadMoreFiles}</span>}
              {pagedFiles.pageError && <PageFailed error={pagedFiles.pageError} />}
            </FilesPane>
          )}
          <FileStack
            change={change}
            head={head}
            rev={rev}
            since={since}
            moved={diffMoved}
            files={listed}
            indexOf={indexOf}
            layout={layout === "split" ? "split" : "unified"}
            notes={{
              findings: findings.items,
              placed,
              comments: comments.items,
              drafts: drafting ? drafts : null,
              thread,
            }}
            jump={jump}
            onCurrent={setCurrent}
            foot={loadMoreFiles && <span>{loadMoreFiles}</span>}
          />
        </div>
      </QueryState>
    </Stack>
  );
}

/** The files' tree in its own scroller beside the stack, its current row kept in the list's view. */
function FilesPane({
  files,
  current,
  onOpen,
  label,
  head,
  foot,
  children,
}: {
  files: readonly CodeDiffFile[];
  current: string;
  onOpen: (f: CodeDiffFile) => void;
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
        <FileTree files={files} current={current} onOpen={onOpen} />
        {children}
      </div>
      <div className="code-tree__foot">{foot}</div>
    </aside>
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
