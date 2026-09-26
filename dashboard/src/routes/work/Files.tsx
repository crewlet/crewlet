/**
 * A project's files — the reports, specs and notes its work produced.
 *
 * # A listing and a download, and nothing that writes
 *
 * Like every other work screen, this one reads: a file is written by a seat's
 * `write_project_file`, by a person's own assistant through the same tool, or
 * over `PUT /work/files/{project}/{path}` — each attributed to whoever wrote
 * it. What the screen adds is the one thing none of those is good at: seeing
 * what is there and taking a copy.
 *
 * # A download is fetched, not linked
 *
 * A plain link carries no bearer token, so on an engine that guards its reads
 * it would save the refusal instead of the file. The bytes are fetched with the
 * operator's token ([rest.blob]) and handed to the browser as a blob.
 *
 * # Paged by path, as the engine pages it
 *
 * The listing is keyset-paged on the path, and a page says where the next one
 * starts. "Next page" is that cursor, and "First page" drops it: nothing is
 * accumulated, so a long project never becomes one very long list.
 */

import { useState } from "react";
import { QueryState } from "~/components/common.tsx";
import { Button, Callout, Card, EmptyState } from "@crewlethq/ui";
import { DescriptionGlyph } from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtBytes, fmtDateTime, relTime } from "~/lib/format.ts";
import { rest, RestError } from "~/protocol/rest.ts";
import type { WorkFile } from "~/protocol/index.ts";

/** The route a file's bytes are served from, each segment escaped. */
export function fileURL(project: string, path: string): string {
  return (
    `/work/files/${encodeURIComponent(project)}/` +
    path.split("/").map(encodeURIComponent).join("/")
  );
}

/** The name a download is saved under: the path's last segment. */
export function fileName(path: string): string {
  return path.slice(path.lastIndexOf("/") + 1) || path;
}

export function ProjectFiles({ project }: { project: string }) {
  const [after, setAfter] = useState<string | undefined>(undefined);
  const state = useQuery("work_files", after ? { project, after } : { project }, {
    pollMs: 60_000,
  });
  const listing = state.data;
  const files = listing?.files ?? [];

  return (
    <Card>
      <Card.Header icon={<DescriptionGlyph size="sm" />}>
        <Card.Title>Files</Card.Title>
      </Card.Header>
      <QueryState error={state.error} loading={state.loading && !listing}>
        {listing && files.length === 0 && !after && (
          <EmptyState
            size="compact"
            icon={<DescriptionGlyph size="xl" />}
            title="No files are kept in this project yet"
            description="A seat writes one with write_project_file — a report, a plan, the notes it leaves for the next — and so can your own assistant at /operator/mcp, attributed to your token. PUT /work/files/{project}/{path} uploads one directly."
          />
        )}
        {listing && !listing.complete && (
          <Callout variant="warning" title="This listing may be missing files">
            This node holds a change it could not apply yet, and it may be to a file here. The
            listing fills in once it has.
          </Callout>
        )}
        {files.length > 0 && (
          <table className="table">
            <thead>
              <tr>
                <th>Path</th>
                <th>Size</th>
                <th>Changed</th>
                <th aria-label="Download" />
              </tr>
            </thead>
            <tbody>
              {files.map((file) => (
                <FileRow key={file.path} file={file} />
              ))}
            </tbody>
          </table>
        )}
        {(after || listing?.next) && (
          <footer className="panel-foot">
            {after && (
              <Button size="small" variant="secondary" onClick={() => setAfter(undefined)}>
                First page
              </Button>
            )}
            <span className="spacer" />
            {listing?.next && (
              <Button size="small" variant="secondary" onClick={() => setAfter(listing.next)}>
                Next page
              </Button>
            )}
          </footer>
        )}
      </QueryState>
    </Card>
  );
}

function FileRow({ file }: { file: WorkFile }) {
  const now = useNow();
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState("");

  const download = async () => {
    setBusy(true);
    setRefusal("");
    try {
      const bytes = await rest.blob(fileURL(file.project, file.path));
      const url = URL.createObjectURL(bytes);
      const link = document.createElement("a");
      link.href = url;
      link.download = fileName(file.path);
      link.click();
      URL.revokeObjectURL(url);
    } catch (err) {
      // A REFUSAL SAYS WHY, and a download that never finished says THAT:
      // either way nothing was saved, and the row says so where the button
      // was pressed rather than failing silently.
      setRefusal(err instanceof RestError || err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <tr>
      <td>
        <span className="mono">{file.path}</span>
        {file.content_type && <span className="t-caption muted"> · {file.content_type}</span>}
        {refusal && <div className="t-caption">Not downloaded: {refusal}</div>}
      </td>
      <td className="t-num">{fmtBytes(file.size)}</td>
      <td className="t-caption" title={fmtDateTime(file.updated_at)}>
        {relTime(file.updated_at, now)}
        {file.updated_by && <> by {file.updated_by}</>}
      </td>
      <td>
        <Button size="small" variant="secondary" disabled={busy} onClick={() => void download()}>
          {busy ? "Downloading…" : "Download"}
        </Button>
      </td>
    </tr>
  );
}
