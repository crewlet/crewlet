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
import {
  askVerbs,
  markReopened,
  takeQueuedQuestion,
  takeReopened,
  type PanelHold,
} from "./control.ts";
import { carryConversation, readTab, reopenChoice, writeTab, type AssistantTab } from "./tab.ts";
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

const ID = "403212ed-5a4a-586d-8af6-c62411a17c2f";
const EMPTY_TAB: AssistantTab = { open: false, conversation: null, shown: null, busy: false };

describe("the tab's record", () => {
  test("keeps the open flag and a conversation id, and nothing it cannot read back", () => {
    writeTab({ open: true, conversation: "403212ed-5a4a-586d-8af6-c62411a17c2f" });
    expect(readTab()).toEqual({ ...EMPTY_TAB, open: true, conversation: ID });
    writeTab({ shown: { conversation: ID, page: "item:worklet/ENG-1" }, busy: true });
    expect(readTab()).toMatchObject({
      shown: { conversation: ID, page: "item:worklet/ENG-1" },
      busy: true,
    });
    sessionStorage.setItem(
      STORAGE_KEYS.assistantTab,
      JSON.stringify({
        open: "yes",
        conversation: "../x",
        shown: { conversation: 3, page: 4 },
        busy: 1,
      }),
    );
    expect(readTab()).toEqual({ ...EMPTY_TAB, shown: { conversation: null, page: null } });
    sessionStorage.setItem(STORAGE_KEYS.assistantTab, "{not json");
    expect(readTab()).toEqual(EMPTY_TAB);
  });

  test("lives in the tab's storage, which the hand-over empties", () => {
    writeTab({ open: true, conversation: "403212ed-5a4a-586d-8af6-c62411a17c2f" });
    expect(localStorage.getItem(STORAGE_KEYS.assistantTab)).toBeNull();
    // `lib/session.ts` hands a tab to somebody else by emptying its storage.
    sessionStorage.clear();
    expect(readTab()).toEqual(EMPTY_TAB);
  });
});

describe("which conversation the drawer opens on", () => {
  const OTHER = "6a1d3c0e-2b4f-4c8e-9d7a-1f2e3d4c5b6a";
  const tab = (change: Partial<AssistantTab>): AssistantTab => ({
    ...EMPTY_TAB,
    conversation: ID,
    shown: { conversation: ID, page: "item:worklet/ENG-1" },
    ...change,
  });

  test("the page it last showed resumes it; any other page starts fresh", () => {
    expect(reopenChoice(tab({}), "item:worklet/ENG-1")).toBe("resume");
    expect(reopenChoice(tab({}), "item:worklet/ENG-2")).toBe("fresh");
    expect(reopenChoice(tab({}), null)).toBe("fresh");
    expect(reopenChoice(EMPTY_TAB, "item:worklet/ENG-1")).toBe("fresh");
  });

  test("an empty conversation started on a page is that page's, so a second reopen makes no other", () => {
    const fresh = tab({ conversation: null, shown: { conversation: null, page: "home:" } });
    expect(reopenChoice(fresh, "home:")).toBe("resume");
  });

  test("a conversation switched elsewhere (Ask Crewlet's page) is not the one shown on this page", () => {
    expect(reopenChoice(tab({ conversation: OTHER }), "item:worklet/ENG-1")).toBe("fresh");
  });

  test("work in flight is shown on every page", () => {
    expect(reopenChoice(tab({ busy: true }), "item:worklet/ENG-2")).toBe("resume");
  });

  test("Ask Crewlet's own navigation carries the conversation to the page it opens", () => {
    writeTab({ conversation: ID, shown: { conversation: ID, page: "home:" } });
    carryConversation("item:worklet/ENG-2");
    expect(reopenChoice(readTab(), "item:worklet/ENG-2")).toBe("resume");
    carryConversation(null);
    expect(readTab().shown?.page).toBe("item:worklet/ENG-2");
  });

  test("only the person's open is a reopen, and it is taken once", () => {
    expect(takeReopened()).toBe(false);
    markReopened();
    expect(takeReopened()).toBe(true);
    expect(takeReopened()).toBe(false);
  });
});

describe("whether the reader may ask", () => {
  const ready = { anonymous: false, asking: false, assistant: null } as const;

  test("a reader the engine allows may", () => {
    expect(assistantUnavailableFor(ready, "Nimbus")).toBeNull();
  });

  test("each of the engine's reasons is its own sentence, and no model names the company", () => {
    const off = assistantUnavailableFor({ ...ready, assistant: "no_model" }, "Nimbus");
    expect(off).toMatchObject({
      reason: "no_model",
      short: "No model is assigned to Ask Crewlet yet.",
    });
    expect(off?.title).toBe("Ask Crewlet has no model for Nimbus yet");
    for (const reason of ["model_problem", "not_a_person", "no_permission"] as const) {
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
      assistantUnavailableFor({ ...ready, asking: true, assistant: "no_model" }, "")?.reason,
    ).toBe("checking");
  });
});

describe("the assistant's model", () => {
  const signedOut = {
    available: false,
    reason: "model_not_ready",
    notice: {
      code: "model_signed_out",
      message: "Ask Crewlet's model is signed out, so it cannot answer.",
      hint: "Ask an administrator to sign Ask Crewlet's model in again.",
    },
    model: null,
    can_assign: false,
    reads_images: false,
  } as const;

  // THE MODEL'S STATE LEAVES THE ASK BUTTON LIT: the drawer opens, with the
  // conversation readable, and says it there instead.
  test("a model that is not ready is the drawer's notice, never a reason to refuse the reader", () => {
    expect(assistantUnavailable(signedOut)).toBeNull();
    expect(assistantModelNotice(signedOut)?.code).toBe("model_signed_out");
    expect(
      assistantModelNotice({
        available: true,
        model: null,
        notice: null,
        can_assign: false,
        reads_images: false,
      }),
    ).toBeNull();
    expect(
      assistantModelNotice({
        available: false,
        reason: "no_permission",
        model: null,
        notice: null,
        can_assign: false,
        reads_images: false,
      }),
    ).toBeNull();
    expect(
      assistantUnavailable({
        available: false,
        reason: "no_permission",
        model: null,
        notice: null,
        can_assign: false,
        reads_images: false,
      }),
    ).toBe("no_permission");
  });

  test("the notice is the engine's sentence and hint, and Send's reason its own phrase", () => {
    const problem = assistantModelFor({ assistantModel: signedOut.notice });
    expect(problem).toEqual({
      code: "model_signed_out",
      short: "Ask Crewlet's model is signed out",
      title: signedOut.notice.message,
      body: signedOut.notice.hint,
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

// THE FRAME'S VERBS: a question about what a screen shows is queued for a new
// conversation and the palette's for the open one; either opens a closed
// panel, and only a question left in the box takes focus from the screen.
// Mutation: queue askAbout as a plain question, drop its `send`, or focus the
// box for a sent one, and a case goes red.
describe("the frame's verbs", () => {
  function hold(over: Partial<PanelHold> = {}) {
    const did: string[] = [];
    const verbs = askVerbs({
      open: false,
      here: false,
      closed: false,
      show: () => did.push("show"),
      focusComposer: () => did.push("focus"),
      ...over,
    });
    return { verbs, did };
  }

  afterEach(() => {
    takeQueuedQuestion();
  });

  test("askAbout queues a new conversation's question and opens a closed panel", () => {
    const { verbs, did } = hold();
    verbs.askAbout("Help me with this message", { send: true });
    expect(takeQueuedQuestion()).toEqual({
      text: "Help me with this message",
      send: true,
      fresh: true,
    });
    expect(did).toEqual(["show"]);
  });

  test("a question left in an open panel's box takes focus; a sent one leaves it", () => {
    const left = hold({ open: true });
    left.verbs.askAbout("What does this finding rest on?", { send: false });
    expect(takeQueuedQuestion()).toEqual({
      text: "What does this finding rest on?",
      send: false,
      fresh: true,
    });
    expect(left.did).toEqual(["focus"]);
    const sent = hold({ open: true });
    sent.verbs.askAbout("Draft a message for me", { send: true });
    expect(sent.did).toEqual([]);
  });

  test("the palette's question goes to the open conversation", () => {
    const { verbs, did } = hold();
    verbs.ask("Who spent most?");
    expect(takeQueuedQuestion()).toEqual({ text: "Who spent most?", send: true, fresh: false });
    expect(did).toEqual(["show"]);
  });

  test("nothing is queued where the reader may not ask", () => {
    const { verbs, did } = hold({ closed: true });
    verbs.askAbout("Help me", { send: true });
    verbs.ask("Who spent most?");
    expect(takeQueuedQuestion()).toBeNull();
    expect(did).toEqual([]);
  });
});
