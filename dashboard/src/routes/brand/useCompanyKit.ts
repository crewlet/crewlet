/**
 * Where the company's brand kit stands: the kit, its newest version a reader
 * may continue, and whether that version is a draft nobody adopted yet.
 *
 * - With a brand adopted, the adopted kit at its newest version: a draft a
 *   setup left (or an agent or an editor published) when there is one, the
 *   adopted version otherwise.
 * - With none adopted, the draft a setup left: the most recently changed brand
 *   kit the reader may write ([draftKit]).
 * - With neither, no kit.
 *
 * The Brand page's draft notice and the brand setup read the same answer, so
 * the draft the page offers to continue is the one the setup opens on.
 */

import { useMemo } from "react";
import type { ArtifactRole } from "~/protocol/index.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { Adoption } from "~/lib/brand/brand.ts";
import { draftKit } from "~/lib/brand/setup.ts";
import { useBrandVersion, type BrandVersionRead } from "~/lib/brand/useBrandVersion.ts";

export interface CompanyKit {
  kit: string;
  /** The version a setup continues: the kit's newest the reader is told of. */
  version: number;
  /** Whether that version is not the adopted one: a draft to continue or adopt. */
  draft: boolean;
  /** That version, read (nothing when the caller asked not to). */
  read: BrandVersionRead;
  /**
   * The reader's role on a kit found among the brand kits they may write,
   * with none adopted; undefined for the adopted kit, which is not listed.
   */
  role: ArtifactRole | undefined;
  /** List the kits again, after a write that changes which kit is the draft. */
  refetch: () => void;
}

/** The brand kits a setup may continue, newest first: enough to find the draft a setup left. */
const KITS_QUERY = { scope: "all", kind: "brand", sort: "updated", "max-keys": 20 } as const;

/**
 * The company's kit for a reader: undefined while it is being found, null
 * when there is none, the kit otherwise. `admin` decides which kits are the
 * reader's to continue when none is adopted. `read: false` finds the kit
 * without reading its version, for a reader the kit may not be shared with.
 */
export function useCompanyKit(
  adoption: Adoption | null | undefined,
  admin: boolean,
  options: { read?: boolean } = {},
): CompanyKit | null | undefined {
  const kits = useQuery("artifacts", { ...KITS_QUERY }, { enabled: adoption === null });
  const draft = useMemo(
    () => (kits.data ? draftKit(kits.data.contents, admin) : null),
    [kits.data, admin],
  );
  const kit = adoption ? adoption.kit : (draft?.id ?? null);
  const version = adoption
    ? (adoption.newer_version ?? adoption.kit_version)
    : (draft?.head_number ?? 0);
  const read = useBrandVersion(options.read === false ? null : kit, version);
  const role = adoption ? undefined : draft?.role;
  const refetch = kits.refetch;
  return useMemo(() => {
    if (adoption === undefined) return undefined;
    if (adoption === null && kits.loading) return undefined;
    if (kit === null) return null;
    return {
      kit,
      version,
      draft: !adoption || Boolean(adoption.newer_version),
      read,
      role,
      refetch,
    };
  }, [adoption, kits.loading, kit, version, read, role, refetch]);
}
