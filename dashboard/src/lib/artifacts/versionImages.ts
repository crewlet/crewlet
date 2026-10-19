/**
 * A native artifact's pictures (a deck's, a doc's), loaded the one way the
 * dashboard's policy allows: the bytes of a version file, fetched with the
 * reader's own credential from the API (which serves them only as an
 * attachment), typed from the file's name, and shown through an object URL.
 * Nothing is loaded from an address the content names.
 *
 * Every object URL is revoked when the version or the set of pictures changes,
 * and when the artifact is closed.
 */

import { useEffect, useState } from "react";
import { versionFile } from "./api.ts";

/** The image types a native kind may show, by extension (the engine's raster and SVG essences). */
const IMAGE_TYPES: Readonly<Record<string, string>> = {
  png: "image/png",
  jpg: "image/jpeg",
  jpeg: "image/jpeg",
  gif: "image/gif",
  webp: "image/webp",
  svg: "image/svg+xml",
};

/** The image type a version file is shown as, or null for one that is not a picture. */
export function imageType(path: string): string | null {
  const ext = path.split(".").pop()?.toLowerCase() ?? "";
  return IMAGE_TYPES[ext] ?? null;
}

/**
 * The version path a relative reference in the file at `from` names, or null
 * for one that is not a file of the version: an address with a scheme, a
 * fragment or a query, an absolute path, or one that climbs above the
 * version's root.
 */
export function resolveVersionPath(from: string, ref: string): string | null {
  const r = ref.trim();
  if (r === "" || /^[a-z][a-z0-9+.-]*:/i.test(r) || /[?#\\]/.test(r) || r.startsWith("/")) {
    return null;
  }
  const parts = from.split("/").slice(0, -1);
  for (const seg of r.split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
      if (parts.length === 0) return null;
      parts.pop();
    } else {
      parts.push(seg);
    }
  }
  return parts.length > 0 ? parts.join("/") : null;
}

/**
 * Loads the pictures at `paths` of one version, and answers the object URL
 * each is shown through: null until it is loaded, and for one that failed or
 * is not a picture, which the caller draws as its placeholder or alt text.
 */
export function useVersionImages(
  id: string,
  version: number,
  paths: readonly string[],
): (path: string) => string | null {
  const [urls, setUrls] = useState<ReadonlyMap<string, string>>(new Map());
  // ONE KEY FOR THE SET, so a caller that rebuilds an equal list each render
  // does not refetch every picture.
  const key = paths.join("\u0000");

  useEffect(() => {
    const wanted = key === "" ? [] : key.split("\u0000");
    if (wanted.length === 0) return undefined;
    const ctl = new AbortController();
    const made: string[] = [];
    void Promise.all(
      wanted.map(async (path) => {
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
  }, [id, version, key]);

  return (path: string) => urls.get(path) ?? null;
}
