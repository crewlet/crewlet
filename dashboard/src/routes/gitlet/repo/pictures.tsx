/** A repository's pictures from its raw route (`downloads.ts`'s rawHref), read with the reader's
 *  session and shown through a typed object URL: the route answers every binary file as
 *  `application/octet-stream`. */
import { useEffect, useState } from "react";
import { Skeleton } from "@crewlethq/ui";
import { baseName } from "~/lib/code.ts";
import { fmtBytes } from "~/lib/format.ts";
import { pathSegment, rest } from "~/protocol/index.ts";

/** Each picture's media type, by its file's extension. */
const PICTURE_TYPES: Readonly<Record<string, string>> = {
  png: "image/png",
  jpg: "image/jpeg",
  jpeg: "image/jpeg",
  gif: "image/gif",
  webp: "image/webp",
  avif: "image/avif",
  bmp: "image/bmp",
  ico: "image/x-icon",
  svg: "image/svg+xml",
};

/** The largest picture shown in place: the tab holds its bytes while the page is open, and one
 *  past this is a download, not a preview. */
export const PICTURE_MAX_BYTES = 10 * 1024 * 1024;

/** The media type of a picture at `path`, or null for a file that is no picture. */
export function pictureType(path: string): string | null {
  const dot = path.lastIndexOf(".");
  return dot < 0 ? null : (PICTURE_TYPES[path.slice(dot + 1).toLowerCase()] ?? null);
}

/** A picture as read: its object URL, or why there is none. */
export type Picture = { url: string } | { failed: "too_large" | "unread" };

/** The pictures at `paths` of `repo` at the whole `commit`, each read once its commit is known and
 *  let go with the page: absent while one is read. */
export function usePictures(
  repo: string,
  commit: string,
  paths: readonly string[],
): ReadonlyMap<string, Picture> {
  const key = commit === "" || paths.length === 0 ? "" : JSON.stringify([repo, commit, ...paths]);
  const [read, setRead] = useState<{ key: string; pictures: Map<string, Picture> }>({
    key: "",
    pictures: new Map(),
  });
  useEffect(() => {
    if (key === "") return undefined;
    const [name = "", at = "", ...wanted] = JSON.parse(key) as string[];
    const abort = new AbortController();
    const urls: string[] = [];
    let live = true;
    void Promise.all(wanted.map((path) => readPicture(name, at, path, abort.signal))).then(
      (pictures) => {
        for (const [, p] of pictures) if ("url" in p) urls.push(p.url);
        if (live) setRead({ key, pictures: new Map(pictures) });
        else for (const url of urls) URL.revokeObjectURL(url);
      },
    );
    return () => {
      live = false;
      abort.abort();
      for (const url of urls) URL.revokeObjectURL(url);
    };
  }, [key]);
  return read.key === key ? read.pictures : EMPTY;
}

const EMPTY: ReadonlyMap<string, Picture> = new Map();

/** One picture's bytes as an object URL of its type, or why not. */
async function readPicture(
  repo: string,
  commit: string,
  path: string,
  signal: AbortSignal,
): Promise<[string, Picture]> {
  const type = pictureType(path);
  if (!type) return [path, { failed: "unread" }];
  try {
    const query = new URLSearchParams({ rev: commit, path });
    const bytes = await rest.blob(`/git/${pathSegment(repo)}/-/raw?${query}`, signal);
    if (bytes.size > PICTURE_MAX_BYTES) return [path, { failed: "too_large" }];
    return [path, { url: URL.createObjectURL(new Blob([bytes], { type })) }];
  } catch {
    return [path, { failed: "unread" }];
  }
}

/** Where a document's picture `src` is in the repository: a path relative to the document's
 *  directory or to the root (`/`), or null for an address elsewhere (a host, a data URL, an
 *  anchor). */
export function picturePath(src: string, dir: string): string | null {
  if (src === "" || /^[a-z][a-z0-9+.-]*:/i.test(src) || src.startsWith("//") || src.startsWith("#"))
    return null;
  const bare = src.replace(/[?#].*$/, "");
  let path: string;
  try {
    path = decodeURIComponent(bare);
  } catch {
    path = bare;
  }
  const parts = (path.startsWith("/") ? path : `${dir ? `${dir}/` : ""}${path}`).split("/");
  const out: string[] = [];
  for (const part of parts) {
    if (part === "" || part === ".") continue;
    if (part === "..") {
      if (out.length === 0) return null;
      out.pop();
    } else out.push(part);
  }
  return out.length > 0 ? out.join("/") : null;
}

/** A picture file's view in place of its lines: shown, or said why not. */
export function FilePicture({
  repo,
  commit,
  path,
  size,
}: {
  repo: string;
  /** The commit the file was read at, whole, or none while it is read. */
  commit: string;
  path: string;
  size: number;
}) {
  const fits = size <= PICTURE_MAX_BYTES;
  const shown = usePictures(repo, commit, fits ? [path] : []).get(path);
  if (!fits) {
    return (
      <p className="code-blob__note">{`A picture of ${fmtBytes(size)}, past the ${fmtBytes(PICTURE_MAX_BYTES)} shown here: download it to see it.`}</p>
    );
  }
  if (!shown) return <Skeleton variant="box" height="8rem" label={`Loading ${baseName(path)}`} />;
  if ("failed" in shown) {
    return <p className="code-blob__note">The picture could not be read: download it to see it.</p>;
  }
  return (
    <div className="code-picture">
      <img src={shown.url} alt={baseName(path)} />
    </div>
  );
}
