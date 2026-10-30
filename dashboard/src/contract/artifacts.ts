/**
 * The artifacts' closed sets and bounds: what an artifact can be, what a
 * version may declare, the role ladder a share grants, who a share can name,
 * how the artifact origin isolates a page, what a listing scopes and sorts by,
 * what a history row is filed under, what a datastore column, rule and write
 * can be, and how long the text a person types may run.
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

/**
 * What the engine decides by kind (`artifacts.KindSpecs`), the half a screen
 * draws by: whether the kind runs author code in a frame on the artifact
 * origin (`framed`) or is drawn here from a closed format (`native`), and what
 * its datastore is to it (`declared` by a page's `db`, the `content` itself,
 * `implied` beside a native kind's files, or `none`).
 */
export const ARTIFACT_KIND_SPECS = {
  html: { render: "framed", store: "declared" },
  doc: { render: "native", store: "none" },
  slides: { render: "native", store: "none" },
  dashboard: { render: "native", store: "implied" },
  sheet: { render: "native", store: "content" },
  design: { render: "framed", store: "declared" },
  brand: { render: "native", store: "none" },
} as const;

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

/** What a listing is about (`artifactsdomain.ListScopes`). */
export const ARTIFACT_LIST_SCOPES = ["mine", "shared", "company", "agents", "all"] as const;

/** How a listing is ordered (`artifactsdomain.ListSorts`), the engine's default first. */
export const ARTIFACT_LIST_SORTS = ["updated", "created", "title", "opened"] as const;

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

/** The longest title, in UTF-8 bytes (`artifacts.MaxTitleBytes`): a tab title and a card line. */
export const ARTIFACT_TITLE_MAX_BYTES = 120;

/** The longest description, in UTF-8 bytes (`artifacts.MaxDescriptionBytes`). */
export const ARTIFACT_DESCRIPTION_MAX_BYTES = 1000;

/** The longest icon word, in UTF-8 bytes (`artifacts.MaxIconBytes`). */
export const ARTIFACT_ICON_MAX_BYTES = 40;

/** The longest version label, in UTF-8 bytes (`artifacts.MaxLabelBytes`). */
export const ARTIFACT_LABEL_MAX_BYTES = 60;

/** The most files one page check reads (`artifactupload.MaxCheckFiles`). */
export const ARTIFACT_CHECK_MAX_FILES = 64;

/** The most text one page check reads, in bytes across its files (`artifactupload.MaxCheckBytes`). */
export const ARTIFACT_CHECK_MAX_BYTES = 2097152;

/** The most tabs one doc holds (`docindex.MaxTabs`). */
export const DOC_MAX_TABS = 32;

/** The longest tab title, in UTF-8 bytes (`docindex.MaxTitleBytes`). */
export const DOC_TAB_TITLE_MAX_BYTES = 120;

/** The most slides a starter deck opens with (`slideformat.MaxPlanSlides`). */
export const DECK_PLAN_MAX_SLIDES = 60;

/** The longest slide title in a starter deck's plan, in UTF-8 bytes (`slideformat.MaxSlideTitleBytes`). */
export const DECK_SLIDE_TITLE_MAX_BYTES = 200;
/**
 * How many days a trashed artifact is kept before it is deleted for good
 * (`artifacts.TrashRetention`): the date every trash sentence names.
 */
export const ARTIFACT_TRASH_RETENTION_DAYS = 30;

/**
 * The visibility a new top-level folder may be given
 * (`artifactsdomain.FolderVisibilities`): everyone at the company, or only its
 * owner. A subfolder takes its top-level folder's. A folder grants nothing on
 * what it holds.
 */
export const ARTIFACT_FOLDER_VISIBILITIES = ["company", "private"] as const;

/** The longest folder name, in UTF-8 bytes (`artifacts.MaxFolderNameBytes`). */
export const ARTIFACT_FOLDER_NAME_MAX_BYTES = 80;

/** How deep a folder may sit, a top-level folder at depth one (`artifacts.MaxFolderDepth`). */
export const ARTIFACT_FOLDER_MAX_DEPTH = 4;

/** The most artifacts one move files (`artifacts.MaxMovesPerCall`): the bulk Move dialog's batch. */
export const ARTIFACT_MOVE_MAX_ITEMS = 25;

/** The one delimiter a listing takes (`artifactsdomain.ListDelimiter`), as S3's ListObjectsV2 names it. */
export const ARTIFACT_LIST_DELIMITER = "/";

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
 * The agent tools whose answer is a version that landed (`create_artifact`
 * and `publish_artifact`, internal/agent/builtin), which a turn's transcript
 * links to the artifact they wrote. Held, with the answer's two fields, by
 * running the tools.
 */
export const ARTIFACT_PUBLISH_TOOLS = ["create_artifact", "publish_artifact"] as const;

/** The field of a publishing tool's answer that names the artifact it wrote. */
export const ARTIFACT_ANSWER_ID = "artifact";

/** The field of a publishing tool's answer that numbers the version that landed. */
export const ARTIFACT_ANSWER_VERSION = "version";

/**
 * The page runtime contract this build serves, `major.minor.patch`: the
 * directory the runtime is built into (`static/artifacts/runtime/<contract>/`)
 * and what the bridge tells a page first. A runtime change a page could tell
 * apart is a new contract, and every older contract a published version pins
 * stays embedded. Held against the embedded tree by `internal/artifactupload`.
 */
export const RUNTIME_CONTRACT = "1.0.0";

/*
 * THE SLIDE SUBSET, as the renderer allows it (`lib/slides.ts`). The engine
 * refuses at publish what is outside it (`internal/artifacts/slideformat`);
 * the renderer holds the same lists again so a tree the engine would not have
 * produced is still never drawn as anything but text. Each is held equal to
 * the engine's by `internal/api/artifactsapi.TestTheRendererDrawsTheSlideSubset`.
 */

/** Every HTML element a slide may hold (`slideformat.Elements`), sorted. */
export const SLIDE_ELEMENTS = [
  "a",
  "aside",
  "b",
  "br",
  "div",
  "h1",
  "h2",
  "h3",
  "hr",
  "i",
  "img",
  "li",
  "ol",
  "p",
  "section",
  "span",
  "svg",
  "table",
  "td",
  "th",
  "tr",
  "u",
  "ul",
  "x-icon",
  "x-shape",
] as const;

/** Every inline style property a slide may declare (`slideformat.StyleProperties`), sorted. */
export const SLIDE_STYLE_PROPERTIES = [
  "-webkit-background-clip",
  "-webkit-text-fill-color",
  "-webkit-text-stroke",
  "align-items",
  "align-self",
  "aspect-ratio",
  "backdrop-filter",
  "background",
  "background-clip",
  "background-color",
  "border",
  "border-bottom",
  "border-left",
  "border-radius",
  "border-right",
  "border-top",
  "bottom",
  "box-shadow",
  "box-sizing",
  "color",
  "display",
  "filter",
  "flex",
  "flex-basis",
  "flex-direction",
  "flex-grow",
  "flex-shrink",
  "flex-wrap",
  "font",
  "font-family",
  "font-size",
  "font-style",
  "font-variant-numeric",
  "font-weight",
  "gap",
  "grid-column",
  "grid-row",
  "grid-template",
  "grid-template-columns",
  "grid-template-rows",
  "height",
  "justify-content",
  "justify-items",
  "justify-self",
  "left",
  "letter-spacing",
  "line-height",
  "margin",
  "max-height",
  "max-width",
  "min-height",
  "min-width",
  "mix-blend-mode",
  "object-fit",
  "opacity",
  "overflow",
  "padding",
  "position",
  "right",
  "text-align",
  "text-decoration",
  "text-shadow",
  "text-transform",
  "top",
  "transform",
  "white-space",
  "width",
] as const;

/** Every element an inline drawing on a slide may hold (`slideformat.SVGElements`), as SVG spells them. */
export const SLIDE_SVG_ELEMENTS = [
  "circle",
  "clipPath",
  "defs",
  "desc",
  "ellipse",
  "feBlend",
  "feColorMatrix",
  "feComposite",
  "feDropShadow",
  "feFlood",
  "feGaussianBlur",
  "feMerge",
  "feMergeNode",
  "feOffset",
  "filter",
  "g",
  "line",
  "linearGradient",
  "marker",
  "mask",
  "path",
  "pattern",
  "polygon",
  "polyline",
  "radialGradient",
  "rect",
  "stop",
  "svg",
  "symbol",
  "text",
  "title",
  "tspan",
  "use",
] as const;

/** Every attribute an inline drawing on a slide may carry (`slideformat.SVGAttributes`), as SVG spells them. */
export const SLIDE_SVG_ATTRIBUTES = [
  "aria-hidden",
  "aria-label",
  "clip-path",
  "clip-rule",
  "clipPathUnits",
  "color",
  "cx",
  "cy",
  "d",
  "display",
  "dominant-baseline",
  "dx",
  "dy",
  "fill",
  "fill-opacity",
  "fill-rule",
  "filter",
  "filterUnits",
  "flood-color",
  "flood-opacity",
  "font-family",
  "font-size",
  "font-style",
  "font-weight",
  "fx",
  "fy",
  "gradientTransform",
  "gradientUnits",
  "height",
  "href",
  "id",
  "in",
  "in2",
  "k1",
  "k2",
  "k3",
  "k4",
  "lengthAdjust",
  "letter-spacing",
  "marker-end",
  "marker-mid",
  "marker-start",
  "markerHeight",
  "markerUnits",
  "markerWidth",
  "mask",
  "maskContentUnits",
  "maskUnits",
  "mode",
  "offset",
  "opacity",
  "operator",
  "orient",
  "paint-order",
  "pathLength",
  "patternContentUnits",
  "patternTransform",
  "patternUnits",
  "points",
  "preserveAspectRatio",
  "primitiveUnits",
  "r",
  "refX",
  "refY",
  "result",
  "role",
  "rotate",
  "rx",
  "ry",
  "shape-rendering",
  "spreadMethod",
  "stdDeviation",
  "stop-color",
  "stop-opacity",
  "stroke",
  "stroke-dasharray",
  "stroke-dashoffset",
  "stroke-linecap",
  "stroke-linejoin",
  "stroke-miterlimit",
  "stroke-opacity",
  "stroke-width",
  "text-anchor",
  "textLength",
  "transform",
  "type",
  "values",
  "vector-effect",
  "viewBox",
  "visibility",
  "width",
  "x",
  "x1",
  "x2",
  "y",
  "y1",
  "y2",
] as const;

/** How a slide may leave the stage in Present (`slideformat.Transitions`). */
export const SLIDE_TRANSITIONS = ["fade", "push"] as const;

/** The shapes an `<x-shape>` draws (`slideformat.ShapeKinds`). */
export const SLIDE_SHAPES = [
  "rect",
  "rounded",
  "ellipse",
  "diamond",
  "arrow-right",
  "arrow-left",
  "arrow-up",
  "arrow-down",
  "line",
] as const;

/** The names an `<x-icon>` draws (`slideformat.Icons`), each mapped to a glyph by the renderer. */
export const SLIDE_ICONS = [
  "Activity",
  "Book",
  "Chart",
  "Chat",
  "Check",
  "CheckCircle",
  "Clock",
  "Code",
  "Database",
  "Globe",
  "Home",
  "Key",
  "Lightning",
  "Link",
  "Lock",
  "PaperPlane",
  "Search",
  "Settings",
  "Star",
  "Tool",
  "Trust",
  "Users",
  "Warning",
  "Wrench",
] as const;

/** A slide's stage width, in CSS pixels (`slideformat.StageWidth`). */
export const SLIDE_STAGE_WIDTH = 1920;

/** A slide's stage height, in CSS pixels (`slideformat.StageHeight`). */
export const SLIDE_STAGE_HEIGHT = 1080;

/** How much of an `x-icon`'s box its glyph fills, in percent (`slideformat.IconGlyphPercent`). */
export const SLIDE_ICON_GLYPH_PERCENT = 50;

/**
 * A dashboard's closed sets and bounds (`internal/artifacts/dashspec`): what a
 * widget draws, where a dataset's rows come from, how a dataset is kept
 * current, what a stat folds and how a number is written, what a column holds,
 * the grid a board lays out on and the shortest interval an engine dataset may
 * refresh at.
 */

/** Every widget a board holds (`dashspec.WidgetTypes`). */
export const DASHBOARD_WIDGET_TYPES = [
  "stat",
  "meter",
  "timeseries",
  "columns",
  "activity",
  "bars",
  "share",
  "table",
  "note",
  "group",
] as const;

/** Where a dataset's rows come from (`dashspec.SourceKinds`). */
export const DASHBOARD_SOURCE_KINDS = ["store", "engine", "file", "calc"] as const;

/** How a dataset is kept current (`dashspec.RefreshMode`). */
export const DASHBOARD_REFRESH_MODES = [
  "live",
  "signal",
  "interval",
  "manual",
  "fixed",
  "derived",
] as const;

/** What a dataset read answers (`datasetsapi.Status*`). */
export const DASHBOARD_DATASET_STATUSES = ["ready", "refused", "unanswered"] as const;

/** The folds a stat or a meter takes (`dashspec.Aggs`). */
export const DASHBOARD_AGGS = ["first", "last", "sum", "avg", "min", "max", "count"] as const;

/** How a widget writes a number (`dashspec.Formats`). */
export const DASHBOARD_NUMBER_FORMATS = ["number", "integer", "percent", "compact"] as const;

/** What a dataset's column holds (`dashspec.ColumnTypes`). */
export const DASHBOARD_COLUMN_TYPES = ["text", "number", "date", "boolean"] as const;

/**
 * The vocabularies a text column's ids are drawn in (`dashspec.WordSets`): a
 * tracker status, status group or priority, or an agent's activity, shown in
 * the words the domain's own screens use.
 */
export const DASHBOARD_COLUMN_WORDS = [
  "tracker_status",
  "tracker_status_group",
  "tracker_priority",
  "agent_activity",
] as const;

/** The grid every board lays out on (`artifacts.DashboardColumns`). */
export const DASHBOARD_COLUMNS = 12;

/** The shortest interval an engine dataset refreshes at, in seconds (`artifacts.MinRefreshSeconds`). */
export const DASHBOARD_MIN_REFRESH_SECONDS = 60;

/** The most widgets one board holds, a group's children counted (`artifacts.MaxDashboardWidgets`). */
export const DASHBOARD_MAX_WIDGETS = 64;

/** The most datasets one board declares (`artifacts.MaxDashboardDatasets`). */
export const DASHBOARD_MAX_DATASETS = 32;

/** The most columns one table widget draws (`artifacts.MaxTableColumns`). */
export const DASHBOARD_MAX_TABLE_COLUMNS = 16;
