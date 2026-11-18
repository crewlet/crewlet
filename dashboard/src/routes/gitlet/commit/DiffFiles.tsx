/** What changed from one commit to another, as a change's Files changed draws it: the totals with
 *  Unified and Split, the tree with Filter files, and every file as one stack walked by
 *  `code_diff patches=true`. Read by whole ids; `file=`, `line=`, `q=` and `layout=` are filters. */

import { useCallback, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Button,
  IconButton,
  Input,
  SegmentedControl,
  Skeleton,
  Stack,
  VisuallyHidden,
} from "@crewlethq/ui";
import {
  Columns2Glyph,
  ListFilterGlyph,
  PanelLeftGlyph,
  Rows2Glyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator, useParam, useRoute } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { shortOID } from "~/lib/code.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type { CodeDiffFile, CodeGitAnswer } from "~/protocol/index.ts";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { FileStack, fileKey, type StackJump, type StackSource } from "../FileStack.tsx";
import { FilesPane, TREE_BESIDE_FROM, useFileArrival } from "../FilesPane.tsx";
import "~/styles/gitlet.css";

/** A `line=` as the address spells it: a line number, or 0 for none. */
function lineOf(param: string): number {
  return /^[1-9][0-9]*$/.test(param) ? Number(param) : 0;
}

// A PAGE OF THE DIFF'S FILES, as `usePaged` reads it: module constants, so the list is the
// same list until a page arrives.
const filesOf = (a: CodeGitAnswer): readonly CodeDiffFile[] => a.diff?.files ?? [];
const nextOf = (a: CodeGitAnswer): string => a.next ?? "";

/** A total in short figures: "12.3k additions". */
const counted = (n: number, one: string): string => `${fmtCount(n)} ${n === 1 ? one : `${one}s`}`;

/** What a host says of the diff: words after the toolbar's totals, and the pane's foot. */
export interface DiffAgainst {
  words?: ReactNode;
  foot: ReactNode;
}

export function DiffFiles({
  repo,
  base,
  head,
  against,
}: {
  repo: string;
  base: string;
  head: string;
  /** A commit's own words; a comparison's are its merge base's. */
  against?: DiffAgainst | undefined;
}) {
  const nav = useNavigator();
  const route = useRoute();
  const [file] = useParam("file", "", "filter");
  const [layout, setLayout] = useParam("layout", "unified", "filter");
  const [lineParam] = useParam("line", "", "filter");
  const [q, setQ] = useParam("q", "", "filter");
  const params = useMemo(() => ({ name: repo, base, head }), [repo, base, head]);
  const first = useCodeQuery("code_diff", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_diff", { ...params, cursor }),
    filesOf,
    fileKey,
    nextOf,
  );
  const listed = paged.items;
  const totals = first.data?.diff;
  const source = useMemo<StackSource>(
    () => ({ kind: "diff", repo, base, head }),
    [repo, base, head],
  );
  const words = q.toLowerCase().split(/\s+/).filter(Boolean);
  const filtering = words.length > 0;
  const visible = filtering
    ? listed.filter((f) => words.every((w) => f.path.toLowerCase().includes(w)))
    : listed;
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
  // A LINE IS THE FILE'S IT WAS NAMED IN: opening another file drops it.
  const go = useCallback(
    (f: CodeDiffFile) => {
      nav.filter({
        file: f.path_b64 ? null : f.path,
        line: null,
        ...(visible.includes(f) ? {} : { q: null }),
      });
      setCurrent(f.path);
      setJump((was) => ({ key: fileKey(f), seq: (was?.seq ?? 0) + 1 }));
    },
    [nav, visible],
  );
  const onArrive = useCallback((f: CodeDiffFile) => {
    setCurrent(f.path);
    setJump({ key: fileKey(f), seq: 1 });
  }, []);
  const { unchanged } = useFileArrival({
    source,
    file,
    listed,
    loaded: first.data !== undefined,
    more: paged.more,
    paging: paged.paging,
    pageError: paged.pageError,
    next: paged.next,
    onArrive,
  });
  const line = lineOf(lineParam);
  const target = useMemo(() => {
    const f = file && line > 0 ? listed.find((one) => one.path === file) : undefined;
    return f ? { key: fileKey(f), line } : undefined;
  }, [file, line, listed]);
  // A FILE'S LINK IS THIS PAGE'S OWN ADDRESS naming it, with no line.
  const { path, query } = route;
  const linkOf = useCallback(
    (f: CodeDiffFile) => {
      const at = Object.fromEntries(query);
      delete at.line;
      return href(path, { ...at, file: f.path });
    },
    [path, query],
  );
  const showing = filtering && (
    <p className="code-files__showing">
      {visible.length === 0
        ? "No file's path holds these words."
        : paged.more
          ? `Showing ${fmtCount(visible.length)} of the ${plural(listed.length, "file")} loaded`
          : `Showing ${fmtCount(visible.length)} of ${plural(listed.length, "file")}`}{" "}
      <Button size="small" variant="ghost" onClick={() => setQ("")}>
        Clear filter
      </Button>
    </p>
  );
  const loadMore = paged.more && (
    <span>
      <Button
        size="small"
        variant="secondary"
        onClick={paged.next}
        disabled={paged.paging}
        loading={paged.paging}
      >
        Load more files
      </Button>
    </span>
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
          {totals ? (
            <p className="code-files-bar__words code-files-bar__totals">
              Showing <strong>{counted(totals.total, "changed file")}</strong>
              {totals.added + totals.deleted === 0 ? (
                " with no line changed"
              ) : (
                <>
                  {" with "}
                  <strong className="code-diffstat__added">
                    {counted(totals.added, "addition")}
                  </strong>
                  {" and "}
                  <strong className="code-diffstat__removed">
                    {counted(totals.deleted, "deletion")}
                  </strong>
                </>
              )}
              {against?.words}
            </p>
          ) : (
            <VisuallyHidden>Reading the files</VisuallyHidden>
          )}
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
          </span>
        </PageToolbar>
        {first.loading && !first.data && (
          <Skeleton variant="text" rows={4} label="Loading the files" />
        )}
        <QueryState
          error={first.error}
          refusal={first.refusal}
          detail={first.detail}
          loading={first.loading && !first.data}
          empty={
            first.data && listed.length === 0 && !paged.more
              ? { title: "No file changed", hint: "The two revisions hold the same files." }
              : undefined
          }
        >
          <div className="code-files" data-tree={treeShown ? "shown" : "hidden"}>
            {treeShown && (
              <FilesPane
                files={visible}
                current={current || file}
                onOpen={go}
                label="Files changed"
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
                  </div>
                }
                foot={
                  <>
                    {against?.foot ?? (
                      <p className="code-tree__said">
                        Compared with the merge base at <code>{shortOID(base)}</code>
                      </p>
                    )}
                    {showing}
                  </>
                }
              >
                {loadMore}
                {paged.pageError && <PageFailed error={paged.pageError} />}
              </FilesPane>
            )}
            <FileStack
              source={source}
              repo={repo}
              head={head}
              moved=""
              files={visible}
              indexOf={indexOf}
              layout={layout === "split" ? "split" : "unified"}
              linkOf={linkOf}
              target={target}
              jump={jump}
              onCurrent={setCurrent}
              lead={
                <>
                  {unchanged && (
                    <p className="muted" role="status">
                      {`The two revisions hold the same ${file}: this diff does not change it.`}
                    </p>
                  )}
                  {showing}
                </>
              }
              foot={loadMore}
            />
          </div>
        </QueryState>
      </Stack>
    </div>
  );
}
