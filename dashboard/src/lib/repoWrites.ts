/**
 * What a person may change of a repository from the Gitlet workspace, and why
 * not: its search mode, its mirrors, a purge of its unreachable history, an
 * import of a new one from another host, a new project, a delete and a
 * restore, its protection rules, and the writes to its branches (a branch
 * made or deleted, a file committed, a commit reverted or picked).
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
 * credential nobody is bound through writes nothing to Gitlet
 * (`gitlet.UnboundDetail`), so each is held for a reader who holds no human
 * seat.
 */

import { fieldHeld, formHeld, type FormHold } from "./formHold.ts";
import type { CodeRepoDetail, CodeRepoSummary } from "~/protocol/index.ts";
import { isRepoName } from "./code.ts";
import { fmtDateTime, plural } from "./format.ts";

/** Why a reader holding no human seat changes nothing here. */
export const PERSON_ONLY =
  "Only a person, signed in as the human seat they hold, makes this change: a mirror, an import and a purge are never a seat's.";

/**
 * Why a credential bound to nobody changes no repository: Gitlet names the
 * person who made every write (`gitlet.UnboundDetail`).
 */
export const UNBOUND =
  "Only a person, signed in as the human seat they hold, changes a repository: a credential bound to nobody writes nothing to Gitlet.";

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

/**
 * Why the repository's details (its description, default branch, owner and
 * who reads it) cannot be changed here, or undefined: a declared repository's
 * are the document's, and the rest is what holds any change to its life.
 */
export function detailsHeld(repo: CodeRepoDetail, viewer: Reader): string | undefined {
  if (repo.managed_by === "config") return DECLARED;
  const held = lifeHeld(repo);
  if (held) return held;
  if (!isBound(viewer)) return UNBOUND;
  return undefined;
}

/**
 * Why a details form cannot be sent yet, or undefined: something changed, and
 * a default branch the repository holds (`branches`, its live branches by
 * name). The default branch it has already is kept even where it holds no
 * commit, since that is no change.
 */
export function detailsFormHeld(
  repo: CodeRepoDetail,
  next: { description: string; defaultBranch: string; owner: string; visibility: string },
  branches: readonly string[],
): FormHold<"defaultBranch"> | undefined {
  const changed =
    next.description.trim() !== (repo.description ?? "") ||
    next.defaultBranch !== repo.default_branch ||
    next.owner !== ownerKey(repo.owner) ||
    next.visibility !== repo.visibility;
  if (!changed) return formHeld("Nothing is changed yet.");
  if (next.defaultBranch !== repo.default_branch && !branches.includes(next.defaultBranch)) {
    return fieldHeld(
      "defaultBranch",
      `${repo.name} holds no branch ${next.defaultBranch}: choose one of its branches.`,
    );
  }
  if (next.owner === "") return formHeld("Choose the unit that owns it.");
  return undefined;
}

/** An owner as a picker's value: its kind and its key or handle, `unit:eng`. */
export function ownerKey(owner: { kind: string; ref: string }): string {
  return `${owner.kind}:${owner.ref}`;
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
 * Why the repository cannot be archived, or undefined: what holds its details,
 * and a pull mirror, which an archive would freeze, since an archived
 * repository takes no sync (`gitlet.archiveRefusal`).
 */
export function archiveHeld(repo: CodeRepoDetail, viewer: Reader): string | undefined {
  const held = detailsHeld(repo, viewer);
  if (held) return held;
  if (repo.mirrored || repo.remotes?.mirror_from) {
    return `${repo.name} is a pull mirror, and an archived repository takes no sync: remove its pull mirror first.`;
  }
  return undefined;
}

/**
 * Why the repository cannot be renamed, or undefined: what holds its details.
 * The new name itself is the form's ([renameFormHeld]).
 */
export function renameHeld(repo: CodeRepoDetail, viewer: Reader): string | undefined {
  return detailsHeld(repo, viewer);
}

/**
 * Why a rename to `typed` cannot be sent yet, or undefined: a name the engine
 * reads, of the repository's own kind. A project's name is one label, and a
 * scratch repository keeps its seat's `<handle>.` (`gitlet.renameForm`).
 */
export function renameFormHeld(repo: CodeRepoSummary, typed: string): FormHold<"name"> | undefined {
  const name = typed.trim();
  if (name === repo.name || name === "") return formHeld("Type its new name.");
  if (repo.kind === "scratch") {
    const prefix = `${repo.owner.ref}.`;
    if (!name.startsWith(prefix) || !isRepoName(name)) {
      return fieldHeld(
        "name",
        `A scratch repository keeps its seat's name: ${prefix}<name>, in lowercase letters, digits and hyphens.`,
      );
    }
    return undefined;
  }
  if (!isRepoName(name) || name.includes(".")) {
    return fieldHeld("name", PROJECT_NAME);
  }
  return undefined;
}

/** What a project's name is, said where one is refused. */
const PROJECT_NAME = "Name the project: lowercase letters, digits and hyphens.";

/** A project's name: refused under its field once typed, so no form opens on an error. */
function projectNameHeld(typed: string): FormHold<"name"> | undefined {
  if (typed.trim() === "") return formHeld(PROJECT_NAME);
  if (!isRepoName(typed.trim()) || typed.includes(".")) return fieldHeld("name", PROJECT_NAME);
  return undefined;
}

/** What the other host's address is, said where one is refused. */
const HTTPS_URL = "Name the other host's repository by its https URL.";

/** The other host's address: refused under its field once typed. */
function httpsUrlHeld(url: string): FormHold<"url"> | undefined {
  if (url.trim() === "") return formHeld(HTTPS_URL);
  if (!url.trim().startsWith("https://")) return fieldHeld("url", HTTPS_URL);
  return undefined;
}

/** What a credential is, said under its field where one is refused. */
const CREDENTIAL_NAME =
  "The credential is a variable's NAME in upper case, never the token itself.";

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
 * must go and none of these is the purge's to empty: every keeper gitlet.md's
 * "What still holds a purged object" lists, in its words. The object store's
 * day is gitlet.RetireGrace. The last keeper never lets go — a clone or a fetch
 * holds the object for as long as its owner keeps it, and nothing on the
 * server reaches it — which is why the sentence ends by asking for the secret
 * to be rotated rather than promising it gone.
 */
export const PURGE_KEEPERS: readonly string[] = [
  "every Gitlet node's copy of the repository, until it syncs past the purge",
  "every backup taken before the purge, for as long as that backup is kept",
  "the object store, for a day after the purge commits (and a versioned S3 bucket for as long as its own rules keep a deleted object)",
  "every clone and fetch that already took it (a person's clone, a coding box's, a CI runner's checkout), which nothing on the server reaches",
];

/**
 * Who keeps copies of what a purge of this repository drops: [PURGE_KEEPERS],
 * and, where the repository has a push mirror, the host it pushes to, BY ITS
 * URL. That host is a clone like any other, but one a person configured on
 * this very tab and may not think of as one: it already holds whatever was
 * pushed to it, and no purge here reaches it.
 */
export function purgeKeepers(repo: CodeRepoDetail): readonly string[] {
  const to = repo.remotes?.mirror_to;
  if (!to) return PURGE_KEEPERS;
  return [
    ...PURGE_KEEPERS,
    `${to.url}, the push mirror's host, which keeps whatever this repository already pushed to it`,
  ];
}

/** The purge confirmation's sentence: what goes, and who still keeps a copy. */
export function purgeMessage(repo: CodeRepoDetail): string {
  return (
    `Every object no branch or tag of ${repo.name} reaches is dropped by its maintainer, with no grace for what a force-push or a deleted branch left. ` +
    `Copies are still kept by ${purgeKeepers(repo).join("; by ")}. A secret that reached any of them must be rotated as well.`
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
 * A credential as Gitlet stores it: one whole `${VAR}` reference to
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
 * by an https URL (the one scheme Gitlet dials), its credential by a
 * variable's name, and a push mirror — which authenticates, as a seat — by
 * both a credential and the seat it pushes as.
 */
export function mirrorFormHeld(
  kind: MirrorKind,
  url: string,
  credential: string,
  as: string,
): FormHold<"url" | "credential"> | undefined {
  const address = httpsUrlHeld(url);
  if (address) return address;
  const ref = credentialRef(credential);
  if (ref === null) return fieldHeld("credential", CREDENTIAL_NAME);
  if (kind === "push" && ref === "")
    return formHeld("A push authenticates: name the variable holding the token.");
  if (kind === "push" && as.trim() === "")
    return formHeld("Name the seat the push is made as, by its handle.");
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
): FormHold<"name" | "url" | "credential"> | undefined {
  const named = projectNameHeld(name);
  if (named) return named;
  const address = httpsUrlHeld(url);
  if (address) return address;
  if (credentialRef(credential) === null) return fieldHeld("credential", CREDENTIAL_NAME);
  if (owner === "") return formHeld("Choose the unit that owns it.");
  return undefined;
}

/** Why a declared repository is not deleted, restored or protected here. */
export const DECLARED_LIFE =
  "The company document declares this repository: it is deleted, restored and protected in Settings › Configuration, which every activation settles.";

/**
 * Why a new-project form cannot be sent yet, or undefined: a project's name
 * in the engine's grammar (a dotted name is a seat's scratch repository, which
 * only that seat creates) and the unit that owns it.
 */
export function createFormHeld(name: string, owner: string): FormHold<"name"> | undefined {
  const named = projectNameHeld(name);
  if (named) return named;
  if (owner === "") return formHeld("Choose the unit that owns it.");
  return undefined;
}

/** Why the reader cannot create a repository, or undefined. */
export function createHeld(viewer: Reader): string | undefined {
  return isBound(viewer) ? undefined : UNBOUND;
}

/**
 * Why the repository cannot be deleted, or undefined: a declared one is the
 * document's, one deleted already is restored first, and deleting takes
 * maintain.
 */
export function deleteHeld(repo: CodeRepoSummary, viewer: Reader): string | undefined {
  if (repo.managed_by === "config") return DECLARED_LIFE;
  if (repo.status === "deleted") return `${repo.name} is deleted already.`;
  if (repo.status === "purging") return `${repo.name} is being purged.`;
  const held = lifeHeld(repo);
  if (held) return held;
  if (!isBound(viewer)) return UNBOUND;
  return undefined;
}

/**
 * Why the repository's files, history and tags are not read, or undefined: a
 * deleted repository answers no read of its content (nor any clone or push)
 * until it is restored, and one being purged never again.
 */
export function contentHeld(repo: CodeRepoSummary): string | undefined {
  if (repo.status === "purging") return `${repo.name} is being purged, with its files and history.`;
  if (repo.status === "deleted") {
    return `Its files, history and tags are read, cloned and pushed to again once ${repo.name} is restored.`;
  }
  return undefined;
}

/** Why the repository cannot be restored, or undefined: only a deleted one is. */
export function restoreHeld(repo: CodeRepoSummary, viewer: Reader): string | undefined {
  if (repo.managed_by === "config") return DECLARED_LIFE;
  if (repo.status === "purging") return `${repo.name} is being purged, and comes back no more.`;
  if (repo.status !== "deleted") return `${repo.name} is not deleted.`;
  if (repo.access && repo.access !== "maintain") {
    return `You ${repo.access} ${repo.name}; restoring it takes maintain.`;
  }
  if (!isBound(viewer)) return UNBOUND;
  return undefined;
}

/** Why the repository's protection rules cannot be changed here, or undefined. */
export function protectHeld(repo: CodeRepoSummary, viewer: Reader): string | undefined {
  if (repo.managed_by === "config") return DECLARED_LIFE;
  const held = lifeHeld(repo);
  if (held) return held;
  if (!isBound(viewer)) return UNBOUND;
  return undefined;
}

/**
 * Why the repository's path owners cannot be changed here, or undefined: what
 * holds its details, and a scratch repository, which takes no path owners
 * (it is its seat's own experiment).
 */
export function pathOwnersHeld(repo: CodeRepoDetail, viewer: Reader): string | undefined {
  const held = detailsHeld(repo, viewer);
  if (held) return held;
  if (repo.kind === "scratch") {
    return "A scratch repository takes no path owners: it is its seat's own.";
  }
  return undefined;
}

/**
 * Why a reader cannot take globs from a path owner, beyond [pathOwnersHeld]:
 * while a rule makes path owners approve merges (`owners_approve`), taking a
 * glob loosens that rule, which only a person does (`gitlet`'s
 * gatingOwnersTaken).
 */
export function pathOwnerTakeHeld(
  repo: CodeRepoDetail,
  viewer: Reader,
  gating: boolean,
): string | undefined {
  const held = pathOwnersHeld(repo, viewer);
  if (held) return held;
  if (gating && !isPerson(viewer)) {
    return "Only a person takes a path from its owner while a protection rule makes path owners approve its merges.";
  }
  return undefined;
}

/** The globs a person typed: by commas, spaces or lines, each once. */
export function globsOf(typed: readonly string[]): string[] {
  return [...new Set(typed.map((g) => g.trim()).filter(Boolean))];
}

/**
 * What one `code_update_repo` sends to move one owner's globs from `had` to
 * `wanted`, globs given and taken by name (`gitlet.PathOwnersChange`), never
 * the list whole, so a glob another request gave meanwhile is kept; or null
 * where nothing moved.
 */
export function pathOwnersArgs(
  owner: { kind: string; ref: string },
  wanted: readonly string[],
  had: readonly string[],
): {
  add?: { owner: typeof owner; paths: string[] }[];
  remove?: { owner: typeof owner; paths: string[] }[];
} | null {
  const want = globsOf(wanted);
  const add = want.filter((g) => !had.includes(g));
  const remove = had.filter((g) => !want.includes(g));
  if (!add.length && !remove.length) return null;
  return {
    ...(add.length ? { add: [{ owner, paths: add }] } : {}),
    ...(remove.length ? { remove: [{ owner, paths: remove }] } : {}),
  };
}

/**
 * Why a path owner form cannot be sent yet, or undefined: an owner chosen, a
 * glob at least, and (for an owner already holding some) something changed.
 */
export function pathOwnerFormHeld(
  owner: string,
  wanted: readonly string[],
  had: readonly string[],
): FormHold<"paths"> | undefined {
  if (owner === "") return formHeld("Choose the unit or seat that owns the paths.");
  const want = globsOf(wanted);
  if (want.length === 0) {
    return formHeld(
      had.length ? "Name a path at least, or remove the owner." : "Name a path at least.",
    );
  }
  if (want.some((g) => g.startsWith("/") || /\s/.test(g))) {
    return fieldHeld(
      "paths",
      "Name each path from the repository's root, with no spaces, such as src/** or docs/*.md.",
    );
  }
  if (want.length === had.length && want.every((g) => had.includes(g))) {
    return formHeld("Nothing is changed yet.");
  }
  return undefined;
}

/**
 * A pattern as a protection rule names refs: a branch's or a tag's whole ref,
 * or a glob of them. A bare name is a branch's.
 */
export function protectionPattern(typed: string): string {
  const t = typed.trim();
  if (t === "" || t.startsWith("refs/")) return t;
  return `refs/heads/${t}`;
}

/** Why a protection form's pattern cannot be sent yet, or undefined. */
export function protectFormHeld(pattern: string): FormHold<"pattern"> | undefined {
  const p = protectionPattern(pattern);
  if (!/^refs\/(heads|tags)\/\S+$/.test(p)) {
    const words =
      "Name the branches or tags it protects: a branch's name, or refs/heads/<glob> or refs/tags/<glob>.";
    return pattern.trim() === "" ? formHeld(words) : fieldHeld("pattern", words);
  }
  return undefined;
}

/**
 * What holds every write to a repository's branches, or undefined: a
 * repository that is deleted, importing, archived or a pull mirror takes no
 * push, and a write takes at least write access.
 */
function pushHeld(repo: CodeRepoSummary, viewer: Reader): string | undefined {
  switch (repo.status) {
    case "deleted":
    case "purging":
      return `${repo.name} is deleted: restore it first.`;
    case "importing":
      return `${repo.name} is being imported from another host, and takes no change until its import is done.`;
    case "archived":
      return `${repo.name} is archived and read-only.`;
  }
  if (repo.mirrored) {
    return `${repo.name} is a pull mirror: it moves only by syncs from its source.`;
  }
  if (!isBound(viewer)) return UNBOUND;
  return undefined;
}

/**
 * Why the reader cannot commit to, create or move a branch of the repository,
 * or undefined: what holds every push, and a grant below write.
 */
export function branchWriteHeld(repo: CodeRepoSummary, viewer: Reader): string | undefined {
  const held = pushHeld(repo, viewer);
  if (held) return held;
  if (repo.access && repo.access !== "write" && repo.access !== "maintain") {
    return `You ${repo.access} ${repo.name}; writing to a branch takes write.`;
  }
  return undefined;
}

/** Why the reader cannot delete a branch of the repository, or undefined: it takes maintain. */
export function branchDeleteHeld(repo: CodeRepoSummary, viewer: Reader): string | undefined {
  const held = pushHeld(repo, viewer);
  if (held) return held;
  if (repo.access && repo.access !== "maintain") {
    return `You ${repo.access} ${repo.name}; deleting a branch takes maintain.`;
  }
  return undefined;
}

/** Why a new-branch form cannot be sent yet, or undefined. */
export function branchFormHeld(name: string): string | undefined {
  const n = name.trim();
  if (n === "" || /\s/.test(n)) return "Name the branch, with no spaces.";
  return undefined;
}

/**
 * Why a commit form cannot be sent yet, or undefined: a message, a path for
 * a new file, and something changed.
 */
export function commitFormHeld(
  message: string,
  path: string,
  changed: boolean,
): string | undefined {
  if (path.trim() === "" || path.startsWith("/") || path.endsWith("/")) {
    return "Name the file by its path from the repository's root.";
  }
  if (!changed) return "Nothing is changed yet.";
  if (message.trim() === "") return "Say what the commit changes.";
  return undefined;
}
