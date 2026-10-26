/**
 * A stored file's picture, as an object URL an `<img>` draws: the one way the
 * drawer shows a file's bytes (a draft's proposed file, a file a deep run
 * delivered, an image the person attached).
 *
 * THE BYTES ARE READ AS THE PERSON, through the protocol's own fetch (the
 * session cookie, a refusal read as the engine's JSON), and TYPED FROM THE
 * REFERENCE the engine checked, never from the response. The URL goes only to
 * an image element, which renders a picture and runs no script, so an SVG
 * that carries one is still only a picture. Each URL is revoked when its file
 * leaves the list or the drawing goes.
 */

import { useEffect, useState } from "react";

/** Whether a media type is one the drawer previews: every image type, SVG included. */
export function isPreviewable(mediaType: string): boolean {
  return mediaType.startsWith("image/");
}

/** A file whose picture is wanted. */
export interface PreviewSource {
  /** Names the file, so a new file is read again and the same one is not. */
  key: string;
  /** The type the engine checked, which the picture is typed as. */
  mediaType: string;
  /** Read the bytes, as the person. */
  load: (signal: AbortSignal) => Promise<Blob>;
}

/**
 * The object URL of each file's bytes, by its key: absent while they are
 * read, and for a file whose bytes cannot be had.
 */
export function useFilePreviews(
  sources: readonly PreviewSource[],
): Readonly<Record<string, string>> {
  const [urls, setUrls] = useState<Readonly<Record<string, string>>>({});
  const identity = sources.map((s) => `${s.key}\u0000${s.mediaType}`).join("\u0001");
  useEffect(() => {
    const controller = new AbortController();
    const made: string[] = [];
    for (const source of sources) {
      source.load(controller.signal).then(
        (blob) => {
          if (controller.signal.aborted) return;
          const url = URL.createObjectURL(new Blob([blob], { type: source.mediaType }));
          made.push(url);
          setUrls((now) => ({ ...now, [source.key]: url }));
        },
        () => {
          // NO PICTURE, the file's mark instead: its name and type still say
          // what it is, and its download says why it cannot be had.
        },
      );
    }
    return () => {
      controller.abort();
      for (const url of made) URL.revokeObjectURL(url);
      setUrls({});
    };
    // `sources` is a fresh array each render over the same files, named by `identity`.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [identity]);
  return urls;
}

/** One file's picture: its object URL, or null while it is read, when it cannot be had, or when not `enabled`. */
export function useFilePreview(
  key: string,
  mediaType: string,
  load: (signal: AbortSignal) => Promise<Blob>,
  enabled = true,
): string | null {
  const urls = useFilePreviews(enabled ? [{ key, mediaType, load }] : []);
  return urls[key] ?? null;
}
