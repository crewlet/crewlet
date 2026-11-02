/**
 * Stored files as the drawer draws them: the files a deep run delivered, on
 * their card, and the images a question was sent with, under its bubble.
 *
 * EVERY BYTE IS READ THROUGH THE FILE'S OWN `download` ROUTE, the path the
 * engine built, checked to be one of the assistant's (`exportFile`), as the
 * person and under the same session: the download a reader presses and the
 * picture an image is previewed by are the same request.
 *
 * A PREVIEW IS ONLY EVER A PICTURE: an image's bytes, typed from the
 * reference, drawn through an object URL in an `<img>` (`filePreview.ts`), an
 * SVG included. Nothing a file carries is put into the page as markup, and a
 * file that is not an image shows its name, type and size alone.
 */

import {
  AttachmentChip,
  DownloadButton,
  FileList,
  StatusCard,
  type FileListItem,
} from "@crewlethq/ui";
import {
  ASSISTANT_DELIVERED_FILES_PER_RUN,
  type AssistantFileCard,
  type AssistantFileRef,
} from "~/contract/assistant.ts";
import { exportFile } from "~/protocol/assistant.ts";
import { fmtBytes, fmtExact } from "~/lib/format.ts";
import { isPreviewable, useFilePreviews, type PreviewSource } from "./filePreview.ts";

/** The words a file's type is said with, by its media type. */
const TYPE_WORDS: Readonly<Record<string, string>> = {
  "image/png": "PNG image",
  "image/jpeg": "JPEG image",
  "image/webp": "WebP image",
  "image/gif": "GIF image",
  "image/svg+xml": "SVG image",
  "text/csv": "CSV file",
  "text/markdown": "Markdown file",
  "text/plain": "Text file",
  "text/html": "HTML file",
  "application/json": "JSON file",
  "application/pdf": "PDF document",
  "application/zip": "ZIP archive",
};

/** A file's type in words: "PNG image", or the media type itself for one without words. */
export function fileTypeWords(mediaType: string): string {
  const essence = mediaType.split(";")[0]!.trim().toLowerCase();
  return TYPE_WORDS[essence] ?? (essence === "" ? "File" : essence);
}

/** Each previewable file's source: its bytes from its own download route. */
function previewSources(files: readonly AssistantFileRef[]): PreviewSource[] {
  return files
    .filter((file) => isPreviewable(file.media_type))
    .map((file) => ({
      key: file.file_id,
      mediaType: file.media_type,
      load: (signal) => exportFile(file.download, signal),
    }));
}

/** A count of files in words: "1 file", "3 files". */
function fileCount(n: number): string {
  return `${fmtExact(n)} ${n === 1 ? "file" : "files"}`;
}

/**
 * The files a deep run delivered: each with its name, type and size, its
 * download, and an image's preview. Bounded at what one run may deliver.
 */
export function FilesCard({ card }: { card: AssistantFileCard }) {
  const files = card.files.slice(0, ASSISTANT_DELIVERED_FILES_PER_RUN);
  const previews = useFilePreviews(previewSources(files));
  const items = files.map((file): FileListItem => ({
    id: file.file_id,
    name: file.name,
    type: fileTypeWords(file.media_type),
    size: fmtBytes(file.size),
    preview: previews[file.file_id],
    action: (
      <DownloadButton
        filename={file.name}
        file={() => exportFile(file.download)}
        aria-label={`Download ${file.name}`}
        failedMessage="The file could not be downloaded. Try again."
      />
    ),
  }));
  return (
    <StatusCard title="Delivered files" subtitle={fileCount(files.length)}>
      <FileList label="Delivered files" files={items} />
    </StatusCard>
  );
}

/** The images a question was sent with, as chips under its bubble, each with its thumbnail. */
export function SentAttachments({ files }: { files: readonly AssistantFileRef[] }) {
  const previews = useFilePreviews(previewSources(files));
  return (
    <>
      {files.map((file) => (
        <AttachmentChip key={file.file_id} name={file.name} preview={previews[file.file_id]} />
      ))}
    </>
  );
}
