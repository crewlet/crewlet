/**
 * The code questions' loader: REST, at this tab's floor on the code log, and
 * an answer by object id held by its tag.
 *
 * Each case is a rule the hook exists to keep: a code question asked on the
 * socket would hold one of its in-flight slots, a read back that ignored the
 * floor would draw the state from before the reader's own press, a floor in
 * the question's key would blank the screen on every write, and an immutable
 * answer read whole again is bytes sent twice for nothing.
 */

import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import { codeRest, codeSearch, useCodeQuery } from "./useCodeQuery.ts";
import { QueryError } from "~/protocol/index.ts";
import { session } from "~/protocol/session.ts";
import { engineJson } from "~/test/engineAnswer.ts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/** What each request asked, and a fetch that answers with `respond`. */
function stub(respond: (url: URL, init: RequestInit, n: number) => Response) {
  const sent: { url: URL; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (raw: string, init: RequestInit) => {
      const url = new URL(raw);
      sent.push({ url, init });
      return respond(url, init, sent.length);
    }),
  );
  return sent;
}

// EVERY ANSWER HERE IS THE ENGINE'S, marked as it marks them: a refusal
// without the mark reads as a proxy's (`protocol/engineAnswer.ts`).
const json = (payload: unknown, status = 200, headers: Record<string, string> = {}) =>
  engineJson(payload, status, headers);

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 9 };

const pushesPage = (ids: string[]) => ({
  pushes: ids.map((id) => ({ push_id: id })),
  position: POSITION,
  level: "stale",
});

/**
 * A position on a stream of the case's own, so the tab's floor RISES to it
 * whatever an earlier case wrote: a floor only ever rises, and a position on
 * another stream is taken as later (`protocol/session.ts`).
 */
let streams = 0;
function freshPosition(): string {
  streams++;
  return `CREWLET_CODE_LOG_CASE_${streams}@1:${100 + streams}`;
}

describe("a code question is asked over REST", () => {
  // NEVER THE SOCKET: the registry's generic form, `GET /query/{what}`, by
  // its kind and its parameters. Mutation: build the path from anything but
  // the kind and the parameters, and the address below is not this.
  test("on the registry's generic form, by its kind and its parameters", async () => {
    const sent = stub(() => json(pushesPage(["p1"])));
    const { result } = renderHook(() => useCodeQuery("code_pushes", { name: "web" }));
    await waitFor(() => expect(result.current.data).not.toBeNull());
    expect(sent).toHaveLength(1);
    expect(sent[0]!.url.pathname).toBe("/query/code_pushes");
    expect(sent[0]!.url.searchParams.get("name")).toBe("web");
    expect(sent[0]!.init.method).toBe("GET");
    expect(result.current.data?.pushes?.map((p) => p.push_id)).toEqual(["p1"]);
    expect(result.current.error).toBeNull();
  });

  test("a question not yet enabled asks nothing and is not loading", () => {
    const sent = stub(() => json(pushesPage([])));
    const { result } = renderHook(() =>
      useCodeQuery("code_pushes", { name: "web" }, { enabled: false }),
    );
    expect(sent).toHaveLength(0);
    expect(result.current.loading).toBe(false);
  });

  // ONE KEY PER VALUE where the grammar reads a key whole: `min_position`
  // names a floor per log, and the engine keeps every value of it.
  test("a key the grammar reads whole is sent once per value", () => {
    const search = codeSearch("code_pushes", {
      name: "web",
      read_level: "session",
      min_position: ["CREWLET_CODE_LOG@1:2", "CREWLET_TRACKER_LOG@1:3"],
    });
    const url = new URL(`/query/code_pushes?${search}`, "http://engine");
    expect(url.searchParams.get("name")).toBe("web");
    expect(url.searchParams.getAll("min_position")).toEqual([
      "CREWLET_CODE_LOG@1:2",
      "CREWLET_TRACKER_LOG@1:3",
    ]);
  });
});

describe("a write to the code log is read back at its floor", () => {
  // THE PRESS IS IN THE ANSWER: the write asks again, at the floor it raised,
  // and what is on screen stays until that answer comes — the floor is never
  // the question's key. Mutation: drop the write listener, or put the floor
  // in the key, and a case below fails.
  test("a write asks again, quietly, naming the floor it raised", async () => {
    let answer = pushesPage(["p1"]);
    const sent = stub(() => json(answer));
    const { result } = renderHook(() => useCodeQuery("code_pushes", { name: "web" }));
    await waitFor(() => expect(result.current.data?.pushes).toHaveLength(1));

    answer = pushesPage(["p2", "p1"]);
    const floor = freshPosition();
    act(() => void session.written("code", floor, []));
    // QUIET: the rows read before the write stay while it is asked again.
    expect(result.current.loading).toBe(false);
    expect(result.current.data?.pushes).toHaveLength(1);
    await waitFor(() => expect(result.current.data?.pushes).toHaveLength(2));

    const again = sent.at(-1)!.url;
    expect(again.searchParams.get("read_level")).toBe("session");
    expect(again.searchParams.getAll("min_position")).toContain(floor);
  });

  test("a write to another log asks nothing again", async () => {
    const sent = stub(() => json(pushesPage(["p1"])));
    const { result } = renderHook(() => useCodeQuery("code_pushes", { name: "web" }));
    await waitFor(() => expect(result.current.data).not.toBeNull());
    act(() => void session.written("tracker", "CREWLET_TRACKER_LOG@1:5", []));
    await Promise.resolve();
    expect(sent).toHaveLength(1);
  });

  // A FRESHNESS THE CALLER NAMED IS THE CALLER'S: a staleness bound or a
  // level beside the tab's floor is a request the engine refuses.
  test("a freshness the caller named is kept, and no floor is added to it", () => {
    session.written("code", freshPosition(), []);
    const url = new URL(
      `/query/code_repos?${codeSearch("code_repos", { read_level: "linearizable" })}`,
      "http://engine",
    );
    expect(url.searchParams.get("read_level")).toBe("linearizable");
    expect(url.searchParams.getAll("min_position")).toEqual([]);
  });
});

describe("an answer by object id is held by its tag", () => {
  // A 304 IS THE ANSWER HELD, and the read names the tag it holds.
  // Mutation: drop the If-None-Match header, or read the 304's empty body as
  // the answer, and a case below fails.
  test("a read again names the tag, and a 304 is the answer it holds", async () => {
    const tag = '"sha256-abc"';
    const sent = stub((_url, _init, n) =>
      n === 1 ? json(pushesPage(["p1"]), 200, { ETag: tag }) : new Response(null, { status: 304 }),
    );
    const { result } = renderHook(() => useCodeQuery("code_pushes", { name: "web" }));
    await waitFor(() => expect(result.current.data?.pushes).toHaveLength(1));
    expect(new Headers(sent[0]!.init.headers).get("If-None-Match")).toBeNull();

    act(() => result.current.refetch());
    await waitFor(() => expect(sent).toHaveLength(2));
    expect(new Headers(sent[1]!.init.headers).get("If-None-Match")).toBe(tag);
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.data?.pushes?.map((p) => p.push_id)).toEqual(["p1"]);
    expect(result.current.error).toBeNull();
  });

  test("an answer with no tag is read whole again, naming none", async () => {
    const sent = stub(() => json(pushesPage(["p1"])));
    const { result } = renderHook(() => useCodeQuery("code_pushes", { name: "web" }));
    await waitFor(() => expect(result.current.data).not.toBeNull());
    act(() => result.current.refetch());
    await waitFor(() => expect(sent).toHaveLength(2));
    expect(new Headers(sent[1]!.init.headers).get("If-None-Match")).toBeNull();
  });
});

describe("a refusal reads as every question's", () => {
  // THE CODE, THE REFUSAL AND THE ENGINE'S OWN SENTENCE, as `QueryState`
  // reads a socket question's — and the sentence on a `not_found` too, which
  // names the repository nobody declared.
  test("a repository nobody declared is not_found, with the engine's sentence", async () => {
    const sentence = 'no repository named "web" is declared in code.repos';
    stub(() => json({ error: "not_found", detail: sentence, reason: "not_declared" }, 404));
    const { result } = renderHook(() => useCodeQuery("code_pushes", { name: "web" }));
    await waitFor(() => expect(result.current.error).toBe("not_found"));
    expect(result.current.detail).toBe(sentence);
    expect(result.current.data).toBeNull();
  });

  test("a request the engine refused is bad_params, with its sentence", async () => {
    const sentence = 'repo: "Web" is not a repository name';
    stub(() => json({ error: "bad_params", detail: sentence }, 400));
    const { result } = renderHook(() => useCodeQuery("code_pushes", { name: "Web" }));
    await waitFor(() => expect(result.current.error).toBe("bad_params"));
    expect(result.current.detail).toBe(sentence);
  });

  test("a node that cannot serve it yet is unavailable, with the wait it named", async () => {
    stub(() =>
      json({ error: "applying", detail: "this node is applying the configuration" }, 503, {
        "Retry-After": "0",
      }),
    );
    const { result } = renderHook(() => useCodeQuery("code_repos"));
    await waitFor(() => expect(result.current.error).toBe("unavailable"));
    expect(result.current.refusal).toMatchObject({ code: null, retryAfter: 0 });
    expect(result.current.detail).toBeUndefined();
  });

  // AN OLDER PAGE'S REFUSAL IS A QueryError whose message is the code, so a
  // pager reads it as it reads a socket question's.
  test("an older page is refused as a QueryError naming the code", async () => {
    const sentence = "cursor: not one this read issued";
    stub(() => json({ error: "bad_params", detail: sentence }, 400));
    const refused = await codeRest
      .query("code_pushes", { name: "web", cursor: "x" })
      .catch((err: unknown) => err);
    expect(refused).toBeInstanceOf(QueryError);
    expect((refused as QueryError).message).toBe("bad_params");
    expect((refused as QueryError).detail).toBe(sentence);
  });
});
