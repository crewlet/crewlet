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
  ARTIFACT_KINDS,
  ARTIFACT_ROLES,
  SHAREABLE_ROLES,
} from "~/contract/artifacts.ts";
import type {
  ArtifactActor,
  ArtifactAudience,
  ArtifactCapability,
  ArtifactHistoryEntry,
  ArtifactKind,
  ArtifactRole,
  ShareableRole,
} from "~/protocol/index.ts";
import { plural } from "../format.ts";

/** What each kind is called, in the eyebrow, the gallery's Type column and its filter. */
export const KIND_WORDS: Readonly<Record<ArtifactKind, string>> = {
  html: "Page",
  markdown: "Document",
  sheet: "Sheet",
};

/** Every kind, in the engine's order, as the gallery's filter offers them. */
export const KINDS: readonly ArtifactKind[] = ARTIFACT_KINDS;

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

/** An actor's name as a row shows it: the seat handle or login, or "the engine". */
export function actorName(
  actor: ArtifactActor | undefined,
  nameOf: (handle: string) => string,
): string {
  if (!actor || !actor.Name) return "the engine";
  if (actor.Kind === "system") return "the engine";
  return nameOf(actor.Name);
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
 * One history row as a sentence after its actor's name. A refused row says it
 * was refused, so the record reads as what was tried and what came of it.
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
      case "prune":
        return s.pruned && s.pruned.length > 0
          ? `retired ${plural(s.pruned.length, "old version")}`
          : "retired old versions";
      case "trash":
        return "moved it to the trash";
      case "restore":
        return "restored it from the trash";
      case "purge":
        return "deleted it for good";
      case "remove":
        return `removed the asset ${s.name ?? s.asset ?? ""}`.trim();
      case "read_elevated":
        return `opened version ${number || "?"} as an administrator`;
      case "opened":
        return "opened it";
      case "pin":
        return "pinned it";
      case "unpin":
        return "unpinned it";
      default:
        return entry.op.replaceAll("_", " ");
    }
  })();
  return entry.outcome === "refused" ? `tried to ${stripPast(said)} and was refused` : said;
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
    ["retired", "retire"],
    ["moved", "move"],
    ["deleted", "delete"],
    ["removed", "remove"],
    ["opened", "open"],
    ["pinned", "pin"],
    ["unpinned", "unpin"],
  ];
  for (const [past, base] of table) {
    if (said.startsWith(`${past} `) || said === past) return base + said.slice(past.length);
  }
  return said;
}
