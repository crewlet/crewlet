// @vitest-environment node
/**
 * The folder tree as a reader sees it: its index (children A to Z, a trail,
 * the listing's prefix, a subtree and its height), the places a move offers,
 * how a phrase finds one, who sees a folder in words, and a name the engine
 * will take.
 */

import { describe, expect, test } from "vitest";
import type { ArtifactFolderItem } from "~/protocol/index.ts";
import {
  artifactPlace,
  destinations,
  folderNameHold,
  folderNameProblem,
  indexFolders,
  matchDestinations,
  pathWords,
  takenBy,
  visibilityWords,
} from "./folders.ts";

function folder(
  over: Partial<ArtifactFolderItem> & { id: string; name: string },
): ArtifactFolderItem {
  return {
    root: over.parent ? "" : over.id,
    owner: "maya",
    owner_kind: "human",
    depth: 1,
    max_depth: 4,
    visibility: "company",
    status: "live",
    created_by: { name: "maya", kind: "human" },
    created_at: "2026-10-20T10:00:00Z",
    updated_by: { name: "maya", kind: "human" },
    updated_at: "2026-10-20T10:00:00Z",
    version: 1,
    artifacts: 0,
    folders: 0,
    role: "contributor",
    ...over,
  } as ArtifactFolderItem;
}

// Marketing › Q4 review › Press kit, Marketing › Campaigns, Board (private).
const TREE = [
  folder({ id: "m", name: "Marketing", folders: 2 }),
  folder({ id: "b", name: "board", visibility: "private", owner: "ada", role: "owner" }),
  folder({ id: "q", name: "Q4 review", parent: "m", root: "m", depth: 2 }),
  folder({ id: "c", name: "Campaigns", parent: "m", root: "m", depth: 2 }),
  folder({ id: "p", name: "Press kit", parent: "q", root: "m", depth: 3 }),
];

describe("indexFolders", () => {
  const index = indexFolders(TREE);

  test("children are siblings A to Z, the top level being the empty parent", () => {
    expect(index.children("").map((f) => f.name)).toEqual(["board", "Marketing"]);
    expect(index.children("m").map((f) => f.name)).toEqual(["Campaigns", "Q4 review"]);
    expect(index.children("p")).toEqual([]);
  });

  test("a trail runs from the top-level folder to the folder, and its prefix and words follow it", () => {
    expect(index.trail("p").map((f) => f.id)).toEqual(["m", "q", "p"]);
    expect(index.prefix("p")).toBe("Marketing/Q4 review/Press kit/");
    expect(index.label("p")).toBe("Marketing › Q4 review › Press kit");
    expect(index.prefix("")).toBe("");
    expect(index.trail("nope")).toEqual([]);
  });

  test("a subtree holds the folder and everything beneath it, and its height counts the levels", () => {
    expect([...index.subtree("m")].sort()).toEqual(["c", "m", "p", "q"]);
    expect(index.height("m")).toBe(3);
    expect(index.height("p")).toBe(1);
    expect(index.height("nope")).toBe(0);
  });

  // THE ENGINE NEVER ANSWERS ONE, and floating it to the top would put it
  // where nobody filed it.
  test("a folder whose parent the reader cannot see is drawn nowhere", () => {
    const orphan = indexFolders([folder({ id: "x", name: "Stray", parent: "gone", depth: 2 })]);
    expect(orphan.children("")).toEqual([]);
    expect(orphan.get("x")?.name).toBe("Stray");
  });

  test("a loop in an answer taken mid-move ends the trail rather than spinning", () => {
    const loop = indexFolders([
      folder({ id: "a", name: "A", parent: "b", depth: 2 }),
      folder({ id: "b", name: "B", parent: "a", depth: 2 }),
    ]);
    expect(loop.trail("a").map((f) => f.id)).toEqual(["b", "a"]);
  });
});

describe("destinations", () => {
  const index = indexFolders(TREE);

  test("the top level comes first, then the tree in its own order, each with the folders above it", () => {
    const all = destinations(index);
    expect(all.map((d) => d.path)).toEqual([
      "Top level",
      "board",
      "Marketing",
      "Marketing › Campaigns",
      "Marketing › Q4 review",
      "Marketing › Q4 review › Press kit",
    ]);
    expect(all.find((d) => d.id === "p")?.above).toBe("Marketing › Q4 review");
  });

  test("a folder the reader only views is no place to file, while what is beneath it still is", () => {
    const viewing = indexFolders([
      folder({ id: "v", name: "Read only", role: "viewer" }),
      folder({ id: "w", name: "Inside", parent: "v", root: "v", depth: 2 }),
    ]);
    expect(destinations(viewing).map((d) => d.id)).toEqual(["", "w"]);
  });

  test("moving a folder leaves out itself, everything beneath it, and every place it would sink too deep", () => {
    const moving = destinations(index, { folder: "q" }).map((d) => d.id);
    expect(moving).not.toContain("q");
    expect(moving).not.toContain("p");
    // Q4 review spans two levels: under Campaigns (depth 2) it reaches 4, the cap.
    expect(moving).toContain("c");
    const shallow = indexFolders(TREE.map((f) => ({ ...f, max_depth: 3 })));
    expect(destinations(shallow, { folder: "q" }).map((d) => d.id)).not.toContain("c");
  });
});

describe("matchDestinations", () => {
  const all = destinations(indexFolders(TREE));

  test("a name that starts with the phrase comes before one that holds it, and a path match last", () => {
    expect(matchDestinations(all, "pre").map((d) => d.id)).toEqual(["p"]);
    expect(matchDestinations(all, "m").map((d) => d.name)).toEqual([
      "Marketing",
      "Campaigns",
      "Q4 review",
      "Press kit",
    ]);
    expect(matchDestinations(all, "  ")).toHaveLength(all.length);
  });

  test("the top level is found by its own words", () => {
    expect(matchDestinations(all, "top")[0]?.id).toBe("");
  });
});

describe("visibilityWords", () => {
  const who = { company: "FairSplit", mine: true, ownerName: "Ada" };

  test("everyone at the company, only you, and only its owner to anybody else who sees it", () => {
    expect(visibilityWords("company", who)).toEqual({
      access: "company",
      short: "Everyone",
      long: "Everyone at FairSplit",
    });
    expect(visibilityWords("private", who).long).toBe("Only you");
    expect(visibilityWords("private", { ...who, mine: false }).long).toBe("Only Ada");
    expect(visibilityWords("custom", who).access).toBe("shared");
    expect(visibilityWords("company", { ...who, company: "" }).long).toBe(
      "Everyone in the company",
    );
  });
});

describe("folder names", () => {
  test("the engine's grammar is said before the press, and an empty name is held, not wrong", () => {
    expect(folderNameProblem("")).toBeUndefined();
    expect(folderNameHold("  ")).toBe("Give it a name.");
    expect(folderNameProblem("Q3/Q4")).toMatch(/cannot hold “\/”/);
    expect(folderNameProblem("..")).toMatch(/cannot be called/);
    expect(folderNameProblem("Plan‮")).toMatch(/invisible or control character/);
    expect(folderNameProblem("a".repeat(81))).toMatch(/81 bytes and the most is 80/);
    expect(folderNameProblem("Été 2026")).toBeUndefined();
  });

  test("a sibling of the same name, case and composition aside, is named; a rename is not its own clash", () => {
    const index = indexFolders(TREE);
    expect(takenBy(index.children("m"), "q4 REVIEW")?.id).toBe("q");
    expect(takenBy(index.children("m"), "Q4 review", "q")).toBeUndefined();
    expect(takenBy(index.children("m"), "Launch")).toBeUndefined();
  });
});

describe("where an artifact is kept", () => {
  const index = indexFolders(TREE);

  test("a listing's path in words, and the top level as nothing", () => {
    expect(pathWords("Marketing/Q4 review/")).toBe("Marketing › Q4 review");
    expect(pathWords(undefined)).toBe("");
  });

  // THE ENGINE NAMES NO FOLDER THE READER CANNOT SEE, so an id the tree does
  // not hold is one it has not caught up with, never a hidden place.
  test("a folder the reader sees by its path, and nothing until the tree holds it", () => {
    expect(artifactPlace(undefined, index)).toEqual({ at: "top" });
    expect(artifactPlace("q", index)).toEqual({
      at: "folder",
      id: "q",
      name: "Q4 review",
      path: "Marketing › Q4 review",
    });
    expect(artifactPlace("new", index)).toEqual({ at: "reading" });
  });
});
