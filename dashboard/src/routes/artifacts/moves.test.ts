/**
 * Filing artifacts: one through its own route and several in batches the
 * engine decides artifact by artifact, and what a move says it did, naming
 * what did not move and why.
 */

import { afterEach, describe, expect, test, vi } from "vitest";
import { keyedWrite, type KeyedAnswer, type KeyedRequest } from "~/lib/keyedWrite.ts";
import { fileEach, movedWords, refusedWords, type Movable } from "./moves.ts";

vi.mock("~/lib/keyedWrite.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/keyedWrite.ts")>("~/lib/keyedWrite.ts");
  return { ...actual, keyedWrite: vi.fn() };
});

const sent: KeyedRequest[] = [];

function items(n: number): Movable[] {
  return Array.from({ length: n }, (_, i) => ({ id: `a${i}`, title: `Doc ${i}`, folder: "" }));
}

afterEach(() => {
  sent.length = 0;
  vi.mocked(keyedWrite).mockReset();
});

describe("fileEach", () => {
  test("one artifact goes through its own route, keyed", async () => {
    vi.mocked(keyedWrite).mockImplementation(async (req) => {
      sent.push(req);
      return { kind: "done", pending: false, body: { artifact: "a0", folder: "f1" } };
    });
    const report = await fileEach(items(1), () => "f1");
    expect(sent).toHaveLength(1);
    expect(sent[0]).toMatchObject({
      method: "PUT",
      path: "/artifacts/a0/folder",
      body: { folder: "f1" },
    });
    expect(sent[0]!.key).toBeTruthy();
    expect(report.moved.map((m) => m.id)).toEqual(["a0"]);
  });

  test("several go in batches of the engine's bound, each artifact answered apart", async () => {
    vi.mocked(keyedWrite).mockImplementation(async (req): Promise<KeyedAnswer> => {
      sent.push(req);
      const body = req.body as { items: { artifact: string }[] };
      return {
        kind: "done",
        pending: false,
        body: {
          results: body.items.map((item) =>
            item.artifact === "a3"
              ? { artifact: item.artifact, outcome: "unauthorized" }
              : { artifact: item.artifact, outcome: "moved" },
          ),
        },
      };
    });
    const report = await fileEach(items(30), () => "f1");
    expect(sent.map((r) => (r.body as { items: unknown[] }).items.length)).toEqual([25, 5]);
    expect(new Set(sent.map((r) => r.key)).size).toBe(2);
    expect(report.moved).toHaveLength(29);
    expect(report.refused).toEqual([
      {
        item: { id: "a3", title: "Doc 3", folder: "" },
        why: "Moving it needs the “Can edit” role on it.",
      },
    ]);
  });

  test("a batch nobody could confirm is reported as not moved, in the engine's words", async () => {
    vi.mocked(keyedWrite).mockResolvedValue({
      kind: "unknown",
      key: "k",
      text: "Try again. It won’t happen twice.",
    });
    const report = await fileEach(items(2), () => "f1");
    expect(report.moved).toEqual([]);
    expect(report.refused.map((r) => r.why)).toEqual([
      "Try again. It won’t happen twice.",
      "Try again. It won’t happen twice.",
    ]);
  });

  test("Undo files each artifact back where it was", async () => {
    vi.mocked(keyedWrite).mockImplementation(async (req) => {
      sent.push(req);
      return { kind: "done", pending: false, body: { results: [] } };
    });
    await fileEach(
      [
        { id: "a", title: "A", folder: "f1" },
        { id: "b", title: "B", folder: "" },
      ],
      (m) => m.folder,
    );
    expect((sent[0]!.body as { items: unknown[] }).items).toEqual([
      { artifact: "a", folder: "f1" },
      { artifact: "b", folder: "" },
    ]);
  });
});

describe("what a move says", () => {
  const target = { id: "f1", name: "Press kit" };
  const one = { id: "a", title: "Q4 board deck", folder: "" };
  const two = { id: "b", title: "City breakdown", folder: "" };

  test("all moved, one by name and several by count, and the top level in words", () => {
    expect(movedWords({ moved: [one], refused: [] }, target)).toBe(
      "Moved “Q4 board deck” to Press kit",
    );
    expect(movedWords({ moved: [one, two], refused: [] }, target)).toBe(
      "Moved 2 artifacts to Press kit",
    );
    expect(movedWords({ moved: [one], refused: [] }, { id: "", name: "" })).toBe(
      "Moved “Q4 board deck” to the top level",
    );
  });

  test("part moved names the count, and the first refusal by name with how many more", () => {
    const report = { moved: [one], refused: [{ item: two, why: "You are a viewer on it." }] };
    expect(movedWords(report, target)).toBe("Moved 1 of 2 to Press kit");
    expect(refusedWords(report)).toBe("“City breakdown” was not moved. You are a viewer on it.");
    expect(
      refusedWords({
        moved: [],
        refused: [
          { item: one, why: "It is in the trash. Restore it first." },
          { item: two, why: "x" },
        ],
      }),
    ).toBe(
      "“Q4 board deck” was not moved. It is in the trash. Restore it first. 1 other was not moved either.",
    );
    expect(movedWords({ moved: [], refused: [{ item: one, why: "x" }] }, target)).toBe(
      "It was not moved",
    );
  });
});
