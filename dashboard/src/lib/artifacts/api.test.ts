// @vitest-environment node
/** The artifacts' write requests, as the engine reads them. */

import { describe, expect, test } from "vitest";
import {
  createFolder,
  createFromStarter,
  createSheet,
  cropBody,
  editRemark,
  fileArtifact,
  fileArtifacts,
  linkWorkItem,
  moveFolder,
  openComment,
  removeFolder,
  removeRemark,
  renameFolder,
  reopenComment,
  replyToComment,
  resolveComment,
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

describe("comments", () => {
  const PNG = "data:image/png;base64,iVBORw0KGgo=";

  test("an open names the version on show and the anchor, and carries the picture as its bytes and type", () => {
    expect(
      openComment("a/1", {
        body: "Too dense",
        version: 3,
        anchor: { kind: "html", page: "index.html", name: "pricing" },
        quote: "Plans from $10",
        crop: PNG,
      }),
    ).toEqual({
      method: "POST",
      path: "/artifacts/a%2F1/comments",
      body: {
        body: "Too dense",
        version: 3,
        anchor: { kind: "html", page: "index.html", name: "pricing" },
        quote: "Plans from $10",
        crop: "iVBORw0KGgo=",
        crop_type: "image/png",
      },
    });
  });

  test("an open on the whole version carries no quote and no picture", () => {
    expect(openComment("a1", { body: "Ship it", version: 2, anchor: { kind: "version" } })).toEqual(
      {
        method: "POST",
        path: "/artifacts/a1/comments",
        body: { body: "Ship it", version: 2, anchor: { kind: "version" } },
      },
    );
  });

  // Mutation: drop the type check and an SVG the page drew is sent as a crop the engine refuses.
  test("a picture the engine would not read is left out rather than sent", () => {
    expect(cropBody("data:image/svg+xml;base64,PHN2Zz4=")).toBeNull();
    expect(cropBody("data:image/png,raw")).toBeNull();
    expect(cropBody("https://example.com/a.png")).toBeNull();
    expect(cropBody("data:image/webp;base64,UklGRg==")).toEqual({
      crop: "UklGRg==",
      crop_type: "image/webp",
    });
    expect(
      openComment("a1", {
        body: "x",
        version: 1,
        anchor: { kind: "version" },
        crop: "data:image/gif;base64,R0lGOD==",
      }).body,
    ).toEqual({ body: "x", version: 1, anchor: { kind: "version" } });
  });

  test("a reply, an edit and a removal each address the thread, and the remark where one is named", () => {
    expect(replyToComment("a1", "t/1", "Agreed")).toEqual({
      method: "POST",
      path: "/artifacts/a1/comments/t%2F1/replies",
      body: { body: "Agreed" },
    });
    expect(replyToComment("a1", "t1", "Agreed", "r1").body).toEqual({
      body: "Agreed",
      reply_to: "r1",
    });
    expect(editRemark("a1", "t1", "r/2", "Fixed")).toEqual({
      method: "PATCH",
      path: "/artifacts/a1/comments/t1/remarks/r%2F2",
      body: { body: "Fixed" },
    });
    expect(removeRemark("a1", "t1", "r2")).toEqual({
      method: "DELETE",
      path: "/artifacts/a1/comments/t1/remarks/r2",
    });
  });

  test("a resolve names the version that addressed it where one is said, and a reopen names none", () => {
    expect(resolveComment("a1", "t1", 5)).toEqual({
      method: "POST",
      path: "/artifacts/a1/comments/t1/resolve",
      body: { version: 5 },
    });
    expect(resolveComment("a1", "t1").body).toEqual({});
    expect(reopenComment("a1", "t1")).toEqual({
      method: "POST",
      path: "/artifacts/a1/comments/t1/reopen",
      body: {},
    });
  });
});
