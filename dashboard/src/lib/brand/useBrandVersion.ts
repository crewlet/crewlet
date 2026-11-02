/**
 * One version of a brand kit, read: its `brand.json` and its manifest's
 * digests, as the document and a source of its files.
 *
 * The one reader of a kit version for the Brand page's review, its draft
 * notice and the brand setup. Kept out of `useBrand.ts`, which the artifact
 * screens import, so the artifact API it reads through stays in the brand
 * chunk.
 */

import { useMemo } from "react";
import type { ArtifactActor, BrandDocument, RestError } from "~/protocol/index.ts";
import { useQuery } from "../useQuery.ts";
import { useRest } from "../useRest.ts";
import { versionText } from "../artifacts/api.ts";
import { BRAND_ENTRY } from "./api.ts";
import { parseBrandDocument } from "./brand.ts";
import { versionSource, type BrandSource } from "./useBrand.ts";

export interface BrandVersionRead {
  /** True while either read is in flight. */
  loading: boolean;
  /** The version's brand, or null before it arrives and when it cannot be read. */
  document: BrandDocument | null;
  /** The version's files, digests included, or null with no document. */
  source: BrandSource | null;
  /** Why the version could not be read: unreachable, or no brand this page reads. */
  error: RestError | null;
  /**
   * Who published the version and when, from its manifest: the last edit of a
   * draft a setup left, since every Continue publishes one. Null until the
   * manifest answers, and where it names nobody.
   */
  edited: { by: ArtifactActor; at: string } | null;
}

/** Read `version` of `kit`; nothing while `kit` is null or `version` is 0. */
export function useBrandVersion(kit: string | null, version: number): BrandVersionRead {
  const on = kit !== null && version > 0;
  const manifest = useQuery("artifact_version", { id: kit ?? "", n: version }, { enabled: on });
  const read = useRest(on ? `brand-version:${kit}:${version}` : null, async (signal) => {
    const doc = parseBrandDocument(await versionText(kit!, version, BRAND_ENTRY, signal));
    if (!doc) throw new Error(`Version ${version} holds no brand this page can read.`);
    return doc;
  });
  const document = read.data;
  const files = manifest.data?.manifest.files;
  const source = useMemo(() => {
    if (!kit || !document) return null;
    const digests = new Map((files ?? []).map((f) => [f.path, f.object.hash] as const));
    return versionSource(kit, version, document, digests);
  }, [kit, version, document, files]);
  const loading = on && (read.loading || manifest.loading);
  const error = read.error;
  const by = manifest.data?.author;
  const at = manifest.data?.created_at;
  const edited = useMemo(() => (by?.name && at ? { by, at } : null), [by, at]);
  return useMemo(
    () => ({ loading, document: document ?? null, source, error, edited }),
    [loading, document, source, error, edited],
  );
}
