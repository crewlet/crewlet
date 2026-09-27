/**
 * The one /config write path keeps its contracts: a transport that resolves
 * with every answer and rejects only on an abort; every write conditional on
 * the revision it edited; and every refusal read one way, whichever screen
 * made the write.
 */

import { afterEach, describe, expect, test, vi } from "vitest";
import { classifyConfigRefusal } from "./configAnswer.ts";
import {
  configTransport,
  dryRunPatch,
  getConfig,
  outcomeOf,
  putEntity,
  savePatch,
} from "./configWrite.ts";

afterEach(() => {
  vi.unstubAllGlobals();
});

function stub(respond: (url: string, init: RequestInit) => Promise<Response>) {
  const calls: { url: string; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init: RequestInit) => {
      calls.push({ url, init });
      return respond(url, init);
    }),
  );
  return calls;
}

const json = (payload: unknown, status: number, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });

// A REFUSAL IS AN ANSWER. The scheduler and the save classify the status and
// the body; a transport that threw on a 409 would report a conflict as an
// engine that could not be reached.
test("a refusal resolves with the engine's status and body", async () => {
  stub(async () => json({ error: "revision_advanced", current_revision_id: "r2" }, 409));
  const answer = await configTransport.send(
    {
      method: "PATCH",
      query: { dry_run: "true" },
      contentType: "application/merge-patch+json",
      headers: { "If-Match": '"r1"' },
      body: { name: "Acme" },
    },
    new AbortController().signal,
  );
  expect(answer.status).toBe(409);
  expect(answer.body).toEqual({ error: "revision_advanced", current_revision_id: "r2" });
});

test("a request that never reached the engine resolves as status 0", async () => {
  stub(() => Promise.reject(new TypeError("Failed to fetch")));
  const answer = await configTransport.current(new AbortController().signal);
  expect(answer.status).toBe(0);
});

// A SUPERSEDED CHECK IS NOT AN UNREACHABLE ENGINE. The check runner drops an
// aborted request; counting it as a failure would back off for nothing.
test("an aborted request rejects rather than resolving as unreachable", async () => {
  stub(
    (_url, init) =>
      new Promise((_resolve, reject) =>
        init.signal?.addEventListener("abort", () =>
          reject(new DOMException("aborted", "AbortError")),
        ),
      ),
  );
  const controller = new AbortController();
  const pending = configTransport.current(controller.signal);
  controller.abort();
  await expect(pending).rejects.toMatchObject({ name: "AbortError" });
});

test("a success carries the entity tag that names the revision", async () => {
  stub(async () => json({ name: "Acme" }, 200, { ETag: '"r1"' }));
  const answer = await configTransport.current(new AbortController().signal);
  expect(answer).toEqual({ status: 200, body: { name: "Acme" }, etag: '"r1"' });
});

test("a revision is read by its id, encoded into the path", async () => {
  const calls = stub(async () => json({ revision_id: "a/b" }, 200));
  await configTransport.revision("a/b", new AbortController().signal);
  expect(new URL(calls[0]!.url).pathname).toBe("/config/revisions/a%2Fb");
});

test("the dry run and the save carry exactly the request the model built", async () => {
  const calls = stub(async () => json({ valid: true }, 200));
  await configTransport.send(
    {
      method: "PUT",
      query: { dry_run: "true" },
      contentType: "application/json",
      headers: { "If-None-Match": "*" },
      body: { name: "Acme" },
    },
    new AbortController().signal,
  );
  const { url, init } = calls[0]!;
  expect(init.method).toBe("PUT");
  expect(new URL(url).search).toBe("?dry_run=true");
  expect((init.headers as Record<string, string>)["If-None-Match"]).toBe("*");
  expect(init.body).toBe(JSON.stringify({ name: "Acme" }));
});

const never = () => new AbortController().signal;

describe("the helpers every /config writer uses", () => {
  test("a read names the revision it is of, so a write can be conditional on it", async () => {
    stub(async () => json({ name: "Acme" }, 200, { ETag: '"r7"' }));
    expect(await getConfig(never())).toEqual({
      kind: "document",
      document: { name: "Acme" },
      etag: '"r7"',
    });
  });

  test("no company yet is its own answer, not a refusal", async () => {
    stub(async () => json({ error: "no_active_revision" }, 404));
    expect(await getConfig(never())).toEqual({ kind: "none" });
  });

  // A DOCUMENT NO WRITE CAN BE CONDITIONAL ON is refused rather than handed
  // over: every write after it would be an overwrite of whatever landed.
  test("a read with no entity tag is not a document", async () => {
    stub(async () => json({ name: "Acme" }, 200));
    expect((await getConfig(never())).kind).toBe("problems");
  });

  test("a dry run is a conditional merge patch that carries no summary", async () => {
    const calls = stub(async () =>
      json({ valid: true, base_revision_id: "r7", warnings: null, derived: {} }, 200),
    );
    const outcome = await dryRunPatch({ token_budget: { day: 5 } }, '"r7"', never());
    expect(outcome).toMatchObject({ kind: "valid", baseRevisionId: "r7", warnings: [] });
    const { url, init } = calls[0]!;
    expect(init.method).toBe("PATCH");
    expect(new URL(url).search).toBe("?dry_run=true");
    const headers = init.headers as Record<string, string>;
    expect(headers["If-Match"]).toBe('"r7"');
    expect(headers["Content-Type"]).toBe("application/merge-patch+json");
    expect(JSON.parse(init.body as string)).toEqual({ token_budget: { day: 5 } });
  });

  test("a save carries its audit summary and answers the revision it stored", async () => {
    const calls = stub(async () =>
      json({ revision_id: "r8", epoch: 9, warnings: null, derived: {} }, 201),
    );
    const outcome = await savePatch({ name: "Acme" }, '"r7"', "Rename", never());
    expect(outcome).toMatchObject({ kind: "saved", revisionId: "r8", epoch: 9 });
    expect(new URL(calls[0]!.url).search).toBe("");
    expect(JSON.parse(calls[0]!.init.body as string)).toEqual({
      name: "Acme",
      _summary: "Rename",
    });
  });

  test("an entity write addresses the entity and is conditional too", async () => {
    const calls = stub(async () =>
      json({ revision_id: "r8", epoch: 9, warnings: null, derived: {} }, 201),
    );
    await putEntity("mcp-servers", "git hub", { name: "git hub" }, '"r7"', "Add", never());
    const { url, init } = calls[0]!;
    expect(init.method).toBe("PUT");
    expect(new URL(url).pathname).toBe("/config/mcp-servers/git%20hub");
    expect((init.headers as Record<string, string>)["If-Match"]).toBe('"r7"');
    expect(JSON.parse(init.body as string)).toMatchObject({ _summary: "Add" });
  });
});

describe("one reading of a /config refusal", () => {
  test.each([
    [401, { error: "invalid_token" }, { kind: "guarded" }],
    [403, {}, { kind: "guarded" }],
    [
      409,
      { error: "revision_advanced", current_revision_id: "r9" },
      { kind: "conflict", reason: "revision_advanced", currentRevisionId: "r9" },
    ],
    [
      412,
      { error: "already_configured" },
      { kind: "conflict", reason: "already_configured", currentRevisionId: null },
    ],
    [
      409,
      { error: "no_active_revision" },
      { kind: "conflict", reason: "no_active_revision", currentRevisionId: null },
    ],
    [
      503,
      { error: "draining", detail: "shutting down" },
      { kind: "draining", detail: "shutting down" },
    ],
    [502, {}, { kind: "unreachable", detail: "" }],
    [0, { error: "unreachable", detail: "offline" }, { kind: "unreachable", detail: "offline" }],
  ])("status %i %j reads as %j", (status, body, want) => {
    expect(classifyConfigRefusal({ status, body })).toEqual(want);
  });

  // THE DRAIN IS THE ONE 5xx THAT IS CERTAIN: refused before the handler ran,
  // so it stored nothing. Every other 5xx may have been raised after the
  // revision was stored, and reading it as a refusal is how a save that
  // landed gets replayed on top of itself.
  test("only the drain gate's 503 is a refusal that stored nothing", () => {
    expect(classifyConfigRefusal({ status: 503, body: { error: "draining" } }).kind).toBe(
      "draining",
    );
    expect(classifyConfigRefusal({ status: 503, body: { error: "unavailable" } }).kind).toBe(
      "unreachable",
    );
  });

  test("a refusal with no problems of its own gets one from its detail", () => {
    const refusal = classifyConfigRefusal({
      status: 400,
      body: { error: "invalid_patch", detail: "not a merge patch", hint: "send an object" },
    });
    expect(refusal).toMatchObject({
      kind: "problems",
      code: "invalid_patch",
      hint: "send an object",
      problems: [{ path: "", kind: "invalid", message: "not a merge patch" }],
    });
  });

  test("a write's success and refusal are read by the same function", () => {
    expect(outcomeOf({ status: 201, body: { revision_id: "r2", epoch: 3 } })).toMatchObject({
      kind: "saved",
      revisionId: "r2",
      epoch: 3,
    });
    expect(outcomeOf({ status: 409, body: { error: "revision_advanced" } }).kind).toBe("conflict");
  });
});
