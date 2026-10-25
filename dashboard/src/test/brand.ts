/**
 * The brand kits a brand suite lists: the draft a setup left, as a reader's
 * listing answers it.
 */

import type { ArtifactListItem } from "~/protocol/index.ts";

/** The id of the draft kit every brand suite lists. */
export const DRAFT_KIT = "01DRAFT";

/** The draft kit in a reader's listing, at version 2, owned by `ana` unless `over` says otherwise. */
export function brandKitRow(over: Partial<ArtifactListItem> = {}): ArtifactListItem {
  return {
    id: DRAFT_KIT,
    kind: "brand",
    title: "FairSplit brand",
    owner: "ana",
    owner_kind: "human",
    head_number: 2,
    role: "owner",
    status: "live",
    capabilities: [],
    share_count: 1,
    created_by: { name: "ana", kind: "human" },
    updated_by: { name: "ana", kind: "human" },
    created_at: "2026-10-24T09:00:00Z",
    updated_at: "2026-10-24T09:00:00Z",
    trashed_at: "0001-01-01T00:00:00Z",
    opened_at: "0001-01-01T00:00:00Z",
    ...over,
  } as ArtifactListItem;
}
