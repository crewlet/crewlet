/**
 * A gesture refused `403 step_up_required` is confirmed once and REPLAYED, in
 * the transport every screen's writes go through.
 *
 * The invariants: the replay is the SAME request, body and headers included,
 * so nothing typed is lost; one confirmation answers every request refused at
 * the same moment; a declined confirmation, or nobody to ask, is the refusal
 * it was; the confirmation is never asked of the step-up route itself; and a
 * replay refused again is not asked twice.
 */

import { afterEach, describe, expect, test, vi } from "vitest";
import { poll } from "~/test/inCase.ts";

import { rest, RestError, setStepUpConfirmer, type StepUpWindow } from "./index.ts";

interface Sent {
  method: string;
  path: string;
  body: string | undefined;
  headers: Record<string, string>;
}

const STEP_UP = {
  status: 403,
  body: {
    error: "step_up_required",
    message: "This action needs you to have confirmed who you are recently.",
    reason: "step_up",
    window: "step_up_sensitive",
    grants: [],
  },
};
const DONE = { status: 200, body: { status: "applied" } };

/** A fetch answering each call from `answers` in turn, recording what was sent. */
function engine(...answers: { status: number; body: unknown }[]): Sent[] {
  const sent: Sent[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      sent.push({
        method: init?.method ?? "GET",
        path: url.pathname,
        body: typeof init?.body === "string" ? init.body : undefined,
        headers: (init?.headers ?? {}) as Record<string, string>,
      });
      const answer = answers.length > 1 ? answers.shift()! : answers[0]!;
      return new Response(JSON.stringify(answer.body), {
        status: answer.status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return sent;
}

let uninstall: (() => void) | null = null;

/** A confirmer that records the windows it was asked for and answers `with`. */
function confirming(withAnswer: boolean | Promise<boolean>) {
  const asked: StepUpWindow[] = [];
  uninstall = setStepUpConfirmer(async (window) => {
    asked.push(window);
    return withAnswer;
  });
  return asked;
}

afterEach(() => {
  uninstall?.();
  uninstall = null;
  vi.unstubAllGlobals();
});

describe("a refused gesture, confirmed", () => {
  test("is sent again, the same request, body and headers", async () => {
    const sent = engine(STEP_UP, DONE);
    const asked = confirming(true);

    const answer = await rest.put(
      "/config",
      { name: "Acme", mission: "Ship" },
      { "If-Match": '"01JREV"' },
    );

    expect(answer).toEqual({ status: "applied" });
    expect(asked).toEqual(["step_up_sensitive"]);
    expect(sent).toHaveLength(2);
    expect(sent[1]).toEqual(sent[0]);
    expect(JSON.parse(sent[1]!.body!)).toEqual({ name: "Acme", mission: "Ship" });
    expect(sent[1]!.headers["If-Match"]).toBe('"01JREV"');
  });

  test("one confirmation answers every request refused at the same moment", async () => {
    const sent = engine(STEP_UP, STEP_UP, DONE);
    let release!: (confirmed: boolean) => void;
    const asked = confirming(new Promise<boolean>((resolve) => (release = resolve)));

    const both = Promise.all([rest.post("/secrets/A", {}), rest.post("/secrets/B", {})]);
    await poll(() => expect(sent).toHaveLength(2));
    release(true);
    await both;

    expect(asked).toHaveLength(1);
    expect(sent.map((s) => s.path)).toEqual([
      "/secrets/A",
      "/secrets/B",
      "/secrets/A",
      "/secrets/B",
    ]);
  });

  // A REPLAY REFUSED AGAIN IS THE REFUSAL. Asking again would be a dialog
  // that reopens for as long as the engine keeps saying no.
  test("is asked once: a replay refused again is the refusal", async () => {
    const sent = engine(STEP_UP);
    const asked = confirming(true);

    const err = await rest.post("/config/rekey", {}).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RestError);
    expect((err as RestError).code).toBe("step_up_required");
    expect(asked).toHaveLength(1);
    expect(sent).toHaveLength(2);
  });
});

describe("a refused gesture, not confirmed", () => {
  test("declined, it is the refusal it was, and nothing is sent again", async () => {
    const sent = engine(STEP_UP, DONE);
    confirming(false);

    const err = await rest.post("/secrets/A", {}).catch((e: unknown) => e);
    expect((err as RestError).code).toBe("step_up_required");
    expect(sent).toHaveLength(1);
  });

  // THE CONTROL FOR THE WHOLE SUITE: with nobody to ask, the transport is
  // exactly what it was before the ceremony existed.
  test("with nobody to ask, it is the refusal it was", async () => {
    const sent = engine(STEP_UP, DONE);
    const err = await rest.post("/secrets/A", {}).catch((e: unknown) => e);
    expect((err as RestError).code).toBe("step_up_required");
    expect(sent).toHaveLength(1);
  });

  // THE STEP-UP ROUTE ANSWERS `step_up_required` ITSELF for a credential
  // nobody present can confirm, and asking to confirm the confirmation is a
  // dialog that never closes.
  test("the step-up route is never asked to confirm itself", async () => {
    const sent = engine(STEP_UP, DONE);
    const asked = confirming(true);
    const err = await rest.post("/auth/step-up", { password: "x" }).catch((e: unknown) => e);
    expect((err as RestError).code).toBe("step_up_required");
    expect(asked).toEqual([]);
    expect(sent).toHaveLength(1);
  });

  test("a caller that gives up while the person is asked is not held to it", async () => {
    engine(STEP_UP, DONE);
    confirming(new Promise<boolean>(() => {}));
    const controller = new AbortController();
    const pending = rest
      .request("POST", "/secrets/A", { body: {}, signal: controller.signal })
      .catch((e: unknown) => e);
    await poll(() => expect(vi.mocked(fetch)).toHaveBeenCalledTimes(1));
    controller.abort();
    expect(((await pending) as Error).name).toBe("AbortError");
  });

  // ONLY THE STEP-UP CODE: a refusal on authority is not cured by a
  // password, and asking for one would teach a person the dialog is noise.
  test("a refusal on authority is not a step-up", async () => {
    const sent = engine({
      status: 403,
      body: { error: "unauthorized", grants: ["secrets:write"] },
    });
    const asked = confirming(true);
    await rest.post("/secrets/A", {}).catch(() => {});
    expect(asked).toEqual([]);
    expect(sent).toHaveLength(1);
  });
});
