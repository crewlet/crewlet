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
 */

import { useEffect, useState } from "react";
import { LogoSlot } from "@crewlethq/ui";
import type { AssistantDraftFileRef } from "~/contract/assistant.ts";
import { conversationFile } from "~/protocol/assistant.ts";

/** The image types a card draws a thumbnail of. */
const IMAGES: readonly string[] = ["image/svg+xml", "image/png", "image/webp", "image/jpeg"];

export function DraftFile({ file }: { file: AssistantDraftFileRef }) {
  const image = IMAGES.includes(file.media_type);
  const [src, setSrc] = useState<string | null>(null);
  useEffect(() => {
    if (!image) return;
    const controller = new AbortController();
    let url: string | null = null;
    conversationFile(file, controller.signal).then(
      (blob) => {
        url = URL.createObjectURL(new Blob([blob], { type: file.media_type }));
        setSrc(url);
      },
      () => setSrc(null),
    );
    return () => {
      controller.abort();
      if (url) URL.revokeObjectURL(url);
    };
  }, [file, image]);
  const words = `${file.name} (${file.media_type})`;
  if (!image) return <>{words}</>;
  return (
    <LogoSlot
      label={file.name}
      description={file.media_type}
      ground="light"
      size="sm"
      guide={false}
      src={src}
      alt={words}
    />
  );
}
