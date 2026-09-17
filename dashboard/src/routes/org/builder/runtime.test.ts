/**
 * The browser bindings keep the model's contracts: a transport that resolves
 * with every answer and rejects only on an abort, and tokens the model
 * accepts.
 */

import { afterEach, expect, test, vi } from "vitest";
import { isMintedKey, mintKey } from "./model/keys.ts";
import { newWriteId } from "./model/writes.ts";
import { randomKeys, restTransport } from "./runtime.ts";

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

test("a request that never reached the engine resolves as status 0", async () => {
  stub(() => Promise.reject(new TypeError("Failed to fetch")));
  const answer = await restTransport.current(new AbortController().signal);
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
  const pending = restTransport.current(controller.signal);
  controller.abort();
  await expect(pending).rejects.toMatchObject({ name: "AbortError" });
});

test("a success carries the entity tag that names the revision", async () => {
  stub(async () => json({ name: "Acme" }, 200, { ETag: '"r1"' }));
  const answer = await restTransport.current(new AbortController().signal);
  expect(answer).toEqual({ status: 200, body: { name: "Acme" }, etag: '"r1"' });
});

test("a revision is read by its id, encoded into the path", async () => {
  const calls = stub(async () => json({ revision_id: "a/b" }, 200));
  await restTransport.revision("a/b", new AbortController().signal);
  expect(new URL(calls[0]!.url).pathname).toBe("/config/revisions/a%2Fb");
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

// The model refuses a token it cannot use, and a token that repeats would
// give two nodes one key or two saves one write id.
test("random tokens are accepted as keys and write ids, and do not repeat", () => {
  const seen = new Set<string>();
  for (let i = 0; i < 200; i++) {
    const token = randomKeys.next();
    expect(() => mintKey({ next: () => token })).not.toThrow();
    expect(() => newWriteId({ next: () => token })).not.toThrow();
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
