/**
 * A project's files — the reports, specs and notes its work produced.
 *
 * # A listing, a download, an upload and a removal
 *
 * A file is written by a seat's `write_project_file`, by a person's own
 * assistant through the same tool, or by a person here: Upload puts each file
 * picked at the project's top level under its own name, a row's Replace puts a
 * new copy over that file, and Remove takes it out of the project. Each is the
 * engine's byte route (`PUT` and `DELETE /worklet/files/{project}/{path}`),
 * keyed so a retry after a lost answer is the same write ([keyedWrite]), and
 * attributed to whoever pressed it, as every write to a project's files is.
 *
 * WHO MAY IS THE ENGINE'S: an upload is anybody who may file work in the
 * project, a removal is the rule the project's chart gives, and a refusal is
 * said under the row (or the upload) that was pressed, in its words. The
 * controls are held only on what the reader can know before pressing: the
 * Worklet access every write here needs ([useWriteAccess] of filing a task,
 * which an upload is decided as), and the engine's size cap, said before a
 * gibibyte is streamed for a refusal at the end of it.
 *
 * REPLACE AND REMOVE NAME THE VERSION ON SCREEN (`If-Match`): a file somebody
 * else rewrote since the listing was read is refused as moved on, rather than
 * overwritten or removed unseen. An upload names none, so a name the project
 * already holds is refused rather than replaced: replacing is the row's.
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
  ConfirmModal,
  EmptyState,
  EmptyValue,
  FilePickButton,
  TableDateCell,
  TableTextCell,
  useToast,
  type PickedFile,
} from "@crewlethq/ui";
import { FileTextGlyph, UploadGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtBytes } from "~/lib/format.ts";
import { refusalText } from "~/lib/refusal.ts";
import { needsWords } from "~/lib/access.ts";
import { remedyFor } from "~/lib/authorityRemedy.ts";
import { pathSegments } from "~/lib/segment.ts";
import { pathSegment, rest, RestError } from "~/protocol/rest.ts";
import { newActOpID, type WorkFile } from "~/protocol/index.ts";
import { session } from "~/protocol/session.ts";
import { PROJECT_FILE_MAX_BYTES } from "~/contract/work.ts";
import { keyedWrite, type KeyedAnswer, type KeyedRequest } from "~/lib/keyedWrite.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";

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

/** The engine's route for one file of a project. */
function fileRoute(project: string, path: string): string {
  return `/worklet/files/${pathSegment(project)}/${pathSegments(path)}`;
}

/**
 * The request that puts `file` at `path` in `project`: its bytes under its own
 * type, and the version it replaces where it replaces one (`If-Match`). No
 * version is a new file, which the engine refuses over one already there.
 */
export function uploadRequest(
  project: string,
  path: string,
  file: Blob,
  replaces?: number,
): Omit<KeyedRequest, "key"> {
  return {
    method: "PUT",
    path: fileRoute(project, path),
    body: file,
    contentType: file.type || "application/octet-stream",
    ...(replaces ? { headers: { "If-Match": `"${replaces}"` } } : {}),
  };
}

/** The request that removes the file on screen, at the version on screen. */
export function removeRequest(file: WorkFile): Omit<KeyedRequest, "key"> {
  return {
    method: "DELETE",
    path: fileRoute(file.project, file.path),
    headers: { "If-Match": `"${file.version}"` },
  };
}

/** Why a picked file is not sent at all: the engine's cap, said before the bytes go. */
export function tooLarge(file: { name: string; size: number }): string | null {
  return file.size > PROJECT_FILE_MAX_BYTES
    ? `${file.name} is ${fmtBytes(file.size)}, and a project keeps files of at most ${fmtBytes(PROJECT_FILE_MAX_BYTES)}.`
    : null;
}

/**
 * A keyed byte write that landed: the listing and the project's feed are asked
 * again. Where it did not, what the engine said (or that nobody knows).
 */
function landed(answer: KeyedAnswer): answer is Extract<KeyedAnswer, { kind: "done" }> {
  if (answer.kind !== "done") return false;
  const position = typeof answer.body.position === "string" ? answer.body.position : null;
  session.written("tracker", position, ["work_files"]);
  return true;
}

/** One write, sent once under its own key: a retry is the person's, from the same control. */
async function send(req: Omit<KeyedRequest, "key">): Promise<KeyedAnswer> {
  return keyedWrite({ ...req, key: newActOpID() });
}

export function ProjectFiles({ project }: { project: string }) {
  const [after, setAfter] = useState<string | undefined>(undefined);
  const state = useQuery("work_files", after ? { project, after } : { project }, {
    pollMs: 60_000,
  });
  const listing = state.data;
  const files = listing?.files ?? [];
  const access = useWriteAccess("create_work_item");
  const toast = useToast();
  const [uploading, setUploading] = useState(false);
  const [notes, setNotes] = useState<string[]>([]);

  // EACH FILE IS ITS OWN UPLOAD AND ITS OWN ANSWER, so one the engine refuses
  // (a name already kept, a type or a size it will not take) is named in its
  // words while the rest land.
  const upload = async (picked: PickedFile[]) => {
    setUploading(true);
    setNotes([]);
    const said: string[] = [];
    let added = 0;
    try {
      for (const one of picked) {
        const held = tooLarge(one.file);
        if (held) {
          said.push(held);
          continue;
        }
        const answer = await send(uploadRequest(project, one.path, one.file));
        if (landed(answer)) added++;
        else said.push(`${one.path}: ${answer.text}`);
      }
    } finally {
      setUploading(false);
    }
    setNotes(said);
    if (added > 0) {
      toast.show({
        variant: "success",
        title: added === 1 ? `Uploaded ${picked[0]!.path}` : `Uploaded ${added} files`,
      });
    }
  };

  return (
    <Card padding="none">
      <Card.Header
        icon={<FileTextGlyph size="sm" />}
        actions={
          <FilePickButton
            size="small"
            variant="secondary"
            multiple
            leadingIcon={<UploadGlyph size="sm" />}
            loading={uploading}
            disabled={!access.can || uploading}
            disabledReason={access.can ? undefined : access.reason}
            onFiles={(picked) => void upload(picked)}
          >
            Upload
          </FilePickButton>
        }
      >
        <Card.Title>Files</Card.Title>
      </Card.Header>
      {notes.length > 0 && (
        <Callout variant="danger" role="alert" title="Not uploaded">
          {notes.map((note) => (
            <div key={note}>{note}</div>
          ))}
        </Callout>
      )}
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
            description="Upload a file, or ask an agent to save one here. Each is kept with who wrote it."
          />
        )}
        {listing && !listing.complete && (
          <Callout variant="warning" title="This listing may be missing files">
            It fills in once this node catches up.
          </Callout>
        )}
        {files.length > 0 && <FileGrid files={files} access={access} />}
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
function FileGrid({
  files,
  access,
}: {
  files: WorkFile[];
  access: { can: boolean; reason?: string };
}) {
  const now = useNow();
  const toast = useToast();
  const [busy, setBusy] = useState("");
  const [refusals, setRefusals] = useState<Record<string, string>>({});
  const [removing, setRemoving] = useState<WorkFile | null>(null);
  const [removal, setRemoval] = useState<string | null>(null);
  const said = (path: string, words: string | null) =>
    setRefusals(({ [path]: _gone, ...rest }) => (words ? { ...rest, [path]: words } : rest));

  const replace = async (file: WorkFile, picked: PickedFile[]) => {
    const one = picked[0];
    if (!one) return;
    const held = tooLarge(one.file);
    if (held) {
      said(file.path, `Not replaced: ${held}`);
      return;
    }
    setBusy(file.path);
    said(file.path, null);
    try {
      const answer = await send(uploadRequest(file.project, file.path, one.file, file.version));
      if (landed(answer)) toast.show({ variant: "success", title: `Replaced ${file.path}` });
      else said(file.path, `Not replaced: ${answer.text}`);
    } finally {
      setBusy("");
    }
  };

  const remove = async (file: WorkFile) => {
    setRemoval(null);
    const answer = await send(removeRequest(file));
    if (landed(answer)) {
      setRemoving(null);
      toast.show({ variant: "success", title: `Removed ${file.path}` });
    } else {
      // THE DIALOG STAYS OPEN ON A REFUSAL, saying it: closed, the reader
      // would read a removal that did not happen as one that did.
      setRemoval(answer.text);
    }
  };

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
      setRefusals((held) => ({ ...held, [file.path]: `Not downloaded: ${said}` }));
    } finally {
      setBusy("");
    }
  };

  return (
    <>
      <DataGrid<WorkFile>
        name="files"
        aria-label="Project files"
        rows={files}
        rowKey={(file) => file.path}
        phoneRows="compact"
        rowAccessory={(file) => refusals[file.path] ?? null}
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
          {
            key: "replace",
            header: "Replace",
            action: true,
            shrink: true,
            drop: 3,
            cell: (file) => (
              <FilePickButton
                size="small"
                variant="ghost"
                aria-label={`Replace ${file.path}`}
                disabled={!access.can || busy === file.path}
                disabledReason={access.can ? undefined : access.reason}
                onFiles={(picked) => void replace(file, picked)}
              >
                Replace
              </FilePickButton>
            ),
          },
          {
            key: "remove",
            header: "Remove",
            action: true,
            shrink: true,
            cell: (file) => (
              <Button
                size="small"
                variant="ghost"
                aria-label={`Remove ${file.path}`}
                disabled={!access.can || busy === file.path}
                disabledReason={access.can ? undefined : access.reason}
                onClick={() => {
                  setRemoval(null);
                  setRemoving(file);
                }}
              >
                Remove
              </Button>
            ),
          },
        ]}
      />
      {removing && (
        <ConfirmModal
          open
          onClose={() => setRemoving(null)}
          title={`Remove ${removing.path}?`}
          message="It leaves the project's files. Its history keeps who wrote and removed it."
          confirmLabel="Remove file"
          destructive
          onConfirm={() => remove(removing)}
        >
          {removal && (
            <Callout variant="danger" role="alert">
              {removal}
            </Callout>
          )}
        </ConfirmModal>
      )}
    </>
  );
}
