/**
 * A project's files — the reports, specs and notes its work produced.
 *
 * # A listing and a download, and nothing that writes
 *
 * Like every other work screen, this one reads: a file is written by a seat's
 * `write_project_file`, by a person's own assistant through the same tool, or
 * over `PUT /worklet/files/{project}/{path}` — each attributed to whoever wrote
 * it. What the screen adds is the one thing none of those is good at: seeing
 * what is there and taking a copy.
 *
 * # A download is fetched, not linked
 *
 * A plain link would carry the session cookie too, but its answer would be the
 * browser's to handle: a refusal saved to disk under the file's own name, and
 * a lost session downloading a 401 rather than sending the reader to sign in.
 * The bytes are fetched over the session ([rest.blob]) and handed to the
 * browser as a blob, so a refusal is the engine's, read and said on the row.
 *
 * # Paged by path, as the engine pages it
 *
 * The listing is keyset-paged on the path, and a page says where the next one
 * starts. "Next page" is that cursor, and "First page" drops it: nothing is
 * accumulated, so a long project never becomes one very long list.
 */

import { useState } from "react";
import { QueryState } from "~/components/common.tsx";
import {
  Button,
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  TableDateCell,
  TableTextCell,
} from "@crewlethq/ui";
import { FileTextGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtBytes } from "~/lib/format.ts";
import { refusalText } from "~/lib/refusal.ts";
import { needsWords } from "~/lib/access.ts";
import { remedyFor } from "~/lib/authorityRemedy.ts";
import { pathSegments } from "~/lib/segment.ts";
import { pathSegment, rest, RestError } from "~/protocol/rest.ts";
import type { WorkFile } from "~/protocol/index.ts";

/** The name a download is saved under: the path's last segment. */
export function fileName(path: string): string {
  return path.slice(path.lastIndexOf("/") + 1) || path;
}

/**
 * What a stored file is, in a word a reader uses. The stored type stays on the
 * cell's title: `text/plain; charset=utf-8` is the record, not a description.
 */
export function fileKind(contentType: string | undefined): string {
  const type = (contentType ?? "").split(";")[0]!.trim().toLowerCase();
  if (!type) return "";
  const named = FILE_KINDS[type];
  if (named) return named;
  const [family, sub = ""] = type.split("/");
  if (family === "image") return "Image";
  if (family === "audio") return "Audio";
  if (family === "video") return "Video";
  if (family === "text") return "Text";
  return sub.replace(/^(x-|vnd\.)/, "").toUpperCase() || type;
}

const FILE_KINDS: Record<string, string> = {
  "text/plain": "Text",
  "text/markdown": "Markdown",
  "text/csv": "CSV",
  "text/html": "HTML",
  "application/json": "JSON",
  "application/pdf": "PDF",
  "application/zip": "Zip",
  "application/octet-stream": "Binary",
};

export function ProjectFiles({ project }: { project: string }) {
  const [after, setAfter] = useState<string | undefined>(undefined);
  const state = useQuery("work_files", after ? { project, after } : { project }, {
    pollMs: 60_000,
  });
  const listing = state.data;
  const files = listing?.files ?? [];

  return (
    <Card padding="none">
      <Card.Header icon={<FileTextGlyph size="sm" />}>
        <Card.Title>Files</Card.Title>
      </Card.Header>
      <QueryState
        error={state.error}
        refusal={state.refusal}
        detail={state.detail}
        loading={state.loading && !listing}
      >
        {listing && files.length === 0 && !after && (
          <EmptyState
            size="compact"
            icon={<FileTextGlyph size="xl" />}
            title="No files are kept in this project yet"
            description="Files agents save for this project appear here."
          />
        )}
        {listing && !listing.complete && (
          <Callout variant="warning" title="This listing may be missing files">
            It fills in once this node catches up.
          </Callout>
        )}
        {files.length > 0 && <FileGrid files={files} />}
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

/**
 * The listing as the kit's grid. PLAIN HEADS: the engine walks one index, by path,
 * so a press could only order the page. Kind and who wrote it give way first; a
 * refused download is said under the row it was pressed on.
 */
function FileGrid({ files }: { files: WorkFile[] }) {
  const now = useNow();
  const [busy, setBusy] = useState("");
  const [refusals, setRefusals] = useState<Record<string, string>>({});

  const download = async (file: WorkFile) => {
    setBusy(file.path);
    setRefusals(({ [file.path]: _gone, ...rest }) => rest);
    try {
      const bytes = await rest.blob(
        `/worklet/files/${pathSegment(file.project)}/${pathSegments(file.path)}`,
      );
      const url = URL.createObjectURL(bytes);
      const link = document.createElement("a");
      link.href = url;
      link.download = fileName(file.path);
      link.click();
      URL.revokeObjectURL(url);
    } catch (err) {
      // A REFUSAL SAYS WHY, and a download that never finished says THAT:
      // either way nothing was saved, and the row says so where the button
      // was pressed rather than failing silently. A refusal on AUTHORITY names
      // the access the engine said would admit the reader (Worklet, the
      // listing's own) where the rule makes that access the way past it
      // ([remedyFor]), and anything else is the engine's own words.
      const said =
        err instanceof RestError && remedyFor(err.reason, err.grants).namesAccess
          ? needsWords("Downloading a project's file", err.grants)
          : refusalText(err);
      setRefusals((held) => ({ ...held, [file.path]: said }));
    } finally {
      setBusy("");
    }
  };

  return (
    <DataGrid<WorkFile>
      name="files"
      aria-label="Project files"
      rows={files}
      rowKey={(file) => file.path}
      phoneRows="compact"
      rowAccessory={(file) =>
        refusals[file.path] ? `Not downloaded: ${refusals[file.path]}` : null
      }
      columns={[
        {
          key: "path",
          header: "Path",
          floor: "14rem",
          overflow: "truncate",
          phoneLead: true,
          cell: (file) => <span className="mono truncate">{file.path}</span>,
        },
        {
          key: "kind",
          header: "Kind",
          shrink: true,
          drop: 2,
          title: (file) => file.content_type,
          cell: (file) => fileKind(file.content_type) || <EmptyValue label="No type recorded" />,
        },
        {
          key: "size",
          header: "Size",
          shrink: true,
          align: "right",
          cell: (file) => <span className="t-num">{fmtBytes(file.size)}</span>,
        },
        {
          key: "by",
          header: "By",
          shrink: true,
          drop: 1,
          cell: (file) =>
            file.updated_by ? (
              <TableTextCell>{file.updated_by}</TableTextCell>
            ) : (
              <EmptyValue label="Nobody recorded" />
            ),
        },
        {
          key: "changed",
          header: "Changed",
          shrink: true,
          align: "right",
          cell: (file) => <TableDateCell value={file.updated_at} now={now} />,
        },
        {
          key: "download",
          header: "Download",
          action: true,
          shrink: true,
          cell: (file) => (
            <Button
              size="small"
              variant="secondary"
              disabled={busy === file.path}
              aria-label={`Download ${file.path}`}
              onClick={() => void download(file)}
            >
              {busy === file.path ? "Downloading…" : "Download"}
            </Button>
          ),
        },
      ]}
    />
  );
}
