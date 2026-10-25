/**
 * Which kit, and which of its versions, the brand setup opens on.
 *
 * The company's kit where it stands ([useCompanyKit]): a draft a setup left
 * (or an agent or an editor published) is where the setup continues, the
 * adopted version otherwise ("Run setup again"), and with no kit at all the
 * first Continue makes one.
 *
 * WHO MAY RUN IT: an administrator, or a person the kit is shared with to
 * edit, who finishes with Save for review. A reader who is neither is told
 * so, and offered nothing.
 */

import { useMemo } from "react";
import type { BrandDocument } from "~/protocol/index.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { roleIncludes } from "~/lib/artifacts/words.ts";
import { mayAdoptBrand } from "~/lib/brand/brand.ts";
import { blankBrand } from "~/lib/brand/setup.ts";
import { adoptionSource, useBrand, type BrandSource } from "~/lib/brand/useBrand.ts";
import { useCompanyKit } from "../useCompanyKit.ts";

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

export function useSetupTarget(): SetupTargetRead {
  const viewer = useViewer();
  const org = useOrg();
  const admin = mayAdoptBrand(viewer);
  const adoption = useBrand().adoption;
  const company = useCompanyKit(adoption, admin);
  const kit = company?.kit ?? null;
  const version = company?.version ?? 0;
  const detail = useQuery("artifact", { id: kit ?? "" }, { enabled: kit !== null });

  return useMemo((): SetupTargetRead => {
    if (company === undefined || viewer.loading) return { state: "loading" };
    if (company === null) {
      // NO KIT YET: making the company's first is an administrator's.
      if (!admin) return { state: "refused" };
      const document = blankBrand(org?.name ?? "");
      return {
        state: "ready",
        admin,
        target: { key: "new", kit: null, version: 0, head: 0, document, source: null },
      };
    }
    const read = company.read;
    if (detail.loading || read.loading) return { state: "loading" };
    const role = detail.data?.role;
    if (!admin && !roleIncludes(role, "editor")) return { state: "refused" };
    const head = detail.data?.head_number ?? version;
    if (read.document) {
      return {
        state: "ready",
        admin,
        target: {
          key: `${kit}:${version}`,
          kit: company.kit,
          version,
          head,
          document: read.document,
          source: read.source,
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
        kit: company.kit,
        version: 0,
        head,
        document,
        source: adoption ? adoptionSource(adoption) : null,
      },
    };
  }, [
    adoption,
    company,
    viewer.loading,
    admin,
    org?.name,
    detail.loading,
    detail.data,
    kit,
    version,
  ]);
}
