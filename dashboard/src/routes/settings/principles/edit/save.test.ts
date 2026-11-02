/**
 * Saving one part of the charter: a colleague's change to ANOTHER part is
 * written around, one to THIS part is the person's to settle, a write whose
 * answer was lost is settled by reading it back, and nothing is written twice.
 */

import { afterEach, describe, expect, test, vi } from "vitest";
import type { Charter } from "./charter.ts";
import {
  CHARTER_ROUTE,
  SAVE_ATTEMPTS,
  readCharter,
  savePart,
  writeCharter,
  type CharterRead,
  type CharterWrite,
  type SaveTransport,
} from "./save.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

const CHARTER: Charter = {
  mission: "Ship it",
  vision: "Every team ships daily",
  policies: ["Pay on time.", "Ask first."],
};

/** A transport that answers reads and writes from two queues, recording each write. */
function scripted({
  reads,
  writes = [],
}: {
  reads: CharterRead[];
  writes?: CharterWrite[];
}): SaveTransport & { written: { charter: Charter; etag: string; summary: string }[] } {
  const written: { charter: Charter; etag: string; summary: string }[] = [];
  return {
    written,
    read: vi.fn(async () => {
      const next = reads.shift();
      if (!next) throw new Error("unexpected read");
      return next;
    }),
    write: vi.fn(async (charter: Charter, etag: string, summary: string) => {
      written.push({ charter, etag, summary });
      const next = writes.shift();
      if (!next) throw new Error("unexpected write");
      return next;
    }),
  };
}

const at = (charter: Charter, etag: string): CharterRead => ({ kind: "charter", charter, etag });
const signal = new AbortController().signal;

describe("savePart", () => {
  test("writes the charter as it is now with only this part replaced, on the tag it read", async () => {
    const t = scripted({
      reads: [at(CHARTER, '"t1"')],
      writes: [{ kind: "saved", activated: { revisionId: "r7", epoch: 7 } }],
    });
    const outcome = await savePart(
      { part: "mission", base: "Ship it", draft: "  Ship it well\n" },
      signal,
      t,
    );
    expect(outcome).toEqual({
      kind: "saved",
      activated: { revisionId: "r7", epoch: 7 },
      summary: "Changed the mission",
    });
    expect(t.written).toEqual([
      {
        charter: { ...CHARTER, mission: "Ship it well" },
        etag: '"t1"',
        summary: "Changed the mission",
      },
    ]);
  });

  // A COLLEAGUE'S CHANGE TO ANOTHER PART is not this card's conflict: the
  // write carries their words for that part, so nothing of theirs is lost.
  test("writes around a colleague's change to another part", async () => {
    const theirs = { ...CHARTER, vision: "A colleague's vision" };
    const t = scripted({
      reads: [at(theirs, '"t2"')],
      writes: [{ kind: "saved", activated: { revisionId: "r8", epoch: 8 } }],
    });
    await savePart({ part: "mission", base: "Ship it", draft: "New mission" }, signal, t);
    expect(t.written[0]!.charter).toEqual({ ...theirs, mission: "New mission" });
  });

  test("is a conflict when somebody changed this part since the person started", async () => {
    const t = scripted({ reads: [at({ ...CHARTER, mission: "Their mission" }, '"t2"')] });
    const outcome = await savePart(
      { part: "mission", base: "Ship it", draft: "My mission" },
      signal,
      t,
    );
    expect(outcome).toEqual({ kind: "conflict", theirs: "Their mission" });
    expect(t.write).not.toHaveBeenCalled();
  });

  test("writes nothing when the company already holds the draft", async () => {
    const t = scripted({ reads: [at({ ...CHARTER, mission: "My mission" }, '"t2"')] });
    expect(
      await savePart({ part: "mission", base: "Ship it", draft: "My mission" }, signal, t),
    ).toEqual({ kind: "unchanged" });
    expect(t.write).not.toHaveBeenCalled();
  });

  // A RACE BETWEEN THE READ AND THE WRITE is decided again on the charter the
  // refusal carries, without a second read.
  test("decides again on the charter a refused write carries", async () => {
    const moved = { ...CHARTER, policies: [...CHARTER.policies, "A colleague's rule."] };
    const t = scripted({
      reads: [at(CHARTER, '"t1"')],
      writes: [
        { kind: "moved", charter: moved, etag: '"t2"' },
        { kind: "saved", activated: { revisionId: "r9", epoch: 9 } },
      ],
    });
    const outcome = await savePart(
      { part: "vision", base: CHARTER.vision, draft: "My vision" },
      signal,
      t,
    );
    expect(outcome.kind).toBe("saved");
    expect(t.written[1]).toMatchObject({
      charter: { ...moved, vision: "My vision" },
      etag: '"t2"',
    });
    expect(t.read).toHaveBeenCalledTimes(1);
  });

  test("a refused write that moved this part is a conflict", async () => {
    const t = scripted({
      reads: [at(CHARTER, '"t1"')],
      writes: [{ kind: "moved", charter: { ...CHARTER, mission: "Theirs" }, etag: '"t2"' }],
    });
    expect(await savePart({ part: "mission", base: "Ship it", draft: "Mine" }, signal, t)).toEqual({
      kind: "conflict",
      theirs: "Theirs",
    });
  });

  test("says the charter kept moving after its attempts", async () => {
    const t = scripted({
      reads: [at(CHARTER, '"t0"')],
      writes: Array.from({ length: SAVE_ATTEMPTS }, (_, i) => ({
        kind: "moved" as const,
        charter: { ...CHARTER, vision: `Vision ${i}` },
        etag: `"t${i + 1}"`,
      })),
    });
    expect(await savePart({ part: "mission", base: "Ship it", draft: "Mine" }, signal, t)).toEqual({
      kind: "busy",
    });
    expect(t.write).toHaveBeenCalledTimes(SAVE_ATTEMPTS);
  });

  describe("a write that got no answer", () => {
    const unreachable: CharterWrite = {
      kind: "refused",
      refusal: { kind: "unsettled", answered: false, detail: "gateway timeout", status: 0 },
    };

    test("landed, when the read after it finds the draft", async () => {
      const t = scripted({
        reads: [at(CHARTER, '"t1"'), at({ ...CHARTER, mission: "Mine" }, '"t2"')],
        writes: [unreachable],
      });
      expect(
        await savePart({ part: "mission", base: "Ship it", draft: "Mine" }, signal, t),
      ).toEqual({ kind: "saved", activated: null, summary: "Changed the mission" });
      expect(t.write).toHaveBeenCalledTimes(1);
    });

    // NEVER SENT AGAIN on its own: it may still arrive.
    test("did not land, when the read after it finds the base, and is not sent again", async () => {
      const t = scripted({
        reads: [at(CHARTER, '"t1"'), at(CHARTER, '"t1"')],
        writes: [unreachable],
      });
      expect(
        await savePart({ part: "mission", base: "Ship it", draft: "Mine" }, signal, t),
      ).toEqual({ kind: "lost", refusal: unreachable.refusal });
      expect(t.write).toHaveBeenCalledTimes(1);
    });

    test("is a conflict, when the read after it finds somebody else's words", async () => {
      const t = scripted({
        reads: [at(CHARTER, '"t1"'), at({ ...CHARTER, mission: "Theirs" }, '"t2"')],
        writes: [unreachable],
      });
      expect(
        await savePart({ part: "mission", base: "Ship it", draft: "Mine" }, signal, t),
      ).toEqual({ kind: "conflict", theirs: "Theirs" });
    });
  });

  test("passes a refusal through", async () => {
    const refusal = { kind: "managed" as const, managedBy: ["deploy"] };
    const t = scripted({ reads: [at(CHARTER, '"t1"')], writes: [{ kind: "refused", refusal }] });
    expect(await savePart({ part: "mission", base: "Ship it", draft: "Mine" }, signal, t)).toEqual({
      kind: "refused",
      refusal,
    });
  });

  // THE ROUTE RETRIES A DOCUMENT THAT MOVED FOR ANOTHER REASON itself; a
  // conflict it still answers is one it kept losing.
  test("reads the route's conflict as the company kept changing", async () => {
    const t = scripted({
      reads: [at(CHARTER, '"t1"')],
      writes: [
        {
          kind: "refused",
          refusal: { kind: "conflict", reason: "revision_advanced", currentRevisionId: null },
        },
      ],
    });
    expect(await savePart({ part: "mission", base: "Ship it", draft: "Mine" }, signal, t)).toEqual({
      kind: "busy",
    });
  });

  test("says when no company is configured", async () => {
    const t = scripted({ reads: [{ kind: "none" }] });
    expect(await savePart({ part: "mission", base: "", draft: "Mine" }, signal, t)).toEqual({
      kind: "none",
    });
  });
});

describe("the charter's route", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function stub(respond: () => Response) {
    const calls: { url: string; init: RequestInit }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init: RequestInit) => {
        calls.push({ url, init });
        return respond();
      }),
    );
    return calls;
  }

  const json = (payload: unknown, status: number, headers: Record<string, string> = {}) =>
    new Response(JSON.stringify(payload), {
      status,
      headers: { ...ENGINE_HEADERS, "Content-Type": "application/json", ...headers },
    });

  test("a read is the charter and the tag a write hands back", async () => {
    stub(() => json(CHARTER, 200, { ETag: '"charter-1"' }));
    expect(await readCharter(signal)).toEqual({
      kind: "charter",
      charter: CHARTER,
      etag: '"charter-1"',
    });
  });

  test("a read of no company is none", async () => {
    stub(() => json({ error: "no_active_revision" }, 404));
    expect(await readCharter(signal)).toEqual({ kind: "none" });
  });

  // CONDITIONAL, AND SAYING WHAT IT DID: every write names the charter it
  // edited and the summary its revision is recorded with.
  test("a write is conditional on the tag and carries its summary", async () => {
    const calls = stub(() =>
      json({ charter: CHARTER, changed: true, revision_id: "r9", epoch: 12 }, 200),
    );
    // THE REVISION BESIDE ITS EPOCH, which is what the page follows this
    // node's apply of the save by. Mutation: drop the revision and a refusal
    // of this save cannot be told from an apply still running.
    expect(await writeCharter(CHARTER, '"charter-1"', "Changed the mission", signal)).toEqual({
      kind: "saved",
      activated: { revisionId: "r9", epoch: 12 },
    });
    const { url, init } = calls[0]!;
    expect(url).toContain(CHARTER_ROUTE);
    expect(init.method).toBe("PUT");
    const headers = new Headers(init.headers);
    expect(headers.get("If-Match")).toBe('"charter-1"');
    expect(headers.get("X-Summary")).toBe("Changed the mission");
    expect(JSON.parse(String(init.body))).toEqual(CHARTER);
  });

  test("a write that changed nothing is unchanged", async () => {
    stub(() => json({ charter: CHARTER, changed: false, revision_id: "r8" }, 200));
    expect(await writeCharter(CHARTER, '"charter-1"', "x", signal)).toEqual({
      kind: "unchanged",
    });
  });

  test("a write refused because the charter moved carries it as it is now", async () => {
    const now = { ...CHARTER, vision: "Moved" };
    stub(() => json({ error: "version_moved", current: now, etag: '"charter-2"' }, 412));
    expect(await writeCharter(CHARTER, '"charter-1"', "x", signal)).toEqual({
      kind: "moved",
      charter: now,
      etag: '"charter-2"',
    });
  });

  test("a write on a managed document is that refusal", async () => {
    stub(() => json({ error: "config_managed", managed_by: ["deploy"] }, 403));
    expect(await writeCharter(CHARTER, '"charter-1"', "x", signal)).toEqual({
      kind: "refused",
      refusal: { kind: "managed", managedBy: ["deploy"] },
    });
  });
});
