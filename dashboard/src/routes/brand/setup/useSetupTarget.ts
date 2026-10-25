/**
 * Which kit, and which of its versions, the brand setup opens on.
 *
 * - With a brand adopted, the company's kit: its newest version, so a draft a
 *   setup left (or an agent or an editor published) is where the setup
 *   continues, and the adopted version otherwise ("Run setup again").
 * - With none adopted, the draft a setup left: the most recently changed brand
 *   kit the reader may write ([draftKit]).
 * - With neither, nothing yet: the first Continue makes the kit.
 *
 * WHO MAY RUN IT: an administrator, or a person the kit is shared with to
 * edit, who finishes with Save for review. A reader who is neither is told
 * so, and offered nothing.
 */

import { useMemo } from "react";
import type { BrandDocument } from "~/protocol/index.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useRest } from "~/lib/useRest.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { versionText } from "~/lib/artifacts/api.ts";
import { roleIncludes } from "~/lib/artifacts/words.ts";
import { BRAND_ENTRY } from "~/lib/brand/api.ts";
import { mayAdoptBrand, parseBrandDocument } from "~/lib/brand/brand.ts";
import { blankBrand, draftKit } from "~/lib/brand/setup.ts";
import { adoptionSource, useBrand, versionSource, type BrandSource } from "~/lib/brand/useBrand.ts";

/** Where the setup opens: the kit, its version, the head a save is decided against, and the draft. */
export interface SetupTarget {
  /** What the setup was opened on, so a change of it opens the setup afresh. */
  key: string;
  /** The kit a save writes, or null before the first Continue makes it. */
  kit: string | null;
  /** The version the setup opened on, whose files a save carries; 0 for none. */
  version: number;
  /** The kit's head a save is decided against (`If-Match`). */
  head: number;
  document: BrandDocument;
  /** Where the opened version's files are read from, for the preview. */
  source: BrandSource | null;
}

export type SetupTargetRead =
  | { state: "loading" }
  | { state: "refused" }
  | { state: "ready"; target: SetupTarget; admin: boolean };

/** The brand kits a setup may continue, newest first: enough to find the draft a setup left. */
const KITS_QUERY = { scope: "all", kind: "brand", sort: "updated", "max-keys": 20 } as const;

export function useSetupTarget(): SetupTargetRead {
  const viewer = useViewer();
  const org = useOrg();
  const admin = mayAdoptBrand(viewer);
  const brand = useBrand();
  const adoption = brand.adoption;
  const kits = useQuery("artifacts", { ...KITS_QUERY }, { enabled: adoption === null });
  const draft = useMemo(
    () => (kits.data ? draftKit(kits.data.contents, admin) : null),
    [kits.data, admin],
  );
  const kit = adoption ? adoption.kit : (draft?.id ?? null);
  const version = adoption
    ? (adoption.newer_version ?? adoption.kit_version)
    : (draft?.head_number ?? 0);
  const detail = useQuery("artifact", { id: kit ?? "" }, { enabled: kit !== null });
  const manifest = useQuery(
    "artifact_version",
    { id: kit ?? "", n: version },
    { enabled: kit !== null && version > 0 },
  );
  const read = useRest(
    kit && version > 0 ? `brand-setup:${kit}:${version}` : null,
    async (signal) => parseBrandDocument(await versionText(kit!, version, BRAND_ENTRY, signal)),
  );

  return useMemo((): SetupTargetRead => {
    if (adoption === undefined || viewer.loading) return { state: "loading" };
    if (adoption === null && kits.loading) return { state: "loading" };
    if (kit === null) {
      // NO KIT YET: making the company's first is an administrator's.
      if (!admin) return { state: "refused" };
      const document = blankBrand(org?.name ?? "");
      return {
        state: "ready",
        admin,
        target: { key: "new", kit: null, version: 0, head: 0, document, source: null },
      };
    }
    if (detail.loading || manifest.loading || read.loading) return { state: "loading" };
    const role = detail.data?.role;
    if (!admin && !roleIncludes(role, "editor")) return { state: "refused" };
    const head = detail.data?.head_number ?? version;
    const doc = read.data;
    if (doc) {
      const digests = new Map(
        (manifest.data?.manifest.files ?? []).map((f) => [f.path, f.object.hash] as const),
      );
      return {
        state: "ready",
        admin,
        target: {
          key: `${kit}:${version}`,
          kit,
          version,
          head,
          document: doc,
          source: versionSource(kit, version, doc, digests),
        },
      };
    }
    // THE KIT'S VERSION COULD NOT BE READ (the kit was trashed; the adoption
    // outlives it): the setup opens on the adopted snapshot, whose files a
    // save brings by their bytes, and the save makes a new kit.
    const document = adoption?.document ?? blankBrand(org?.name ?? "");
    return {
      state: "ready",
      admin,
      target: {
        key: `${kit}:snapshot`,
        kit,
        version: 0,
        head,
        document,
        source: adoption ? adoptionSource(adoption) : null,
      },
    };
  }, [
    adoption,
    viewer.loading,
    kits.loading,
    kit,
    admin,
    org?.name,
    detail.loading,
    detail.data,
    manifest.loading,
    manifest.data,
    read.loading,
    read.data,
    version,
  ]);
}
