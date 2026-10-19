/**
 * A deck's pictures and typefaces, loaded the one way the dashboard's policy
 * allows: the bytes of a version file, fetched with the reader's own
 * credential from the API (which serves them only as an attachment), shown
 * through an object URL ([useVersionImages]) or registered as a `FontFace`
 * from the bytes. Nothing is loaded from an address a slide names.
 *
 * Everything is let go when the version changes or the deck is closed: every
 * object URL is revoked and every face removed from the document.
 */

import { useEffect, useMemo } from "react";
import type { ArtifactDeck, SlideNode } from "~/protocol/index.ts";
import { versionFile } from "./api.ts";
import { useVersionImages } from "./versionImages.ts";

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
  const paths = useMemo(() => (deck ? imagePaths(deck) : []), [deck]);
  const image = useVersionImages(id, version, paths);
  const faces = deck?.faces;

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

  return image;
}
