/**
 * What a person may change of a repository from the Code workspace, and why
 * not: its search mode, its mirrors, a purge of its unreachable history, and
 * an import of a new one from another host.
 *
 * # Every control is drawn, held with its reason
 *
 * A WriteButton is never hidden (`components/WriteButton.tsx`): whether this
 * browser may make the change at all is `useAct`'s (`lib/useWriteAccess.ts`),
 * and what holds THIS change on THIS repository is a sentence from here, so
 * the reader learns why from the control itself rather than from a refusal.
 * Each sentence restates a rule the engine holds, never a second one: the
 * engine decides every press again, and its refusal is drawn beside the
 * control when the two disagree.
 *
 * # A person's, never a seat's
 *
 * A mirror, an import and a purge are a PERSON'S (U49 a; the purge and the
 * import are human-only in the authority table): a mirror and an import reach
 * another host for the company, and a purge erases what its code held. A
 * credential nobody is bound through writes nothing to the code host
 * (`code.UnboundDetail`), so each is held for a reader who holds no human
 * seat.
 */

import type { CodeRepoDetail, CodeRepoSummary } from "~/protocol/index.ts";
import { isRepoName } from "./code.ts";
import { fmtDateTime, plural } from "./format.ts";

/** Why a reader holding no human seat changes nothing here. */
export const PERSON_ONLY =
  "Only a person, signed in as the human seat they hold, makes this change: a mirror, an import and a purge are never a seat's.";

/**
 * Why a credential bound to nobody changes no repository: the code host
 * names the person who made every write (`code.UnboundDetail`).
 */
export const UNBOUND =
  "Only a person, signed in as the human seat they hold, changes a repository: a credential bound to nobody writes nothing to the code host.";

/** Why a declared repository's settings are not changed here. */
export const DECLARED =
  "The company document declares this repository: its settings are changed in Settings › Configuration, which every activation settles.";

/**
 * Who is reading, as far as a hold asks: the kind of seat the directory binds
 * them to and its handle (`lib/viewer.ts`'s ViewerState, or the wire's
 * Viewer), or null before anybody has answered.
 */
export type Reader = { readonly kind: string; readonly handle: string } | null;

/** The reader holds a human seat. */
function isPerson(viewer: Reader): boolean {
  return viewer !== null && viewer.kind === "human" && viewer.handle !== "";
}

/** The directory binds the reader to a seat, a person's or an agent's. */
function isBound(viewer: Reader): boolean {
  return viewer !== null && viewer.handle !== "";
}

/** What holds any change to a repository's life on its page, or undefined. */
function lifeHeld(repo: CodeRepoSummary): string | undefined {
  switch (repo.status) {
    case "deleted":
      return `${repo.name} is deleted: restore it first.`;
    case "importing":
      return `${repo.name} is being imported from another host, and takes no change until its import is done.`;
  }
  if (repo.access && repo.access !== "maintain") {
    return `You ${repo.access} ${repo.name}; changing it takes maintain.`;
  }
  return undefined;
}

/**
 * Why the repository's search mode cannot be set to `mode`, or undefined: a
 * declared repository's is the document's, a scratch repository is never
 * indexed, and the mode it has already is no change.
 */
export function searchModeHeld(
  repo: CodeRepoDetail,
  viewer: Reader,
  mode: string,
): string | undefined {
  if (repo.managed_by === "config") return DECLARED;
  const held = lifeHeld(repo);
  if (held) return held;
  if (repo.kind === "scratch" && mode === "indexed") {
    return "A scratch repository is searched only with grep: an index is kept for a project alone.";
  }
  if (mode === repo.search) return `${repo.name} is searched this way already.`;
  if (!isBound(viewer)) return UNBOUND;
  return undefined;
}

/** Why the repository's mirrors cannot be set or removed here, or undefined. */
export function mirrorsHeld(repo: CodeRepoDetail, viewer: Reader): string | undefined {
  if (repo.managed_by === "config") return DECLARED;
  if (repo.kind === "scratch") return "A scratch repository takes no mirror.";
  const held = lifeHeld(repo);
  if (held) return held;
  if (!isPerson(viewer)) return PERSON_ONLY;
  return undefined;
}

/**
 * Why a pull mirror cannot be SET on the repository, beyond [mirrorsHeld]:
 * an archived repository takes none, since it would move a repository nothing
 * may write.
 */
export function pullMirrorHeld(repo: CodeRepoDetail, viewer: Reader): string | undefined {
  const held = mirrorsHeld(repo, viewer);
  if (held) return held;
  if (repo.status === "archived")
    return `${repo.name} is archived: unarchive it to make it a pull mirror.`;
  return undefined;
}

/**
 * Why the repository's unreachable history cannot be purged, or undefined —
 * a declared repository's included: a purge changes none of its settings.
 */
export function purgeHeld(repo: CodeRepoDetail, viewer: Reader): string | undefined {
  const held = lifeHeld(repo);
  if (held) return held;
  if (!isPerson(viewer)) return PERSON_ONLY;
  if (!repo.head && repo.live_packs === 0) return `${repo.name} holds no object to purge.`;
  return undefined;
}

/** Why the reader cannot import a repository, or undefined. */
export function importHeld(viewer: Reader): string | undefined {
  return isPerson(viewer) ? undefined : PERSON_ONLY;
}

/**
 * WHO KEEPS COPIES OF A PURGED OBJECT, in the order they let go of it: what a
 * person is told BEFORE they press, since a purge is asked because something
 * must go and none of these is the purge's to empty (code-host.md, "Expunge").
 * The object store's day is code.RetireGrace.
 */
export const PURGE_KEEPERS: readonly string[] = [
  "every git node's copy of the repository, until it syncs past the purge",
  "every backup taken before the purge, for as long as that backup is kept",
  "the object store, for a day after the purge commits (and a versioned S3 bucket for as long as its own rules keep a deleted object)",
];

/** The purge confirmation's sentence: what goes, and who still keeps a copy. */
export function purgeMessage(repo: CodeRepoDetail): string {
  return (
    `Every object no branch or tag of ${repo.name} reaches is dropped by its maintainer, with no grace for what a force-push or a deleted branch left. ` +
    `Copies are still kept by ${PURGE_KEEPERS.join("; by ")}. A secret that reached any of them must be rotated as well.`
  );
}

/**
 * Where a purge stands, in words, or null for a repository never purged with
 * none asked: a request asked and not yet served, or the last purge's
 * commit and what it took.
 */
export function purgeStanding(repo: CodeRepoDetail): string | null {
  const e = repo.expunge;
  if (!e) return null;
  if (e.request) return "A purge is asked and waits for the repository's maintainer.";
  if (e.open) return "A purge is running.";
  if (e.expunged_at) {
    return `Last purged ${fmtDateTime(e.expunged_at)}: ${plural(e.dropped_objects ?? 0, "object")} dropped.`;
  }
  return null;
}

/**
 * The import's progress, in words, or null for a repository its create did
 * not import: running (with the chunks landed), done, or ended short and why.
 */
export function importStanding(repo: CodeRepoDetail): string | null {
  const imp = repo.remotes?.import;
  if (!imp) return null;
  const landed = plural(imp.chunks, "chunk");
  if (!imp.done_at) return `Importing from ${imp.source.url}: ${landed} landed so far.`;
  if (imp.error)
    return `Imported from ${imp.source.url} in part, ${fmtDateTime(imp.done_at)}: ${imp.error}`;
  return `Imported from ${imp.source.url}, ${fmtDateTime(imp.done_at)}.`;
}

/** A pull mirror's standing, in words, or null for a repository that is none. */
export function pullMirrorStanding(repo: CodeRepoDetail): string | null {
  const from = repo.remotes?.mirror_from;
  if (!from) return null;
  const synced = repo.remotes?.mirror_synced_at;
  return synced
    ? `Follows ${from.url}; last synced ${fmtDateTime(synced)}.`
    : `Follows ${from.url}; not synced yet.`;
}

/** A push mirror's standing, in words, or null for a repository that is none. */
export function pushMirrorStanding(repo: CodeRepoDetail): string | null {
  const to = repo.remotes?.mirror_to;
  if (!to) return null;
  const refs = to.refs?.length ? to.refs.join(", ") : "its default branch and refs/tags/*";
  return `Pushes ${refs} to ${to.url} as ${to.as}.`;
}

/**
 * A credential as the code host stores it: one whole `${VAR}` reference to
 * the NAME a person types, or "" for none — never a token, which every record
 * of the repository's settings would carry. Null where what was typed is no
 * variable's name.
 */
export function credentialRef(name: string): string | null {
  const n = name.trim();
  if (n === "") return "";
  return /^[A-Z_][A-Z0-9_]*$/.test(n) ? `\${${n}}` : null;
}

/** The variable's name a stored reference names, for a form to start from. */
export function credentialName(ref: string | undefined): string {
  const m = /^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$/.exec(ref ?? "");
  return m ? m[1]! : "";
}

/** Which mirror a form sets: the one it follows, or the one it pushes to. */
export type MirrorKind = "pull" | "push";

/**
 * Why a mirror form cannot be sent yet, or undefined: the other host is named
 * by an https URL (the one scheme the code host dials), its credential by a
 * variable's name, and a push mirror — which authenticates, as a seat — by
 * both a credential and the seat it pushes as.
 */
export function mirrorFormHeld(
  kind: MirrorKind,
  url: string,
  credential: string,
  as: string,
): string | undefined {
  if (!url.trim().startsWith("https://"))
    return "Name the other host's repository by its https URL.";
  const ref = credentialRef(credential);
  if (ref === null)
    return "The credential is a variable's NAME in upper case, never the token itself.";
  if (kind === "push" && ref === "")
    return "A push authenticates: name the variable holding the token.";
  if (kind === "push" && as.trim() === "")
    return "Name the seat the push is made as, by its handle.";
  return undefined;
}

/**
 * Why an import form cannot be sent yet, or undefined: a project's name in
 * the engine's grammar (a dotted name is a seat's scratch repository, never
 * an import), an https URL, a credential by its variable's name, and the
 * unit that owns it.
 */
export function importFormHeld(
  name: string,
  url: string,
  credential: string,
  owner: string,
): string | undefined {
  if (!isRepoName(name.trim()) || name.includes(".")) {
    return "Name the project: lowercase letters, digits and hyphens.";
  }
  if (!url.trim().startsWith("https://"))
    return "Name the other host's repository by its https URL.";
  if (credentialRef(credential) === null) {
    return "The credential is a variable's NAME in upper case, never the token itself.";
  }
  if (owner === "") return "Choose the unit that owns it.";
  return undefined;
}
