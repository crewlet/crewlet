/**
 * Stored files as the drawer draws them: the images a question was sent with,
 * under its bubble.
 *
 * EVERY BYTE IS READ THROUGH THE FILE'S OWN `download` ROUTE, the path the
 * engine built, checked to be one of the assistant's (`exportFile`), as the
 * person and under the same session: the download a reader presses and the
 * picture an image is previewed by are the same request.
 *
 * A PREVIEW IS ONLY EVER A PICTURE: an image's bytes, typed from the
 * reference, drawn through an object URL in an `<img>` (`filePreview.ts`), an
 * SVG included. Nothing a file carries is put into the page as markup.
 */

import { AttachmentChip } from "@crewlethq/ui";
import type { AssistantFileRef } from "~/contract/assistant.ts";
import { exportFile } from "~/protocol/assistant.ts";
import { isPreviewable, useFilePreviews, type PreviewSource } from "./filePreview.ts";

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
