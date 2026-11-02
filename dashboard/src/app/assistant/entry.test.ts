/**
 * The assistant's entry: what the frame holds before anybody opens it.
 *
 * THE BUS IS A MODULE, NOT A WINDOW EVENT: an artifact's frame can post a
 * message to this window and any script can dispatch a DOM event, so neither
 * reaches a listener here. THE TAB KEEPS AN ID AND A FLAG, read back only
 * when it is one, and the hand-over that empties a tab's storage takes it.
 * AND THE BUTTON'S REASON IS THE ENGINE'S, with "checking" for the second it
 * takes to ask, so nobody is told the assistant is off while it is answering.
 */

import { afterEach, describe, expect, test } from "vitest";
import { emit, on } from "./bus.ts";
import { readTab, writeTab } from "./tab.ts";
import { assistantModelFor, assistantUnavailableFor } from "./availability.ts";
import { assistantModelNotice, assistantUnavailable } from "~/protocol/assistantWire.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";

afterEach(() => sessionStorage.clear());

describe("the bus", () => {
  test("hands an event to its own listeners and to nobody else", () => {
    const heard: string[] = [];
    const off = on("assistant.navigation.open", (e) => heard.push(e.path.join("/")));
    const other = on("assistant.context.invalidate", () => heard.push("wrong"));
    emit("assistant.navigation.open", { path: ["worklet", "ENG-1"], query: {}, mode: "here" });
    off();
    other();
    emit("assistant.navigation.open", { path: ["worklet", "ENG-2"], query: {}, mode: "here" });
    expect(heard).toEqual(["worklet/ENG-1"]);
  });

  test("is not reachable by a message posted to the window", async () => {
    const heard: unknown[] = [];
    const off = on("assistant.action.apply", (e) => heard.push(e));
    window.postMessage(
      {
        type: "assistant.action.apply",
        correlation_id: "x",
        action: "a.b",
        payload: 1,
        source_revision: "",
      },
      "*",
    );
    window.dispatchEvent(new CustomEvent("assistant.action.apply", { detail: {} }));
    await new Promise((r) => setTimeout(r, 0));
    off();
    expect(heard).toEqual([]);
  });

  test("a listener that throws does not silence the others, and its fault is reported", () => {
    const heard: string[] = [];
    const a = on("assistant.context.invalidate", () => {
      throw new Error("screen fault");
    });
    const b = on("assistant.context.invalidate", () => heard.push("b"));
    expect(() =>
      emit("assistant.context.invalidate", { reason: "data", revision: "r", revisions: {} }),
    ).toThrow("screen fault");
    a();
    b();
    expect(heard).toEqual(["b"]);
  });
});

describe("the tab's record", () => {
  test("keeps the open flag and a conversation id, and nothing it cannot read back", () => {
    writeTab({ open: true, conversation: "403212ed-5a4a-586d-8af6-c62411a17c2f" });
    expect(readTab()).toEqual({ open: true, conversation: "403212ed-5a4a-586d-8af6-c62411a17c2f" });
    sessionStorage.setItem(
      STORAGE_KEYS.assistantTab,
      JSON.stringify({ open: "yes", conversation: "../x" }),
    );
    expect(readTab()).toEqual({ open: false, conversation: null });
    sessionStorage.setItem(STORAGE_KEYS.assistantTab, "{not json");
    expect(readTab()).toEqual({ open: false, conversation: null });
  });

  test("lives in the tab's storage, which the hand-over empties", () => {
    writeTab({ open: true, conversation: "403212ed-5a4a-586d-8af6-c62411a17c2f" });
    expect(localStorage.getItem(STORAGE_KEYS.assistantTab)).toBeNull();
    // `lib/session.ts` hands a tab to somebody else by emptying its storage.
    sessionStorage.clear();
    expect(readTab()).toEqual({ open: false, conversation: null });
  });
});

describe("whether the reader may ask", () => {
  const ready = { anonymous: false, asking: false, assistant: null } as const;

  test("a reader the engine allows may", () => {
    expect(assistantUnavailableFor(ready, "Nimbus")).toBeNull();
  });

  test("each of the engine's reasons is its own sentence, and off names the company", () => {
    const off = assistantUnavailableFor({ ...ready, assistant: "disabled" }, "Nimbus");
    expect(off).toMatchObject({ reason: "disabled", short: "An admin turned Ask Crewlet off." });
    expect(off?.title).toBe("Ask Crewlet is off for Nimbus");
    for (const reason of ["no_model", "not_a_person", "no_permission"] as const) {
      const words = assistantUnavailableFor({ ...ready, assistant: reason }, "Nimbus");
      expect(words?.reason).toBe(reason);
      expect(words?.short).not.toBe(off?.short);
    }
  });

  test("nobody signed in is told to sign in, and a first read in flight is checking", () => {
    expect(assistantUnavailableFor({ ...ready, anonymous: true }, "")?.short).toBe(
      "Sign in to ask.",
    );
    expect(
      assistantUnavailableFor({ ...ready, asking: true, assistant: "disabled" }, "")?.reason,
    ).toBe("checking");
  });
});

describe("the assistant's model", () => {
  const signedOut = {
    available: false,
    reason: "model_not_ready",
    model: {
      code: "model_signed_out",
      message: "Ask Crewlet's model is signed out, so it cannot answer.",
      hint: "Ask an administrator to sign Ask Crewlet's model in again.",
    },
  } as const;

  // THE MODEL'S STATE LEAVES THE ASK BUTTON LIT: the drawer opens, with the
  // conversation readable, and says it there instead.
  test("a model that is not ready is the drawer's notice, never a reason to refuse the reader", () => {
    expect(assistantUnavailable(signedOut)).toBeNull();
    expect(assistantModelNotice(signedOut)?.code).toBe("model_signed_out");
    expect(assistantModelNotice({ available: true })).toBeNull();
    expect(assistantModelNotice({ available: false, reason: "no_permission" })).toBeNull();
    expect(assistantUnavailable({ available: false, reason: "no_permission" })).toBe(
      "no_permission",
    );
  });

  test("the notice is the engine's sentence and hint, and Send's reason its own phrase", () => {
    const problem = assistantModelFor({ assistantModel: signedOut.model });
    expect(problem).toEqual({
      code: "model_signed_out",
      short: "Ask Crewlet's model is signed out",
      title: signedOut.model.message,
      body: signedOut.model.hint,
    });
    expect(assistantModelFor({ assistantModel: null })).toBeNull();
  });

  test("a spent plan says when the reader can ask again", () => {
    const busy = assistantModelFor({
      assistantModel: {
        code: "model_busy",
        message: "Ask Crewlet's model has reached its usage limit for now.",
        hint: "It is tried again once the limit resets.",
        retry_at: "2026-10-25T14:05:00Z",
      },
    });
    expect(busy?.short).toBe("Ask Crewlet's model is at its usage limit");
    expect(busy?.body).toMatch(
      /^It is tried again once the limit resets\. You can ask again from .+\.$/,
    );
  });

  test("no copy carries an en or em dash", () => {
    for (const code of ["model_signed_out", "model_busy", "model_unavailable"] as const) {
      const problem = assistantModelFor({ assistantModel: { code, message: "m" } });
      expect(problem?.short).not.toMatch(/[\u2013\u2014]/);
    }
  });
});
