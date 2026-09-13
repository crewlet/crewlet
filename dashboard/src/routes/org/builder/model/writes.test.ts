// @vitest-environment node
/**
 * Saves, and settling a save whose answer never arrived.
 *
 * What these protect: an unanswered save, or a 409 or 412 right after one, is
 * never taken as somebody else's write until the active revision has been
 * read; the write landed exactly when that revision's parent is the draft's
 * base and its summary carries the write id; and a conflicted draft is updated
 * only onto the conflict's revision or a descendant of it, with the engine's
 * derivation of that document.
 */

import { describe, expect, test } from "vitest";
import { fixtureCompany, fixtureDerived } from "./testkit.ts";
import type { ConfigRequest, ConfigTransport, HttpAnswer } from "./transport.ts";
import {
  UPDATE_ANCESTRY_LIMIT,
  classifySave,
  isRevisionOfWrite,
  newWriteId,
  readyToUpdate,
  settleUnknownWrite,
  signedSummary,
  summaryCarries,
  type SaveAttempt,
} from "./writes.ts";

const EDIT: SaveAttempt = { writeId: "w1234567", mode: "edit", baseRevision: "base" };
const CREATE: SaveAttempt = { writeId: "w7654321", mode: "create", baseRevision: null };
const signal = new AbortController().signal;

/** Answers `GET /config`, each revision by id, and dry runs, from a script; records every call. */
class Engine implements ConfigTransport {
  calls: string[] = [];
  constructor(
    private readonly script: {
      current?: HttpAnswer;
      revisions?: Record<string, HttpAnswer>;
      send?: HttpAnswer;
    },
  ) {}
  async current(): Promise<HttpAnswer> {
    this.calls.push("current");
    return this.script.current ?? { status: 0, body: null };
  }
  async revision(id: string): Promise<HttpAnswer> {
    this.calls.push(`revision ${id}`);
    return this.script.revisions?.[id] ?? { status: 404, body: { error: "not_found" } };
  }
  async send(request: ConfigRequest): Promise<HttpAnswer> {
    this.calls.push(
      `${request.method} ${JSON.stringify(request.query)} ${JSON.stringify(request.headers)}`,
    );
    return this.script.send ?? { status: 0, body: null };
  }
}

const revision = (parent: string | undefined, summary: string): HttpAnswer => ({
  status: 200,
  body: {
    revision_id: "x",
    summary,
    source: "api",
    created_by: "op",
    created_at: "t",
    ...(parent ? { parent_revision_id: parent } : {}),
    payload: {},
  },
});

describe("signing a save", () => {
  test("a write id is a bounded token, and the summary carries it where no sentence does", () => {
    expect(newWriteId({ next: () => "abcdefgh" })).toBe("abcdefgh");
    expect(() => newWriteId({ next: () => "short" })).toThrow(RangeError);
    const summary = signedSummary(" Moved Dev to Sales ", "abcdefgh");
    expect(summary).toBe("Moved Dev to Sales (write abcdefgh)");
    expect(summaryCarries(summary, "abcdefgh")).toBe(true);
    expect(summaryCarries("Mentioned (write abcdefgh) in passing", "abcdefgh")).toBe(false);
    expect(summaryCarries(summary, "abcdefgX")).toBe(false);
  });

  test("the revision of a write has the draft's base as its parent and the write id in its summary", () => {
    const signed = signedSummary("Edit", EDIT.writeId);
    expect(isRevisionOfWrite({ parent_revision_id: "base", summary: signed }, EDIT)).toBe(true);
    expect(isRevisionOfWrite({ parent_revision_id: "other", summary: signed }, EDIT)).toBe(false);
    expect(
      isRevisionOfWrite({ parent_revision_id: "base", summary: "A colleague's save" }, EDIT),
    ).toBe(false);
    expect(isRevisionOfWrite({ summary: signedSummary("Create", CREATE.writeId) }, CREATE)).toBe(
      true,
    );
  });
});

describe("classifySave", () => {
  test("a 201 is saved, with what the engine said about it", () => {
    const derived = fixtureDerived(fixtureCompany());
    expect(
      classifySave(
        { status: 201, body: { revision_id: "r2", epoch: 7, warnings: null, derived } },
        EDIT,
        false,
      ),
    ).toEqual({
      kind: "saved",
      revisionId: "r2",
      epoch: 7,
      warnings: [],
      derived,
    });
  });

  test("no answer is unknown, and so is a 409 or 412 right after no answer", () => {
    expect(classifySave({ status: 0, body: null }, EDIT, false)).toEqual({
      kind: "unknown",
      currentRevisionId: null,
    });
    expect(
      classifySave(
        { status: 409, body: { error: "revision_advanced", current_revision_id: "r2" } },
        EDIT,
        true,
      ),
    ).toEqual({
      kind: "unknown",
      currentRevisionId: "r2",
    });
    expect(
      classifySave(
        { status: 412, body: { error: "already_configured", current_revision_id: "r1" } },
        CREATE,
        true,
      ),
    ).toEqual({
      kind: "unknown",
      currentRevisionId: "r1",
    });
  });

  test("a 409 on a first attempt is a conflict, and a refused document its problems", () => {
    expect(
      classifySave(
        { status: 409, body: { error: "revision_advanced", current_revision_id: "r2" } },
        EDIT,
        false,
      ),
    ).toEqual({
      kind: "refused",
      outcome: { status: "conflict", reason: "revision_advanced", currentRevisionId: "r2" },
    });
    expect(
      classifySave({ status: 400, body: { error: "validation_error", detail: "bad" } }, EDIT, true),
    ).toMatchObject({
      kind: "refused",
      outcome: { status: "problems" },
    });
  });
});

describe("settleUnknownWrite", () => {
  test("landed when the revision a conflict named is this write's", async () => {
    const engine = new Engine({
      revisions: { r2: revision("base", signedSummary("Edit", EDIT.writeId)) },
    });
    expect(await settleUnknownWrite(engine, EDIT, "r2", signal)).toEqual({
      kind: "landed",
      revisionId: "r2",
    });
    expect(engine.calls).toEqual(["revision r2"]);
  });

  test("not landed when that revision is a colleague's", async () => {
    const engine = new Engine({
      revisions: { r2: revision("base", "A colleague's save (write zzzzzzzz)") },
    });
    expect(await settleUnknownWrite(engine, EDIT, "r2", signal)).toEqual({
      kind: "not_landed",
      currentRevisionId: "r2",
    });
  });

  test("without a named revision, reads the active one first", async () => {
    const landed = new Engine({
      current: { status: 200, body: {}, etag: '"r3"' },
      revisions: { r3: revision("base", signedSummary("Edit", EDIT.writeId)) },
    });
    expect(await settleUnknownWrite(landed, EDIT, null, signal)).toEqual({
      kind: "landed",
      revisionId: "r3",
    });
    expect(landed.calls).toEqual(["current", "revision r3"]);

    const still = new Engine({ current: { status: 200, body: {}, etag: '"base"' } });
    expect(await settleUnknownWrite(still, EDIT, null, signal)).toEqual({
      kind: "not_landed",
      currentRevisionId: "base",
    });
    expect(still.calls).toEqual(["current"]);
  });

  test("a create that did not land finds no company", async () => {
    const engine = new Engine({ current: { status: 404, body: { error: "no_active_revision" } } });
    expect(await settleUnknownWrite(engine, CREATE, null, signal)).toEqual({
      kind: "not_landed",
      currentRevisionId: null,
    });
  });

  test("stays unknown while the engine cannot be asked or does not hold the revision yet", async () => {
    expect(await settleUnknownWrite(new Engine({}), EDIT, null, signal)).toMatchObject({
      kind: "unknown",
    });
    expect(await settleUnknownWrite(new Engine({}), EDIT, "r9", signal)).toEqual({
      kind: "unknown",
      detail: "This node does not hold the active revision yet.",
    });
  });
});

describe("readyToUpdate", () => {
  const doc = fixtureCompany();
  const derived = fixtureDerived(doc);
  const dryRun: HttpAnswer = {
    status: 200,
    body: { valid: true, base_revision_id: "", warnings: null, derived },
  };

  test("behind while the node still serves the draft's base", async () => {
    const engine = new Engine({ current: { status: 200, body: doc, etag: '"base"' } });
    expect(
      await readyToUpdate(engine, { baseRevision: "base", conflictRevisionId: "r2" }, signal),
    ).toEqual({ kind: "behind" });
  });

  test("ready on the conflict's revision, with the engine's derivation of it from a dry run conditional on it", async () => {
    const engine = new Engine({ current: { status: 200, body: doc, etag: '"r2"' }, send: dryRun });
    expect(
      await readyToUpdate(engine, { baseRevision: "base", conflictRevisionId: "r2" }, signal),
    ).toEqual({
      kind: "ready",
      revisionId: "r2",
      document: doc,
      derived,
    });
    expect(engine.calls).toEqual(["current", 'PATCH {"dry_run":"true"} {"If-Match":"\\"r2\\""}']);
  });

  test("ready on a descendant of the conflict's revision, behind on a revision that does not descend from it", async () => {
    const descendant = new Engine({
      current: { status: 200, body: doc, etag: '"r4"' },
      revisions: { r4: revision("r3", "later"), r3: revision("r2", "later") },
      send: dryRun,
    });
    expect(
      await readyToUpdate(descendant, { baseRevision: "base", conflictRevisionId: "r2" }, signal),
    ).toMatchObject({
      kind: "ready",
      revisionId: "r4",
    });

    const sibling = new Engine({
      current: { status: 200, body: doc, etag: '"s1"' },
      revisions: { s1: revision("base", "raced") },
      send: dryRun,
    });
    expect(
      await readyToUpdate(sibling, { baseRevision: "base", conflictRevisionId: "r2" }, signal),
    ).toEqual({ kind: "behind" });
  });

  test("gives up after the ancestry limit rather than walking the whole history", async () => {
    const revisions: Record<string, HttpAnswer> = {};
    for (let i = 0; i < UPDATE_ANCESTRY_LIMIT + 5; i++)
      revisions[`n${i}`] = revision(`n${i + 1}`, "later");
    const engine = new Engine({
      current: { status: 200, body: doc, etag: '"n0"' },
      revisions,
      send: dryRun,
    });
    expect(
      await readyToUpdate(engine, { baseRevision: "base", conflictRevisionId: "r2" }, signal),
    ).toEqual({ kind: "behind" });
    expect(engine.calls.filter((c) => c.startsWith("revision"))).toHaveLength(
      UPDATE_ANCESTRY_LIMIT,
    );
  });

  test("unknown when the document cannot be read or moved again while it was", async () => {
    expect(
      await readyToUpdate(
        new Engine({}),
        { baseRevision: "base", conflictRevisionId: "r2" },
        signal,
      ),
    ).toMatchObject({
      kind: "unknown",
    });
    const moved = new Engine({
      current: { status: 200, body: doc, etag: '"r2"' },
      send: { status: 409, body: { error: "revision_advanced", current_revision_id: "r3" } },
    });
    expect(
      await readyToUpdate(moved, { baseRevision: "base", conflictRevisionId: "r2" }, signal),
    ).toMatchObject({
      kind: "unknown",
      detail: expect.stringContaining("changed again"),
    });
  });
});
