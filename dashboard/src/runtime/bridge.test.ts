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
  capabilityOf,
  readCall,
  readDoc,
  readEvent,
  readInit,
  readResult,
  readViewer,
} from "./bridge.ts";

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
        members: [{ focus: { version: 2, doc: "r1", field: "name" } }],
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

describe("every method", () => {
  test("needs the capability the contract gives it, and only openLink and print need none", () => {
    const free = (Object.keys(BRIDGE_METHODS) as (keyof typeof BRIDGE_METHODS)[]).filter(
      (m) => capabilityOf(m) === null,
    );
    expect(free).toEqual(["openLink", "print"]);
    expect(capabilityOf("data.write")).toBe("db");
    expect(capabilityOf("state.set")).toBe("state");
  });
});
