/**
 * The browser bindings keep the model's contracts: a transport that resolves
 * with every answer and rejects only on an abort, and tokens the model
 * accepts.
 */

import { afterEach, expect, test, vi } from "vitest";
import { isMintedKey, mintKey } from "./model/keys.ts";
import { isWriteId } from "./model/writes.ts";
import { newWriteId, randomKeys, restTransport } from "./runtime.ts";

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
  const answer = await restTransport.send(
    {
      method: "PATCH",
      path: "/config",
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

// A 503 THE ENGINE WROTE SAYS WHEN TO ASK AGAIN, and the check waits it out:
// its Retry-After, or zero where it sent none — the engine's word that
// waiting will not change it. A 503 nobody at the engine wrote, a proxy's,
// says nothing, and neither does any other refusal.
test.each([
  ["a 503 with a Retry-After", json({ error: "behind" }, 503, { "Retry-After": "12" }), 12],
  ["a 503 the engine wrote with none", json({ error: "log_full" }, 503), 0],
  ["a proxy's 503", new Response("<html>bad gateway</html>", { status: 503 }), null],
  ["a refusal that is not a 503", json({ error: "revision_advanced" }, 409), null],
])("%s carries the hint the check waits", async (_, response, hint) => {
  stub(async () => response);
  const answer = await restTransport.chart(new AbortController().signal);
  expect(answer.retryAfter).toBe(hint);
});

test("a request that never reached the engine resolves as status 0", async () => {
  stub(() => Promise.reject(new TypeError("Failed to fetch")));
  const answer = await restTransport.settings(new AbortController().signal);
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
  const pending = restTransport.settings(controller.signal);
  controller.abort();
  await expect(pending).rejects.toMatchObject({ name: "AbortError" });
});

test("a success carries the entity tag that names the revision", async () => {
  stub(async () => json({ name: "Acme" }, 200, { ETag: '"r1"' }));
  const answer = await restTransport.settings(new AbortController().signal);
  expect(answer).toEqual({ status: 200, body: { name: "Acme" }, etag: '"r1"' });
});

test("a revision is read by its id, encoded into the path", async () => {
  const calls = stub(async () => json({ revision_id: "a/b" }, 200));
  await restTransport.revision("a/b", new AbortController().signal);
  expect(new URL(calls[0]!.url).pathname).toBe("/config/revisions/a%2Fb");
});

test("the chart is read with its runtime half asked for, which the engine serves where the reader may see it", async () => {
  const calls = stub(async () => json({ units: [], seats: [], runtime: true }, 200));
  await restTransport.chart(new AbortController().signal);
  const url = new URL(calls[0]!.url);
  expect(url.pathname).toBe("/chart");
  expect(url.searchParams.get("runtime")).toBe("true");
});

test("a chart write carries the operation id a retry must resend", async () => {
  const calls = stub(async () =>
    json({ outcome: "applied", position: "L@1:2", op_id: "w-1" }, 200),
  );
  await restTransport.send(
    {
      method: "PATCH",
      path: "/chart/seats/dev",
      query: {},
      contentType: "application/json",
      headers: { "Idempotency-Key": "w-1" },
      body: { goal: "Ship" },
    },
    new AbortController().signal,
  );
  const { url, init } = calls[0]!;
  expect(new URL(url).pathname).toBe("/chart/seats/dev");
  expect((init.headers as Record<string, string>)["Idempotency-Key"]).toBe("w-1");
});

test("the dry run and the save carry exactly the request the model built", async () => {
  const calls = stub(async () => json({ valid: true }, 200));
  await restTransport.send(
    {
      method: "PUT",
      path: "/config",
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

// A WRITE ID IS AN OPERATION ID IN THE ENGINE'S GRAMMAR (statelog.NewOpID),
// carrying the instant it was minted: the chart surface refuses a key that is
// not one and reads the instant back out of it to decide whether its ledger
// can vouch for a retry. Two saves never share one.
test("a write id is minted in the engine's grammar at its instant, and does not repeat", () => {
  const at = Date.UTC(2026, 8, 30, 12, 0, 0, 250);
  const id = newWriteId(at, () => new Uint8Array(10).fill(0xab));
  expect(isWriteId(id)).toBe(true);
  expect(parseInt(id.replace(/-/g, "").slice(0, 12), 16)).toBe(at);
  const seen = new Set<string>();
  for (let i = 0; i < 200; i++) {
    const fresh = newWriteId();
    expect(isWriteId(fresh)).toBe(true);
    seen.add(fresh);
  }
  expect(seen.size).toBe(200);
});

// The model refuses a token it cannot use, and a token that repeats would
// give two nodes one key.
test("random tokens are accepted as keys, and do not repeat", () => {
  const seen = new Set<string>();
  for (let i = 0; i < 200; i++) {
    const token = randomKeys.next();
    expect(() => mintKey({ next: () => token })).not.toThrow();
    seen.add(token);
  }
  expect(seen.size).toBe(200);
});

// THE DASHBOARD IS SERVED OVER PLAIN HTTP. `crypto.randomUUID` exists only in
// a secure context, so on every engine reached by its address rather than as
// localhost it is absent, and a source that called it would throw on the
// first Add. The Builder's one source reads `getRandomValues`, which every
// browsing context has.
test("random tokens need no randomUUID", () => {
  // It lives on Crypto.prototype, so hiding it means an own property that
  // shadows it, and putting it back means deleting that property again.
  const own = Object.getOwnPropertyDescriptor(crypto, "randomUUID");
  Object.defineProperty(crypto, "randomUUID", { value: undefined, configurable: true });
  try {
    expect(crypto.randomUUID).toBeUndefined();
    const keys = new Set([mintKey(randomKeys), mintKey(randomKeys)]);
    expect(keys.size).toBe(2);
    for (const key of keys) expect(isMintedKey(key)).toBe(true);
  } finally {
    if (own) Object.defineProperty(crypto, "randomUUID", own);
    else delete (crypto as { randomUUID?: unknown }).randomUUID;
  }
  expect(typeof crypto.randomUUID).toBe("function");
});
