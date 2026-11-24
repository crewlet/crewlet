/**
 * A question asked in the drawer, end to end, over the engine's own stream.
 *
 * THE BYTES ARE THE ENGINE'S GOLDEN (`internal/assistant/wire/testdata/
 * frames.sse`), served through a stubbed `fetch`: the drawer creates the
 * conversation, sends the question under an operation key with the page
 * attached, reads the turn's stream, and draws every card the engine built,
 * the run chip ending where the turn's `done` says. A frame the engine starts
 * writing differently moves this case rather than a fixture typed to agree.
 */

import { readFileSync } from "node:fs";
import { AppAnnouncer } from "~/app/announcer.tsx";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { relTime } from "~/lib/format.ts";
import { repoRoot } from "~/test/engineFiles.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { FrameReadings } from "~/app/Shell.tsx";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { ASSISTANT_PROTOCOL_VERSION } from "~/contract/assistant.ts";
import { resetPageContextForTest, setFramePage } from "../context.ts";
import { assistantActivity } from "../status.ts";
import { on as onBus } from "../bus.ts";
import { queueAbout, queueQuestion, takeQueuedQuestion } from "../control.ts";
import { readTab, writeTab } from "../tab.ts";
import {
  attachClosedFor,
  type AssistantModelProblem,
  type AssistantUnavailable,
  type AttachClosed,
} from "../availability.ts";
import { Drawer } from "./Drawer.tsx";
import { conversation, resetSessionForTest } from "./session.ts";

/*
 * THE THREAD, COUNTED. A keystroke must render the composer alone: the case
 * that types a follow-up asserts the thread under it was not rendered at all.
 * The real component draws everything; the wrapper only counts.
 */
const threadRenders = vi.hoisted(() => ({ count: 0 }));
vi.mock("./Thread.tsx", async (importOriginal) => {
  const real = await importOriginal<typeof import("./Thread.tsx")>();
  return {
    ...real,
    Thread: (props: Parameters<typeof real.Thread>[0]) => {
      threadRenders.count += 1;
      return real.Thread(props);
    },
  };
});

const GOLDEN = readFileSync(
  join(repoRoot(), "internal/assistant/wire/testdata/frames.sse"),
  "utf8",
);
const TURN = "3db96f52-a891-5953-9735-0ae522cf6347";
const FIRST = GOLDEN.split("\n\n")
  .filter((block) => block.includes(`${TURN}.`))
  .map((block) => `${block}\n\n`)
  .join("");

beforeEach(() => {
  resetSessionForTest();
  resetPageContextForTest();
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

/**
 * The engine, as the drawer meets it: each request recorded and answered.
 * `refusal`, where given, is how it answers the question itself.
 */
function engine(refusal?: () => Response | Promise<Response>) {
  const sent: { url: string; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      sent.push({ url, init });
      const path = new URL(url).pathname;
      if (path === "/assistant/conversations" && init.method === "POST") {
        return json(
          {
            conversation: {
              id: "403212ed-5a4a-586d-8af6-c62411a17c2f",
              title: "",
              created_at: "2026-10-24T09:41:00Z",
              updated_at: "2026-10-24T09:41:00Z",
              last_activity_at: "2026-10-24T09:41:00Z",
            },
          },
          201,
        );
      }
      if (path.endsWith("/turns")) {
        if (refusal) return refusal();
        return json(
          {
            turn_id: TURN,
            stream_url: `/assistant/conversations/403212ed-5a4a-586d-8af6-c62411a17c2f/stream?turn=${TURN}`,
            after_seq: 0,
          },
          202,
        );
      }
      if (path.endsWith("/stream")) {
        return new Response(FIRST, {
          status: 200,
          headers: { "Content-Type": "text/event-stream" },
        });
      }
      return json({ error: "not_found" }, 404);
    }),
  );
  return sent;
}

function mount(
  unavailable: AssistantUnavailable | null = null,
  model: AssistantModelProblem | null = null,
  admin = false,
  attach: AttachClosed | null = null,
  viewer: unknown = {},
) {
  const store = new Store();
  store.applyOrg(withDerived({ name: "Nimbus", roles: [] }));
  const socket = new LiveSocket(store);
  // THE VIEWER, where a case says who reads the panel; every other question answers nothing.
  (socket as unknown as { query: (name: string) => Promise<unknown> }).query = (name) =>
    Promise.resolve(name === "viewer" ? viewer : {});
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <FrameReadings>
          <ToastProvider>
            <LayerHost>
              <AppAnnouncer />
              <Drawer
                unavailable={unavailable}
                model={model}
                attach={attach}
                admin={admin}
                onClose={() => {}}
                initialFocus="composer"
                name="Ada Lovelace"
                shape="column"
              />
            </LayerHost>
          </ToastProvider>
        </FrameReadings>
      </Router>
    </ClientContext.Provider>,
  );
}

test("a question is sent with its page, streamed, and drawn with every card the engine built", async () => {
  const sent = engine();
  setFramePage({
    screen: "worklet-tasks",
    title: "Worklet › Tasks",
    path: ["worklet", "tasks"],
    params: {},
    query: {},
  });
  mount();
  expect(
    screen.getByRole("heading", { name: /^(Hello|Good (morning|afternoon|evening)), Ada$/ }),
  ).toBeDefined();

  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  fireEvent.change(box, { target: { value: "Fix the flaky sign-in test" } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });

  await waitFor(() => expect(conversation().turns[0]?.phase).toBe("awaiting_approval"));
  const turn = sent.find((r) => new URL(r.url).pathname.endsWith("/turns"))!;
  // THE QUESTION WENT UNDER AN OPERATION KEY, with the page it was asked on.
  expect(new Headers(turn.init.headers).get("Idempotency-Key")).toMatch(/^[0-9a-f-]{36}$/);
  const body = JSON.parse(String(turn.init.body));
  expect(body).toMatchObject({
    protocol_version: ASSISTANT_PROTOCOL_VERSION,
    input: { text: "Fix the flaky sign-in test" },
  });
  expect(body.page_context).toMatchObject({ screen: "worklet-tasks", title: "Worklet › Tasks" });

  // The question, the page it carried, and the engine's cards.
  expect(screen.getByRole("article", { name: "You said" }).textContent).toContain(
    "Fix the flaky sign-in test",
  );
  expect(screen.getAllByText("Worklet › Tasks").length).toBeGreaterThan(0);
  expect(screen.getAllByText(/^Matching tasks/).length).toBeGreaterThan(0);
  expect(screen.getAllByText("No open task covers it yet.").length).toBeGreaterThan(0);
  // A CHANGE WAITS ON THE PERSON, and the Ask button would say so.
  expect(assistantActivity()).toBe("waiting");
});

test("a follow-up typed fast under a turn with cards keeps every keystroke and renders only the composer", async () => {
  engine();
  setFramePage({
    screen: "worklet-tasks",
    title: "Worklet › Tasks",
    path: ["worklet", "tasks"],
    params: {},
    query: {},
  });
  mount();
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" }) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "Fix the flaky sign-in test" } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  await waitFor(() => expect(conversation().turns[0]?.phase).toBe("awaiting_approval"));
  expect(screen.getAllByText(/^Matching tasks/).length).toBeGreaterThan(0);

  // KEYSTROKES AS A BROWSER DELIVERS THEM WHEN THEY QUEUE UP: one native
  // input event after another, each flushed by React on its own, outside
  // `act`. `act` drains every update before the next keystroke and would hide
  // the chain that threw a keystroke away (React error #185) once a turn's
  // cards were rendered by every keystroke.
  const environment = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean | undefined };
  const wasActEnvironment = environment.IS_REACT_ACT_ENVIRONMENT;
  const errors: unknown[] = [];
  const onError = (event: ErrorEvent) => errors.push(event.error ?? event.message);
  const logged = vi.spyOn(console, "error").mockImplementation((...args) => errors.push(args));
  const setValue = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
  const typed =
    "Why did FAIR-3 fail, and is the QA Engineer right to pick up FAIR-10 next? ".repeat(3);
  threadRenders.count = 0;
  window.addEventListener("error", onError);
  environment.IS_REACT_ACT_ENVIRONMENT = false;
  try {
    for (const key of typed) {
      setValue.call(box, box.value + key);
      box.dispatchEvent(new Event("input", { bubbles: true }));
    }
  } finally {
    environment.IS_REACT_ACT_ENVIRONMENT = wasActEnvironment;
    window.removeEventListener("error", onError);
    logged.mockRestore();
  }

  expect(errors).toEqual([]);
  expect(box.value).toBe(typed);
  expect(threadRenders.count).toBe(0);
  expect(screen.getByRole("button", { name: "Send" }).getAttribute("aria-disabled")).not.toBe(
    "true",
  );
});

// ASK CREWLET HAS ONE MODEL: no question asks for another, and the composer
// offers no Think harder.
test("a question runs on Ask Crewlet's one model, with no Think harder", async () => {
  const sent = engine();
  mount();
  expect(screen.queryByRole("button", { name: "Think harder" })).toBeNull();
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  fireEvent.change(box, { target: { value: "Which way should we route it?" } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  const turns = () => sent.filter((r) => new URL(r.url).pathname.endsWith("/turns"));
  await waitFor(() => expect(turns()).toHaveLength(1));
  expect(Object.keys(JSON.parse(String(turns()[0]!.init.body)))).not.toContain("deliberate");
});

// THE PAGE HOLDS THE SAME CONVERSATION: Open in full page goes to Ask
// Crewlet's page on the one open here, and to a new one where none is.
test("Open in full page opens Ask Crewlet's page on the drawer's conversation", async () => {
  engine();
  const opened: string[][] = [];
  const stop = onBus("assistant.navigation.open", ({ path }) => opened.push([...path]));
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Open in full page" }));
  expect(opened).toEqual([["ask"]]);

  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  fireEvent.change(box, { target: { value: "Which agents are stuck?" } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  await waitFor(() => expect(conversation().id).toBe("403212ed-5a4a-586d-8af6-c62411a17c2f"));
  fireEvent.click(screen.getByRole("button", { name: "Open in full page" }));
  expect(opened.at(-1)).toEqual(["ask", "403212ed-5a4a-586d-8af6-c62411a17c2f"]);
  stop();
});

test("a question past the length a turn takes is not sent", async () => {
  const sent = engine();
  mount();
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  fireEvent.change(box, { target: { value: "x".repeat(4001) } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  expect(sent).toEqual([]);
});

test("the page goes with a question until the reader removes it", async () => {
  const sent = engine();
  setFramePage({
    screen: "worklet-tasks",
    title: "Worklet › Tasks",
    path: ["worklet", "tasks"],
    params: {},
    query: {},
  });
  mount();
  fireEvent.click(
    screen.getByRole("button", { name: "Remove Worklet › Tasks from this question" }),
  );
  expect(
    screen.getByRole("button", { name: "Add Worklet › Tasks to this question" }),
  ).toBeDefined();
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  fireEvent.change(box, { target: { value: "Who spent most?" } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  await waitFor(() =>
    expect(sent.some((r) => new URL(r.url).pathname.endsWith("/turns"))).toBe(true),
  );
  const turn = sent.find((r) => new URL(r.url).pathname.endsWith("/turns"))!;
  expect(JSON.parse(String(turn.init.body)).page_context).toBeNull();
});

// A QUESTION TYPED IN THE PALETTE is asked as the panel would ask it, once
// the panel is drawn, and then it is the panel's: nothing waits behind it.
test("a question queued from elsewhere is asked once the panel draws", async () => {
  const sent = engine();
  queueQuestion("Who spent most this week?");
  mount();
  await waitFor(() =>
    expect(sent.some((r) => new URL(r.url).pathname.endsWith("/turns"))).toBe(true),
  );
  const turn = sent.find((r) => new URL(r.url).pathname.endsWith("/turns"))!;
  expect(JSON.parse(String(turn.init.body)).input).toEqual({ text: "Who spent most this week?" });
  expect(takeQueuedQuestion()).toBeNull();
});

/** A conversation this tab was on before a reload, which the drawer reopens as it mounts. */
const SAVED = "403212ed-5a4a-586d-8af6-0000000000aa";

/**
 * The engine for the cases that hold two conversations: each create answers
 * a conversation of its own, and every turn is recorded under the one its
 * address names.
 */
function engineOfMany() {
  const created: string[] = [];
  const turns: { conversation: string; body: Record<string, unknown> }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const path = new URL(url).pathname;
      if (path === "/assistant/conversations" && init.method === "POST") {
        const id = `403212ed-5a4a-586d-8af6-${String(created.length + 1).padStart(12, "0")}`;
        created.push(id);
        const at = "2026-10-24T09:41:00Z";
        return json(
          {
            conversation: { id, title: "", created_at: at, updated_at: at, last_activity_at: at },
          },
          201,
        );
      }
      const upload = /^\/assistant\/conversations\/([^/]+)\/files$/.exec(path);
      if (upload) {
        const file = `f-${upload[1]!.slice(-1)}`;
        const download = `/assistant/conversations/${upload[1]!}/files/${file}`;
        return json(
          {
            file: {
              file_id: file,
              name: "settings.png",
              media_type: "image/png",
              size: 4,
              download,
            },
          },
          201,
        );
      }
      if (path === `/assistant/conversations/${SAVED}` && init.method !== "POST") {
        const at = "2026-10-24T09:00:00Z";
        return json({
          conversation: {
            id: SAVED,
            title: "Earlier",
            created_at: at,
            updated_at: at,
            last_activity_at: at,
          },
          messages: [
            { id: "q0", turn_id: "t0", role: "user", text: "An earlier question", created_at: at },
          ],
          steps: [],
          cards: [],
          open_turn: null,
          audit_reads: [],
          turns: [{ turn_id: "t0", state: "completed" }],
        });
      }
      const turn = /^\/assistant\/conversations\/([^/]+)\/turns$/.exec(path);
      if (turn) {
        turns.push({ conversation: turn[1]!, body: JSON.parse(String(init.body)) });
        return json(
          {
            turn_id: TURN,
            stream_url: `/assistant/conversations/${turn[1]!}/stream?turn=${TURN}`,
            after_seq: 0,
          },
          202,
        );
      }
      if (path.endsWith("/stream")) {
        return new Response(FIRST, {
          status: 200,
          headers: { "Content-Type": "text/event-stream" },
        });
      }
      return json({ error: "not_found" }, 404);
    }),
  );
  return { created, turns };
}

const TASKS = {
  screen: "worklet-tasks",
  title: "Worklet › Tasks",
  path: ["worklet", "tasks"],
  params: {},
  query: {},
};

/** Ask `text` in the panel's own box, and wait for its turn to be sent. */
async function askInBox(text: string, turns: () => number) {
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  const before = turns();
  fireEvent.change(box, { target: { value: text } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  await waitFor(() => expect(turns()).toBe(before + 1));
}

// ASK CREWLET ABOUT THIS IS A NEW CONVERSATION, whatever the panel held: the
// question about a message goes to a conversation of its own, with the page
// it was asked on, and the one open before stays in history as it was. A
// question typed elsewhere (the palette) stays in the conversation open.
//
// Mutation: drop the `fresh` branch in the panel, and the question about the
// message is put in the first conversation's box instead.
test("a question about something on screen starts a new conversation; the palette's stays", async () => {
  const { created, turns } = engineOfMany();
  setFramePage(TASKS);
  mount();
  await askInBox("Fix the flaky sign-in test", () => turns.length);
  await waitFor(() => expect(conversation().turns[0]?.phase).toBe("awaiting_approval"));
  const first = conversation().id;
  expect(first).toBe(created[0]);

  // The palette's question waits in the open conversation's box while its turn waits.
  act(() => queueQuestion("Who spent most this week?"));
  const box = () => screen.getByRole("textbox", { name: "Ask Crewlet" }) as HTMLTextAreaElement;
  await waitFor(() => expect(box().value).toBe("Who spent most this week?"));
  expect(conversation().id).toBe(first);
  expect(created).toHaveLength(1);

  setFramePage({
    screen: "chatlet-room",
    title: "Chat › general",
    path: ["chat", "general"],
    params: {},
    query: {},
  });
  act(() => queueAbout("Help me with this message from Ada Lovelace: #/chat/general", true));
  await waitFor(() => expect(turns).toHaveLength(2));
  expect(created).toHaveLength(2);
  expect(turns[1]!.conversation).toBe(created[1]);
  expect(turns[1]!.body).toMatchObject({
    input: { text: "Help me with this message from Ada Lovelace: #/chat/general" },
    page_context: { screen: "chatlet-room", title: "Chat › general" },
  });
  await waitFor(() => expect(conversation().id).toBe(created[1]));
  expect(conversation().turns.map((t) => t.question)).toEqual([
    "Help me with this message from Ada Lovelace: #/chat/general",
  ]);
});

// A QUESTION A SCREEN OFFERS waits in a NEW conversation's box, with the page
// attached again even where the reader had taken it off the last question,
// and nothing is sent for them.
test("a question a screen offers waits in a new conversation's box, with its page", async () => {
  const { created, turns } = engineOfMany();
  setFramePage(TASKS);
  mount();
  await askInBox("Fix the flaky sign-in test", () => turns.length);
  await waitFor(() => expect(conversation().id).toBe(created[0]));
  fireEvent.click(
    screen.getByRole("button", { name: "Remove Worklet › Tasks from this question" }),
  );

  act(() => queueAbout("What does this finding rest on?", false));
  const box = () => screen.getByRole("textbox", { name: "Ask Crewlet" }) as HTMLTextAreaElement;
  await waitFor(() => expect(box().value).toBe("What does this finding rest on?"));
  expect(conversation().id).toBeNull();
  expect(conversation().turns).toEqual([]);
  expect(readTab().conversation).toBeNull();
  expect(
    screen.getByRole("button", { name: "Remove Worklet › Tasks from this question" }),
  ).toBeDefined();
  expect(turns).toHaveLength(1);
  expect(created).toHaveLength(1);
});

// A QUESTION ABOUT A SCREEN TAKES NONE OF THE BOX'S IMAGES: they are files of
// the conversation just left, which the engine refuses in another.
// Mutation: let it take the composer's images, and its turn names the last
// conversation's file.
test("a question about something on screen carries none of the images waiting in the box", async () => {
  const { created, turns } = engineOfMany();
  vi.stubGlobal(
    "URL",
    Object.assign(URL, { createObjectURL: () => "blob:local/1", revokeObjectURL: () => {} }),
  );
  mount();
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  const shot = new File([new Uint8Array([137, 80, 78, 71])], "settings.png", { type: "image/png" });
  await act(async () => {
    fireEvent.paste(box, { clipboardData: { files: [shot], getData: () => "" } });
  });
  await waitFor(() =>
    expect(document.querySelector(".crewlet-attachment-chip--ready")).not.toBeNull(),
  );
  expect(created).toHaveLength(1);

  act(() => queueAbout("Help me with this message from Ada Lovelace: #/chat/general", true));
  await waitFor(() => expect(turns).toHaveLength(1));
  expect(turns[0]!.conversation).toBe(created[1]);
  expect(turns[0]!.body.attachments).toBeUndefined();
});

// A QUESTION ABOUT A SCREEN ASKED AS THE PANEL FIRST OPENS wins over the
// conversation the tab was on, which the panel reopens as it mounts: that
// read, answered late, does not take the new conversation's place.
test("a question about something on screen asked as the panel opens beats the tab's saved one", async () => {
  engineOfMany();
  writeTab({ conversation: SAVED });
  act(() => queueAbout("What does this finding rest on?", false));
  mount();
  const box = () => screen.getByRole("textbox", { name: "Ask Crewlet" }) as HTMLTextAreaElement;
  await waitFor(() => expect(box().value).toBe("What does this finding rest on?"));
  // Every answer the mount's reads were given has been taken.
  await afterMicrotasks();
  expect(conversation().id).toBeNull();
  expect(conversation().turns).toEqual([]);
  expect(readTab().conversation).toBeNull();
  expect(screen.queryByText("An earlier question")).toBeNull();
});

// THE PANEL OPENED AGAIN is the conversation it held: closing it is not
// starting over, and nothing about a screen was asked.
test("the panel opened again resumes the conversation it held", async () => {
  const { created, turns } = engineOfMany();
  const first = mount();
  await askInBox("Fix the flaky sign-in test", () => turns.length);
  await waitFor(() => expect(conversation().id).toBe(created[0]));
  first.unmount();
  mount();
  await act(async () => {});
  expect(conversation().id).toBe(created[0]);
  expect(screen.getByRole("article", { name: "You said" }).textContent).toContain(
    "Fix the flaky sign-in test",
  );
});

// THE BODY IS FLUSH ONLY FOR THE THREAD, which scrolls itself. The offline
// notice does not, so it keeps the panel's own scrolling body and stays
// readable in a short window.
test("the panel's body is flush for the thread and scrolls for the offline notice", () => {
  engine();
  const { container, unmount } = mount();
  expect(container.ownerDocument.querySelector(".crewlet-side-panel__body--flush")).not.toBeNull();
  unmount();

  mount({
    reason: "no_model",
    short: "No model is assigned to Ask Crewlet yet.",
    title: "Ask Crewlet has no model for Nimbus yet",
    body: "An admin has not chosen the model Ask Crewlet runs on.",
  });
  expect(screen.getByText("Ask Crewlet has no model for Nimbus yet")).toBeDefined();
  expect(document.querySelector(".crewlet-side-panel__body--flush")).toBeNull();
});

const SIGNED_OUT: AssistantModelProblem = {
  code: "model_signed_out",
  short: "Ask Crewlet's model is signed out",
  title: "Ask Crewlet's model is signed out, so it cannot answer.",
  body: "Ask an administrator to sign Ask Crewlet's model in again.",
};

// A MODEL THE ENGINE KNOWS CANNOT ANSWER is said before the reader asks: the
// panel stays open with the conversation readable, the notice stands above the
// composer in the engine's words, and Send is off with the reason, so nothing
// is sent and nothing imitates a working assistant.
test("a model that cannot answer is said above the composer, and Send stays off", async () => {
  const sent = engine();
  mount(null, SIGNED_OUT);
  expect(screen.getByText(SIGNED_OUT.title)).toBeDefined();
  expect(screen.getByText(SIGNED_OUT.body)).toBeDefined();
  // Not the offline state: the home is drawn and the composer is there.
  expect(
    screen.getByRole("heading", { name: /^(Hello|Good (morning|afternoon|evening)), Ada$/ }),
  ).toBeDefined();
  expect(screen.queryByRole("button", { name: "Model settings" })).toBeNull();

  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  fireEvent.change(box, { target: { value: "Who spent most?" } });
  const send = screen.getByRole("button", { name: /send/i });
  expect(send.getAttribute("aria-disabled")).toBe("true");
  expect(document.body.textContent).toContain(SIGNED_OUT.short);
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
    fireEvent.click(send);
  });
  expect(sent.some((r) => new URL(r.url).pathname.endsWith("/turns"))).toBe(false);
});

// AN ADMINISTRATOR'S NOTICE OPENS THE MODELS' SETTINGS, where each model's
// sign-in is shown; anybody else is told whom to ask instead.
test("an administrator's model notice opens the models' settings", () => {
  engine();
  mount(null, SIGNED_OUT, true);
  expect(screen.getByRole("button", { name: "Model settings" })).toBeDefined();
});

// A READY MODEL DRAWS NO NOTICE and Send works as before.
test("a ready model draws no notice", () => {
  engine();
  mount();
  expect(screen.queryByText(SIGNED_OUT.title)).toBeNull();
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  fireEvent.change(box, { target: { value: "Who spent most?" } });
  expect(screen.getByRole("button", { name: /send/i }).getAttribute("aria-disabled")).not.toBe(
    "true",
  );
});

// AN IMAGE IS TAKEN ONLY WHERE ASK CREWLET'S MODEL READS IT: where it does
// not, Attach image is drawn disabled with that reason, and a pasted
// screenshot is refused in words that say what works, never uploaded and kept
// unread.
test("a model that reads no image takes none, and says what works instead", () => {
  const sent = engine();
  const closed = attachClosedFor({ assistantReadsImages: false })!;
  mount(null, null, false, closed);
  const attachButton = screen.getByRole("button", { name: /Attach image/ });
  expect(attachButton.getAttribute("aria-disabled")).toBe("true");
  expect(document.body.textContent).toContain(closed.short);

  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  const shot = new File([new Uint8Array([137, 80, 78, 71])], "settings.png", { type: "image/png" });
  fireEvent.paste(box, { clipboardData: { files: [shot], getData: () => "" } });
  expect(screen.getByText(closed.body)).toBeDefined();
  expect(sent.some((r) => new URL(r.url).pathname.endsWith("/files"))).toBe(false);
  expect(attachClosedFor({ assistantReadsImages: true })).toBeNull();
});

// A DROPPED IMAGE IS REFUSED IN PLACE where none is taken: the drag and the
// drop are both taken, so the browser never opens the file in the tab in place
// of the dashboard and its draft, and the refusal says why as Attach does.
// Mutation: disable the drop target again and the drop is not cancelled.
test("an image dropped where none is taken is refused in place, never opened in the tab", async () => {
  const sent = engine();
  const closed = attachClosedFor({ assistantReadsImages: false })!;
  mount(null, null, false, closed);
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  const shot = new File([new Uint8Array([137, 80, 78, 71])], "settings.png", { type: "image/png" });
  const carried = { types: ["Files"], files: [shot], items: [] };
  expect(fireEvent.dragOver(box, { dataTransfer: carried })).toBe(false);
  expect(fireEvent.drop(box, { dataTransfer: carried })).toBe(false);
  await waitFor(() => expect(screen.getByText(closed.body)).toBeDefined());
  expect(sent.some((r) => new URL(r.url).pathname.endsWith("/files"))).toBe(false);
});

// AND WHERE NOTHING MAY BE ASKED NOW, a drop says why in the composer's own words.
test("an image dropped while the composer is closed is refused with the composer's reason", async () => {
  const sent = engine();
  mount(null, SIGNED_OUT);
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  const shot = new File([new Uint8Array([137, 80, 78, 71])], "settings.png", { type: "image/png" });
  const carried = { types: ["Files"], files: [shot], items: [] };
  expect(fireEvent.drop(box, { dataTransfer: carried })).toBe(false);
  await waitFor(() =>
    expect(screen.getByText(`${SIGNED_OUT.short}, so nothing was attached.`)).toBeDefined(),
  );
  expect(sent.some((r) => new URL(r.url).pathname.endsWith("/files"))).toBe(false);
});

test("the history offers Rename and Delete only for a conversation with something in it", async () => {
  const sent = engine();
  const answered = vi.mocked(fetch).getMockImplementation()!;
  vi.mocked(fetch).mockImplementation(async (url, init) => {
    if (new URL(String(url)).pathname === "/assistant/conversations" && init?.method !== "POST") {
      return json({ conversations: [] });
    }
    return answered(url, init);
  });
  mount();
  const history = screen.getByRole("button", { name: "Conversations" });

  // AN EMPTY CONVERSATION HAS NOTHING TO DELETE.
  await act(async () => {
    fireEvent.click(history);
  });
  await waitFor(() => expect(screen.getByText("Your recent conversations")).toBeDefined());
  expect(screen.queryByText("Delete this conversation")).toBeNull();
  expect(screen.queryByText("Rename this conversation")).toBeNull();
  await act(async () => {
    fireEvent.click(history);
  });
  await waitFor(() => expect(screen.queryByText("Your recent conversations")).toBeNull());

  // ONE WITH A QUESTION IN IT DOES.
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  fireEvent.change(box, { target: { value: "Fix the flaky sign-in test" } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  await waitFor(() => expect(conversation().turns.length).toBe(1));
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Conversations" }));
  });
  await waitFor(() => expect(screen.getByText("Delete this conversation")).toBeDefined());
  expect(sent.length).toBeGreaterThan(0);
  // RENAME OPENS ITS DIALOG on the open conversation.
  await act(async () => {
    fireEvent.click(screen.getByRole("menuitem", { name: /Rename this conversation/ }));
  });
  await screen.findByRole("dialog", { name: "Rename conversation" });
});

test("an empty conversation opened from the history offers no Delete", async () => {
  const EMPTY_ID = "5b0c7a52-41a3-4f0e-9d0e-2b8f1c3e9a10";
  const summary = {
    id: EMPTY_ID,
    title: "",
    created_at: "2026-10-24T09:41:00Z",
    updated_at: "2026-10-24T09:41:00Z",
    last_activity_at: "2026-10-24T09:41:00Z",
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      const path = new URL(url).pathname;
      if (path === "/assistant/conversations") return json({ conversations: [summary] });
      if (path === `/assistant/conversations/${EMPTY_ID}`) {
        return json({
          conversation: summary,
          messages: [],
          steps: [],
          cards: [],
          open_turn: null,
          audit_reads: [],
          turns: [],
        });
      }
      return json({ error: "not_found" }, 404);
    }),
  );
  mount();
  const history = screen.getByRole("button", { name: "Conversations" });
  await act(async () => {
    fireEvent.click(history);
  });
  const entry = await screen.findByText("Untitled conversation");
  await act(async () => {
    fireEvent.click(entry);
  });
  await waitFor(() => expect(conversation().id).toBe(EMPTY_ID));
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Conversations" }));
  });
  await screen.findByText("Your recent conversations");
  expect(screen.queryByText("Delete this conversation")).toBeNull();
});

// THE OPEN CONVERSATION CARRIES THE HISTORY'S CHECK, every other listed one
// says it is not the current one, and Delete stays an action after the
// separator rather than an answer of the choice. Each is shown at the moment
// the list is ordered by (last_activity_at), never the record's updated_at,
// which a new title moves too. Mutation: drop `checked` from the listed
// entries, or show `updated_at`, and this fails.
test("the history marks the open conversation and shows when each was last used", async () => {
  const OPEN_ID = "6c1d8b63-52b5-4a1f-8e1f-3c9a2d4f0b21";
  const OTHER_ID = "7d2e9c74-63c6-4b2a-9f2a-4dab3e5a1c32";
  const now = Date.now();
  const at = (minutesAgo: number) => new Date(now - minutesAgo * 60_000).toISOString();
  const open = {
    id: OPEN_ID,
    title: "Payroll",
    created_at: at(90),
    // RETITLED JUST NOW, used two and a half hours ago: the menu shows the
    // use. Half an hour from any boundary, so the shell's clock (which reads
    // up to a tick behind this case's `now`) says the same "2h ago".
    updated_at: at(0),
    last_activity_at: at(150),
  };
  const other = {
    id: OTHER_ID,
    title: "Hiring plan",
    created_at: at(30),
    updated_at: at(5),
    last_activity_at: at(5),
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      const path = new URL(url).pathname;
      if (path === "/assistant/conversations") return json({ conversations: [other, open] });
      if (path === `/assistant/conversations/${OPEN_ID}`) {
        return json({
          conversation: open,
          messages: [
            {
              id: "q1",
              turn_id: "t1",
              role: "user",
              text: "What did payroll cost?",
              created_at: open.last_activity_at,
            },
          ],
          steps: [],
          cards: [],
          open_turn: null,
          audit_reads: [],
          turns: [],
        });
      }
      return json({ error: "not_found" }, 404);
    }),
  );
  mount();
  const history = screen.getByRole("button", { name: "Conversations" });
  await act(async () => {
    fireEvent.click(history);
  });
  // BEFORE ONE IS OPEN, none is checked.
  const before = await screen.findAllByRole("menuitemradio");
  expect(before.map((item) => item.getAttribute("aria-checked"))).toEqual(["false", "false"]);
  await act(async () => {
    fireEvent.click(screen.getByText("Payroll"));
  });
  await waitFor(() => expect(conversation().id).toBe(OPEN_ID));
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Conversations" }));
  });
  await screen.findByText("Your recent conversations");
  const answers = screen.getAllByRole("menuitemradio");
  expect(answers.map((item) => [item.textContent, item.getAttribute("aria-checked")])).toEqual([
    [expect.stringContaining("Hiring plan"), "false"],
    [expect.stringContaining("Payroll"), "true"],
  ]);
  expect(answers[1]!.textContent).toContain(relTime(open.last_activity_at, now));
  expect(answers[1]!.textContent).not.toContain(relTime(open.updated_at, now));
  const remove = screen.getByRole("menuitem", { name: /Delete this conversation/ });
  expect(remove.closest("[role='group']")).toBeNull();
});

// AN ANSWER THAT ENDS BETWEEN TWO TICKS HAS JUST HAPPENED. The drawer kept a
// clock of its own that slowed to once a minute when the turn ended, without
// reading the time again, so the turn's foot said "in 1s" beside Copy for up
// to a minute. It reads the shell's one clock now, which ticks every second.
// Mutation: give the drawer back a clock that slows when no turn runs, and the
// foot still reads the instant before the answer ended.
test("an answer that ended between the clock's ticks never reads as in the future", async () => {
  const interrupted = "75099765-5fa3-5aa4-b185-7d8dd69869bd";
  const frames = GOLDEN.split("\n\n")
    .filter((block) => block.includes(`${interrupted}.`))
    .map((block) => `${block}\n\n`)
    .join("");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const path = new URL(url).pathname;
      if (path === "/assistant/conversations" && init.method === "POST") {
        return json(
          {
            conversation: {
              id: "403212ed-5a4a-586d-8af6-c62411a17c2f",
              title: "",
              created_at: "2026-10-24T09:40:58Z",
              updated_at: "2026-10-24T09:40:58Z",
              last_activity_at: "2026-10-24T09:40:58Z",
            },
          },
          201,
        );
      }
      if (path.endsWith("/turns")) {
        return json(
          {
            turn_id: interrupted,
            stream_url: `/assistant/conversations/403212ed-5a4a-586d-8af6-c62411a17c2f/stream?turn=${interrupted}`,
            after_seq: 0,
          },
          202,
        );
      }
      if (path.endsWith("/stream")) {
        return new Response(frames, {
          status: 200,
          headers: { "Content-Type": "text/event-stream" },
        });
      }
      return json({ error: "not_found" }, 404);
    }),
  );
  // The answer ends at 09:41:00.4, a second and a half after the last tick.
  vi.useFakeTimers({ toFake: ["Date", "setInterval", "clearInterval"] });
  try {
    vi.setSystemTime(Date.parse("2026-10-24T09:40:58.9Z"));
    mount();
    const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
    fireEvent.change(box, { target: { value: "Is the deploy done?" } });
    await act(async () => {
      fireEvent.keyDown(box, { key: "Enter" });
    });
    await waitFor(() => expect(conversation().turns[0]?.phase).toBe("interrupted"));
    act(() => {
      vi.advanceTimersByTime(2_000);
    });
    const foot = document.querySelector("time.crewlet-relative-time");
    expect(foot?.getAttribute("datetime")).toBe("2026-10-24T09:41:00.400Z");
    expect(foot?.textContent).toBe("just now");
  } finally {
    vi.useRealTimers();
  }
});

test("the home offers a brief of the page, and no list of its own of what waits", () => {
  engine();
  setFramePage({
    screen: "worklet-tasks",
    title: "Worklet › Tasks",
    path: ["worklet", "tasks"],
    params: {},
    query: {},
  });
  mount();
  expect(screen.getByRole("region", { name: "This page" })).toBeDefined();
  expect(screen.getByRole("button", { name: /^Brief me on this page/ })).toBeDefined();
  expect(screen.queryByRole("region", { name: "Needs you" })).toBeNull();
  expect(screen.getByRole("region", { name: "Anywhere in Crewlet" })).toBeDefined();
});

test("with a record open the row names it, and asks a brief of that record alone", async () => {
  const sent = engine();
  setFramePage({
    screen: "item",
    title: "Tasks › ENG-7",
    path: ["worklet", "ENG-7"],
    params: { id: "ENG-7" },
    query: {},
  });
  mount();
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" }) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "half a question" } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /^Brief me on this task/ }));
  });
  await waitFor(() =>
    expect(sent.some((r) => new URL(r.url).pathname.endsWith("/turns"))).toBe(true),
  );
  const turn = sent.find((r) => new URL(r.url).pathname.endsWith("/turns"))!;
  const body = JSON.parse(String(turn.init.body));
  expect(body).toMatchObject({
    mode: "brief",
    input: { text: "Brief me on this task" },
    page_context: { screen: "item", selection: { kind: "task", id: "ENG-7", noun: "task" } },
  });
  // WHAT THE READER WAS TYPING STAYS: a brief is not their question.
  expect(box.value).toBe("half a question");
});

/** The golden's brief turn, and the engine that answers a brief with it. */
const BRIEF_TURN = "b7d720ed-6183-5418-829b-8067fd903172";
const BRIEF_CONVERSATION = "a51f5e8d-b74a-53fd-8dbc-db770396a8ad";
const BRIEF = GOLDEN.split("\n\n")
  .filter((block) => block.includes(`${BRIEF_TURN}.`))
  .map((block) => `${block}\n\n`)
  .join("");

function briefEngine() {
  const sent: { url: string; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      sent.push({ url, init });
      const path = new URL(url).pathname;
      if (path === "/assistant/conversations" && init.method === "POST") {
        return json(
          {
            conversation: {
              id: BRIEF_CONVERSATION,
              title: "",
              created_at: "2026-10-24T09:41:00Z",
              updated_at: "2026-10-24T09:41:00Z",
              last_activity_at: "2026-10-24T09:41:00Z",
            },
          },
          201,
        );
      }
      if (path.endsWith("/turns")) {
        return json(
          {
            turn_id: BRIEF_TURN,
            stream_url: `/assistant/conversations/${BRIEF_CONVERSATION}/stream?turn=${BRIEF_TURN}`,
            after_seq: 0,
          },
          202,
        );
      }
      if (path.endsWith("/stream")) {
        return new Response(BRIEF, {
          status: 200,
          headers: { "Content-Type": "text/event-stream" },
        });
      }
      return json({ error: "not_found" }, 404);
    }),
  );
  return sent;
}

test("a brief is the conversation's first answer: its card, its press and its follow-ups", async () => {
  const sent = briefEngine();
  setFramePage({ screen: "inbox", title: "Inbox", path: ["inbox"], params: {}, query: {} });
  mount();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /^Brief me on this page/ }));
  });
  await waitFor(() => expect(conversation().turns[0]?.phase).toBe("completed"));
  const turn = sent.find((r) => new URL(r.url).pathname.endsWith("/turns"))!;
  expect(JSON.parse(String(turn.init.body))).toMatchObject({ mode: "brief" });
  const brief = screen.getByRole("region", { name: "Brief 1 thing needs you" });
  expect(brief.textContent).toContain("Answer ENG-133: test the store build");
  expect(brief.textContent).toContain("Decision ENG-133");
  expect(screen.getByRole("button", { name: "Open ENG-133" })).toBeDefined();
  expect(screen.getByRole("button", { name: "What did QA test last?" })).toBeDefined();
});

// THE NOTICE NEVER PROMISES WHAT THE DRAWER DOES NOT DO. A question the engine
// turns away unsent goes back in the box, and only then does the notice say so;
// only the person's own question limit holds the box shut, since a spent budget
// clears the moment somebody raises the ceiling. The incident: a spent budget
// said "your question is back in the box below" over an empty box, because the
// sentence followed the wait and the restore followed one code.
//
// Mutation: put the question back for `assistant_busy` alone, or hold the box
// for every refusal that names a wait, and a case fails.
test.each([
  {
    name: "a spent budget",
    answer: {
      error: "budget_exhausted",
      message: "The token budget for this window is spent.",
      scope: "person",
      window: "2026-10-24",
      remedy: { setting: "assistant_token_budget" },
    },
    wait: "35160",
    title: "Your own Ask Crewlet budget for this window is spent.",
    line: "It resets in 9h 46m, or as soon as the ceiling is raised.",
    held: false,
  },
  {
    name: "the person's question limit",
    answer: { error: "assistant_busy", message: "Too many questions at once." },
    wait: "30",
    title: "Question limit reached.",
    line: "You can ask again in 30s.",
    held: true,
  },
])("$name puts the question back, says so, and holds the box only for a wait", async (c) => {
  engine(
    () =>
      new Response(JSON.stringify(c.answer), {
        status: 429,
        headers: { "Content-Type": "application/json", "Retry-After": c.wait },
      }),
  );
  mount();
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" }) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "Who spent most this week?" } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  await waitFor(() => expect(conversation().turns[0]?.phase).toBe("refused"));

  expect(box.value).toBe("Who spent most this week?");
  expect(screen.getByText(c.title)).toBeDefined();
  expect(document.body.textContent).toContain(`${c.line} Your question is back in the box below.`);
  const send = screen.getByRole("button", { name: /send/i });
  expect(send.getAttribute("aria-disabled") === "true").toBe(c.held);
  expect(box.disabled).toBe(c.held);
});

// THE BOX IS NEVER FILLED OVER WHAT THE READER TYPED SINCE, and then the notice
// does not claim the question is in it.
test("a refused question is not put back over a new one, and the notice does not say it is", async () => {
  let answer: (r: Response) => void = () => {};
  engine(() => new Promise<Response>((r) => (answer = r)));
  mount();
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" }) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "Who spent most this week?" } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  // TYPED WHILE THE QUESTION WAS ON ITS WAY.
  fireEvent.change(box, { target: { value: "And last week?" } });
  await act(async () => {
    answer(json({ error: "budget_exhausted", remedy: { setting: "company_token_budget" } }, 429));
  });
  await waitFor(() => expect(conversation().turns[0]?.phase).toBe("refused"));
  expect(box.value).toBe("And last week?");
  expect(screen.getByText("The company's token budget for this window is spent.")).toBeDefined();
  expect(document.body.textContent).not.toContain("back in the box");
});

// A QUESTION IS PUT BACK ONCE PER TURN, not once per drawer: closing the
// drawer unmounts it, and a question the reader deleted must not come back
// when they open it again. The notice still says where it went.
//
// Mutation: keep the decision in the drawer's own state, and the reopened box
// holds the deleted question again.
test("a restored question the reader deleted stays deleted when the drawer opens again", async () => {
  engine(
    () =>
      new Response(
        JSON.stringify({ error: "budget_exhausted", remedy: { setting: "company_token_budget" } }),
        { status: 429, headers: { "Content-Type": "application/json", "Retry-After": "60" } },
      ),
  );
  const first = mount();
  const box = () => screen.getByRole("textbox", { name: "Ask Crewlet" }) as HTMLTextAreaElement;
  fireEvent.change(box(), { target: { value: "Who spent most this week?" } });
  await act(async () => {
    fireEvent.keyDown(box(), { key: "Enter" });
  });
  await waitFor(() => expect(conversation().turns[0]?.phase).toBe("refused"));
  expect(box().value).toBe("Who spent most this week?");
  fireEvent.change(box(), { target: { value: "" } });

  first.unmount();
  mount();
  await act(async () => {});
  expect(box().value).toBe("");
  expect(document.body.textContent).toContain("Your question is back in the box below.");
});

// ASK CREWLET'S MODEL IS CHOSEN IN SETTINGS › MODELS ALONE: the panel offers no
// picker and names no model; with none, a reader who may choose it is sent
// there, and anybody else reads why with no way there.
const NO_MODEL_HERE: AssistantUnavailable = {
  reason: "no_model",
  short: "No model is assigned to Ask Crewlet yet.",
  title: "Ask Crewlet has no model for Nimbus yet",
  body: "An admin has not chosen the model Ask Crewlet runs on.",
};

test("with no model, the panel sends a reader who may choose one to Settings › Models & Fleets", async () => {
  engine();
  const opened: unknown[] = [];
  const off = onBus("assistant.navigation.open", (target) => opened.push(target));
  mount(NO_MODEL_HERE, null, true, null, {
    login: "ada",
    grants: ["admin"],
    assistant: {
      available: false,
      reason: "no_model",
      model: null,
      notice: null,
      can_assign: true,
      reads_images: true,
    },
  });
  await screen.findByText(/Ask Crewlet needs a model to answer/);
  expect(screen.queryByRole("combobox")).toBeNull();
  expect(screen.queryByRole("button", { name: "Choose its model" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Model settings" }));
  expect(opened).toEqual([{ path: ["settings", "models"], query: {}, mode: "here" }]);
  off();
});

test("with no model, anybody else reads why in the panel, with no way to choose one", async () => {
  engine();
  mount(NO_MODEL_HERE);
  expect(await screen.findByText("Ask Crewlet has no model for Nimbus yet")).toBeDefined();
  expect(screen.getByText("An admin has not chosen the model Ask Crewlet runs on.")).toBeDefined();
  expect(screen.queryByRole("button", { name: "Model settings" })).toBeNull();
  expect(screen.queryByRole("combobox")).toBeNull();
});
