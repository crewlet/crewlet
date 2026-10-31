/**
 * What changed from one commit (or tree) to another: the files, a page at a
 * time, and one file's patch drawn by the dashboard's one hueless `DiffView`.
 *
 * READ BY OBJECT ID, WHOLE ON BOTH SIDES: a commit's page and a comparison
 * each name the two commits by their whole ids (`code_diff` `base=`, `head=`),
 * so every answer here is immutable and the hook keeps its tag — a file read
 * again is not sent again. `file=` is the file whose patch is drawn and
 * `layout=` how, each a filter of the page it is on.
 */

import { useMemo } from "react";
import { Button, Callout, Select, Skeleton, Stack } from "@crewlethq/ui";
import { useParam } from "~/app/router.tsx";
import { DiffView } from "~/components/DiffView.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { fmtCount, plural } from "~/lib/format.ts";
import { parsePatch } from "~/lib/patch.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import type { CodeDiffFile, CodeGitAnswer } from "~/protocol/index.ts";

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
  const [file, setFile] = useParam("file", "", "filter");
  const [layout, setLayout] = useParam("layout", "unified", "filter");
  const params = useMemo(() => ({ name: repo, base, head }), [repo, base, head]);
  const hold = usePageHold<CodeGitAnswer>(params);
  const first = useCodeQuery("code_diff", params, { hold: hold.holding });
  const paged = usePaged(
    hold,
    first,
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
  const entry = files.find((f) => f.path === shown);
  return (
    <Stack gap={3}>
      <div className="toolbar">
        {totals && (
          <span className="t-caption">
            {`${plural(totals.total, "file")} changed, +${fmtCount(totals.added)} −${fmtCount(totals.deleted)}`}
          </span>
        )}
        <Select
          width="auto"
          value={layout}
          onChange={(value) => setLayout(String(value))}
          options={[
            { value: "unified", label: "Unified" },
            { value: "split", label: "Side by side" },
          ]}
          ariaLabel="Layout"
        />
      </div>
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
                    onClick={() => setFile(f.path)}
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
                  onClick={paged.older}
                  disabled={paged.paging}
                  loading={paged.paging}
                >
                  Load more files
                </Button>
              </span>
            )}
            {paged.pageError && <PageFailed error={paged.pageError} />}
          </Stack>
          {entry ? (
            <FilePatch
              repo={repo}
              base={base}
              head={head}
              entry={entry}
              layout={layout === "split" ? "split" : "unified"}
            />
          ) : (
            files.length > 0 && <p className="muted">Choose a file to read its changes.</p>
          )}
        </div>
      </QueryState>
    </Stack>
  );
}

/** One file's patch, as git wrote it, in the one `DiffView`. */
function FilePatch({
  repo,
  base,
  head,
  entry,
  layout,
}: {
  repo: string;
  base: string;
  head: string;
  entry: CodeDiffFile;
  layout: "unified" | "split";
}) {
  const read = useCodeQuery(
    "code_diff",
    { name: repo, base, head, file: entry.path },
    { enabled: !entry.binary },
  );
  const text = read.data?.diff?.patch ?? "";
  const hunks = useMemo(() => parsePatch(text), [text]);
  return (
    <section aria-label={`Changes to ${entry.path}`} className="code-file">
      {entry.binary ? (
        <p className="muted">A binary file: git draws no lines of it.</p>
      ) : read.loading && !read.data ? (
        <Skeleton variant="text" rows={8} label="Loading the patch" />
      ) : read.error ? (
        <QueryState error={read.error} refusal={read.refusal} detail={read.detail} loading={false}>
          {null}
        </QueryState>
      ) : hunks.length === 0 ? (
        <p className="muted">
          {entry.old_mode !== entry.new_mode
            ? `Its mode changed, from ${entry.old_mode ?? "none"} to ${entry.new_mode ?? "none"}: no line did.`
            : "No line of it changed."}
        </p>
      ) : (
        <>
          <DiffView hunks={hunks} layout={layout} aria-label={`Changes to ${entry.path}`} />
          {read.data?.next && (
            <Callout variant="neutral">
              This file's patch runs past one page: its end is not drawn.
            </Callout>
          )}
        </>
      )}
    </section>
  );
}
