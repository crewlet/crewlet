/**
 * Each repository write is held with the reason the engine would refuse it
 * for, and a purge names who keeps copies before it is asked.
 *
 * The invariants worth breaking a build over: a reader holding no human seat
 * changes no mirror, imports nothing and purges nothing; a declared
 * repository's search mode and mirrors are the document's, while a purge is
 * offered for it; a deleted or importing repository takes no change; a
 * scratch repository is never indexed and takes no mirror; the mode a
 * repository has is no change; the purge's confirmation names every keeper,
 * a push mirror's host by its URL;
 * and a credential is a reference to a variable's name, never a token.
 */

import { expect, test } from "vitest";
import type { CodeRepoDetail, Viewer } from "~/protocol/index.ts";
import {
  DECLARED,
  DECLARED_LIFE,
  PERSON_ONLY,
  PURGE_KEEPERS,
  UNBOUND,
  branchDeleteHeld,
  branchFormHeld,
  branchWriteHeld,
  commitFormHeld,
  createFormHeld,
  createHeld,
  credentialName,
  credentialRef,
  importFormHeld,
  importHeld,
  importStanding,
  mirrorFormHeld,
  mirrorsHeld,
  protectFormHeld,
  protectHeld,
  protectionPattern,
  pullMirrorHeld,
  pullMirrorStanding,
  purgeHeld,
  purgeKeepers,
  purgeMessage,
  restoreHeld,
  searchModeHeld,
  deleteHeld,
  archiveHeld,
  pathOwnerFormHeld,
  pathOwnerTakeHeld,
  pathOwnersArgs,
  pathOwnersHeld,
  renameFormHeld,
  renameHeld,
} from "./repoWrites.ts";

const PERSON = {
  login: "jane",
  grants: [],
  handle: "jane",
  name: "Jane",
  kind: "human",
  owner: "jane",
  acts: [],
} as unknown as Viewer;
const MACHINE = { ...PERSON, handle: "", kind: "", login: "ci:deploy" } as unknown as Viewer;
const AGENT = { ...PERSON, handle: "cto", kind: "agent" } as unknown as Viewer;

function repo(extra: Partial<CodeRepoDetail> = {}): CodeRepoDetail {
  return {
    id: "id-web",
    name: "web",
    status: "active",
    managed_by: "tools",
    kind: "project",
    owner: { kind: "unit", ref: "engineering" },
    visibility: "company",
    search: "grep",
    default_branch: "main",
    access: "maintain",
    head: "0123456789abcdef0123456789abcdef01234567",
    live_packs: 2,
    branches: 1,
    tags: 0,
    tombstones: 0,
    created_at: "2026-10-01T10:00:00Z",
    ...extra,
  } as unknown as CodeRepoDetail;
}

test("a reader holding no human seat sets no mirror, imports nothing and purges nothing", () => {
  for (const viewer of [MACHINE, AGENT, null]) {
    expect(mirrorsHeld(repo(), viewer)).toBe(PERSON_ONLY);
    expect(importHeld(viewer)).toBe(PERSON_ONLY);
    expect(purgeHeld(repo(), viewer)).toBe(PERSON_ONLY);
  }
  expect(mirrorsHeld(repo(), PERSON)).toBeUndefined();
  expect(importHeld(PERSON)).toBeUndefined();
  expect(purgeHeld(repo(), PERSON)).toBeUndefined();
});

test("a declared repository's search mode and mirrors are the document's, and its purge is offered", () => {
  const declared = repo({ managed_by: "config" });
  expect(searchModeHeld(declared, PERSON, "indexed")).toBe(DECLARED);
  expect(mirrorsHeld(declared, PERSON)).toBe(DECLARED);
  expect(purgeHeld(declared, PERSON)).toBeUndefined();
});

test("each control is held with its own reason", () => {
  expect(searchModeHeld(repo(), PERSON, "grep")).toMatch(/already/);
  expect(searchModeHeld(repo(), PERSON, "indexed")).toBeUndefined();
  expect(searchModeHeld(repo({ kind: "scratch" }), PERSON, "indexed")).toMatch(/scratch/);
  expect(mirrorsHeld(repo({ kind: "scratch" }), PERSON)).toMatch(/scratch/);
  expect(pullMirrorHeld(repo({ status: "archived" }), PERSON)).toMatch(/archived/);
  expect(mirrorsHeld(repo({ status: "archived" }), PERSON)).toBeUndefined();
  for (const status of ["deleted", "importing"] as const) {
    const r = repo({ status });
    expect(searchModeHeld(r, PERSON, "indexed")).toMatch(
      status === "deleted" ? /deleted/ : /imported/,
    );
    expect(mirrorsHeld(r, PERSON)).toMatch(status === "deleted" ? /deleted/ : /imported/);
    expect(purgeHeld(r, PERSON)).toMatch(status === "deleted" ? /deleted/ : /imported/);
  }
  expect(purgeHeld(repo({ access: "write" }), PERSON)).toMatch(/takes maintain/);
  expect(purgeHeld(repo({ head: undefined, live_packs: 0 }), PERSON)).toMatch(/no object/);
});

// THE CONFIRMATION NAMES EVERY KEEPER gitlet.md's "What still holds a
// purged object" lists — the Gitlet nodes' caches, the backups, the object store
// and every clone and fetch that already took the object — each by what it
// is, so a keeper dropped from the list, or the list dropped from the
// sentence, fails here rather than reading as a purge that leaves nothing
// behind. Mutation: drop the clones, or join fewer keepers, and this fails.
test("the purge's confirmation names every keeper of a copy gitlet.md lists", () => {
  const message = purgeMessage(repo());
  for (const keeper of PURGE_KEEPERS) expect(message).toContain(keeper);
  expect(message).toMatch(/every Gitlet node's copy/);
  expect(message).toMatch(/every backup taken before the purge/);
  expect(message).toMatch(/the object store, for a day/);
  expect(message).toMatch(/every clone and fetch that already took it/);
  expect(message).toMatch(/a person's clone, a coding box's, a CI runner's checkout/);
  expect(message).toMatch(/nothing on the server reaches/);
  expect(message).toMatch(/rotated/);
  expect(message).not.toMatch(/push mirror/);
});

// A PUSH MIRROR'S HOST IS NAMED BY ITS URL: it already holds whatever the
// repository pushed to it, and no purge here reaches it — the one clone the
// person purging configured themselves, on the same tab. Mutation: leave the
// mirror out of the keepers, and this fails.
test("a repository with a push mirror names its host among a purge's keepers", () => {
  const url = "https://mirror.example.com/acme/web.git";
  const mirrored = repo({
    remotes: { mirror_to: { url, credential: "${MIRROR_TOKEN}", as: "cto" } },
  } as Partial<CodeRepoDetail>);
  const message = purgeMessage(mirrored);
  expect(message).toContain(url);
  expect(message).toMatch(/push mirror/);
  for (const keeper of PURGE_KEEPERS) expect(message).toContain(keeper);
  expect(purgeKeepers(mirrored)).toHaveLength(PURGE_KEEPERS.length + 1);
});

test("a credential is a reference to a variable's name, never a token", () => {
  expect(credentialRef("FORGE_TOKEN")).toBe("${FORGE_TOKEN}");
  expect(credentialRef("")).toBe("");
  expect(credentialRef("glpat-abc123")).toBeNull();
  expect(credentialRef("forge_token")).toBeNull();
  expect(credentialName("${FORGE_TOKEN}")).toBe("FORGE_TOKEN");
  expect(credentialName(undefined)).toBe("");
});

test("an import and a pull mirror say where they stand", () => {
  const source = { url: "https://forge.example.com/acme/web.git" };
  expect(importStanding(repo())).toBeNull();
  expect(importStanding(repo({ remotes: { import: { source, chunks: 2 } } }))).toMatch(
    /Importing from .*: 2 chunks landed/,
  );
  expect(
    importStanding(
      repo({
        remotes: {
          import: { source, chunks: 2, done_at: "2026-10-02T10:00:00Z", error: "too big" },
        },
      }),
    ),
  ).toMatch(/in part.*too big/);
  expect(pullMirrorStanding(repo({ remotes: { mirror_from: source } }))).toMatch(/not synced yet/);
  expect(
    pullMirrorStanding(
      repo({ remotes: { mirror_from: source, mirror_synced_at: "2026-10-02T10:00:00Z" } }),
    ),
  ).toMatch(/last synced/);
});

test("a search mode is set by whoever a seat binds, never by a credential bound to nobody", () => {
  expect(searchModeHeld(repo(), MACHINE, "indexed")).toBe(UNBOUND);
  expect(searchModeHeld(repo(), null, "indexed")).toBe(UNBOUND);
  expect(searchModeHeld(repo(), AGENT, "indexed")).toBeUndefined();
  expect(searchModeHeld(repo(), PERSON, "indexed")).toBeUndefined();
});

test("a mirror form is sent only with an https URL, a variable's name, and a push mirror's seat", () => {
  const url = "https://forge.example.com/acme/web.git";
  expect(mirrorFormHeld("pull", url, "", "")).toBeUndefined();
  expect(mirrorFormHeld("pull", "http://forge.example.com/web.git", "", "")).toMatch(/https URL/);
  expect(mirrorFormHeld("pull", "git@forge.example.com:acme/web.git", "", "")).toMatch(/https URL/);
  expect(mirrorFormHeld("pull", url, "glpat-abc123", "")).toMatch(/never the token/);
  expect(mirrorFormHeld("push", url, "", "cto")).toMatch(/push authenticates/);
  expect(mirrorFormHeld("push", url, "FORGE_TOKEN", " ")).toMatch(/seat the push is made as/);
  expect(mirrorFormHeld("push", url, "FORGE_TOKEN", "cto")).toBeUndefined();
});

test("an import form is sent only with a project's name, an https URL, a variable's name and an owner", () => {
  const url = "https://forge.example.com/acme/web.git";
  expect(importFormHeld("web", url, "", "engineering")).toBeUndefined();
  expect(importFormHeld("web", url, "FORGE_TOKEN", "engineering")).toBeUndefined();
  expect(importFormHeld("Web_App", url, "", "engineering")).toMatch(/Name the project/);
  expect(importFormHeld("cto.scratch", url, "", "engineering")).toMatch(/Name the project/);
  expect(importFormHeld("web", "http://forge.example.com/web.git", "", "engineering")).toMatch(
    /https URL/,
  );
  expect(importFormHeld("web", url, "ghp_secret", "engineering")).toMatch(/never the token/);
  expect(importFormHeld("web", url, "", "")).toMatch(/owns it/);
});

// A REPOSITORY'S LIFE IS HELD WHERE THE ENGINE REFUSES IT: a declared one is
// the company document's (`config_managed`), a deleted one is restored and
// never deleted again, a live one is deleted and never restored, and a
// credential bound to nobody writes nothing to Gitlet. Mutation: drop a branch
// of any hold and the matching line goes red.
test("a repository is deleted and restored only where the engine would take it", () => {
  expect(deleteHeld(repo(), PERSON)).toBeUndefined();
  expect(deleteHeld(repo({ managed_by: "config" }), PERSON)).toBe(DECLARED_LIFE);
  expect(deleteHeld(repo({ status: "deleted" }), PERSON)).toMatch(/deleted already/);
  expect(deleteHeld(repo({ status: "importing" }), PERSON)).toMatch(/being imported/);
  expect(deleteHeld(repo({ access: "write" }), AGENT)).toMatch(/takes maintain/);
  expect(deleteHeld(repo(), MACHINE)).toBe(UNBOUND);

  const gone = repo({ status: "deleted" });
  expect(restoreHeld(gone, PERSON)).toBeUndefined();
  expect(restoreHeld(repo(), PERSON)).toMatch(/is not deleted/);
  expect(restoreHeld(repo({ status: "purging" }), PERSON)).toMatch(/comes back no more/);
  expect(restoreHeld({ ...gone, managed_by: "config" }, PERSON)).toBe(DECLARED_LIFE);
  expect(restoreHeld(gone, MACHINE)).toBe(UNBOUND);
});

test("a new project is named in the engine's grammar, owned by a unit, by whoever a seat binds", () => {
  expect(createHeld(PERSON)).toBeUndefined();
  expect(createHeld(AGENT)).toBeUndefined();
  expect(createHeld(MACHINE)).toBe(UNBOUND);
  expect(createFormHeld("web", "engineering")).toBeUndefined();
  expect(createFormHeld("Web", "engineering")).toMatch(/Name the project/);
  // A DOTTED NAME IS A SEAT'S SCRATCH REPOSITORY, which only that seat makes.
  expect(createFormHeld("cto.notes", "engineering")).toMatch(/Name the project/);
  expect(createFormHeld("web", "")).toMatch(/owns it/);
});

test("a protection rule names refs and is set only on a repository a tool made", () => {
  expect(protectionPattern("main")).toBe("refs/heads/main");
  expect(protectionPattern(" refs/tags/v* ")).toBe("refs/tags/v*");
  expect(protectionPattern("")).toBe("");
  expect(protectFormHeld("release/*")).toBeUndefined();
  expect(protectFormHeld("refs/tags/v*")).toBeUndefined();
  expect(protectFormHeld("")).toMatch(/Name the branches or tags/);
  expect(protectFormHeld("refs/notes/x")).toMatch(/Name the branches or tags/);
  expect(protectHeld(repo(), PERSON)).toBeUndefined();
  expect(protectHeld(repo({ managed_by: "config" }), PERSON)).toBe(DECLARED_LIFE);
  expect(protectHeld(repo({ status: "deleted" }), PERSON)).toMatch(/restore it first/);
});

// A BRANCH TAKES NO WRITE WHERE NO PUSH WOULD LAND: a deleted, importing,
// archived or pull-mirrored repository moves by nothing a person commits, a
// commit takes write and a branch's delete maintain (`gitlet` access table).
test("a branch is written where a push would land, and deleted by a maintainer", () => {
  expect(branchWriteHeld(repo(), PERSON)).toBeUndefined();
  expect(branchWriteHeld(repo({ status: "archived" }), PERSON)).toMatch(/archived/);
  expect(branchWriteHeld(repo({ status: "deleted" }), PERSON)).toMatch(/restore it first/);
  expect(branchWriteHeld(repo({ mirrored: true }), PERSON)).toMatch(/pull mirror/);
  expect(branchWriteHeld(repo({ access: "read" }), AGENT)).toMatch(/takes write/);
  expect(branchWriteHeld(repo({ access: "write" }), AGENT)).toBeUndefined();
  expect(branchWriteHeld(repo(), MACHINE)).toBe(UNBOUND);
  expect(branchDeleteHeld(repo({ access: "write" }), AGENT)).toMatch(/takes maintain/);
  expect(branchDeleteHeld(repo(), PERSON)).toBeUndefined();
  expect(branchFormHeld("fix/login")).toBeUndefined();
  expect(branchFormHeld("fix login")).toMatch(/no spaces/);
  expect(branchFormHeld(" ")).toMatch(/Name the branch/);
});

test("a commit from the browser names a file from the root, a change and a message", () => {
  expect(commitFormHeld("Fix a typo", "docs/a.md", true)).toBeUndefined();
  expect(commitFormHeld("Fix a typo", "", true)).toMatch(/by its path/);
  expect(commitFormHeld("Fix a typo", "/docs/a.md", true)).toMatch(/by its path/);
  expect(commitFormHeld("Fix a typo", "docs/", true)).toMatch(/by its path/);
  expect(commitFormHeld("Fix a typo", "docs/a.md", false)).toMatch(/Nothing is changed/);
  expect(commitFormHeld(" ", "docs/a.md", true)).toMatch(/Say what the commit changes/);
});

// A RENAME KEEPS THE REPOSITORY'S KIND: a project's name is one label, a
// scratch repository's keeps its seat's handle (`gitlet.renameForm`), and the
// name it has is no change. Mutation: let a dotted name rename a project, or a
// scratch repository leave its seat's prefix, and this fails.
test("a rename keeps a project one label and a scratch repository its seat's", () => {
  expect(renameFormHeld(repo(), "web")).toBe("Type its new name.");
  expect(renameFormHeld(repo(), "storefront")).toBeUndefined();
  expect(renameFormHeld(repo(), "cto.web")).toMatch(/Name the project/);
  const scratch = repo({ kind: "scratch", name: "cto.try", owner: { kind: "seat", ref: "cto" } });
  expect(renameFormHeld(scratch, "cto.again")).toBeUndefined();
  expect(renameFormHeld(scratch, "pm.again")).toMatch(/cto\.<name>/);
  expect(renameFormHeld(scratch, "again")).toMatch(/cto\.<name>/);
  expect(renameHeld(repo({ managed_by: "config" }), PERSON)).toBe(DECLARED);
  expect(renameHeld(repo(), MACHINE)).toBe(UNBOUND);
});

// A PULL MIRROR IS NEVER ARCHIVED (`gitlet.archiveRefusal`): an archived
// repository takes no sync. Mutation: let a pull mirror's archive through, and
// this fails.
test("a pull mirror is held from an archive, with why", () => {
  expect(archiveHeld(repo(), PERSON)).toBeUndefined();
  expect(archiveHeld(repo({ mirrored: true }), PERSON)).toMatch(/is a pull mirror/);
  expect(archiveHeld(repo({ status: "importing" }), PERSON)).toMatch(/being imported/);
  expect(archiveHeld(repo({ managed_by: "config" }), PERSON)).toBe(DECLARED);
});

// AN OWNER'S GLOBS MOVE BY NAME (`gitlet.PathOwnersChange`): given and taken,
// never the list whole. Mutation: send the wanted list as `add`, or a glob both
// ways, and this fails.
test("a path owner's globs are given and taken by name", () => {
  const unit = { kind: "unit", ref: "engineering" };
  expect(pathOwnersArgs(unit, ["src/**", "app/**", " app/** "], ["src/**", "docs/**"])).toEqual({
    add: [{ owner: unit, paths: ["app/**"] }],
    remove: [{ owner: unit, paths: ["docs/**"] }],
  });
  expect(pathOwnersArgs(unit, ["src/**"], ["src/**"])).toBeNull();
  expect(pathOwnersArgs(unit, [], ["src/**"])).toEqual({
    remove: [{ owner: unit, paths: ["src/**"] }],
  });
});

// A PATH OWNER FORM NAMES ITS OWNER AND A GLOB, FROM THE ROOT; a scratch
// repository takes none; and taking a glob while a rule gates on path owners
// is a person's. Mutation: let any of them through, and this fails.
test("a path owner form is held until it names an owner and a path, and taking one may be a person's", () => {
  expect(pathOwnerFormHeld("", ["src/**"], [])).toMatch(/Choose the unit or seat/);
  expect(pathOwnerFormHeld("unit:eng", [], [])).toBe("Name a path at least.");
  expect(pathOwnerFormHeld("unit:eng", [], ["src/**"])).toMatch(/or remove the owner/);
  expect(pathOwnerFormHeld("unit:eng", ["/src/**"], [])).toMatch(/from the repository's root/);
  expect(pathOwnerFormHeld("unit:eng", ["src/**"], ["src/**"])).toBe("Nothing is changed yet.");
  expect(pathOwnerFormHeld("unit:eng", ["src/**"], [])).toBeUndefined();
  expect(pathOwnersHeld(repo({ kind: "scratch" }), PERSON)).toMatch(/takes no path owners/);
  expect(pathOwnerTakeHeld(repo(), AGENT, true)).toMatch(/Only a person/);
  expect(pathOwnerTakeHeld(repo(), AGENT, false)).toBeUndefined();
  expect(pathOwnerTakeHeld(repo(), PERSON, true)).toBeUndefined();
});
