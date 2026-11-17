// @vitest-environment node
/**
 * The bridge's readers: everything that crosses the port is read field by
 * field, and anything that is not exactly a message of the contract is
 * dropped, in both directions.
 */

import { describe, expect, test } from "vitest";
import {
  BRIDGE_MESSAGE,
  BRIDGE_METHODS,
  COMMENT_LAYOUT_MAX_ANCHORS,
  capabilityOf,
  isAnchorName,
  readCall,
  readCommentCrop,
  readCommentLayout,
  readCommentPick,
  readDeclaredCapabilities,
  readDoc,
  readEvent,
  readInit,
  readPerson,
  readResult,
  readViewer,
} from "./bridge.ts";
import { ARTIFACT_COMMENT_CROP_MAX_BYTES } from "../contract/artifactsworkspace.ts";

const viewer = { seat: "maya", kind: "human", displayName: "Maya", role: "editor" };
const doc = {
  id: "r1",
  data: { name: "One" },
  version: 2,
  createdBy: "maya",
  createdAt: "2026-10-18T10:00:00Z",
  updatedBy: "agent-cto",
  updatedAt: "2026-10-18T10:05:00Z",
};

describe("a call", () => {
  test("is read with its params, which default to none", () => {
    expect(readCall({ type: BRIDGE_MESSAGE.call, id: 1, method: "identity.me" })).toEqual({
      type: BRIDGE_MESSAGE.call,
      id: 1,
      method: "identity.me",
      params: {},
    });
  });

  test("is dropped for a method the contract does not name, prototype keys included", () => {
    for (const method of ["data.drop", "constructor", "__proto__", "toString", "hasOwnProperty"]) {
      expect(readCall({ type: BRIDGE_MESSAGE.call, id: 1, method })).toBeNull();
    }
  });

  test("is dropped for an id that is not a positive whole number, or params that are not an object", () => {
    for (const id of [0, -1, 1.5, "1", Number.MAX_SAFE_INTEGER + 2]) {
      expect(readCall({ type: BRIDGE_MESSAGE.call, id, method: "identity.me" })).toBeNull();
    }
    for (const params of [null, [], "x"]) {
      expect(readCall({ type: BRIDGE_MESSAGE.call, id: 1, method: "data.get", params })).toBeNull();
    }
    expect(readCall({ type: BRIDGE_MESSAGE.result, id: 1, method: "identity.me" })).toBeNull();
    expect(readCall("bridge.call")).toBeNull();
  });
});

describe("the capabilities on the runtime's element", () => {
  test("are read as the names a version may declare, once each", () => {
    expect(readDeclaredCapabilities("db identity presence")).toEqual([
      "db",
      "identity",
      "presence",
    ]);
    expect(readDeclaredCapabilities("  db\tdb  assets ")).toEqual(["db", "assets"]);
  });
  test("leave out anything that is not one, and read nothing from no attribute", () => {
    expect(readDeclaredCapabilities("db cookies <script>")).toEqual(["db"]);
    expect(readDeclaredCapabilities("")).toEqual([]);
    expect(readDeclaredCapabilities(null)).toEqual([]);
  });
});

describe("the init message", () => {
  const init = {
    type: BRIDGE_MESSAGE.init,
    contract: "1.0.0",
    capabilities: ["db", "presence"],
    viewer,
    connection: "live",
  };

  test("is read whole", () => {
    expect(readInit(init)).toEqual(init);
    expect(readInit({ ...init, viewer: null })?.viewer).toBeNull();
  });

  test("is dropped for a capability or a connection the contract does not name", () => {
    expect(readInit({ ...init, capabilities: ["db", "network"] })).toBeNull();
    expect(readInit({ ...init, connection: "online" })).toBeNull();
    expect(readInit({ ...init, viewer: { ...viewer, role: "admin" } })).toBeNull();
  });
});

describe("a viewer", () => {
  test("carries display facts only", () => {
    expect(readViewer({ ...viewer, token: "secret", email: "maya@example.com" })).toEqual(viewer);
  });

  test("is dropped for a kind or role the contract does not name", () => {
    expect(readViewer({ ...viewer, kind: "robot" })).toBeNull();
    expect(readViewer({ ...viewer, role: "superuser" })).toBeNull();
  });
});

describe("a person", () => {
  test("carries an agent's face, and a malformed one is left off rather than refusing them", () => {
    const ada = { seat: "ada", kind: "agent", displayName: "Ada" };
    expect(readPerson({ ...ada, avatar: { character: "otter", hue: "cyan", url: "x" } })).toEqual({
      ...ada,
      avatar: { character: "otter", hue: "cyan" },
    });
    expect(readPerson({ ...ada, avatar: { character: 3 } })).toEqual(ada);
    expect(readPerson({ ...ada, displayName: undefined })).toBeNull();
  });
});

describe("a result", () => {
  test("is read as an answer or a refusal", () => {
    expect(readResult({ type: BRIDGE_MESSAGE.result, id: 3, ok: true, value: 7 })).toEqual({
      type: BRIDGE_MESSAGE.result,
      id: 3,
      ok: true,
      value: 7,
    });
    expect(
      readResult({
        type: BRIDGE_MESSAGE.result,
        id: 3,
        ok: false,
        error: { code: "forbidden", message: "no" },
      }),
    ).toMatchObject({ ok: false, error: { code: "forbidden", message: "no" } });
  });

  test("is dropped when a refusal carries no error", () => {
    expect(readResult({ type: BRIDGE_MESSAGE.result, id: 3, ok: false })).toBeNull();
    expect(readResult({ type: BRIDGE_MESSAGE.result, id: 3, ok: "yes" })).toBeNull();
  });
});

describe("a document", () => {
  test("is read, pending only when it says so", () => {
    expect(readDoc(doc)).toEqual({ ...doc, pending: false });
    expect(readDoc({ ...doc, pending: true })?.pending).toBe(true);
    expect(readDoc({ ...doc, pending: "yes" })?.pending).toBe(false);
  });

  test("is dropped without its data or its attribution", () => {
    expect(readDoc({ ...doc, data: null })).toBeNull();
    expect(readDoc({ ...doc, updatedBy: 4 })).toBeNull();
  });
});

describe("an event", () => {
  test("of data is read with every document, and dropped for one that is not", () => {
    const event = {
      type: BRIDGE_MESSAGE.event,
      topic: "data",
      subscription: 1,
      upsert: [doc],
      remove: ["r2"],
    };
    expect(readEvent(event)).toMatchObject({ topic: "data", remove: ["r2"] });
    expect(readEvent({ ...event, upsert: [doc, { id: "bad" }] })).toBeNull();
    expect(readEvent({ ...event, remove: [3] })).toBeNull();
  });

  test("of presence keeps only the focus fields the contract names", () => {
    const event = readEvent({
      type: BRIDGE_MESSAGE.event,
      topic: "presence",
      roster: {
        members: [
          {
            session: "s1",
            seat: "maya",
            kind: "human",
            displayName: "Maya Ortiz",
            activity: "editing",
            at: "2026-10-18T10:00:00Z",
            focus: { version: 2, doc: "r1", field: "name", cookie: "x" },
          },
        ],
        counts: { human: 1, agent: 0 },
        truncated: false,
      },
    });
    expect(event).toMatchObject({
      topic: "presence",
      roster: {
        members: [{ displayName: "Maya Ortiz", focus: { version: 2, doc: "r1", field: "name" } }],
        counts: { human: 1, agent: 0 },
      },
    });
    if (event?.topic !== "presence") throw new Error("not a presence event");
    expect(event.roster.members[0]?.focus).not.toHaveProperty("cookie");
  });

  test("of a topic the contract does not name is dropped", () => {
    expect(readEvent({ type: BRIDGE_MESSAGE.event, topic: "navigate", url: "x" })).toBeNull();
    expect(readEvent({ type: BRIDGE_MESSAGE.event, topic: "connection", state: "up" })).toBeNull();
  });
});

describe("the comment layer", () => {
  const event = (fields: Record<string, unknown>) =>
    readEvent({ type: BRIDGE_MESSAGE.event, topic: "comments", ...fields });

  test("hears the host's mode, a part to reveal and a part to draw", () => {
    expect(event({ mode: "place" })).toEqual({
      type: BRIDGE_MESSAGE.event,
      topic: "comments",
      mode: "place",
    });
    expect(event({ reveal: { name: "pricing" } })).toMatchObject({ reveal: { name: "pricing" } });
    expect(event({ reveal: { point: { x: 0.25, y: 2 } } })).toMatchObject({
      reveal: { point: { x: 0.25, y: 1 } },
    });
    expect(event({ capture: { id: 3, name: "plans.annual" } })).toMatchObject({
      capture: { id: 3, name: "plans.annual" },
    });
    // A POINT OF THE VIEWPORT is in pixels, never held to a fraction.
    expect(event({ capture: { id: 4, view: { x: 30, y: 1200 } } })).toMatchObject({
      capture: { id: 4, view: { x: 30, y: 1200 } },
    });
  });

  test("drops a request that is not exactly one", () => {
    for (const fields of [
      { mode: "edit" },
      { reveal: { name: "Pricing" } },
      { reveal: { point: { x: Number.NaN, y: 0 } } },
      { reveal: {} },
      { capture: { id: 0, name: "pricing" } },
      { capture: { id: 1.5, point: { x: 0, y: 0 } } },
      { capture: { id: 2 } },
      { capture: { id: 5, view: { x: 1, y: Number.POSITIVE_INFINITY } } },
      { capture: { id: 6, view: "30,40" } },
      {},
    ]) {
      expect(event(fields)).toBeNull();
    }
  });

  test("takes a name the engine indexes, and no other", () => {
    for (const name of ["pricing", "0", "plans.annual", "a-b_c", "x".repeat(64)]) {
      expect(isAnchorName(name)).toBe(true);
    }
    for (const name of ["", "Pricing", "-lead", ".x", "a b", "x".repeat(65), 7, null]) {
      expect(isAnchorName(name)).toBe(false);
    }
  });

  const layout = {
    anchors: [{ name: "pricing", x: 0, y: 640, w: 1200, h: 480 }],
    doc: { w: 1280, h: 4000 },
    scroll: { x: 0, y: 320 },
    viewport: { w: 1280, h: 800 },
  };

  test("reads a layout whole, the hovered anchor with it while placing", () => {
    expect(readCommentLayout(layout)).toEqual(layout);
    expect(readCommentLayout({ ...layout, hover: "pricing" })?.hover).toBe("pricing");
    expect(readCommentLayout({ ...layout, hover: null })).not.toHaveProperty("hover");
  });

  test("drops a layout with a box that is not finite pixels, a name it refuses, or too many anchors", () => {
    const anchor = layout.anchors[0]!;
    for (const bad of [
      { ...layout, anchors: [{ ...anchor, name: "Pricing" }] },
      { ...layout, anchors: [{ ...anchor, w: -1 }] },
      { ...layout, anchors: [{ ...anchor, y: Number.POSITIVE_INFINITY }] },
      { ...layout, anchors: [{ ...anchor, x: "0" }] },
      { ...layout, doc: { w: 0, h: 10 } },
      { ...layout, scroll: { x: Number.NaN, y: 0 } },
      { ...layout, viewport: { w: -1, h: 0 } },
      { ...layout, hover: "Not a name" },
      { ...layout, anchors: "pricing" },
      { ...layout, anchors: Array.from({ length: COMMENT_LAYOUT_MAX_ANCHORS + 1 }, () => anchor) },
    ]) {
      expect(readCommentLayout(bad)).toBeNull();
    }
  });

  test("reads a pick, its point held to the page, and refuses a quote past 1 KiB", () => {
    expect(
      readCommentPick({ name: "pricing", quote: "Plans", point: { x: 0.5, y: -0.1 } }),
    ).toEqual({ name: "pricing", quote: "Plans", point: { x: 0.5, y: 0 } });
    expect(readCommentPick({ name: null, point: { x: 0.1, y: 0.2 } })).toEqual({
      name: null,
      quote: "",
      point: { x: 0.1, y: 0.2 },
    });
    // 342 three-byte letters are 1026 bytes: over, though 342 characters.
    expect(readCommentPick({ name: null, quote: "€".repeat(342), point: { x: 0, y: 0 } })).toBe(
      null,
    );
    expect(readCommentPick({ name: "x", quote: "€".repeat(341), point: { x: 0, y: 0 } })).not.toBe(
      null,
    );
    expect(readCommentPick({ name: "Bad", quote: "", point: { x: 0, y: 0 } })).toBeNull();
    expect(readCommentPick({ name: null, quote: "" })).toBeNull();
  });

  test("reads a picture as one the engine would keep, and anything else as none", () => {
    const png = "data:image/png;base64,iVBORw0KGgo=";
    expect(readCommentCrop({ id: 2, image: png })).toEqual({ id: 2, image: png });
    expect(readCommentCrop({ id: 2, image: null })).toEqual({ id: 2, image: null });
    expect(readCommentCrop({ id: 2, image: "data:image/svg+xml;base64,PHN2Zz4=" })).toEqual({
      id: 2,
      image: null,
    });
    const big = "A".repeat(Math.ceil((ARTIFACT_COMMENT_CROP_MAX_BYTES * 4) / 3) + 8);
    expect(readCommentCrop({ id: 2, image: `data:image/webp;base64,${big}` })?.image).toBeNull();
    expect(readCommentCrop({ id: 0, image: png })).toBeNull();
    expect(readCommentCrop({ image: png })).toBeNull();
  });
});

describe("every method", () => {
  test("needs the capability the contract gives it, and only the host's own and the comment layer's need none", () => {
    const free = (Object.keys(BRIDGE_METHODS) as (keyof typeof BRIDGE_METHODS)[]).filter(
      (m) => capabilityOf(m) === null,
    );
    expect(free).toEqual([
      "openLink",
      "print",
      "comments.layout",
      "comments.pick",
      "comments.crop",
      "comments.escape",
    ]);
    expect(capabilityOf("data.write")).toBe("db");
    expect(capabilityOf("state.set")).toBe("state");
    expect(capabilityOf("data.undo")).toBe("db");
    expect(capabilityOf("data.redo")).toBe("db");
    expect(capabilityOf("identity.people")).toBe("identity");
  });
});
