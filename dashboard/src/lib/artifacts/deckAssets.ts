/**
 * A deck's pictures and typefaces, loaded the one way the dashboard's policy
 * allows: the bytes of a version file, fetched with the reader's own
 * credential from the API (which serves them only as an attachment), typed
 * from the file's name, and shown through an object URL, or registered as a
 * `FontFace` from the bytes. Nothing is loaded from an address a slide names.
 *
 * Everything is let go when the version changes or the deck is closed: every
 * object URL is revoked and every face removed from the document.
 */

import { useEffect, useMemo, useState } from "react";
import type { ArtifactDeck, SlideNode } from "~/protocol/index.ts";
import { versionFile } from "./api.ts";

/** The image types a slide may show, by extension (`slideformat`'s image essences). */
const IMAGE_TYPES: Readonly<Record<string, string>> = {
  png: "image/png",
  jpg: "image/jpeg",
  jpeg: "image/jpeg",
  gif: "image/gif",
  webp: "image/webp",
  svg: "image/svg+xml",
};

/** The image type a version file is shown as, or null for one a slide may not show. */
export function imageType(path: string): string | null {
  const ext = path.split(".").pop()?.toLowerCase() ?? "";
  return IMAGE_TYPES[ext] ?? null;
}

/** Every image path a deck's slides show, once each. */
export function imagePaths(deck: ArtifactDeck): string[] {
  const out = new Set<string>();
  const walk = (n: SlideNode) => {
    if (n.tag === "img" && n.attrs?.src) out.add(n.attrs.src);
    for (const c of n.children ?? []) walk(c);
  };
  for (const s of deck.slides ?? []) walk(s.root);
  return [...out];
}

/**
 * Loads a deck's images and faces for one version, and answers the object URL
 * each image is shown through (null until it is loaded, and for one that
 * failed, which is drawn as its sized placeholder).
 */
export function useDeckAssets(
  id: string,
  version: number,
  deck: ArtifactDeck | null,
): (path: string) => string | null {
  const [urls, setUrls] = useState<ReadonlyMap<string, string>>(new Map());
  const paths = useMemo(() => (deck ? imagePaths(deck) : []), [deck]);
  const faces = deck?.faces;

  useEffect(() => {
    if (paths.length === 0) return undefined;
    const ctl = new AbortController();
    const made: string[] = [];
    void Promise.all(
      paths.map(async (path) => {
        const type = imageType(path);
        if (!type) return null;
        try {
          const blob = await versionFile(id, version, path, ctl.signal);
          const url = URL.createObjectURL(new Blob([blob], { type }));
          made.push(url);
          return [path, url] as const;
        } catch {
          return null;
        }
      }),
    ).then((entries) => {
      if (ctl.signal.aborted) return;
      setUrls(new Map(entries.filter((e): e is readonly [string, string] => e !== null)));
    });
    return () => {
      ctl.abort();
      for (const url of made) URL.revokeObjectURL(url);
      setUrls(new Map());
    };
  }, [id, version, paths]);

  useEffect(() => {
    if (!faces || faces.length === 0 || typeof FontFace === "undefined") return undefined;
    const ctl = new AbortController();
    const added: FontFace[] = [];
    for (const face of faces) {
      void versionFile(id, version, face.src, ctl.signal)
        .then((blob) => blob.arrayBuffer())
        .then((bytes) => new FontFace(face.family, bytes).load())
        .then((loaded) => {
          if (ctl.signal.aborted) return;
          document.fonts.add(loaded);
          added.push(loaded);
        })
        .catch(() => {
          // A FACE THAT DID NOT LOAD falls back to the next family the slide
          // names, as in any stylesheet; the deck still reads.
        });
    }
    return () => {
      ctl.abort();
      for (const f of added) document.fonts.delete(f);
    };
  }, [id, version, faces]);

  return (path: string) => urls.get(path) ?? null;
}
