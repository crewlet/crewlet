/**
 * What changed from one commit (or tree) to another: the files, a page at a
 * time, and one file's patch drawn by the dashboard's one hueless `DiffView`.
 *
 * READ BY OBJECT ID, WHOLE ON BOTH SIDES: a commit's page and a comparison
 * each name the two commits by their whole ids (`code_diff` `base=`, `head=`),
 * so every answer here is immutable and the tab keeps it (`lib/useCodeQuery.ts`,
 * its most recently read 64) — a file read again, a reviewer moving from one
 * file to another and back, is answered with no request while the tab keeps
 * it. `file=` is the file whose patch is drawn, `line=` the line of its newer
 * side the patch marks and opens at (a check's failure points there), and
 * `layout=` how, each a filter of the page it is on.
 */

import { useCallback, useEffect, useMemo } from "react";
import { Button, Select, Skeleton, Stack } from "@crewlethq/ui";
import { useNavigator, useParam } from "~/app/router.tsx";
import { DiffView, type DiffHunk, type DiffLine } from "~/components/DiffView.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { fmtCount, plural } from "~/lib/format.ts";
import { usePatchPages } from "~/lib/patchPages.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type { CodeDiffFile, CodeGitAnswer } from "~/protocol/index.ts";
import { PatchPager } from "../PatchPager.tsx";
import { repoHref } from "../paths.ts";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";

/** A `line=` as the address spells it: a line number, or 0 for none. */
function lineOf(param: string): number {
  return /^[1-9][0-9]*$/.test(param) ? Number(param) : 0;
}

/**
 * Where the hunks read so far stand to a line of the newer side: whether one
 * draws it, and the furthest newer line any of them reaches (0 for none).
 * Git writes a file's hunks in the newer side's order, each over a run of its
 * lines, so a line the hunks reach past without drawing lies between two of
 * them: unchanged, outside what the diff changes.
 */
function placeOf(hunks: readonly DiffHunk[], line: number): { drawn: boolean; reached: number } {
  let drawn = false;
  let reached = 0;
  for (const hunk of hunks) {
    for (const l of hunk.lines) {
      if (l.after === null) continue;
      if (l.after === line) drawn = true;
      if (l.after > reached) reached = l.after;
    }
  }
  return { drawn: line > 0 && drawn, reached };
}

/** What git's status letter says of a file. */
function statusWord(f: CodeDiffFile): string {
  switch (f.status) {
    case "A":
      return "added";
    case "D":
      return "deleted";
    case "R":
      return f.old_path ? `renamed from ${f.old_path}` : "renamed";
    case "T":
      return "its type changed";
  }
  return "modified";
}

export function DiffFiles({ repo, base, head }: { repo: string; base: string; head: string }) {
  const nav = useNavigator();
  const [file] = useParam("file", "", "filter");
  const [layout, setLayout] = useParam("layout", "unified", "filter");
  const [lineParam] = useParam("line", "", "filter");
  const params = useMemo(() => ({ name: repo, base, head }), [repo, base, head]);
  const first = useCodeQuery("code_diff", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_diff", { ...params, cursor }),
    (a: CodeGitAnswer): readonly CodeDiffFile[] => a.diff?.files ?? [],
    (f: CodeDiffFile) => f.path_b64 ?? f.path,
    (a: CodeGitAnswer) => a.next ?? "",
  );
  const files = paged.items;
  const totals = first.data?.diff;
  // THE FIRST FILE, where the address names none: a diff is read for its
  // changes, and a list of names alone is a click away from any of them.
  const shown = file || files[0]?.path || "";
  return (
    <Stack gap={3}>
      <PageToolbar>
        {totals && (
          <span className="t-caption">
            {`${plural(totals.total, "file")} changed, +${fmtCount(totals.added)} −${fmtCount(totals.deleted)}`}
          </span>
        )}
        <Select
          size="sm"
          width="auto"
          value={layout}
          onChange={(value) => setLayout(String(value))}
          options={[
            { value: "unified", label: "Unified" },
            { value: "split", label: "Side by side" },
          ]}
          ariaLabel="Layout"
        />
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
          first.data && files.length === 0 && !paged.more
            ? { title: "No file changed", hint: "The two revisions hold the same files." }
            : undefined
        }
      >
        <div className="code-files">
          <Stack gap={2}>
            <ul className="code-files__list" aria-label="Files changed">
              {files.map((f) => (
                <li key={f.path_b64 ?? f.path}>
                  <button
                    type="button"
                    className="code-files__file"
                    aria-current={f.path === shown || undefined}
                    // A PATH GIT CANNOT SPELL AS TEXT names no file an
                    // address can carry: listed, and not opened.
                    disabled={Boolean(f.path_b64)}
                    title={statusWord(f)}
                    // A LINE IS THE FILE'S IT WAS NAMED IN: opening another
                    // file drops it, rather than marking that file's line
                    // of the same number.
                    onClick={() => nav.filter({ file: f.path, line: null })}
                  >
                    <span className="mono">{f.path}</span>{" "}
                    <span className="muted">
                      {f.status} +{f.added} −{f.deleted}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
            {paged.more && (
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
            )}
            {paged.pageError && <PageFailed error={paged.pageError} />}
          </Stack>
          {shown && (
            // THE FILE THE ADDRESS NAMES, WHEREVER IT IS LISTED: a diff's
            // files are a page at a time, so a link or a reload naming a file
            // past the pages loaded reads its patch all the same — the patch's
            // own answer describes the file.
            <FilePatch
              repo={repo}
              base={base}
              head={head}
              path={shown}
              listed={files.find((f) => f.path === shown)}
              layout={layout === "split" ? "split" : "unified"}
              // A LINE IS THE FILE'S THE ADDRESS NAMES, never the first file
              // drawn where it names none.
              line={file !== "" && shown === file ? lineOf(lineParam) : 0}
            />
          )}
        </div>
      </QueryState>
    </Stack>
  );
}

/**
 * One file's patch, as git wrote it, in the one `DiffView`: every page of it
 * the reader has asked for (`lib/patchPages.ts`).
 *
 * THE FILE IS THE PATCH'S OWN ANSWER'S where the listing read so far holds
 * none of it: the engine answers a file's patch with the file — its status,
 * its modes and whether git calls it binary — so a file the address names
 * past the pages listed is described by the read that draws it. One the
 * listing holds is known binary before anything is asked, and nothing is.
 *
 * A LINE THE ADDRESS NAMES (`line`, of the newer side) is marked where the
 * patch draws it and brought into view once, as it is first drawn, by the
 * diff itself, which knows which of its blocks holds it however long the
 * patch is (`DiffView`'s `reveal`). The patch is read on to the page that
 * holds it, a page at a time with no press, and no further. One the patch
 * does not draw is outside what the diff changes — known once a line past it
 * is read, or the whole patch was read short of it: said, with the file
 * itself at `head` offered on that line, rather than a patch drawn as if it
 * held the line.
 */
function FilePatch({
  repo,
  base,
  head,
  path,
  listed,
  layout,
  line,
}: {
  repo: string;
  base: string;
  head: string;
  path: string;
  listed: CodeDiffFile | undefined;
  layout: "unified" | "split";
  line: number;
}) {
  const params = useMemo(() => ({ name: repo, base, head, file: path }), [repo, base, head, path]);
  const read = useCodeQuery("code_diff", params, { enabled: !listed?.binary });
  const { paged, hunks, file } = usePatchPages(read, params, (cursor) =>
    codeRest.query("code_diff", { ...params, cursor }),
  );
  const entry = listed ?? file;
  // ONE PREDICATE FOR THE LINE, kept across renders: the diff marks what it
  // accepts and reveals it once for it, wherever its window is (`DiffView`).
  const at = useCallback((l: DiffLine) => l.after === line, [line]);
  const { drawn, reached } = useMemo(() => placeOf(hunks, line), [hunks, line]);
  // THE PATCH IS FOLLOWED TO THE LINE: its hunks are in the newer side's
  // order, so while every line read ends before it and git wrote more, the
  // page holding it is still to come, and it is asked for without a press —
  // only up to that page, never the rest of the file. A page that failed
  // stops it, its pager offering the press that asks again.
  const short = line > 0 && !drawn && reached < line;
  const follow = short && paged.more && !paged.paging && !paged.pageError;
  const { next } = paged;
  useEffect(() => {
    if (follow) next();
  }, [follow, next]);
  // OUTSIDE AS SOON AS THE ORDER SAYS SO: a line read past it, or the whole
  // patch read short of it, never the reader paging to the end by hand.
  const outside =
    line > 0 &&
    !drawn &&
    !!entry &&
    !entry.binary &&
    !!read.data &&
    !read.loading &&
    (!short || !paged.more);
  return (
    <section aria-label={`Changes to ${path}`} className="code-file">
      {entry?.binary ? (
        <p className="muted">A binary file: git draws no lines of it.</p>
      ) : read.loading && !read.data ? (
        <Skeleton variant="text" rows={8} label="Loading the patch" />
      ) : read.error ? (
        <QueryState error={read.error} refusal={read.refusal} detail={read.detail} loading={false}>
          {null}
        </QueryState>
      ) : !entry ? (
        // A FILE THE DIFF DOES NOT CHANGE has no patch, and the engine
        // answers its read with no file: said, rather than drawn as a file
        // with no line changed.
        <p className="muted">{`The two revisions hold the same ${path}: this diff does not change it.`}</p>
      ) : hunks.length === 0 ? (
        <p className="muted">
          {entry.old_mode !== entry.new_mode
            ? `Its mode changed, from ${entry.old_mode ?? "none"} to ${entry.new_mode ?? "none"}: no line did.`
            : "No line of it changed."}
        </p>
      ) : (
        <Stack gap={2}>
          <DiffView
            // A NEW FILE IS A NEW DIFF: its window, and the line it reveals
            // once, start over.
            key={path}
            hunks={hunks}
            layout={layout}
            aria-label={`Changes to ${path}`}
            picked={line > 0 ? at : undefined}
            reveal={line > 0 ? at : undefined}
          />
          <PatchPager paged={paged} />
        </Stack>
      )}
      {outside && (
        <p className="muted">
          {`Line ${line} is outside what this diff changes. `}
          <a
            className="t-link"
            href={repoHref(repo, { tab: "files", ref: head, path, lines: String(line) })}
          >
            {`Open ${path} at line ${line}`}
          </a>
        </p>
      )}
    </section>
  );
}
