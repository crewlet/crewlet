// @vitest-environment node
/** The artifacts' write requests, as the engine reads them. */

import { describe, expect, test } from "vitest";
import {
  createFolder,
  createFromStarter,
  createSheet,
  fileArtifact,
  fileArtifacts,
  linkWorkItem,
  moveFolder,
  removeFolder,
  renameFolder,
} from "./api.ts";

describe("linkWorkItem", () => {
  test("names the work item, and no floor for an item filed earlier", () => {
    expect(linkWorkItem("a1", "ENG-12")).toEqual({
      method: "POST",
      path: "/artifacts/a1/work-items",
      body: { work_item: "ENG-12" },
    });
  });

  test("carries the create's position as after, so the engine reads the item from there on", () => {
    expect(linkWorkItem("a1", "ENG-12", "CREWLET_TRACKER_LOG@1:9")).toEqual({
      method: "POST",
      path: "/artifacts/a1/work-items",
      body: { work_item: "ENG-12", after: "CREWLET_TRACKER_LOG@1:9" },
    });
  });

  test("sends no after for an empty position", () => {
    expect(linkWorkItem("a1", "ENG-12", "").body).toEqual({ work_item: "ENG-12" });
  });
});

describe("folders", () => {
  test("a top-level folder says who sees it, and a subfolder says only its parent", () => {
    expect(createFolder({ name: "Marketing", visibility: "company" })).toEqual({
      method: "POST",
      path: "/artifact-folders",
      body: { name: "Marketing", visibility: "company" },
    });
    expect(createFolder({ name: "Q4", parent: "f1", visibility: "private" }).body).toEqual({
      name: "Q4",
      parent: "f1",
    });
  });

  test("a rename, a move and a widening move are one PATCH each", () => {
    expect(renameFolder("f/1", "Press")).toEqual({
      method: "PATCH",
      path: "/artifact-folders/f%2F1",
      body: { name: "Press" },
    });
    expect(moveFolder("f1", "").body).toEqual({ parent: "" });
    expect(moveFolder("f1", "f2", true).body).toEqual({ parent: "f2", confirm_widen: true });
  });

  test("a remove carries no body", () => {
    expect(removeFolder("f1")).toEqual({ method: "DELETE", path: "/artifact-folders/f1" });
  });

  test("filing one artifact, and several, each name the folder, the top level as empty", () => {
    expect(fileArtifact("a1", "")).toEqual({
      method: "PUT",
      path: "/artifacts/a1/folder",
      body: { folder: "" },
    });
    expect(fileArtifacts([{ artifact: "a1", folder: "f1" }])).toEqual({
      method: "POST",
      path: "/artifacts/folder-moves",
      body: { items: [{ artifact: "a1", folder: "f1" }] },
    });
  });

  test("a create files the new artifact where it was started, and nowhere when it was not", () => {
    expect(createSheet({ title: "Plan", folder: "f1" }).body).toEqual({
      kind: "sheet",
      title: "Plan",
      folder: "f1",
    });
    expect(createFromStarter({ kind: "doc", title: "Notes", workItem: "ENG-1" }).body).toEqual({
      kind: "doc",
      title: "Notes",
      starter: true,
      work_item: "ENG-1",
    });
  });
});
