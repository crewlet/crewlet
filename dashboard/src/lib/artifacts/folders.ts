/**
 * The artifact folders as this reader sees them: the tree, one signal that a
 * folder write landed, and the readings every folder surface draws from (a
 * folder's trail and path, the destinations a move offers, the words for who
 * sees a folder, a name the engine will take).
 *
 * # A folder is a place, never a permission
 *
 * An artifact sits in one folder or at the top level, and a folder grants
 * nothing on what it holds (ADR "An artifact lives in one folder, and a folder
 * grants nothing"). So nothing here filters an artifact by its folder: the
 * engine decides every artifact on its own shares, and the tree is only the
 * folders the reader may see, each with the counts of what directly inside it
 * they can open.
 *
 * # Addressed by id, listed by path
 *
 * The dashboard addresses a folder by its id (`#/artifacts/folders/<id>`), so
 * a link survives a rename, and asks the listing by the folder's path as S3's
 * ListObjectsV2 names it (`prefix=Marketing/Q4/`), which [FolderIndex.prefix]
 * builds from the tree. Every ancestor of a folder the reader sees is one they
 * see too (a subfolder is decided by its top-level folder), so a trail drawn
 * from the tree never stops short.
 *
 * # One signal for every reader of the tree
 *
 * The tree is drawn in several places at once (a folder page's tiles and
 * header, the Move dialog, an artifact's Details, the Save in chip). A write
 * in one of them tells every reader of [useFoldersChanged] to ask again, as a
 * pin does (`pins.ts`), so a rename is never shown in one place and not the
 * other until a poll comes round.
 */

import { useEffect, useMemo, useRef } from "react";
import { ARTIFACT_FOLDER_NAME_MAX_BYTES } from "~/contract/artifacts.ts";
import type { ArtifactFolderItem, ArtifactFolderVisibility } from "~/protocol/index.ts";
import { utf8Bytes } from "../format.ts";
import { collator } from "../formatters.ts";
import { useQuery } from "../useQuery.ts";

/**
 * How often the tree is read again with nothing pushed: as often as the
 * gallery reads its listing, so a tab left open is current within a minute
 * and a folder page's tiles agree with its listing.
 */
export const FOLDER_TREE_POLL_MS = 60_000;

/** How a folder's path is written for a person: the names, joined by this. */
export const PATH_SEPARATOR = " › ";

/** How a folder's path is written for the listing (`prefix`): names joined by `/`, ending in one. */
const PREFIX_SEPARATOR = "/";

const listeners = new Set<() => void>();

/** Tells every reader of the tree that a folder or a filing changed. */
export function announceFoldersChanged(): void {
  for (const listener of [...listeners]) listener();
}

/** Calls `refetch` whenever a folder write or a move lands anywhere in this tab. */
export function useFoldersChanged(refetch: () => void): void {
  const latest = useRef(refetch);
  latest.current = refetch;
  useEffect(() => {
    const listener = () => latest.current();
    listeners.add(listener);
    return () => void listeners.delete(listener);
  }, []);
}

/** The reader's folder tree, indexed, and kept current by every folder write in this tab. */
export function useFolderTree(enabled = true) {
  const tree = useQuery("artifact_folders", {}, { enabled, pollMs: FOLDER_TREE_POLL_MS });
  useFoldersChanged(tree.refetch);
  const index = useMemo(() => indexFolders(tree.data?.folders ?? []), [tree.data]);
  return { ...tree, index };
}

/** The reader's tree, with the questions every surface asks of it. */
export interface FolderIndex {
  /** Every folder the reader sees, parents before children, siblings A to Z. */
  readonly folders: readonly ArtifactFolderItem[];
  /** A folder by its id, or undefined for one the reader cannot see. */
  get(id: string): ArtifactFolderItem | undefined;
  /** The folders directly in `parent`, A to Z; `""` is the top level. */
  children(parent: string): readonly ArtifactFolderItem[];
  /** The folder and every folder above it, outermost first; empty for one the reader cannot see. */
  trail(id: string): readonly ArtifactFolderItem[];
  /** The folder's path as the listing's `prefix` (`Marketing/Q4/`); `""` for the top level. */
  prefix(id: string): string;
  /** The folder's path in words (`Marketing › Q4`); `""` for one the reader cannot see. */
  label(id: string): string;
  /** The folder and every folder beneath it, by id. */
  subtree(id: string): ReadonlySet<string>;
  /** How many levels the folder spans, itself included: 1 for a folder with no subfolders. */
  height(id: string): number;
}

/** Index a tree answer. Pure, so a test and every surface read one implementation. */
export function indexFolders(folders: readonly ArtifactFolderItem[]): FolderIndex {
  const byId = new Map<string, ArtifactFolderItem>();
  const kids = new Map<string, ArtifactFolderItem[]>();
  for (const folder of folders) byId.set(folder.id, folder);
  for (const folder of folders) {
    // A FOLDER WHOSE PARENT THE READER CANNOT SEE is drawn nowhere rather than
    // at the top level: the engine never answers one (every ancestor of a
    // visible folder is visible), and floating it to the top would put it
    // where nobody filed it.
    const parent = folder.parent ?? "";
    if (parent !== "" && !byId.has(parent)) continue;
    const list = kids.get(parent);
    if (list) list.push(folder);
    else kids.set(parent, [folder]);
  }
  // ONE COLLATOR FOR EVERY SORT OF THE INDEX, taken before them: asked for in
  // the comparator, the cache derived its key again on every comparison of
  // every sort, which was most of what a sort cost.
  const order = collator(undefined, NAME_ORDER);
  const byName = (a: ArtifactFolderItem, b: ArtifactFolderItem) => order.compare(a.name, b.name);
  for (const list of kids.values()) list.sort(byName);

  const trail = (id: string): ArtifactFolderItem[] => {
    const out: ArtifactFolderItem[] = [];
    const seen = new Set<string>();
    let at = byId.get(id);
    // BOUNDED BY WHAT WAS SEEN, so a tree answered mid-move with a loop in it
    // ends rather than spins.
    while (at && !seen.has(at.id)) {
      seen.add(at.id);
      out.unshift(at);
      at = at.parent ? byId.get(at.parent) : undefined;
    }
    return out;
  };

  const subtree = (id: string): Set<string> => {
    const out = new Set<string>();
    const walk = (at: string) => {
      if (out.has(at)) return;
      out.add(at);
      for (const child of kids.get(at) ?? []) walk(child.id);
    };
    if (byId.has(id)) walk(id);
    return out;
  };

  const height = (id: string): number => {
    const below = (at: string, seen: Set<string>): number => {
      if (seen.has(at)) return 0;
      seen.add(at);
      let most = 0;
      for (const child of kids.get(at) ?? []) most = Math.max(most, below(child.id, seen));
      return 1 + most;
    };
    return byId.has(id) ? below(id, new Set()) : 0;
  };

  return {
    folders,
    get: (id) => byId.get(id),
    children: (parent) => kids.get(parent) ?? [],
    trail,
    prefix: (id) => {
      const names = trail(id).map((f) => f.name);
      return names.length > 0 ? `${names.join(PREFIX_SEPARATOR)}${PREFIX_SEPARATOR}` : "";
    },
    label: (id) =>
      trail(id)
        .map((f) => f.name)
        .join(PATH_SEPARATOR),
    subtree,
    height,
  };
}

/**
 * How siblings are ordered — A to Z, as the engine orders them, by the
 * reader's own collation, case apart and numbers by value.
 */
const NAME_ORDER: Intl.CollatorOptions = Object.freeze({ sensitivity: "base", numeric: true });

/**
 * A listing's `folder_path` (`Marketing/Q4/`) in words (`Marketing › Q4`), or
 * `""` for the top level. A name never holds `/` (the engine refuses one), so
 * splitting on it is exact.
 */
export function pathWords(folderPath: string | undefined): string {
  if (!folderPath) return "";
  return folderPath.split(PREFIX_SEPARATOR).filter(Boolean).join(PATH_SEPARATOR);
}

/** A place a move can file into: a folder, or the top level (`id` `""`). */
export interface Destination {
  id: string;
  /** The folder's own name, or "Top level". */
  name: string;
  /** The folders above it in words (`Marketing › Q4`), `""` at the top level and for a top-level folder. */
  above: string;
  /** The whole path in words, which the reader searches by. */
  path: string;
  visibility: ArtifactFolderVisibility | null;
}

/** What the top level is called where a move offers it. */
export const TOP_LEVEL = "Top level";

/**
 * Every place a move may file into, the top level first and then the tree in
 * its own order (parents before children, siblings A to Z), so a list read
 * down reads as the tree.
 *
 * A FOLDER THE READER ONLY VIEWS is left out (filing needs a contributor),
 * and so, MOVING A FOLDER, is the folder itself and everything beneath it (a
 * folder cannot sit inside itself) and every folder it would push past the
 * depth stamped on the destination: the engine refuses each, and a list that
 * offered them would offer a refusal.
 */
export function destinations(index: FolderIndex, moving?: { folder: string }): Destination[] {
  const out: Destination[] = [
    { id: "", name: TOP_LEVEL, above: "", path: TOP_LEVEL, visibility: null },
  ];
  const own = moving ? index.subtree(moving.folder) : new Set<string>();
  const height = moving ? index.height(moving.folder) : 0;
  const walk = (parent: string) => {
    for (const folder of index.children(parent)) {
      if (own.has(folder.id)) continue;
      const fits = !moving || folder.depth + height <= folder.max_depth;
      if (fits && folder.role !== "viewer") {
        const trail = index.trail(folder.id);
        out.push({
          id: folder.id,
          name: folder.name,
          above: trail
            .slice(0, -1)
            .map((f) => f.name)
            .join(PATH_SEPARATOR),
          path: trail.map((f) => f.name).join(PATH_SEPARATOR),
          visibility: folder.visibility,
        });
      }
      walk(folder.id);
    }
  };
  walk("");
  return out;
}

/**
 * The destinations a search phrase matches, best first: a folder whose own
 * name starts with the phrase, then one whose name holds it, then one whose
 * path holds it; the tree's order within each. The top level stays first while
 * nothing is typed, and is found by its own words after.
 */
export function matchDestinations(all: readonly Destination[], phrase: string): Destination[] {
  const needle = phrase.trim().toLocaleLowerCase();
  if (!needle) return [...all];
  const scored: { d: Destination; score: number; at: number }[] = [];
  all.forEach((d, at) => {
    const name = d.name.toLocaleLowerCase();
    const path = d.path.toLocaleLowerCase();
    const score = name.startsWith(needle)
      ? 0
      : name.includes(needle)
        ? 1
        : path.includes(needle)
          ? 2
          : -1;
    if (score >= 0) scored.push({ d, score, at });
  });
  scored.sort((a, b) => a.score - b.score || a.at - b.at);
  return scored.map((s) => s.d);
}

/** The words for who sees a folder, said the way its tile, its chip and the New folder dialog say them. */
export interface VisibilityWords {
  /** The access glyph the kit draws for it (`ArtifactAccessGlyph`). */
  access: "company" | "private" | "shared";
  /** A tile's short word: "Everyone", "Only you". */
  short: string;
  /** A header chip's and a dialog's whole phrase: "Everyone at Acme". */
  long: string;
}

/**
 * Who sees a folder, in words. A private folder is "Only you" to its owner and
 * "Only <owner>" to anybody else who sees it (an owner's lead or an
 * administrator); a folder whose shares no preset describes is "Shared".
 */
export function visibilityWords(
  visibility: ArtifactFolderVisibility,
  who: { company: string; mine: boolean; ownerName: string },
): VisibilityWords {
  switch (visibility) {
    case "company":
      return {
        access: "company",
        short: "Everyone",
        long: who.company ? `Everyone at ${who.company}` : "Everyone in the company",
      };
    case "private": {
      const words = who.mine ? "Only you" : `Only ${who.ownerName}`;
      return { access: "private", short: words, long: words };
    }
    case "custom":
      return { access: "shared", short: "Shared", long: "Shared with some people" };
  }
}

/**
 * What is wrong with a folder name, in the sentence the field says under it,
 * or undefined when the engine will take it: the engine's own grammar
 * (`foldername.Check`), checked here so the reader hears it before the press.
 * The engine decides; a name this passes can still be refused, and that
 * refusal is said in the engine's words.
 *
 * An empty name is not wrong while it is being typed: the create action says
 * "Give it a name." instead ([folderNameHold]).
 */
export function folderNameProblem(raw: string): string | undefined {
  const name = raw.normalize("NFC").trim();
  if (name === "") return undefined;
  const bytes = utf8Bytes(name);
  if (bytes > ARTIFACT_FOLDER_NAME_MAX_BYTES) {
    return `The name is ${bytes} bytes and the most is ${ARTIFACT_FOLDER_NAME_MAX_BYTES}. Shorten it.`;
  }
  if (name.includes("/")) return "A folder name cannot hold “/”. Use another character.";
  if (name === "." || name === "..") return "A folder cannot be called “.” or “..”. Use a word.";
  // CONTROL, FORMAT AND SEPARATOR CHARACTERS: invisible marks (bidi
  // overrides, zero-width joiners) that would let two names look alike.
  if (/[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/u.test(name)) {
    return "The name holds an invisible or control character. Type it again without one.";
  }
  return undefined;
}

/** Why a folder create is held for its name, or undefined when it may go. */
export function folderNameHold(raw: string): string | undefined {
  return raw.trim() === "" ? "Give it a name." : folderNameProblem(raw);
}

/**
 * A sibling the reader can see that already carries this name, compared as
 * the engine compares (case and composition aside), or undefined. A hint said
 * before the press; the engine's own check is what decides, among siblings
 * the reader may not see too.
 */
export function takenBy(
  siblings: readonly ArtifactFolderItem[],
  raw: string,
  except?: string,
): ArtifactFolderItem | undefined {
  const key = nameKey(raw);
  return siblings.find((f) => f.id !== except && nameKey(f.name) === key);
}

function nameKey(name: string): string {
  return name.normalize("NFC").trim().toLocaleLowerCase("und").normalize("NFC");
}

/**
 * Where an artifact is kept, as this reader can say it: at the top level, in
 * a folder they see (its path), or not yet known while the tree is read. The
 * engine names a folder only to a reader who sees it, so an artifact in a
 * folder the reader cannot see arrives at their top level, as the listing
 * places it, and a folder the tree does not hold is one it has not caught up
 * with yet.
 */
export type ArtifactPlace =
  { at: "top" } | { at: "folder"; id: string; name: string; path: string } | { at: "reading" };

/** The place of an artifact filed in `folder` (absent or `""` for the top level), read off the reader's tree. */
export function artifactPlace(folder: string | undefined, index: FolderIndex): ArtifactPlace {
  if (!folder) return { at: "top" };
  const found = index.get(folder);
  if (found) return { at: "folder", id: found.id, name: found.name, path: index.label(found.id) };
  return { at: "reading" };
}
