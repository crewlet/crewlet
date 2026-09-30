// @vitest-environment node
/**
 * The settings write of a save, settling one whose answer never arrived, and
 * reading the company a draft stands on.
 *
 * What these protect: an unanswered settings write is never taken as somebody
 * else's until the line of revisions back to the draft's base has been read;
 * the write landed exactly when a revision in that line has the base as its
 * parent and the write id in its summary, even under a colleague's later save;
 * a conflicted draft is updated only onto the conflict's revision or a
 * descendant of it; and a reading of the company is the settings AND the chart,
 * or says why it is not.
 */

import { describe, expect, test } from "vitest";
import { chartOf, fixtureChart, fixtureSettings } from "./testkit.ts";
import type { EngineTransport, HttpAnswer } from "./transport.ts";
import {
  UPDATE_ANCESTRY_LIMIT,
  isRevisionOfWrite,
  isWriteId,
  readChart,
  readCompany,
  readUpdate,
  readyToUpdate,
  saveStepID,
  settleUnknownWrite,
  signedSummary,
  summaryCarries,
  type SaveAttempt,
} from "./writes.ts";

const EDIT: SaveAttempt = { writeId: "w1234567", mode: "edit", baseRevision: "base" };
const CREATE: SaveAttempt = { writeId: "w7654321", mode: "create", baseRevision: null };
const signal = new AbortController().signal;

/** Answers `GET /config`, each revision by id, and `GET /chart` from a script; records every call. */
class Engine implements EngineTransport {
  calls: string[] = [];
  constructor(
    private readonly script: {
      settings?: HttpAnswer;
      revisions?: Record<string, HttpAnswer>;
      chart?: HttpAnswer;
    },
  ) {}
  async settings(): Promise<HttpAnswer> {
    this.calls.push("settings");
    return this.script.settings ?? { status: 0, body: null };
  }
  async revision(id: string): Promise<HttpAnswer> {
    this.calls.push(`revision ${id}`);
    return this.script.revisions?.[id] ?? { status: 404, body: { error: "not_found" } };
  }
  async chart(): Promise<HttpAnswer> {
    this.calls.push("chart");
    return this.script.chart ?? { status: 0, body: null };
  }
  async send(): Promise<HttpAnswer> {
    throw new Error("nothing here writes");
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
  test("a write id is an operation id, and the summary carries it where no sentence does", () => {
    // THE ENGINE'S GRAMMAR (statelog.NewOpID): the chart surface refuses a
    // key that is not one. A random token of the old shape is not a write id.
    const id = "01a0f246-7d2d-7c92-b7f0-78dd8229b774";
    expect(isWriteId(id)).toBe(true);
    expect(isWriteId("abcdefgh")).toBe(false);
    expect(isWriteId("0123456789abcdef0123456789abcdef")).toBe(false);
    // A v4 uuid carries no instant, so it is not one either.
    expect(isWriteId("3f2504e0-4f89-41d3-9a0c-0305e82c3301")).toBe(false);
    // Every step is a step of the save's own operation, and inherits its instant.
    expect(saveStepID(id, 2)).toBe(`${id}.builder-save.2`);
    const summary = signedSummary(" Moved Dev to Sales ", id);
    expect(summary).toBe(`Moved Dev to Sales (write ${id})`);
    expect(summaryCarries(summary, id)).toBe(true);
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

describe("settleUnknownWrite", () => {
  test("landed when the revision a conflict named is this write's", async () => {
    const engine = new Engine({
      revisions: { r2: revision("base", signedSummary("Edit", EDIT.writeId)) },
    });
    expect(await settleUnknownWrite(engine, EDIT, "r2", signal)).toEqual({
      kind: "landed",
      revisionId: "r2",
      activeRevisionId: "r2",
    });
    expect(engine.calls).toEqual(["revision r2"]);
  });

  test("landed when a colleague's save already built on it, found by reading back towards the base", async () => {
    const engine = new Engine({
      revisions: {
        r3: revision("r2", "A colleague's save (write zzzzzzzz)"),
        r2: revision("base", signedSummary("Edit", EDIT.writeId)),
      },
    });
    expect(await settleUnknownWrite(engine, EDIT, "r3", signal)).toEqual({
      kind: "landed",
      revisionId: "r2",
      activeRevisionId: "r3",
    });
    expect(engine.calls).toEqual(["revision r3", "revision r2"]);

    // A create that landed and was edited since is the first revision of all.
    const created = new Engine({
      revisions: {
        r2: revision("r1", "An edit"),
        r1: revision(undefined, signedSummary("Create", CREATE.writeId)),
      },
    });
    expect(await settleUnknownWrite(created, CREATE, "r2", signal)).toEqual({
      kind: "landed",
      revisionId: "r1",
      activeRevisionId: "r2",
    });
  });

  test("not landed once the walk reaches the base without meeting the write, and unknown past the limit", async () => {
    const colleague = new Engine({
      revisions: {
        r3: revision("r2", "Another save"),
        r2: revision("base", "A colleague's save (write zzzzzzzz)"),
      },
    });
    expect(await settleUnknownWrite(colleague, EDIT, "r3", signal)).toEqual({
      kind: "not_landed",
      currentRevisionId: "r3",
    });
    // The base is never read: nothing before it can be this write.
    expect(colleague.calls).toEqual(["revision r3", "revision r2"]);

    const revisions: Record<string, HttpAnswer> = {};
    for (let i = 0; i <= UPDATE_ANCESTRY_LIMIT; i++) {
      revisions[`r${i}`] = revision(`r${i + 1}`, "Somebody else");
    }
    const long = new Engine({ revisions });
    expect(await settleUnknownWrite(long, EDIT, "r0", signal)).toMatchObject({ kind: "unknown" });
    expect(long.calls).toHaveLength(UPDATE_ANCESTRY_LIMIT);
  });

  test("without a named revision, reads the active one first", async () => {
    const landed = new Engine({
      settings: { status: 200, body: {}, etag: '"r3"' },
      revisions: { r3: revision("base", signedSummary("Edit", EDIT.writeId)) },
    });
    expect(await settleUnknownWrite(landed, EDIT, null, signal)).toEqual({
      kind: "landed",
      revisionId: "r3",
      activeRevisionId: "r3",
    });
    expect(landed.calls).toEqual(["settings", "revision r3"]);

    const still = new Engine({ settings: { status: 200, body: {}, etag: '"base"' } });
    expect(await settleUnknownWrite(still, EDIT, null, signal)).toEqual({
      kind: "not_landed",
      currentRevisionId: "base",
    });
    expect(still.calls).toEqual(["settings"]);
  });

  test("a create that did not land finds no company", async () => {
    const engine = new Engine({ settings: { status: 404, body: { error: "no_active_revision" } } });
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
  const doc = fixtureSettings();
  const conflict = { baseRevision: "base", conflictRevisionId: "r2" };

  test("behind while the node still serves the draft's base", async () => {
    const engine = new Engine({ settings: { status: 200, body: doc, etag: '"base"' } });
    expect(await readyToUpdate(engine, conflict, signal)).toEqual({ kind: "behind" });
  });

  test("ready on the conflict's revision or a descendant, behind on one that does not descend from it", async () => {
    const same = new Engine({ settings: { status: 200, body: doc, etag: '"r2"' } });
    expect(await readyToUpdate(same, conflict, signal)).toEqual({
      kind: "ready",
      revisionId: "r2",
      document: doc,
    });
    const descendant = new Engine({
      settings: { status: 200, body: doc, etag: '"r4"' },
      revisions: { r4: revision("r3", "later"), r3: revision("r2", "later") },
    });
    expect(await readyToUpdate(descendant, conflict, signal)).toMatchObject({
      kind: "ready",
      revisionId: "r4",
    });
    const sibling = new Engine({
      settings: { status: 200, body: doc, etag: '"s1"' },
      revisions: { s1: revision("base", "raced") },
    });
    expect(await readyToUpdate(sibling, conflict, signal)).toEqual({ kind: "behind" });
  });

  test("a conflict that named no revision — the chart moved, not the settings — takes what is active", async () => {
    const engine = new Engine({ settings: { status: 200, body: doc, etag: '"base"' } });
    expect(
      await readyToUpdate(engine, { baseRevision: "base", conflictRevisionId: null }, signal),
    ).toEqual({ kind: "ready", revisionId: "base", document: doc });
  });

  test("gives up after the ancestry limit rather than walking the whole history", async () => {
    const revisions: Record<string, HttpAnswer> = {};
    for (let i = 0; i < UPDATE_ANCESTRY_LIMIT + 5; i++)
      revisions[`n${i}`] = revision(`n${i + 1}`, "later");
    const engine = new Engine({ settings: { status: 200, body: doc, etag: '"n0"' }, revisions });
    expect(await readyToUpdate(engine, conflict, signal)).toEqual({ kind: "behind" });
    expect(engine.calls.filter((c) => c.startsWith("revision"))).toHaveLength(
      UPDATE_ANCESTRY_LIMIT,
    );
  });

  test("unknown when the settings cannot be read or name no revision", async () => {
    expect(await readyToUpdate(new Engine({}), conflict, signal)).toMatchObject({
      kind: "unknown",
    });
    expect(
      await readyToUpdate(new Engine({ settings: { status: 200, body: doc } }), conflict, signal),
    ).toEqual({ kind: "unknown", detail: "The engine did not name its active revision." });
  });
});

describe("reading the company", () => {
  const settings: HttpAnswer = { status: 200, body: fixtureSettings(), etag: '"r1"' };
  const chart: HttpAnswer = { status: 200, body: fixtureChart() };

  test("the chart is read whole, and any other answer is handed back as it came", async () => {
    expect(await readChart(new Engine({ chart }), signal)).toEqual({
      kind: "read",
      chart: fixtureChart(),
    });
    const refused: HttpAnswer = { status: 403, body: { error: "unauthorized" } };
    expect(await readChart(new Engine({ chart: refused }), signal)).toEqual({
      kind: "refused",
      answer: refused,
    });
  });

  test("a company is its settings and its chart, and one with no settings yet is still read", async () => {
    expect(await readCompany(new Engine({ settings, chart }), signal)).toEqual({
      kind: "read",
      reading: { settings: fixtureSettings(), revision: "r1", chart: fixtureChart() },
    });
    const none = new Engine({
      settings: { status: 404, body: { error: "no_active_revision" } },
      chart: { status: 200, body: chartOf({}) },
    });
    expect(await readCompany(none, signal)).toMatchObject({
      kind: "read",
      reading: { settings: null, revision: null },
    });
    // Control: a chart that cannot be read is no reading at all.
    expect(await readCompany(new Engine({ settings }), signal)).toMatchObject({
      kind: "failed",
      detail: "The engine could not be reached.",
    });
  });

  test("an update of a create draft reads the company once, and of an edit waits for the conflict's revision", async () => {
    const create = new Engine({ settings, chart });
    expect(
      await readUpdate(create, { baseRevision: null, conflictRevisionId: null }, signal),
    ).toMatchObject({ kind: "ready", reading: { revision: "r1" } });
    expect(create.calls.filter((c) => c === "chart")).toHaveLength(1);

    const behind = new Engine({ settings: { ...settings, etag: '"base"' }, chart });
    expect(
      await readUpdate(behind, { baseRevision: "base", conflictRevisionId: "r1" }, signal),
    ).toEqual({ kind: "behind" });
    const ready = new Engine({ settings, chart });
    expect(
      await readUpdate(ready, { baseRevision: "base", conflictRevisionId: "r1" }, signal),
    ).toEqual({
      kind: "ready",
      reading: { settings: fixtureSettings(), revision: "r1", chart: fixtureChart() },
    });
  });
});
