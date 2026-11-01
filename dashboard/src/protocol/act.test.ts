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
import { UNCONFIRMED_SENTENCE } from "./writeOutcome.ts";
import { SessionFloors } from "./session.ts";
import { currentSessionNeed, sessionRestored, setStepUpConfirmer } from "./signin.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

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
    headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
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

  // A CAVEAT IS READ IN A PERSON'S WORDS, from `caveats`, and a receipt's
  // `warnings` (written for a model, naming tools and arguments) is never read
  // as one, whatever an engine sends there.
  test("carries the caveats the engine answered for a person, and only those", async () => {
    stub(async () =>
      json(
        {
          ...APPLIED,
          receipt: { views: ["v1"], warnings: ["Use ask: ana, if you expect a reply."] },
          caveats: [
            { code: "default_assignee_gone", message: "Assign it.", action: "required" },
            { code: "body_links_capped", message: "Only some are linked.", action: "none" },
            { code: "from_a_newer_engine", message: "Something new.", action: "escalate" },
            { code: "no_words", message: " ", action: "none" },
            "not a caveat",
          ],
        },
        200,
      ),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result.kind).toBe("applied");
    if (result.kind !== "applied") return;
    expect(result.caveats).toEqual([
      { code: "default_assignee_gone", message: "Assign it.", action: "required" },
      { code: "body_links_capped", message: "Only some are linked.", action: "none" },
      // AN ACTION THIS BUILD DOES NOT KNOW asks nothing: a toast nobody can
      // say why it stays must not demand dismissal.
      { code: "from_a_newer_engine", message: "Something new.", action: "none" },
    ]);
    expect(JSON.stringify(result.caveats)).not.toContain("ask:");
  });

  // A ROLLING UPGRADE: an older node answers no `caveats` and still carries
  // its model's `warnings` on the receipt. The answer reads as one with
  // nothing to add, and the model's words stay where nothing draws them.
  test("an older node's answer, with no caveats and the model's warnings, adds nothing", async () => {
    stub(async () =>
      json(
        {
          ...APPLIED,
          receipt: { views: ["v1"], warnings: ["Use ask: ana, if you expect a reply."] },
        },
        200,
      ),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result.kind).toBe("applied");
    if (result.kind !== "applied") return;
    expect(result.caveats).toEqual([]);
  });

  test("an answer with no caveats carries an empty list", async () => {
    stub(async () => json(APPLIED, 200));
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result.kind === "applied" && result.caveats).toEqual([]);
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

  // THE ENGINE'S UNKNOWN IS TOLD IN A PERSON'S WORDS: its `detail` is the
  // operation key's retry rule for a client sending the header, which a
  // person never sees (ADR-0060).
  test("the engine saying an interrupted call's outcome is unknown is unknown", async () => {
    stub(async () =>
      json(
        {
          error: "unavailable",
          tool: "set_pins",
          outcome: "unknown",
          detail:
            "Couldn't confirm this change was saved. Retry with the same op_id as the Idempotency-Key header.",
        },
        503,
      ),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({ kind: "unknown", reason: UNCONFIRMED_SENTENCE });
    expect(JSON.stringify(result)).not.toMatch(/op_id|Idempotency-Key/);
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
    expect(result).toMatchObject({ kind: "unknown", opId, reason: UNCONFIRMED_SENTENCE });
  });

  // WHETHER WAITING CHANGES IT IS THE ENGINE'S TO SAY: a `503` it wrote
  // carries a `Retry-After` where it does, and none where it does not.
  test("a tool that refused unavailable wrote nothing: refused, and worth retrying", async () => {
    stub(
      async () =>
        new Response(JSON.stringify({ error: "unavailable", tool: "set_pins", detail: "sealed" }), {
          status: 503,
          headers: { ...ENGINE_HEADERS, "Content-Type": "application/json", "Retry-After": "2" },
        }),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    // AND IT CARRIES THE ENGINE'S DETAIL, which a refusal class carries only
    // in a person's words (ADR-0060), for a screen that shows the engine's
    // words for every refusal.
    expect(result).toMatchObject({
      kind: "refused",
      code: "unavailable",
      retryable: true,
      detail: "sealed",
    });
  });

  test("an unavailable the engine says no wait clears is not worth retrying", async () => {
    stub(async () =>
      json(
        {
          error: "unavailable",
          tool: "set_pins",
          refusal: "log_full",
          message: "This can't be done here, and waiting won't change that.",
        },
        503,
      ),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({ kind: "refused", code: "unavailable", retryable: false });
  });

  // A FIXED SENTENCE IS SHOWN ONLY WHERE THE ENGINE'S ANSWER CANNOT CONTRADICT
  // IT. "Try again in a moment" drawn over an answer carrying no Retry-After
  // told a person to wait for a full log, an evicted node or a note the running
  // node could not verify, none of which a wait changes.
  describe("the sentence a refusal is shown in", () => {
    const answer = (payload: Record<string, unknown>, status: number, retryAfter?: string) =>
      stub(
        async () =>
          new Response(JSON.stringify(payload), {
            status,
            headers: {
              ...ENGINE_HEADERS,
              "Content-Type": "application/json",
              ...(retryAfter === undefined ? {} : { "Retry-After": retryAfter }),
            },
          }),
      );
    const refused = async () => {
      const result = await act("steer_turn", {}, { floors: new SessionFloors() });
      if (result.kind !== "refused") throw new Error(`answered ${result.kind}`);
      return result;
    };
    const sentenceOf = async () => (await refused()).sentence;

    // THE ENGINE WROTE THE DETAIL FOR THE PERSON: a refusal class carries a
    // detail only in words written for whoever pressed (ADR-0060), about this
    // refusal where the dashboard's sentence is about every refusal of its
    // code, so it is shown whatever the code and whatever the Retry-After.
    // Mutation: drop the detail rule, or let the wait rule run first, and this
    // fails.
    test("a detail the engine wrote for the person is shown, wait or none", async () => {
      const answered = "This question was already answered by @ana, so your answer was not used.";
      answer({ error: "already_answered", detail: answered }, 409);
      expect(await sentenceOf()).toBe(answered);

      const spent =
        "The company's token budget for this window is spent, so nothing was sent. It comes back when the window resets, or as soon as the ceiling is raised.";
      answer({ error: "budget_exhausted", detail: spent }, 409);
      expect(await sentenceOf()).toBe(spent);

      const stuck = "Nothing more can be saved here until an administrator makes room for it.";
      answer(
        {
          error: "unavailable",
          message: "This can't be done here, and waiting won't change that.",
          detail: stuck,
        },
        503,
      );
      expect(await sentenceOf()).toBe(stuck);

      const soon = "No node could take this right now. Try again in a moment.";
      answer({ error: "unavailable", detail: soon }, 503, "2");
      expect(await sentenceOf()).toBe(soon);
    });

    // A REUSED KEY IS TOLD IN THE ENGINE'S WORDS FOR IT: the one sentence every
    // surface taking a key answers it with (`opkey.Reused`), a person's, which
    // says to look again and names no key. Mutation: leave `invalid_input` out
    // of the codes whose detail is a person's and this reads the dashboard's
    // sentence instead.
    test("a reused key is told in the engine's words for it", async () => {
      const reused =
        "What this change was about moved since it was first sent, so nothing was written. Look again, then make the change.";
      answer({ error: "invalid_input", field: "Idempotency-Key", detail: reused }, 409);
      expect(await sentenceOf()).toBe(reused);
    });

    // A TRANSPORT'S DETAIL IS NEVER SHOWN: the request guard's and the
    // transport's own codes carry a detail written for whoever built the
    // request (a draining node's, a request from another site), so the
    // dashboard's sentence for the code stands, and the detail goes no further
    // than the protocol. Mutation: take every code's detail as a person's and
    // this fails.
    test("a transport's detail is never shown, nor handed on", async () => {
      answer(
        {
          error: "draining",
          detail:
            "this node is draining for a shutdown: the turns already running finish, and nothing new is started here",
          hint: "retry against another node, or once this one has restarted; /ready answers 503 for as long as the drain lasts",
        },
        503,
        "30",
      );
      expect(await refused()).toMatchObject({ sentence: ACT_ERRORS.draining, detail: "" });

      answer(
        {
          error: "csrf_origin",
          detail: "https://elsewhere.example isn't an address for this deployment.",
          hint: "Set this deployment's address in `api.external_url`, and any other hostname in `api.auth.allowed_origins`.",
        },
        403,
      );
      expect(await refused()).toMatchObject({ sentence: ACT_ERRORS.csrf_origin, detail: "" });
    });

    // NO DETAIL AND NO WAIT: nobody has worded the condition for a person
    // (ADR-0060), and the engine's own sentence for the code says waiting
    // won't change it, where the dashboard's would ask for a wait.
    test("an unavailable no wait clears is the engine's sentence for it", async () => {
      answer(
        {
          error: "unavailable",
          message: "This can't be done here, and waiting won't change that.",
        },
        503,
      );
      expect(await sentenceOf()).toBe("This can't be done here, and waiting won't change that.");

      answer(
        {
          error: "identity_unavailable",
          message: "Can't check who you are here, and waiting won't change that.",
        },
        503,
      );
      expect(await sentenceOf()).toBe(
        "Can't check who you are here, and waiting won't change that.",
      );
    });

    test("a refusal a wait clears keeps the dashboard's sentence", async () => {
      answer({ error: "unavailable", message: "Can't answer right now." }, 503, "2");
      expect(await sentenceOf()).toBe(ACT_ERRORS.unavailable);
      answer({ error: "identity_unavailable", message: "Can't check who you are." }, 503, "2");
      expect(await sentenceOf()).toBe(ACT_ERRORS.identity_unavailable);
    });

    // EVERY FIXED SENTENCE THAT ASKS FOR A WAIT gives way to the engine's where
    // the answer carries none, except the codes the engine always answers with
    // a Retry-After, each named here with where it writes one. A sentence
    // reworded to ask for a wait without being one of those fails here; so does
    // a name below whose sentence stopped asking.
    test("every fixed sentence asking for a wait yields where the answer carries none", async () => {
      const alwaysWaits = new Set([
        "draining", // internal/api/drain.go: drainGate sets DrainRetryAfter
        "starting", // internal/api/boot.go: BootRetryAfter
        "no_active_revision", // httpjson.NoActiveRevision: NoActiveRevisionRetry
      ]);
      const asksForAWait = (sentence: string) => /\btry again\b|\bin a moment\b/i.test(sentence);
      for (const code of alwaysWaits) {
        expect(asksForAWait(ACT_ERRORS[code as keyof typeof ACT_ERRORS] ?? "")).toBe(true);
      }
      const yielding = Object.entries(ACT_ERRORS).filter(
        ([code, sentence]) => sentence !== null && asksForAWait(sentence) && !alwaysWaits.has(code),
      );
      expect(yielding.map(([code]) => code).sort()).toEqual([
        "identity_unavailable",
        "unavailable",
      ]);
      for (const [code] of yielding) {
        answer({ error: code, message: `the engine's own for ${code}` }, 503);
        expect(await sentenceOf()).toBe(`the engine's own for ${code}`);
      }
    });
  });

  // AN INVALID ARGUMENT IS SAID BY WHAT REFUSED IT: the dashboard keeps no
  // sentence for `invalid`, and the engine's detail is the one it wrote for
  // the person (ADR-0060).
  test("an invalid argument is shown in the engine's detail", async () => {
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

  // A REFUSAL NOBODY WORDED FOR A PERSON CARRIES NO DETAIL (ADR-0060), and
  // its code's own sentence is the whole of what is shown.
  test("an invalid refusal with no detail is shown in its code's own sentence", async () => {
    const message =
      "That change was refused, and nothing was written. Check what was entered, then try again.";
    stub(async () => json({ error: "invalid", tool: "create_work_item", message }, 422));
    const result = await act("create_work_item", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({ kind: "refused", code: "invalid", sentence: message });
  });

  // A STALE VERSION NOBODY WORDED FOR A PERSON carries no detail (ADR-0060),
  // and is shown in the dashboard's sentence for the code.
  test("a stale version is shown in the dashboard's words", async () => {
    stub(async () => json({ error: "stale_version", tool: "update_work_item" }, 409));
    const result = await act(
      "update_work_item",
      { item: "ENG-1" },
      { floors: new SessionFloors() },
    );
    expect(result).toMatchObject({ kind: "refused", code: "stale_version" });
    expect(result.kind === "refused" && result.sentence).toMatch(/Changed by somebody else/);
  });

  // AN ANSWER TO A QUESTION THE RUN MOVED ON FROM ASKS FOR THE ONE IT ASKS
  // NOW. `answer_run` names the question it answers (`launch_id`), and a run
  // that went on to another refuses it `stale_version`: the screen drew a
  // question that is gone, so the refusal asks the run board and the
  // decisions again and the question waiting now is the one drawn.
  //
  // Mutation: refresh nothing on a stale answer, and the old question stays.
  test("an answer to a question that moved on asks for the one asked now", async () => {
    stub(async () => json({ error: "stale_version", tool: "answer_run" }, 409));
    const floors = new SessionFloors();
    const heard: [string | null, readonly string[]][] = [];
    floors.onWritten((domain, refreshes) => heard.push([domain, refreshes]));
    const result = await act("answer_run", { turn_id: "t-1", launch_id: "q1" }, { floors });
    expect(result).toMatchObject({ kind: "refused", code: "stale_version" });
    expect(heard).toEqual([[null, ["executors", "decisions"]]]);
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
      json({ error: "unauthorized", reason: "no_grant", grants: ["admin"], tool: "set_pins" }, 403),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({
      kind: "refused",
      code: "unauthorized",
      grants: ["admin"],
      sentence: ACT_ERRORS.unauthorized,
    });
  });

  // THE RULE SAYS WHAT A REFUSAL ON AUTHORITY SAYS: `unauthorized` is every
  // rule's, so its sentence is the engine's for the rule that refused, and the
  // rule travels beside the grants. Mutation: say ACT_ERRORS' one sentence for
  // the code, or leave the reason behind, and this fails.
  test("a refusal on authority says its rule's sentence, and carries the rule", async () => {
    const said =
      "Your role on this doesn't allow that. Whoever owns it can share it with you at a higher one.";
    stub(async () =>
      json(
        {
          error: "unauthorized",
          message: said,
          reason: "role",
          role: "editor",
          grants: ["admin"],
          tool: "set_pins",
        },
        403,
      ),
    );
    const result = await act("set_pins", {}, { floors: new SessionFloors() });
    expect(result).toMatchObject({
      kind: "refused",
      code: "unauthorized",
      reason: "role",
      grants: ["admin"],
      sentence: said,
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
