/**
 * Where a repository's bytes download (`/git/{repo}/-/raw` and `archive.zip`)
 * and whether this fleet serves them: one `HEAD` per name, which a serving build answers `405`.
 */
import { useEffect, useState } from "react";
import { pathSegment, rest, RestError } from "~/protocol/index.ts";

/** A whole commit's files as a ZIP, on the dashboard's own origin so the session cookie carries it. */
export function archiveHref(name: string, commit: string): string {
  return `/git/${pathSegment(name)}/-/archive.zip?${new URLSearchParams({ rev: commit })}`;
}

/** One file's bytes at a whole commit; `download` asks for an attachment whatever the file is. */
export function rawHref(name: string, commit: string, path: string, download = false): string {
  const query = new URLSearchParams({ rev: commit, path, ...(download ? { download: "1" } : {}) });
  return `/git/${pathSegment(name)}/-/raw?${query}`;
}

/** Whether downloads are served: `checking` until the probe answers, `unknown` where it cannot tell. */
export type DownloadsServed = "yes" | "no" | "unknown" | "checking";

/** Each name's probe, asked once per page load. */
const probes = new Map<string, Promise<DownloadsServed>>();

function probe(name: string): Promise<DownloadsServed> {
  let asked = probes.get(name);
  if (!asked) {
    asked = rest.request("HEAD", `/git/${pathSegment(name)}/-/archive.zip`, { read: "text" }).then(
      (): DownloadsServed => "unknown",
      (err: unknown): DownloadsServed =>
        err instanceof RestError && err.status === 405
          ? "yes"
          : err instanceof RestError && err.status === 404
            ? "no"
            : "unknown",
    );
    probes.set(name, asked);
  }
  return asked;
}

/** Forget every probe: a test's fresh page load. */
export function forgetDownloadProbes(): void {
  probes.clear();
}

/** Whether `name`'s downloads are served, probed once `ask` first holds. */
export function useDownloadsServed(name: string, ask: boolean): DownloadsServed {
  const [answer, setAnswer] = useState<{ name: string; served: DownloadsServed } | null>(null);
  useEffect(() => {
    if (!ask) return undefined;
    let live = true;
    void probe(name).then((served) => {
      if (live) setAnswer({ name, served });
    });
    return () => {
      live = false;
    };
  }, [name, ask]);
  return answer?.name === name ? answer.served : "checking";
}
