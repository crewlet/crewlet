/**
 * The words an artifact's closed sets are read as: what each kind, role,
 * capability and history row says to a person.
 *
 * RECORDS OVER THE CONTRACT'S UNIONS, so a kind, a role or a capability the
 * engine grew (and the contract gained, under its gate) is a compile error
 * here until somebody writes its words, rather than a raw value on screen.
 */

import {
  ARTIFACT_CAPABILITIES,
  ARTIFACT_HISTORY_OPS,
  ARTIFACT_KINDS,
  ARTIFACT_ROLES,
  ARTIFACT_TRASH_RETENTION_DAYS,
  SHAREABLE_ROLES,
} from "~/contract/artifacts.ts";
import type {
  ArtifactActor,
  ArtifactAudience,
  ArtifactCapability,
  ArtifactHistoryEntry,
  ArtifactHistoryOp,
  ArtifactKind,
  ArtifactRole,
  ColumnType,
  DataFieldChange,
  DataHistoryEntry,
  DataVerdict,
  ShareableRole,
} from "~/protocol/index.ts";
import { fmtDate, parseUTC, plural } from "../format.ts";

/**
 * What each column type is called, in the schema editor and in a new sheet's
 * column settings: a select is a "Choice" and a URL a "Link", the words a
 * person filling the sheet in uses.
 */
export const COLUMN_TYPE_WORDS: Readonly<Record<ColumnType, string>> = {
  text: "Text",
  longtext: "Long text",
  number: "Number",
  checkbox: "Checkbox",
  select: "Choice",
  multiselect: "Choices",
  status: "Status",
  person: "Person",
  date: "Date",
  url: "Link",
  rating: "Rating",
};

/**
 * What each kind is called, in the eyebrow, the gallery's Type column and its
 * filter. An `html` artifact is "HTML", never "Page": a page is Doclet's
 * word, and "Page" beside Doclet's Pages named two things one way.
 */
export const KIND_WORDS: Readonly<Record<ArtifactKind, string>> = {
  html: "HTML",
  doc: "Doc",
  slides: "Slides",
  dashboard: "Dashboard",
  sheet: "Sheet",
  design: "Design",
  brand: "Brand",
};

/**
 * The order the kinds are offered to make in, by the gallery's Make row and
 * its New menu: what people write first, then what they lay out, then data.
 *
 * NOT EVERY KIND: a brand kit is made and adopted on the company's Brand page
 * (`#/company/brand`), the one place the company decides how it looks, so it
 * has no create page under Artifacts and is never offered beside the kinds
 * people make for work. Its kits still list in the gallery under their own
 * kind.
 */
export const MAKE_ORDER = [
  "doc",
  "slides",
  "design",
  "dashboard",
  "html",
  "sheet",
] as const satisfies readonly ArtifactKind[];

/** A kind a person makes from the gallery: one with a create page under `#/artifacts/new/`. */
export type MakeKind = (typeof MAKE_ORDER)[number];

/** Whether a value names a kind with a create page under Artifacts. */
export function isMakeKind(value: string): value is MakeKind {
  return (MAKE_ORDER as readonly string[]).includes(value);
}

/**
 * What each kind's create page is called, in its bar and the trail
 * ("Artifacts › New doc"). Slides are made as a deck, and an HTML artifact is
 * a page, so each is said as what a person starts.
 */
export const NEW_KIND_TITLES: Readonly<Record<MakeKind, string>> = {
  html: "New HTML page",
  doc: "New doc",
  slides: "New slide deck",
  dashboard: "New dashboard",
  sheet: "New sheet",
  design: "New design",
};

/**
 * The article each kind's word takes in a sentence, by how it is said rather
 * than how it is spelled: "an HTML artifact", "a Doc artifact".
 */
export const KIND_ARTICLES: Readonly<Record<ArtifactKind, "a" | "an">> = {
  html: "an",
  doc: "a",
  slides: "a",
  dashboard: "a",
  sheet: "a",
  design: "a",
  brand: "a",
};

/** Every kind, in the engine's order, as the gallery's filter offers them. */
export const KINDS: readonly ArtifactKind[] = ARTIFACT_KINDS;

/** What each kind is called where a sentence names several of it: "docs, slides and sheets". */
export const KIND_PLURAL_WORDS: Readonly<Record<ArtifactKind, string>> = {
  html: "HTML pages",
  doc: "docs",
  slides: "slides",
  dashboard: "dashboards",
  sheet: "sheets",
  design: "designs",
  brand: "brand kits",
};

/**
 * Every kind in one phrase, for a sentence that says what Artifacts holds:
 * "HTML pages, docs, slides, dashboards, sheets and designs". Derived from the
 * kind table, so a kind the engine grows is named wherever the workspace
 * describes itself, rather than each description listing the three it knew.
 */
export const KIND_LIST = wordList(KINDS.map((kind) => KIND_PLURAL_WORDS[kind]));

/** Whether a value off the wire is a kind this build draws. */
export function isArtifactKind(value: string): value is ArtifactKind {
  return (ARTIFACT_KINDS as readonly string[]).includes(value);
}

/**
 * What each role lets its holder do, in the share dialog's words. A
 * contributor fills in what a sheet's rules admit without changing the page
 * itself: the role a review sheet's reviewers hold.
 */
export const ROLE_WORDS: Readonly<Record<ArtifactRole, string>> = {
  viewer: "Can view",
  commenter: "Can comment",
  contributor: "Can fill in",
  editor: "Can edit",
  owner: "Owner",
};

/** The ladder, least first. */
export const ROLES: readonly ArtifactRole[] = ARTIFACT_ROLES;

/** The roles a share grants. */
export const GRANTABLE: readonly ShareableRole[] = SHAREABLE_ROLES;

/** Whether `role` includes `floor` on the ladder: each role holds every role before it. */
export function roleIncludes(role: ArtifactRole | "" | undefined, floor: ArtifactRole): boolean {
  if (!role) return false;
  return ARTIFACT_ROLES.indexOf(role) >= ARTIFACT_ROLES.indexOf(floor);
}

/** Whether a value off the wire is a role. */
export function isArtifactRole(value: string): value is ArtifactRole {
  return (ARTIFACT_ROLES as readonly string[]).includes(value);
}

/** What each capability a version declares lets its page do, in words, for Settings and the Info rail. */
export const CAPABILITY_WORDS: Readonly<Record<ArtifactCapability, string>> = {
  assets: "Uses files from the artifact's asset store",
  db: "Reads and writes the artifact's shared data",
  state: "Keeps each viewer's own state, private to them",
  identity: "Knows who is viewing",
  presence: "Sees who else is here and shares where you are",
};

/** Every capability, in the engine's order. */
export const CAPABILITIES: readonly ArtifactCapability[] = ARTIFACT_CAPABILITIES;

/**
 * Why the datastore did not take a write, in a reader's words: the sentence
 * after "Not saved:". The applier's own `reason` names the rule it applied for
 * the agent tools and the logs (op indices, `if_version`, field keys), which a
 * person reading a sheet can act on none of, so no screen draws it.
 */
export const DATA_REFUSAL_WORDS: Readonly<Record<Exclude<DataVerdict, "applied">, string>> = {
  conflict: "it changed since it was read.",
  invalid: "the value does not fit this column.",
  forbidden: "you cannot change this.",
  quota: "the sheet has reached its storage limit.",
  gone: "the data was deleted.",
};

/** One refused write said whole: "Not saved: it changed since it was read." */
export function dataRefusalText(verdict: Exclude<DataVerdict, "applied">): string {
  return `Not saved: ${DATA_REFUSAL_WORDS[verdict]}`;
}

/** A refusal's reason as a sentence of its own, beside a "Not saved" tag. */
export function dataRefusalReason(verdict: Exclude<DataVerdict, "applied">): string {
  const words = DATA_REFUSAL_WORDS[verdict];
  return words.charAt(0).toUpperCase() + words.slice(1);
}

/** Words in a sentence: "a", "a and b", "a, b and c". */
function wordList(words: readonly string[]): string {
  if (words.length < 2) return words.join("");
  return `${words.slice(0, -1).join(", ")} and ${words.at(-1)}`;
}

/** Names in a sentence: "Notes", "Notes and Gate", "3 columns". */
function fieldList(labels: readonly string[]): string {
  if (labels.length === 0) return "";
  if (labels.length === 1) return labels[0]!;
  if (labels.length === 2) return `${labels[0]} and ${labels[1]}`;
  return plural(labels.length, "column");
}

/**
 * One datastore write as a sentence after its actor's name: what it changed,
 * by the columns' own labels, or what it tried to change when the store
 * refused it. The refusal's reason is said beside it (`dataRefusalReason`),
 * never the applier's own detail.
 */
export function dataChangeWords(
  entry: DataHistoryEntry,
  labelOf: (collection: string, field: string) => string,
): string {
  if (entry.op === "configure") {
    return entry.verdict === "applied" ? "changed the schema" : "tried to change the schema";
  }
  if (entry.op === "purge") return "deleted the data";
  const labels = [
    ...new Set(entry.changes.flatMap((c) => (c.f ?? []).map((f) => labelOf(c.c, f.f)))),
  ];
  const rows = new Set(entry.changes.filter((c) => c.d).map((c) => `${c.c}/${c.d}`)).size;
  const fields = fieldList(labels);
  if (entry.verdict !== "applied") {
    if (!fields) return "tried to make a change";
    return `tried to change ${fields}${rows > 0 ? ` in ${plural(rows, "row")}` : ""}`;
  }
  const removed = entry.changes.filter((c) => c.op === "delete").length;
  if (removed > 0 && removed === entry.changes.length) return `deleted ${plural(removed, "row")}`;
  const verb = entry.op === "undo" ? "undid a change to" : "changed";
  return `${verb} ${fields || "nothing"}${rows > 0 ? ` in ${plural(rows, "row")}` : ""}`;
}

/** The most of a value a history line quotes before it is cut. */
const QUOTED_MAX = 80;

/** A stored value as a history line quotes it, or null for an empty one. */
function quoted(value: unknown): string | null {
  if (value === undefined || value === null || value === "") return null;
  if (Array.isArray(value) && value.length === 0) return null;
  const text = Array.isArray(value)
    ? value.map((v) => (typeof v === "string" ? v : JSON.stringify(v))).join(", ")
    : typeof value === "string"
      ? value
      : JSON.stringify(value);
  const cut = text.length > QUOTED_MAX ? `${text.slice(0, QUOTED_MAX - 1).trimEnd()}…` : text;
  return `“${cut}”`;
}

/**
 * One cell's change as its history says it: "Set to “Ready”", "Changed from
 * “a” to “b”", "Cleared “a”". A long value the history keeps only a digest
 * of is said to have changed, without either side.
 */
export function cellChangeWords(change: DataFieldChange): string {
  if (change.bh || change.ah) return "Changed the text";
  const before = quoted(change.b);
  const after = quoted(change.a);
  if (before === null && after === null) return "Left empty";
  if (before === null) return `Set to ${after}`;
  if (after === null) return `Cleared ${before}`;
  return `Changed from ${before} to ${after}`;
}

/** An actor's name as a row shows it: the seat handle or login, or "the engine". */
export function actorName(
  actor: ArtifactActor | undefined,
  nameOf: (handle: string) => string,
): string {
  if (!actor || !actor.name) return "the engine";
  if (actor.kind === "system") return "the engine";
  return nameOf(actor.name);
}

/** A share's audience in words: a seat's name, a team's name, the company's. */
export function audienceWords(
  audience: ArtifactAudience,
  names: { seat: (handle: string) => string; unit: (key: string) => string; company: string },
): string {
  switch (audience.kind) {
    case "seat":
      return names.seat(audience.id ?? "");
    case "unit":
      return names.unit(audience.id ?? "");
    case "company":
      return `Everyone in ${names.company}`;
  }
}

/** A share's audience as the engine spells it in a path (`Audience.String()`). */
export function audienceKey(audience: ArtifactAudience): string {
  return audience.kind === "company" ? "company" : `${audience.kind}:${audience.id ?? ""}`;
}

/**
 * Why a stored share reaches nobody, as its row in the share dialog says it:
 * the engine marks it `invalid` (a sign-in named as a seat, which a build
 * before the grant rule wrote), and its owner is told what it is and what to
 * do instead. The sign-in is named as written, since that is what was stored.
 */
export function invalidShareWords(audience: ArtifactAudience): string {
  return `${audience.id ?? ""} is a sign-in, not a seat. Remove it and share with their seat.`;
}

/**
 * One history row as a sentence after its actor's name. A refused row says it
 * was refused, so the record reads as what was tried and what came of it, and
 * a change an administrator made with no share of their own says so once: an
 * administrator's read already names it in its own words.
 * `audience` reads a share's audience as the engine spells it (`seat:pm`,
 * `unit:ops`, `company`) and `seat` a seat handle, each as the names the
 * reader knows them by.
 */
export function historyWords(
  entry: ArtifactHistoryEntry,
  names: { audience: (raw: string) => string; seat: (handle: string) => string },
): string {
  const s = entry.summary;
  const number = entry.number ?? 0;
  const said = ((): string => {
    switch (entry.op) {
      case "create":
        return entry.summary.files === 0 || entry.summary.files === undefined
          ? "created it"
          : `created it with ${plural(entry.summary.files, "file")}`;
      case "publish":
        return `${s.forced ? "forced" : "published"} version ${number}${s.label ? ` “${s.label}”` : ""}`;
      case "restore_version":
        return `restored version ${s.restored_from ?? "?"} as version ${number}`;
      case "patch":
        if (s.label !== undefined && number > 0) {
          return s.label
            ? `labelled version ${number} “${s.label}”`
            : `cleared version ${number}'s label`;
        }
        return s.fields && s.fields.length > 0
          ? `changed its ${s.fields.join(", ")}`
          : "changed it";
      case "share": {
        if (!s.audience) return "changed who it is shared with";
        const role =
          s.role && isArtifactRole(s.role) ? `: ${ROLE_WORDS[s.role].toLowerCase()}` : "";
        return `shared it with ${names.audience(s.audience)}${role}`;
      }
      case "unshare":
        return s.audience
          ? `stopped sharing it with ${names.audience(s.audience)}`
          : "changed who it is shared with";
      case "transfer":
        return `gave it to ${s.to ? names.seat(s.to) : "another seat"}`;
      case "trash":
        return "moved it to the trash";
      case "restore":
        return "restored it from the trash";
      case "purge_begin":
        return "began deleting it for good";
      case "asset_create":
        return `added the asset ${s.name ?? s.asset ?? ""}`.trim();
      case "asset_remove":
        return `removed the asset ${s.name ?? s.asset ?? ""}`.trim();
      case "link_work_item":
        return entry.work_item ? `linked it to ${entry.work_item}` : "linked it to a work item";
      case "read_elevated":
        return `opened version ${number || "?"} as an administrator`;
      case "move":
        // THE ROW NAMES FOLDERS BY ID ALONE, so a reader is never told the
        // name of a folder they cannot see: the sentence says where it went,
        // never which folder.
        if (!s.to_folder) return "moved it to the top level";
        return s.from_folder ? "moved it to another folder" : "filed it in a folder";
      default:
        return entry.op.replaceAll("_", " ");
    }
  })();
  const tried = entry.outcome === "refused" ? `tried to ${stripPast(said)} and was refused` : said;
  return entry.elevated && entry.op !== "read_elevated" ? `${tried} (as an administrator)` : tried;
}

/**
 * A history row's op as one short verb, for the company audit's What column,
 * where every source's rows share one narrow track and the row's sentence is
 * its Detail. Keyed by every op a history row is filed under
 * (`ARTIFACT_HISTORY_OPS`, held to `artifactsdomain.HistoryOps`), so an op the
 * engine grew is a compile error here until it has its word; an op a newer
 * engine sends that this build has not met is read with its underscores as
 * spaces.
 */
const HISTORY_OP_TAGS: Readonly<Record<ArtifactHistoryOp, string>> = {
  create: "create",
  publish: "publish",
  restore_version: "restore version",
  patch: "edit",
  share: "share",
  unshare: "unshare",
  transfer: "transfer",
  trash: "trash",
  restore: "untrash",
  purge_begin: "begin purge",
  asset_create: "add asset",
  asset_remove: "remove asset",
  link_work_item: "link work item",
  read_elevated: "admin read",
  move: "move",
};

/** A history op as the audit's What column says it: "admin read", "restore version". */
export function historyOpTag(op: string): string {
  return isHistoryOp(op) ? HISTORY_OP_TAGS[op] : op.replaceAll("_", " ");
}

/** Whether a history row's op is one this build words. */
function isHistoryOp(op: string): op is ArtifactHistoryOp {
  return (ARTIFACT_HISTORY_OPS as readonly string[]).includes(op);
}

/**
 * Why the artifact log refused a record, as the sentence after "Refused:".
 * Keyed by the engine's reasons (`artifactsdomain.Refusal`); a reason this
 * build has not met is read with its underscores as spaces.
 */
const HISTORY_REFUSAL_WORDS: Readonly<Record<string, string>> = {
  invalid: "the request was malformed",
  not_found: "the artifact or asset does not exist",
  exists: "it already exists",
  purged: "it was deleted for good",
  trashed: "it is in the trash",
  live: "it is not in the trash",
  purging: "it is being deleted for good",
  head_moved: "a newer version was published first",
  carried_object_gone: "a file it kept from the last version is gone",
  object_named_elsewhere: "a file belongs to another artifact",
  quota_artifacts: "the company has reached its artifact limit",
  quota_bytes: "the company has reached its storage limit",
  label_taken: "another version holds that label",
  version_not_found: "the version does not exist",
  source_taken: "another artifact holds that source",
  folder_gone: "the folder was removed first",
};

/** A refused history row's reason in words: "a newer version was published first". */
export function historyRefusalWords(reason: string): string {
  return HISTORY_REFUSAL_WORDS[reason] ?? reason.replaceAll("_", " ");
}

/** A sentence's verb in the infinitive, for "tried to …": a crude but closed table. */
function stripPast(said: string): string {
  const table: [string, string][] = [
    ["created", "create"],
    ["published", "publish"],
    ["forced", "force"],
    ["restored", "restore"],
    ["labelled", "label"],
    ["cleared", "clear"],
    ["changed", "change"],
    ["shared", "share"],
    ["stopped", "stop"],
    ["gave", "give"],
    ["moved", "move"],
    ["filed", "file"],
    ["began", "begin"],
    ["added", "add"],
    ["removed", "remove"],
    ["linked", "link"],
    ["opened", "open"],
  ];
  for (const [past, base] of table) {
    if (said.startsWith(`${past} `) || said === past) return base + said.slice(past.length);
  }
  return said;
}

/** How long the trash keeps an artifact, in words: "30 days". */
export const TRASH_KEPT = plural(ARTIFACT_TRASH_RETENTION_DAYS, "day");

/** When a trashed artifact is deleted for good, as an instant, or null where the trash time is unknown. */
export function purgeAt(trashedAt: string | null | undefined): string | null {
  const at = parseUTC(trashedAt);
  if (!at || at.getUTCFullYear() < 2000) return null;
  return new Date(at.getTime() + ARTIFACT_TRASH_RETENTION_DAYS * 86_400_000).toISOString();
}

/** "Deleted for good on 17 November 2026 unless restored.", or the retention where the date is unknown. */
export function purgeSentence(trashedAt: string | null | undefined): string {
  const at = purgeAt(trashedAt);
  return at
    ? `It is deleted for good on ${fmtDate(at)} unless its owner restores it.`
    : `It is deleted for good ${TRASH_KEPT} after it was trashed unless its owner restores it.`;
}
