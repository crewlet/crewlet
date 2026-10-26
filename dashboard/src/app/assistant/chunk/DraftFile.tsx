/**
 * A file a draft change proposes, as its approval card draws it: its name and
 * type, and for an image a thumbnail.
 *
 * DRAWN THROUGH AN IMAGE, NEVER INLINED. The bytes are read as the person
 * through the owner's route (`conversationFile`), typed from the reference
 * the engine checked rather than from the response, and shown through an
 * object URL in the design system's `LogoSlot` thumbnail, an `<img>`: an SVG
 * drawn that way runs no script and reaches nothing on the page. A file that
 * is not an image, or whose bytes cannot be had, is its name and type alone.
 *
 * ON THE GROUND ITS FIELD DECLARES: a logo for dark grounds is drawn on the
 * brand's dark ground, as the Logos step draws it, so a light wordmark is
 * readable on the card.
 */

import { LogoSlot } from "@crewlethq/ui";
import type { AssistantDraftFileRef } from "~/contract/assistant.ts";
import { conversationFile } from "~/protocol/assistant.ts";
import { useFilePreview } from "./filePreview.ts";

/** The image types a card draws a thumbnail of. */
const IMAGES: readonly string[] = ["image/svg+xml", "image/png", "image/webp", "image/jpeg"];

export function DraftFile({
  file,
  ground,
}: {
  file: AssistantDraftFileRef;
  /** The ground the field it is proposed for shows it on: a dark-ground logo on a dark ground. */
  ground?: { tone: "light" | "dark"; color?: string };
}) {
  const image = IMAGES.includes(file.media_type);
  const src = useFilePreview(
    `${file.conversation_id}/${file.file_id}`,
    file.media_type,
    (signal) => conversationFile(file, signal),
    image,
  );
  const words = `${file.name} (${file.media_type})`;
  if (!image) return <>{words}</>;
  return (
    <LogoSlot
      label={file.name}
      description={file.media_type}
      ground={ground?.tone ?? "light"}
      groundColor={ground?.color}
      size="sm"
      guide={false}
      src={src}
      alt={words}
    />
  );
}
