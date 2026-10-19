import { describe, expect, it } from "vitest";
import { writtenArtifact } from "./transcript.ts";

const ID = "32f4d7b8-57d4-5ab7-a82f-413394e1825b";
const landed = (fields: Record<string, unknown> = {}) =>
  JSON.stringify({ outcome: "created", artifact: ID, version: 1, write: "applied", ...fields });

describe("writtenArtifact", () => {
  it("links a create that landed, with the title and kind it named", () => {
    expect(
      writtenArtifact(
        "create_artifact",
        JSON.stringify({ kind: "sheet", title: " Vendor review " }),
        landed(),
        false,
      ),
    ).toEqual({ id: ID, version: 1, title: "Vendor review", kind: "sheet" });
  });

  it("links a publish, whose arguments name only the artifact", () => {
    expect(
      writtenArtifact(
        "publish_artifact",
        JSON.stringify({ artifact: ID, base_version: 2 }),
        landed({ version: 3 }),
        false,
      ),
    ).toEqual({ id: ID, version: 3, title: "", kind: null });
  });

  it("links nothing for a failed call, another tool, or prose", () => {
    expect(writtenArtifact("create_artifact", "{}", landed(), true)).toBeNull();
    expect(writtenArtifact("update_artifact", "{}", landed(), false)).toBeNull();
    expect(writtenArtifact("create_artifact", "{}", "Refused: no access", false)).toBeNull();
    expect(writtenArtifact("create_artifact", "{}", "[1, 2]", false)).toBeNull();
  });

  it("never makes a link of an id that is not the engine's canonical uuid", () => {
    for (const id of [ID.toUpperCase(), `{${ID}}`, "../settings", "javascript:alert(1)", "", 42]) {
      expect(writtenArtifact("create_artifact", "{}", landed({ artifact: id }), false)).toBeNull();
    }
  });

  it("links nothing for an answer without a version that landed", () => {
    for (const version of [0, -1, 1.5, "1", null]) {
      expect(writtenArtifact("create_artifact", "{}", landed({ version }), false)).toBeNull();
    }
  });

  it("ignores a kind the engine does not define", () => {
    expect(
      writtenArtifact("create_artifact", JSON.stringify({ kind: "video" }), landed(), false)?.kind,
    ).toBeNull();
  });
});
