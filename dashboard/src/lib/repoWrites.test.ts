/**
 * Each repository write is held with the reason the engine would refuse it
 * for, and a purge names who keeps copies before it is asked.
 *
 * The invariants worth breaking a build over: a reader holding no human seat
 * changes no mirror, imports nothing and purges nothing; a declared
 * repository's search mode and mirrors are the document's, while a purge is
 * offered for it; a deleted or importing repository takes no change; a
 * scratch repository is never indexed and takes no mirror; the mode a
 * repository has is no change; the purge's confirmation names every keeper;
 * and a credential is a reference to a variable's name, never a token.
 */

import { expect, test } from "vitest";
import type { CodeRepoDetail, Viewer } from "~/protocol/index.ts";
import {
  DECLARED,
  PERSON_ONLY,
  PURGE_KEEPERS,
  UNBOUND,
  credentialName,
  credentialRef,
  importFormHeld,
  importHeld,
  importStanding,
  mirrorFormHeld,
  mirrorsHeld,
  pullMirrorHeld,
  pullMirrorStanding,
  purgeHeld,
  purgeMessage,
  searchModeHeld,
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

test("the purge's confirmation names every keeper of a copy", () => {
  const message = purgeMessage(repo());
  expect(PURGE_KEEPERS.length).toBe(3);
  for (const keeper of PURGE_KEEPERS) expect(message).toContain(keeper);
  expect(message).toMatch(/git node/);
  expect(message).toMatch(/backup/);
  expect(message).toMatch(/object store/);
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
