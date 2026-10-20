// @vitest-environment node
/**
 * One artifact's datastore on this tab: reads and pushes converge by version
 * whatever order they land in, and a reader's own edit is drawn at once and
 * settled honestly by what the engine answered (D23).
 */

import { afterEach, describe, expect, test, vi } from "vitest";
import {
  QueryError,
  RestError,
  type DataDoc,
  type DataDocPage,
  type LiveSocket,
} from "~/protocol/index.ts";
import type { KeyedAnswer, KeyedRequest } from "../keyedWrite.ts";
import { ArtifactDataStore, packedPosition, resultOf, stepRefusal } from "./datastore.ts";

afterEach(() => {
  vi.useRealTimers();
});

const actor = { name: "maya", kind: "human" };

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
  schema: unknown = {},
) {
  const query = vi.fn(async (kind: string) => {
    if (kind === "artifact_data") return read();
    if (kind === "artifact_data_schema") return schema;
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
  body: { verdict: "applied", seq, position: { generation: 0, seq: position } },
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

  test("a paged read adopts its through only once the last page is in, and the lowest one", async () => {
    const answers: (DataDocPage | Error)[] = [
      // A WHOLE READ OF TWO PAGES: the second was read later, at 12.
      { ...page([doc("r1", 3, { name: "One" })], 10), after: "p2" },
      page([doc("r2", 11, { name: "Two" })], 12),
      // A CATCH-UP FROM 10 whose second page fails.
      { ...page([doc("r3", 13, { name: "Three" })], 13), after: "p2" },
      new Error("the socket dropped"),
      // AND THE NEXT ONE.
      page([], 15),
    ];
    const asked: Record<string, unknown>[] = [];
    const query = vi.fn(async (kind: string, params: Record<string, unknown>) => {
      if (kind !== "artifact_data") return {};
      asked.push(params);
      const next = answers.shift();
      if (!next || next instanceof Error) throw next ?? new Error("not scripted");
      return next;
    });
    const s = new ArtifactDataStore({
      artifact: "a1",
      socket: { query } as unknown as LiveSocket,
      via: { via: "dashboard" },
      clientID: "client-1",
      newKey: () => "key",
      write: async () => applied(1, 1),
    });
    await s.load("rows");
    await s.catchUp("rows");
    expect(asked[2]).toMatchObject({ since: 10 });
    await s.catchUp("rows");
    expect(asked[4]).toMatchObject({ since: 10 });
    expect(asked[4]).not.toHaveProperty("cursor");
    expect(fieldsOf(s, "r3")?.fields).toEqual({ name: "Three" });
  });

  test("a refused read says it failed; a fault of the dashboard's own also reaches the console", async () => {
    const refused = store(
      async () => {
        throw new QueryError("query_failed");
      },
      async () => applied(1, 1),
    );
    await refused.s.load("rows");
    expect(refused.s.view("rows")).toMatchObject({ status: "failed" });
    // A page the dashboard cannot read (a `docs` that came back null) is no
    // refusal of the engine's: the view says the read failed, and the fault
    // is not swallowed.
    const broken = store(
      async () => ({ ...page([], 1), docs: null }) as unknown as DataDocPage,
      async () => applied(1, 1),
    );
    await expect(broken.s.load("rows")).rejects.toThrow(TypeError);
    expect(broken.s.view("rows")).toMatchObject({ status: "failed" });
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
    expect(s.pending.refusal("r1", "name")).toBe("Not saved: you cannot change this.");
  });

  test("of long text names the version it edited, which the engine requires", async () => {
    const { s, sent } = store(
      async () => page([doc("r1", 3, { name: "Gate", notes: "Old" })], 3),
      async () => applied(4, 4),
      {
        collections: [
          {
            name: "rows",
            columns: [
              { key: "name", label: "Name", type: "text" },
              { key: "notes", label: "Notes", type: "longtext" },
            ],
          },
        ],
      },
    );
    await s.loadSchema();
    await s.load("rows");
    await s.edit("rows", [{ rowId: "r1", field: "notes", value: "New" }]);
    await s.edit("rows", [{ rowId: "r1", field: "name", value: "Renamed" }]);
    const ops = sent.map((req) => (req.body as { ops: { if_version?: number }[] }).ops[0]);
    expect(ops[0]).toMatchObject({ op: "merge", doc: "r1", if_version: 3 });
    // A SHORT VALUE IS NOT GUARDED: the engine merges it field by field.
    expect(ops[1]).not.toHaveProperty("if_version");
    s.stop();
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
    // THE BODY SAYS PENDING TOO, in the one field that says how a write ended.
    expect(
      resultOf({
        kind: "done",
        pending: false,
        body: { verdict: "pending", position: { generation: 0, seq: 9 } },
      }),
    ).toEqual({ verdict: "pending", position: 9 });
    expect(
      resultOf({
        kind: "refused",
        text: "Slow down",
        stale: false,
        error: new RestError(429, { error: "write_budget" }, 2),
      }),
    ).toEqual({
      verdict: "rate_limited",
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
    ).toMatchObject({
      verdict: "conflict",
      reason: "Not saved: it changed since it was read.",
      conflict: [{ id: "r1" }],
    });
  });

  test("is the verdict a refusal before the log means, as the applier would say it", () => {
    const refused = (status: number, body: Record<string, unknown>) =>
      resultOf({
        kind: "refused",
        text: "Refused.",
        stale: false,
        error: new RestError(status, body),
      });
    expect(refused(422, { error: "invalid", detail: "score is a number" })).toEqual({
      verdict: "invalid",
      reason: "Refused.",
    });
    expect(refused(403, { error: "forbidden" })).toEqual({
      verdict: "forbidden",
      reason: "You can view this but not change it.",
    });
    expect(refused(409, { error: "artifact_trashed" })).toEqual({
      verdict: "gone",
      reason: "This artifact is in the trash, so nothing can be changed.",
    });
    expect(refused(410, { error: "data_gone" })).toEqual({
      verdict: "gone",
      reason: "This artifact was deleted, so nothing can be changed.",
    });
    // A QUOTA IS THE APPLIER'S VERDICT and the engine's one 409 for it, read
    // by its code where the verdict is missing — never a conflict.
    expect(refused(409, { error: "quota_exceeded", verdict: "quota" })).toMatchObject({
      verdict: "quota",
    });
    expect(refused(409, { error: "quota_exceeded" })).toMatchObject({ verdict: "quota" });
    expect(refused(409, { error: "stale_version" })).toMatchObject({ verdict: "conflict" });
    expect(refused(409, { error: "not_undoable" })).toMatchObject({ verdict: "invalid" });
    // THE APPLIER'S DETAIL IS NEVER SAID: it names op indices and protocol
    // terms for the agent tools, and the reader is told the verdict in words.
    expect(
      refused(422, {
        error: "invalid",
        verdict: "invalid",
        detail:
          "op 0: notes is long text, written whole only guarded: name the if_version you edited, or append",
      }),
    ).toEqual({
      verdict: "invalid",
      reason: "Not saved: the value does not fit this column.",
    });
  });
});

describe("an undo", () => {
  test("the engine decided nothing about is the reader's to take again", async () => {
    const answers: KeyedAnswer[] = [
      applied(4, 4),
      {
        kind: "refused",
        text: "Slow down",
        stale: false,
        error: new RestError(429, { error: "write_budget" }, 1),
      },
      applied(5, 5),
    ];
    const { s, sent } = store(
      async () => page([], 0),
      async () => answers.shift() ?? applied(9, 9),
    );
    await s.load("rows");
    await s.write([{ op: "merge", collection: "rows", doc: "r1", fields: { name: "New" } }]);
    expect(await s.undo()).toMatchObject({ verdict: "rate_limited" });
    // THE SAME STEP AGAIN, naming the same write: nothing was decided.
    expect(await s.undo()).toMatchObject({ verdict: "applied", seq: 5 });
    expect(sent.slice(1).map((r) => (r.body as { seq: number }).seq)).toEqual([4, 4]);
    expect(await s.undo()).toBeNull();
    s.stop();
  });

  test("says a collaborator overtook it only for a conflict", () => {
    expect(stepRefusal({ verdict: "applied", seq: 1, position: 1 }, "undone")).toBeNull();
    expect(stepRefusal({ verdict: "conflict", reason: "moved" }, "undone")).toBe(
      "That change was overtaken by another and cannot be undone.",
    );
    expect(stepRefusal({ verdict: "rate_limited", reason: "Wait a moment." }, "redone")).toBe(
      "Wait a moment.",
    );
    expect(stepRefusal({ verdict: "invalid", reason: "No longer in the history." }, "undone")).toBe(
      "No longer in the history.",
    );
  });
});
