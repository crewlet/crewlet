/**
 * The write client says exactly what became of a change, and never more.
 *
 * Every case names an outcome a person acts on differently: a change that
 * landed, one this node has not applied yet, one nobody can vouch for, and one
 * the engine refused. Collapsing any two of them tells somebody something
 * false about their own change.
 */

import { afterEach, describe, expect, test, vi } from "vitest";
import { ACT_ERRORS } from "../contract/errors.ts";
import { act, newActOpID } from "./act.ts";
import { SessionFloors } from "./floors.ts";
import { currentSessionNeed, sessionRestored, setStepUpConfirmer } from "./session.ts";

afterEach(() => {
  vi.unstubAllGlobals();
  sessionRestored();
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

/** The operation key a call sent, off its `Idempotency-Key` header. */
const keyOf = (call: Call) => (call.init.headers as Record<string, string>)["Idempotency-Key"];

describe("the request", () => {
  // THE OPERATION RIDES THE HEADER every surface reads it from, and the body is
  // the tool's arguments and nothing else: the engine refuses a body key it
  // does not read, so an operation key sent anywhere but the header would be
  // refused on every press.
  test("is the arguments as JSON, to the tool's route, under its operation key", async () => {
    const calls = stub(async () => json(APPLIED, 200));
    const floors = new SessionFloors();
    const opId = newActOpID();
    await act("set_pins", { views: { add: ["v1"] } }, { opId, floors });
    const { url, init } = calls[0]!;
    expect(init.method).toBe("POST");
    expect(new URL(url).pathname).toBe("/operator/act/set_pins");
    expect((init.headers as Record<string, string>)["Content-Type"]).toBe("application/json");
    expect(keyOf(calls[0]!)).toBe(opId);
    expect(JSON.parse(init.body as string)).toEqual({ args: { views: { add: ["v1"] } } });
  });

  // THE ENGINE REFUSES A KEY THAT IS NOT A UUIDv7 — it reads the instant the
  // key carries — and a browser reached over plain http has no
  // `crypto.randomUUID` to mint one with.
  test("mints a version-7 UUID without randomUUID", () => {
    const own = Object.getOwnPropertyDescriptor(crypto, "randomUUID");
    Object.defineProperty(crypto, "randomUUID", { value: undefined, configurable: true });
    try {
      const ids = new Set(Array.from({ length: 50 }, () => newActOpID()));
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
    const id = newActOpID(0x0192_7c3a_4e5b);
    expect(id.startsWith("01927c3a-4e5b-7")).toBe(true);
  });

  test("sends the same operation key again when the caller retries a press", async () => {
    const calls = stub(async () => json(APPLIED, 200));
    const floors = new SessionFloors();
    const first = await act("set_pins", { views: { add: ["v1"] } }, { floors });
    await act("set_pins", { views: { add: ["v1"] } }, { opId: first.opId, floors });
    expect(keyOf(calls[0]!)).toBe(first.opId);
    expect(keyOf(calls[1]!)).toBe(first.opId);
  });

  // A PRESS REFUSED FOR A STALE PROOF IS MADE ONCE THE PERSON HAS CONFIRMED,
  // under the SAME operation: the replay is the press, not a second one.
  test("a step-up is confirmed and the same operation replayed", async () => {
    let n = 0;
    const calls = stub(async () =>
      n++ === 0 ? json({ error: "step_up_required", window: "step_up" }, 403) : json(APPLIED, 200),
    );
    const stop = setStepUpConfirmer(async () => true);
    try {
      const result = await act("set_pins", {}, { floors: new SessionFloors() });
      expect(result.kind).toBe("applied");
      expect(calls).toHaveLength(2);
      expect(keyOf(calls[1]!)).toBe(keyOf(calls[0]!));
    } finally {
      stop();
    }
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

  test("the engine's unknown names the operation a retry sends again", async () => {
    const opId = newActOpID();
    stub(async () =>
      json(
        { error: "unavailable", outcome: "unknown", op_id: opId, detail: "nobody can say" },
        503,
      ),
    );
    const result = await act("set_pins", {}, { opId, floors: new SessionFloors() });
    expect(result).toMatchObject({ kind: "unknown", opId, reason: "nobody can say" });
  });

  // WHETHER WAITING CHANGES IT IS THE ENGINE'S TO SAY: a `503` it wrote
  // carries a `Retry-After` where it does, and none where it does not.
  test("a tool that refused unavailable wrote nothing: refused, and worth retrying", async () => {
    stub(
      async () =>
        new Response(JSON.stringify({ error: "unavailable", tool: "set_pins", detail: "sealed" }), {
          status: 503,
          headers: { "Content-Type": "application/json", "Retry-After": "2" },
        }),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({ kind: "refused", code: "unavailable", retryable: true });
  });

  test("an unavailable the engine says no wait clears is not worth retrying", async () => {
    stub(async () =>
      json({ error: "unavailable", tool: "set_pins", refusal: "log_full", detail: "full" }, 503),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({ kind: "refused", code: "unavailable", retryable: false });
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

  // A REFUSAL ON AUTHORITY SAYS WHAT WOULD ADMIT THE CALLER, read off the
  // answer — a screen never names a grant itself.
  test("a refusal on authority carries the grants that would admit the caller", async () => {
    stub(async () =>
      json(
        { error: "unauthorized", reason: "no_grant", grants: ["fleet:operate"], tool: "set_pins" },
        403,
      ),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({
      kind: "refused",
      code: "unauthorized",
      grants: ["fleet:operate"],
      sentence: ACT_ERRORS.unauthorized,
    });
  });

  // NOBODY SIGNED IN IS THE SESSION'S NEED, not a token to set: the browser
  // holds nothing a script could read, so the repair is a sign-in.
  test("a refused credential asks for a sign-in", async () => {
    stub(async () => json({ error: "invalid_token" }, 401));
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({ kind: "refused", code: "invalid_token" });
    expect(currentSessionNeed()).toBe("sign_in");
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

// NO SENTENCE MENTIONS A TOKEN: the dashboard holds no credential a person
// could set, so a sentence telling them to is advice they cannot take. (A
// TOKEN BUDGET is the model's tokens, which is another word.)
test("no refusal sentence tells a person about a token", () => {
  const offenders = Object.entries(ACT_ERRORS)
    .filter(([, sentence]) => sentence !== null && /\btokens?\b(?! budget)/i.test(sentence))
    .map(([code]) => code);
  expect(offenders).toEqual([]);
});
