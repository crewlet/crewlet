// @vitest-environment node
/**
 * One artifact's datastore on this tab: reads and pushes converge by version
 * whatever order they land in, and a reader's own edit is drawn at once and
 * settled honestly by what the engine answered (D23).
 */

import { afterEach, describe, expect, test, vi } from "vitest";
import { RestError, type DataDoc, type DataDocPage, type LiveSocket } from "~/protocol/index.ts";
import type { KeyedAnswer, KeyedRequest } from "../keyedWrite.ts";
import { ArtifactDataStore, packedPosition, resultOf } from "./datastore.ts";

afterEach(() => {
  vi.useRealTimers();
});

const actor = { Name: "maya", Kind: "human" };

function doc(
  id: string,
  version: number,
  fields: Record<string, unknown>,
  deleted = false,
): DataDoc {
  return {
    collection: "rows",
    id,
    fields,
    version,
    ...(deleted ? { deleted: true } : {}),
    created_by: actor,
    created_at: "2026-10-18T10:00:00Z",
    updated_by: actor,
    updated_at: "2026-10-18T10:00:00Z",
  };
}

function page(docs: DataDoc[], through: number): DataDocPage {
  return {
    docs,
    through,
    position: { stream: "data", generation: 0, seq: through },
    complete: true,
  };
}

/** A store over a socket whose collection read answers `read`, and a write the suite decides. */
function store(
  read: () => Promise<DataDocPage>,
  write: (req: KeyedRequest) => Promise<KeyedAnswer>,
) {
  const query = vi.fn(async (kind: string) => {
    if (kind === "artifact_data") return read();
    if (kind === "artifact_data_doc") throw new Error("not asked in this suite");
    return {};
  });
  const sent: KeyedRequest[] = [];
  let n = 0;
  const s = new ArtifactDataStore({
    artifact: "a1",
    socket: { query } as unknown as LiveSocket,
    via: { via: "dashboard" },
    clientID: "client-1",
    newKey: () => `key-${++n}`,
    write: async (req) => {
      sent.push(req);
      return write(req);
    },
  });
  return { s, sent, query };
}

const applied = (seq: number, position: number): KeyedAnswer => ({
  kind: "done",
  pending: false,
  body: { outcome: "applied", seq, position: { generation: 0, seq: position } },
});

const fieldsOf = (s: ArtifactDataStore, id: string) =>
  s.docsOf("rows").find((d) => d.doc.id === id);

describe("reading", () => {
  test("a push newer than the read wins, and a stale one never brings a deleted row back", async () => {
    let release: (p: DataDocPage) => void = () => {};
    const { s } = store(
      () => new Promise((resolve) => (release = resolve)),
      async () => applied(1, 1),
    );
    const loading = s.load("rows");
    // A PUSH LANDS WHILE THE READ IS OUT: r1 moved to version 5.
    s.applyPush({
      artifact: "a1",
      deltas: [
        {
          collection: "rows",
          doc: "r1",
          version: 5,
          body: { name: "Pushed" },
          read: "all",
          created_by: "maya",
          updated_by: actor,
          updated_at: "2026-10-18T10:01:00Z",
        },
      ],
    });
    release(page([doc("r1", 3, { name: "Read" }), doc("r2", 4, { name: "Two" })], 4));
    await loading;
    expect(fieldsOf(s, "r1")?.fields).toEqual({ name: "Pushed" });

    // r2 IS DELETED at 6, and an older delta for it arrives after.
    s.applyPush({
      artifact: "a1",
      deltas: [
        {
          collection: "rows",
          doc: "r2",
          version: 6,
          deleted: true,
          read: "all",
          created_by: "maya",
          updated_by: actor,
          updated_at: "2026-10-18T10:02:00Z",
        },
      ],
    });
    s.applyPush({
      artifact: "a1",
      deltas: [
        {
          collection: "rows",
          doc: "r2",
          version: 4,
          body: { name: "Two" },
          read: "all",
          created_by: "maya",
          updated_by: actor,
          updated_at: "2026-10-18T10:00:00Z",
        },
      ],
    });
    expect(fieldsOf(s, "r2")).toBeUndefined();
  });

  test("the packed position is the generation above the sequence", () => {
    expect(packedPosition({ stream: "data", generation: 2, seq: 7 })).toBe(2 * 2 ** 40 + 7);
    expect(packedPosition(undefined)).toBe(0);
  });
});

describe("an edit", () => {
  test("is drawn at once, pending, and gives way to the engine's body once it catches up", async () => {
    vi.useFakeTimers();
    let answer: (a: KeyedAnswer) => void = () => {};
    const { s, sent } = store(
      async () => page([doc("r1", 3, { name: "Old" })], 3),
      () => new Promise((resolve) => (answer = resolve)),
    );
    await s.load("rows");
    const writing = s.edit("rows", [{ rowId: "r1", field: "name", value: "New" }]);
    expect(fieldsOf(s, "r1")).toMatchObject({ fields: { name: "New" }, pending: true });
    expect(sent[0]).toMatchObject({ key: "key-1" });

    answer(applied(11, 9));
    await writing;
    // CONFIRMED, and still drawn over the old body until the document reaches the write.
    expect(fieldsOf(s, "r1")).toMatchObject({ fields: { name: "New" }, pending: false });
    s.applyPush({
      artifact: "a1",
      deltas: [
        {
          collection: "rows",
          doc: "r1",
          version: 9,
          body: { name: "New, as stored" },
          read: "all",
          created_by: "maya",
          updated_by: actor,
          updated_at: "2026-10-18T10:03:00Z",
        },
      ],
    });
    expect(fieldsOf(s, "r1")?.fields).toEqual({ name: "New, as stored" });
    s.stop();
  });

  test("the engine refused reverts the cell and says why on it", async () => {
    const { s } = store(
      async () => page([doc("r1", 3, { name: "Old" })], 3),
      async () => ({
        kind: "refused",
        text: "Refused",
        stale: false,
        error: new RestError(409, {
          error: "write_refused",
          verdict: "forbidden",
          detail: "Only the reviewer sets this.",
        }),
      }),
    );
    await s.load("rows");
    await s.edit("rows", [{ rowId: "r1", field: "name", value: "New" }]);
    expect(fieldsOf(s, "r1")).toMatchObject({ fields: { name: "Old" }, pending: false });
    expect(s.pending.refusal("r1", "name")).toBe("Only the reviewer sets this.");
  });

  test("whose outcome nobody knows is kept, and Retry sends exactly it again", async () => {
    const answers: KeyedAnswer[] = [
      { kind: "unknown", key: "key-1", text: "The engine could not be reached." },
    ];
    const { s, sent } = store(
      async () => page([doc("r1", 3, { name: "Old" })], 3),
      async () => answers.shift() ?? applied(12, 10),
    );
    await s.load("rows");
    vi.useFakeTimers();
    await s.edit("rows", [{ rowId: "r1", field: "name", value: "New" }]);
    expect(s.unsettledCount).toBe(1);
    expect(fieldsOf(s, "r1")?.fields).toEqual({ name: "New" });
    await s.retry();
    expect(s.unsettledCount).toBe(0);
    expect(sent).toHaveLength(2);
    expect(sent[1]).toBe(sent[0]);
    s.stop();
  });
});

describe("a keyed answer", () => {
  test("is read as the write it was", () => {
    expect(resultOf(applied(4, 9))).toEqual({ verdict: "applied", seq: 4, position: 9 });
    expect(
      resultOf({ kind: "done", pending: true, body: { position: { generation: 0, seq: 9 } } }),
    ).toEqual({ verdict: "pending", position: 9 });
    expect(
      resultOf({
        kind: "refused",
        text: "Slow down",
        stale: false,
        error: new RestError(429, { error: "write_budget" }, 2),
      }),
    ).toEqual({
      verdict: "refused",
      code: "write_budget",
      reason: "Too many changes at once. Wait a moment, then try again.",
      retryAfterMs: 2000,
    });
    expect(
      resultOf({
        kind: "refused",
        text: "Moved on",
        stale: true,
        error: new RestError(409, {
          error: "write_refused",
          verdict: "conflict",
          conflict: [doc("r1", 4, {})],
        }),
      }),
    ).toMatchObject({ verdict: "conflict", reason: "Moved on", conflict: [{ id: "r1" }] });
  });
});
