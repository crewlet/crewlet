/**
 * The artifacts' closed sets the frame reads: what an artifact can be, what a
 * version may declare, the role ladder a share grants, what a history row is
 * filed under, how long the trash keeps one, and the most one listing
 * answers, which the sidebar's pinned list asks for.
 *
 * EXACTLY THE ENGINE'S, in the engine's own order, each held by the gate its
 * row in `internal/clientsource/contract.go` names. A kind or a role the
 * engine grew that this lacks is one no screen can draw or grant; one this
 * keeps that the engine dropped is a choice every write refuses.
 *
 * ONLY WHAT THE FRAME READS. This module is in the entry: the frame words an
 * artifact through `lib/artifacts/words.ts`, which its routes, its navigation
 * and the access words import, and the sidebar asks for its pins here. A
 * bundler places a module whole, so a declaration here rides every reader's
 * first paint, read or not. Everything else of the artifacts' contract, which
 * only the artifacts workspace reads, is `contract/artifactsworkspace.ts`,
 * which the entry never reaches. `app/lazy.test.tsx` holds both: a
 * declaration here the entry does not read, and an entry that reaches the
 * workspace's module, each fail it.
 *
 * The types screens use are derived beside the wire shapes
 * (`protocol/types.ts`), so this module stays the declarations a gate reads.
 */

/**
 * Every kind of artifact (`artifacts.Kinds`): a page that runs code, a
 * doc, a deck, a dashboard of figures, a native sheet, a design's canvas of
 * framed artboards, and a brand kit (the company's logos, colours, type and
 * voice, which an administrator adopts).
 */
export const ARTIFACT_KINDS = [
  "html",
  "doc",
  "slides",
  "dashboard",
  "sheet",
  "design",
  "brand",
] as const;

/** Every runtime capability a version may declare (`artifacts.Capabilities`). */
export const ARTIFACT_CAPABILITIES = ["assets", "db", "state", "identity", "presence"] as const;

/** The role ladder, least first (`artifacts.Roles`): each role includes every role before it. */
export const ARTIFACT_ROLES = ["viewer", "commenter", "contributor", "editor", "owner"] as const;

/** The roles a share may grant (`artifacts.ShareableRoles`): owner moves only by transfer. */
export const SHAREABLE_ROLES = ["viewer", "commenter", "contributor", "editor"] as const;

/**
 * Every op an artifact's history row is filed under (`artifactsdomain.HistoryOps`),
 * in the engine's order: what a history row and the company trail are worded
 * by. Not every op the log carries: a purge takes the history with it, and a
 * pin, an unpin and an opened mark are one person's browsing, so none of them
 * is ever a row; an asset's records are filed under the asset's own op. A
 * move names the folders by id only, never by name.
 */
export const ARTIFACT_HISTORY_OPS = [
  "create",
  "publish",
  "restore_version",
  "patch",
  "share",
  "unshare",
  "transfer",
  "trash",
  "restore",
  "purge_begin",
  "asset_create",
  "asset_remove",
  "link_work_item",
  "read_elevated",
  "move",
] as const;

/**
 * How many days a trashed artifact is kept before it is deleted for good
 * (`artifacts.TrashRetention`): the date every trash sentence names.
 */
export const ARTIFACT_TRASH_RETENTION_DAYS = 30;

/**
 * The most artifacts one listing answers (`artifactsdomain.MaxListLimit`): a
 * larger `max-keys` is read as this, and the sidebar's pinned list asks for
 * this many. Held by
 * `internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps`.
 */
export const ARTIFACT_LIST_MAX = 200;
