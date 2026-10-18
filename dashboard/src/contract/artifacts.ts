/**
 * The artifacts' closed sets and bounds: what an artifact can be, what a
 * version may declare, the role ladder a share grants, who a share can name,
 * how the artifact origin isolates a page, what a listing scopes and sorts by,
 * what a datastore column, rule and write can be, and how long the text a
 * person types may run.
 *
 * EXACTLY THE ENGINE'S, in the engine's own order, each held by the gate its
 * row in `internal/clientsource/contract.go` names. A kind or a role the
 * engine grew that this lacks is one no screen can draw or grant; one this
 * keeps that the engine dropped is a choice every write refuses; and a bound
 * set looser than the engine's is a form that accepts what the engine will
 * refuse, said only after the person pressed Save.
 *
 * The types screens use are derived beside the wire shapes
 * (`protocol/types.ts`), so this module stays the declarations a gate reads.
 */

/** Every kind of artifact (`artifacts.Kinds`): a page that runs code, a document, a native sheet. */
export const ARTIFACT_KINDS = ["html", "markdown", "sheet"] as const;

/** Every runtime capability a version may declare (`artifacts.Capabilities`). */
export const ARTIFACT_CAPABILITIES = ["assets", "db", "state", "identity", "presence"] as const;

/** The role ladder, least first (`artifacts.Roles`): each role includes every role before it. */
export const ARTIFACT_ROLES = ["viewer", "commenter", "contributor", "editor", "owner"] as const;

/** The roles a share may grant (`artifacts.ShareableRoles`): owner moves only by transfer. */
export const SHAREABLE_ROLES = ["viewer", "commenter", "contributor", "editor"] as const;

/** Who a share names (`artifacts.AudienceKinds`): a seat, a unit and everything below it, the company. */
export const AUDIENCE_KINDS = ["seat", "unit", "company"] as const;

/** How the artifact origin isolates a page (`artifacts.OriginModes`). */
export const ORIGIN_MODES = ["per_artifact", "shared"] as const;

/** Who is present in an artifact's room is doing what (`artifacts.PresenceActivities`). */
export const PRESENCE_ACTIVITIES = ["viewing", "editing", "idle"] as const;

/** What a listing is about (`artifactsdomain.ListScopes`). */
export const ARTIFACT_LIST_SCOPES = ["mine", "shared", "company", "agents", "all"] as const;

/** How a listing is ordered (`artifactsdomain.ListSorts`), the engine's default first. */
export const ARTIFACT_LIST_SORTS = ["updated", "created", "title", "opened"] as const;

/** The longest title, in UTF-8 bytes (`artifacts.MaxTitleBytes`): a tab title and a card line. */
export const ARTIFACT_TITLE_MAX_BYTES = 120;

/** The longest description, in UTF-8 bytes (`artifacts.MaxDescriptionBytes`). */
export const ARTIFACT_DESCRIPTION_MAX_BYTES = 1000;

/** The longest icon word, in UTF-8 bytes (`artifacts.MaxIconBytes`). */
export const ARTIFACT_ICON_MAX_BYTES = 40;

/** The longest version label, in UTF-8 bytes (`artifacts.MaxLabelBytes`). */
export const ARTIFACT_LABEL_MAX_BYTES = 60;

/** Every column type a datastore validates (`artifacts.ColumnTypes`). */
export const COLUMN_TYPES = [
  "text",
  "longtext",
  "number",
  "checkbox",
  "select",
  "multiselect",
  "status",
  "person",
  "date",
  "url",
  "rating",
] as const;

/** Who reads a collection's rows, on every read path (`artifacts.ReadRules`). */
export const READ_RULES = ["all", "own_rows", "editors", "private"] as const;

/** The collection policies people pick by intent (`artifacts.Presets`). */
export const COLLECTION_PRESETS = ["shared", "contribute", "author", "private"] as const;

/** The categories a status column's options each name (`artifactdata.StatusCategories`). */
export const STATUS_CATEGORIES = ["todo", "in_progress", "done", "blocked"] as const;

/** What narrows a column's write rule to some rows (`artifacts.FieldConditions`). */
export const FIELD_CONDITIONS = ["own_row", "assigned_to_viewer"] as const;

/** Every op one datastore write may carry (`artifacts.DataOpKinds`). */
export const DATA_OP_KINDS = [
  "set",
  "merge",
  "delete",
  "increment",
  "append",
  "add",
  "remove",
] as const;

/** Every verdict the datastore's applier writes (`artifacts.DataVerdicts`). */
export const DATA_VERDICTS = [
  "applied",
  "conflict",
  "invalid",
  "forbidden",
  "quota",
  "gone",
] as const;

/** The most ops one datastore write carries (`artifacts.MaxBatchOps`): a paste larger is refused whole. */
export const DATA_BATCH_MAX_OPS = 100;

/** The most collections one store keeps (`artifacts.MaxCollections`). */
export const DATA_COLLECTIONS_MAX = 64;

/** The most columns one collection declares (`artifacts.MaxColumns`). */
export const DATA_COLUMNS_MAX = 128;

/**
 * A collection's name (`artifactdata.ValidCollectionName`): a lowercase
 * letter, then lowercase letters, digits and `_`, at most 48 bytes, and none
 * of [RESERVED_COLLECTIONS]. Held by BEHAVIOUR, as the login grammars are: the
 * gate compiles it and runs a corpus of names through it, the reserved list
 * and the engine's own check, failing on any name the two answer differently.
 */
export const COLLECTION_NAME_PATTERN = "^[a-z][a-z0-9_]{0,47}$";

/**
 * The names the datastore's own routes take under `/artifacts/{id}/data/`,
 * which no collection may be called. Held with [COLLECTION_NAME_PATTERN].
 */
export const RESERVED_COLLECTIONS = [
  "schema",
  "history",
  "state",
  "writes",
  "undo",
  "export",
] as const;

/**
 * A field's name (`query.ValidField`): a letter, then letters, digits and
 * `_`, at most 64 bytes. A leading `_` is the engine's. Held by behaviour.
 */
export const FIELD_NAME_PATTERN = "^[A-Za-z][A-Za-z0-9_]{0,63}$";

/** The engine's per-viewer state collection (`artifactdata.StateCollection`), which no screen lists. */
export const STATE_COLLECTION = "_state";

/**
 * The most presence frames one socket sends a second (`artifacts.PresenceClientRate`).
 * The engine drops the rest, so the viewer coalesces what it says to this.
 */
export const PRESENCE_FRAMES_PER_SECOND = 10;
