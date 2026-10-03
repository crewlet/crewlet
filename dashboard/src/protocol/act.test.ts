/**
 * The write client says exactly what became of a change, and never more.
 *
 * Every case names an outcome a person acts on differently: a change that
 * landed, one this node has not applied yet, one nobody can vouch for, and one
 * the engine refused. Collapsing any two of them tells somebody something
 * false about their own change.
 */

import { afterEach, describe, expect, test, vi } from "vitest";
import { act, newRequestId } from "./act.ts";
import { onTokenRequested } from "./authToken.ts";
import { SessionFloors } from "./session.ts";

afterEach(() => {
  vi.unstubAllGlobals();
});

interface Call {
  url: string;
  init: RequestInit;
}

function stub(respond: (call: Call) => Promise<Response>): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init: RequestInit) => {
      const call = { url, init };
      calls.push(call);
      return respond(call);
    }),
  );
  return calls;
}

const json = (payload: unknown, status: number) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

const APPLIED = {
  tool: "set_pins",
  outcome: "applied",
  position: "CREWLET_TRACKER_LOG@1:4711",
  receipt: { views: ["v1"] },
};

describe("the request", () => {
  test("is one JSON object carrying the request id and the arguments, to the tool's route", async () => {
    const calls = stub(async () => json(APPLIED, 200));
    const floors = new SessionFloors();
    const requestId = newRequestId();
    await act("set_pins", { views: { add: ["v1"] } }, { requestId, floors });
    const { url, init } = calls[0]!;
    expect(init.method).toBe("POST");
    expect(new URL(url).pathname).toBe("/operator/act/set_pins");
    expect((init.headers as Record<string, string>)["Content-Type"]).toBe("application/json");
    expect(JSON.parse(init.body as string)).toEqual({
      request_id: requestId,
      args: { views: { add: ["v1"] } },
    });
  });

  // THE ENGINE REFUSES A REQUEST ID THAT IS NOT A UUIDv7 — it reads the
  // instant the id carries — and a browser reached over plain http has no
  // `crypto.randomUUID` to mint one with.
  test("mints a version-7 UUID without randomUUID", () => {
    const own = Object.getOwnPropertyDescriptor(crypto, "randomUUID");
    Object.defineProperty(crypto, "randomUUID", { value: undefined, configurable: true });
    try {
      const ids = new Set(Array.from({ length: 50 }, () => newRequestId()));
      expect(ids.size).toBe(50);
      for (const id of ids) {
        expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
      }
    } finally {
      if (own) Object.defineProperty(crypto, "randomUUID", own);
      else delete (crypto as { randomUUID?: unknown }).randomUUID;
    }
  });

  // THE INSTANT IS THE GESTURE'S: the engine reads it back as the operation's
  // mint instant, so the leading 48 bits are the millisecond it was pressed.
  test("carries the instant the gesture began", () => {
    const id = newRequestId(0x0192_7c3a_4e5b);
    expect(id.startsWith("01927c3a-4e5b-7")).toBe(true);
  });

  test("sends the same request id again when the caller retries a press", async () => {
    const calls = stub(async () => json(APPLIED, 200));
    const floors = new SessionFloors();
    const first = await act("set_pins", { views: { add: ["v1"] } }, { floors });
    await act("set_pins", { views: { add: ["v1"] } }, { requestId: first.requestId, floors });
    const ids = calls.map(
      (c) => (JSON.parse(c.init.body as string) as { request_id: string }).request_id,
    );
    expect(ids[0]).toBe(ids[1]);
  });
});

describe("what a change came to", () => {
  test("applied raises this tab's floor for the domain to where the record landed", async () => {
    stub(async () => json(APPLIED, 200));
    const floors = new SessionFloors();
    const heard: unknown[] = [];
    floors.onWritten((domain, refreshes) => heard.push([domain, refreshes]));
    const result = await act("set_pins", { views: { add: ["v1"] } }, { floors });
    expect(result).toMatchObject({
      kind: "applied",
      position: "CREWLET_TRACKER_LOG@1:4711",
      domain: "tracker",
      receipt: { views: ["v1"] },
    });
    expect(floors.floor("tracker")).toBe("CREWLET_TRACKER_LOG@1:4711");
    expect(heard).toEqual([["tracker", []]]);
  });

  test("pending raises the floor too: the reads that follow wait for it", async () => {
    stub(async () => json({ ...APPLIED, outcome: "pending" }, 200));
    const floors = new SessionFloors();
    const result = await act("set_pins", {}, { floors });
    expect(result.kind).toBe("pending");
    expect(floors.floor("tracker")).toBe("CREWLET_TRACKER_LOG@1:4711");
  });

  // AN OUTCOME NOBODY VOUCHED FOR IS NOT A SUCCESS, and a floor at a position
  // that may never land would make every read of the domain wait for it.
  test("an unknown outcome raises nothing", async () => {
    stub(async () => json({ ...APPLIED, outcome: "unknown" }, 200));
    const floors = new SessionFloors();
    const result = await act("set_pins", {}, { floors });
    expect(result.kind).toBe("unknown");
    expect(floors.floor("tracker")).toBeNull();
  });

  test("an outcome this build does not know is unknown, never applied", async () => {
    stub(async () => json({ ...APPLIED, outcome: "committed" }, 200));
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result.kind).toBe("unknown");
  });

  // THE REQUEST LEFT AND NOTHING CAME BACK: it may have landed. Never a
  // refusal, never retried here.
  test("a connection lost after the request left is unknown, and is sent once", async () => {
    const calls = stub(() => Promise.reject(new TypeError("Failed to fetch")));
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result.kind).toBe("unknown");
    expect(calls).toHaveLength(1);
  });

  test("a gateway's own 502 is unknown: the engine behind it may have written", async () => {
    stub(async () => new Response("<html>bad gateway</html>", { status: 502 }));
    expect((await act("set_pins", {}, { floors: new SessionFloors() })).kind).toBe("unknown");
  });

  test("the engine saying an interrupted call's outcome is unknown is unknown", async () => {
    stub(async () =>
      json(
        {
          error: "unavailable",
          tool: "set_pins",
          outcome: "unknown",
          detail: "the call was interrupted before it answered",
        },
        503,
      ),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({
      kind: "unknown",
      reason: "the call was interrupted before it answered",
    });
  });

  test("a tool that refused unavailable wrote nothing: refused, and worth retrying", async () => {
    stub(async () =>
      json({ error: "unavailable", tool: "set_pins", detail: "sealed for maintenance" }, 503),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({ kind: "refused", code: "unavailable", retryable: true });
  });

  // THE TOOL'S OWN SENTENCE ONLY WHERE IT NAMES THE ARGUMENT; everywhere else
  // it is written for a model, and a person is shown the dashboard's.
  test("an invalid argument is shown in the tool's own words", async () => {
    stub(async () =>
      json({ error: "invalid", tool: "update_work_item", detail: "status: no such status" }, 422),
    );
    const result = await act(
      "update_work_item",
      { item: "ENG-1" },
      { floors: new SessionFloors() },
    );
    expect(result).toMatchObject({
      kind: "refused",
      code: "invalid",
      sentence: "status: no such status",
      retryable: false,
    });
  });

  test("a stale version is shown in the dashboard's words", async () => {
    stub(async () =>
      json({ error: "stale_version", tool: "update_work_item", detail: "if_match 3 ≠ 4" }, 409),
    );
    const result = await act(
      "update_work_item",
      { item: "ENG-1" },
      { floors: new SessionFloors() },
    );
    expect(result).toMatchObject({ kind: "refused", code: "stale_version" });
    expect(result.kind === "refused" && result.sentence).toMatch(/Changed by somebody else/);
  });

  // A REFUSAL THAT SAYS THE SCREEN IS OUT OF DATE ASKS AGAIN, without a floor:
  // the page was drawn at a version somebody else has moved past, and until it
  // is redrawn every press sends that version and is refused again.
  test("a stale version asks the write's questions again and raises no floor", async () => {
    stub(async () =>
      json({ error: "stale_version", tool: "update_work_item", detail: "if_match 3 ≠ 4" }, 409),
    );
    const floors = new SessionFloors();
    const heard: [string | null, readonly string[]][] = [];
    floors.onWritten((domain, refreshes) => heard.push([domain, refreshes]));
    await act("update_work_item", { item: "ENG-1", if_match: 3 }, { floors });
    expect(heard).toEqual([["tracker", ["work_search"]]]);
    expect(floors.floor("tracker")).toBeNull();
  });

  test("a refusal about the request itself asks nothing again", async () => {
    stub(async () =>
      json({ error: "invalid", tool: "update_work_item", detail: "status: no such status" }, 422),
    );
    const floors = new SessionFloors();
    const heard = vi.fn();
    floors.onWritten(heard);
    await act("update_work_item", { item: "ENG-1" }, { floors });
    expect(heard).not.toHaveBeenCalled();
  });

  test("an unbound token is refused with the engine's remedy beside it", async () => {
    stub(async () =>
      json(
        {
          error: "unbound",
          tool: "set_pins",
          detail: "the token ci is bound to no seat",
          hint: "give a human seat contact.crewlet_operator_id: ci",
        },
        403,
      ),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({
      kind: "refused",
      code: "unbound",
      hint: "give a human seat contact.crewlet_operator_id: ci",
    });
  });

  test("a refused token asks the shell for another", async () => {
    stub(async () => json({ error: "invalid_token" }, 401));
    const asked = vi.fn();
    const stop = onTokenRequested(asked);
    try {
      const result = await act("set_pins", {}, { floors: new SessionFloors() });
      expect(result).toMatchObject({ kind: "refused", code: "invalid_token" });
      expect(asked).toHaveBeenCalledOnce();
    } finally {
      stop();
    }
  });

  test("the caller's own abort rejects: it is not an answer", async () => {
    stub(
      ({ init }) =>
        new Promise((_resolve, reject) =>
          init.signal?.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          ),
        ),
    );
    const controller = new AbortController();
    const pending = act("set_pins", {}, { signal: controller.signal, floors: new SessionFloors() });
    controller.abort();
    await expect(pending).rejects.toMatchObject({ name: "AbortError" });
  });
});
