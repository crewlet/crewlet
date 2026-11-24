/**
 * Ask Crewlet's page, over the engine's own routes, stubbed at `fetch`: the
 * reader's history beside the conversation the address names, a question
 * asked with no page attached, and Delete.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { on as onBus } from "../bus.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { AppAnnouncer } from "~/app/announcer.tsx";
import { ENGINE_HEADER, ENGINE_HEADER_VALUE } from "~/contract/engine.ts";
import { Router, useRoute } from "~/app/router.tsx";
import { resolve } from "~/app/routes.ts";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { resetPageContextForTest, setFramePage } from "../context.ts";
import { useAskReading, type AskReading } from "../place.ts";
import { AskPage } from "./AskPage.tsx";
import { conversation, resetSessionForTest } from "./session.ts";

vi.mock("../place.ts", () => ({ useAskReading: vi.fn() }));

const READY: AskReading = {
  unavailable: null,
  model: null,
  attach: null,
  admin: false,
  name: "Ada Lovelace",
};

const PAYROLL = "6c1d8b63-52b5-4a1f-8e1f-3c9a2d4f0b21";
const HIRING = "7d2e9c74-63c6-4b2a-9f2a-4dab3e5a1c32";
const BUDGET = "8e3fad85-74d7-4c3b-8a3b-5ebc4f6b2d43";
const ARCHIVE = "9f4abe96-85e8-4d4c-9b4c-6fcd5a7c3e54";
const MADE = "403212ed-5a4a-586d-8af6-c62411a17c2f";
const DAY = 86_400_000;

const at = (ago: number) => new Date(Date.now() - ago).toISOString();
const summary = (id: string, title: string | null, ago: number, state?: string) => ({
  id,
  title,
  title_set_by: title === null ? null : "model",
  created_at: at(ago),
  updated_at: at(ago),
  last_activity_at: at(ago),
  ...(state ? { last_turn_state: state } : {}),
});
const transcript = (row: ReturnType<typeof summary>, question: string) => ({
  conversation: row,
  messages: [
    { id: "q1", turn_id: "t1", role: "user", text: question, created_at: row.last_activity_at },
  ],
  steps: [],
  cards: [],
  open_turn: null,
  audit_reads: [],
  turns: [{ turn_id: "t1", state: "completed" }],
});

// THE ENGINE'S MARK on every answer, as the engine sets it: a refusal without
// it is one nobody here can say the engine wrote.
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", [ENGINE_HEADER]: ENGINE_HEADER_VALUE },
  });

/** The engine as the page meets it: every request recorded, each route answered from `rows`. */
function engine(
  rows: ReturnType<typeof summary>[],
  pages?: Record<string, { rows: typeof rows; next?: string }>,
  transcripts: {
    /** Answers a transcript read with this status instead. */
    status?: number;
    /** Holds every transcript read until it settles. */
    hold?: Promise<void>;
  } = {},
) {
  const sent: { url: URL; init: RequestInit }[] = [];
  let held = [...rows];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init: RequestInit = {}) => {
      const url = new URL(input);
      sent.push({ url, init });
      const path = url.pathname;
      const method = init.method ?? "GET";
      if (path === "/assistant/conversations" && method === "GET") {
        const cursor = url.searchParams.get("cursor") ?? "";
        if (pages && pages[cursor]) {
          const page = pages[cursor]!;
          return json({
            conversations: page.rows,
            ...(page.next ? { next_cursor: page.next } : {}),
          });
        }
        const q = (url.searchParams.get("q") ?? "").toLowerCase();
        return json({
          conversations: held.filter((r) => (r.title ?? "").toLowerCase().includes(q)),
        });
      }
      if (path === "/assistant/conversations" && method === "POST") {
        const made = summary(MADE, null, 0);
        held = [made, ...held];
        return json({ conversation: made }, 201);
      }
      const one = /^\/assistant\/conversations\/([^/]+)$/.exec(path);
      if (one && method === "GET") {
        await transcripts.hold;
        if (transcripts.status) {
          return json(
            { error: "internal", message: "Something went wrong on the engine." },
            transcripts.status,
          );
        }
        const row = held.find((r) => r.id === one[1]);
        return row
          ? json(transcript(row, `About ${row.title}`))
          : json({ error: "not_found" }, 404);
      }
      if (one && method === "PATCH") {
        const title = (JSON.parse(String(init.body)) as { title: string }).title;
        if (title.includes("refused")) {
          return json({ error: "invalid", message: "That title is not taken here." }, 422);
        }
        held = held.map((r) => (r.id === one[1] ? { ...r, title, title_set_by: "person" } : r));
        return json(held.find((r) => r.id === one[1]));
      }
      if (one && method === "DELETE") {
        held = held.filter((r) => r.id !== one[1]);
        return json({ outcome: "applied", position: null });
      }
      if (path.endsWith("/turns")) {
        return json(
          {
            turn_id: "0f9c",
            stream_url: `/assistant/conversations/${MADE}/stream?turn=0f9c`,
            after_seq: 0,
          },
          202,
        );
      }
      if (path.endsWith("/stream")) return new Response(null, { status: 204 });
      return json({ error: "not_found" }, 404);
    }),
  );
  return sent;
}

function mount(address: string, reading: AskReading = READY, viewer: unknown = {}) {
  vi.mocked(useAskReading).mockReturnValue(reading);
  window.location.hash = address;
  const store = new Store();
  store.applyOrg(withDerived({ name: "Nimbus", roles: [] }));
  const socket = new LiveSocket(store);
  // THE VIEWER, where a case says who reads the page; every other question answers nothing.
  (socket as unknown as { query: (name: string) => Promise<unknown> }).query = (name) =>
    Promise.resolve(name === "viewer" ? viewer : {});
  // THE PAGE AS THE APP DRAWS IT: on the conversation the address names.
  function Routed() {
    const where = resolve(useRoute().path);
    return (
      <AskPage
        conversation={where.resolved && where.screen === "ask" ? where.conversation : undefined}
      />
    );
  }
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <FrameReadings>
          <ToastProvider>
            <LayerHost>
              <AppAnnouncer />
              <Routed />
            </LayerHost>
          </ToastProvider>
        </FrameReadings>
      </Router>
    </ClientContext.Provider>,
  );
}

beforeEach(() => {
  resetSessionForTest();
  resetPageContextForTest();
  sessionStorage.clear();
  // A FIXED MIDDAY: the list groups conversations by the reader's calendar
  // day, so one last used ten minutes ago fell under Yesterday near midnight
  // when the fixtures read the real clock.
  vi.useFakeTimers({ now: Date.parse("2026-06-10T12:00:00Z"), toFake: ["Date"] });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  window.location.hash = "";
});

test("your conversations are listed under the day each was last used, newest first", async () => {
  engine([
    summary(PAYROLL, "Payroll", 10 * 60_000),
    summary(HIRING, "Hiring plan", DAY + 60_000),
    summary(BUDGET, "Budget review", 3 * DAY),
    summary(ARCHIVE, "Old incident", 20 * DAY),
  ]);
  mount("#/ask");
  const list = await screen
    .findByRole("list", { name: "Your conversations" })
    .catch(() => screen.findByLabelText("Your conversations"));
  await within(list).findByText("Payroll");
  const groups = ["Today", "Yesterday", "Previous 7 days", "Older"];
  for (const group of groups) expect(within(list).getByText(group)).toBeDefined();
  const order = within(list)
    .getAllByText(/Payroll|Hiring plan|Budget review|Old incident/)
    .map((el) => el.textContent);
  expect(order).toEqual(["Payroll", "Hiring plan", "Budget review", "Old incident"]);
});

test("a conversation the engine has not named yet is listed as untitled", async () => {
  engine([summary(PAYROLL, "Payroll", 60_000), summary(HIRING, null, 2 * 60_000)]);
  mount("#/ask");
  const list = await screen
    .findByRole("list", { name: "Your conversations" })
    .catch(() => screen.findByLabelText("Your conversations"));
  await within(list).findByText("Payroll");
  expect(within(list).getByText("Untitled conversation")).toBeDefined();
});

test("the address names the conversation: it is read from the engine, drawn, and marked in the list", async () => {
  const sent = engine([
    summary(PAYROLL, "Payroll", 60_000),
    summary(HIRING, "Hiring plan", 120_000),
  ]);
  mount(`#/ask/${PAYROLL}`);
  await screen.findByText("About Payroll");
  expect(conversation().id).toBe(PAYROLL);
  expect(sent.some((r) => r.url.pathname === `/assistant/conversations/${PAYROLL}`)).toBe(true);
  expect(screen.getByRole("heading", { name: "Payroll" })).toBeDefined();
  const row = (await screen.findByText("Hiring plan")).closest("[aria-current], li, a, button");
  expect(row?.getAttribute("aria-current")).not.toBe("true");
  const open = screen.getAllByText("Payroll").find((el) => el.closest("[data-conversation]"));
  expect(open?.closest("[data-conversation]")?.getAttribute("data-conversation")).toBe(PAYROLL);
});

test("a question asked on the page carries no screen, and its new conversation is written into the address", async () => {
  const sent = engine([]);
  // THE FRAME DESCRIBES A SCREEN UNDER THE PAGE: the page must not send it.
  setFramePage({
    screen: "ask",
    title: "Ask Crewlet",
    path: ["ask"],
    params: {},
    query: {},
    crumbs: ["Ask Crewlet"],
  } as unknown as Parameters<typeof setFramePage>[0]);
  mount("#/ask");
  const box = await screen.findByRole("textbox", { name: "Ask Crewlet" });
  expect(screen.queryByRole("button", { name: /Remove .* from this question/ })).toBeNull();
  fireEvent.change(box, { target: { value: "Which agents are stuck?" } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  const turns = () => sent.filter((r) => r.url.pathname.endsWith("/turns"));
  await waitFor(() => expect(turns()).toHaveLength(1));
  expect(JSON.parse(String(turns()[0]!.init.body)).page_context).toBeNull();
  await waitFor(() => expect(window.location.hash).toBe(`#/ask/${MADE}`));
});

test("Delete asks first, then deletes the conversation from the engine and the list", async () => {
  const sent = engine([
    summary(PAYROLL, "Payroll", 60_000),
    summary(HIRING, "Hiring plan", 120_000),
  ]);
  mount(`#/ask/${PAYROLL}`);
  await screen.findByText("About Payroll");
  const hiring = await screen.findByText("Hiring plan");
  const row = hiring.closest("li") ?? hiring.parentElement!;
  await act(async () => {
    fireEvent.click(within(row as HTMLElement).getByRole("button", { name: "More actions" }));
  });
  fireEvent.click(await screen.findByRole("menuitem", { name: /Delete/ }));
  const dialog = await screen.findByRole("alertdialog", { name: "Delete this conversation?" });
  expect(sent.some((r) => (r.init.method ?? "GET") === "DELETE")).toBe(false);
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete conversation" }));
  });
  await waitFor(() =>
    expect(
      sent.some(
        (r) =>
          r.init.method === "DELETE" && r.url.pathname === `/assistant/conversations/${HIRING}`,
      ),
    ).toBe(true),
  );
  await waitFor(() => expect(screen.queryByText("Hiring plan")).toBeNull());
  // THE OPEN ONE STAYS OPEN: another was deleted.
  expect(conversation().id).toBe(PAYROLL);
  expect(window.location.hash).toBe(`#/ask/${PAYROLL}`);
});

test("deleting the open conversation leaves for a new one", async () => {
  engine([summary(PAYROLL, "Payroll", 60_000)]);
  mount(`#/ask/${PAYROLL}`);
  await screen.findByText("About Payroll");
  fireEvent.click(screen.getByRole("button", { name: "Delete this conversation" }));
  const dialog = await screen.findByRole("alertdialog", { name: "Delete this conversation?" });
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete conversation" }));
  });
  await waitFor(() => expect(conversation().id).toBeNull());
  await waitFor(() => expect(window.location.hash).toBe("#/ask"));
  expect(screen.getByRole("heading", { name: "New conversation" })).toBeDefined();
});

test("a conversation the engine does not give this reader is said in place, with a way to a new one", async () => {
  engine([summary(PAYROLL, "Payroll", 60_000)]);
  mount(`#/ask/${HIRING}`);
  await screen.findByText("This conversation isn't in your history");
  expect(screen.queryByRole("textbox", { name: "Ask Crewlet" })).toBeNull();
  fireEvent.click(screen.getAllByRole("button", { name: "New conversation" }).at(-1)!);
  await waitFor(() => expect(window.location.hash).toBe("#/ask"));
  await screen.findByRole("textbox", { name: "Ask Crewlet" });
});

test("a search asks the engine by title, and says when nothing matches", async () => {
  const sent = engine([
    summary(PAYROLL, "Payroll", 60_000),
    summary(HIRING, "Hiring plan", 120_000),
  ]);
  mount("#/ask");
  await screen.findByText("Hiring plan");
  fireEvent.change(screen.getByRole("textbox", { name: "Find a conversation" }), {
    target: { value: "hiring" },
  });
  await waitFor(() => expect(screen.queryByText("Payroll")).toBeNull());
  expect(screen.getByText("Hiring plan")).toBeDefined();
  expect(sent.some((r) => r.url.searchParams.get("q") === "hiring")).toBe(true);
  fireEvent.change(screen.getByRole("textbox", { name: "Find a conversation" }), {
    target: { value: "zebra" },
  });
  await screen.findByText("No conversation matches");
});

test("older conversations are read a page at a time", async () => {
  const sent = engine([], {
    "": { rows: [summary(PAYROLL, "Payroll", 60_000)], next: "page-2" },
    "page-2": { rows: [summary(ARCHIVE, "Old incident", 20 * DAY)] },
  });
  mount("#/ask");
  await screen.findByText("Payroll");
  expect(screen.queryByText("Old incident")).toBeNull();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Show older conversations" }));
  });
  await screen.findByText("Old incident");
  expect(screen.getByText("Payroll")).toBeDefined();
  expect(sent.some((r) => r.url.searchParams.get("cursor") === "page-2")).toBe(true);
  expect(screen.queryByRole("button", { name: "Show older conversations" })).toBeNull();
});

test("where the reader may not ask, the page says why and offers no composer", async () => {
  engine([]);
  mount("#/ask", {
    ...READY,
    unavailable: {
      reason: "no_permission",
      short: "Your access does not include Ask Crewlet.",
      title: "Ask Crewlet is not part of your access",
      body: "Everything else you can open in Crewlet works as usual. Ask an admin if you need it.",
    },
  });
  await screen.findByText("Ask Crewlet is not part of your access");
  expect(screen.queryByRole("textbox", { name: "Ask Crewlet" })).toBeNull();
});

const NO_MODEL: AskReading = {
  ...READY,
  unavailable: {
    reason: "no_model",
    short: "No model is assigned to Ask Crewlet yet.",
    title: "Ask Crewlet has no model for Nimbus yet",
    body: "An admin has not chosen the model Ask Crewlet runs on.",
  },
};
const assistantViewer = (assistant: Record<string, unknown>) => ({
  login: "ada",
  grants: ["admin"],
  assistant: {
    available: false,
    reason: "no_model",
    model: null,
    notice: null,
    can_assign: false,
    reads_images: true,
    ...assistant,
  },
});

// ASK CREWLET'S MODEL IS CHOSEN IN SETTINGS › MODELS ALONE: with none, a reader
// who may choose it is sent there, by a step and no picker; the page names no
// model and offers no choice of one.
test("with no model, a reader who may choose one is sent to Settings › Models & Fleets, with no picker", async () => {
  engine([]);
  const opened: unknown[] = [];
  const off = onBus("assistant.navigation.open", (target) => opened.push(target));
  mount("#/ask", NO_MODEL, assistantViewer({ can_assign: true }));
  await screen.findByText(/Ask Crewlet needs a model to answer/);
  expect(screen.queryByRole("combobox")).toBeNull();
  expect(screen.queryByRole("textbox", { name: "Ask Crewlet" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Model settings" }));
  expect(opened).toEqual([{ path: ["settings", "models"], query: {}, mode: "here" }]);
  off();
});

test("with no model, anybody else is told an administrator has not chosen one, with no way there", async () => {
  engine([]);
  mount("#/ask", NO_MODEL, assistantViewer({ can_assign: false }));
  await screen.findByText("Ask Crewlet has no model for Nimbus yet");
  expect(screen.getByText("An admin has not chosen the model Ask Crewlet runs on.")).toBeDefined();
  expect(screen.queryByRole("button", { name: "Model settings" })).toBeNull();
  expect(screen.queryByRole("combobox")).toBeNull();
});

test("the conversation's head names no model and offers no picker", async () => {
  engine([]);
  mount(
    "#/ask",
    READY,
    assistantViewer({
      available: true,
      reason: undefined,
      can_assign: true,
      model: { key: "sonnet", label: "Claude Sonnet" },
    }),
  );
  const head = await waitFor(() => document.querySelector<HTMLElement>(".ask-page__head")!);
  await act(async () => {});
  expect(head.textContent).not.toMatch(/sonnet|Claude Sonnet|Runs on/);
  expect(screen.queryByRole("combobox")).toBeNull();
});

// ONLY A 404 SAYS THE CONVERSATION IS NOT THEIRS. Mutation: read every
// rejection as missing and a server error tells the reader it is gone.
test("a transcript the engine failed to read says so in its words, with Try again", async () => {
  const sent = engine([summary(PAYROLL, "Payroll", 60_000)], undefined, { status: 500 });
  mount(`#/ask/${PAYROLL}`);
  await screen.findByText("This conversation could not be read");
  expect(screen.queryByText("This conversation isn't in your history")).toBeNull();
  expect(document.body.textContent).toContain("Something went wrong on the engine.");
  const reads = () =>
    sent.filter((r) => r.url.pathname === `/assistant/conversations/${PAYROLL}`).length;
  const before = reads();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  });
  await waitFor(() => expect(reads()).toBe(before + 1));
});

// A CONVERSATION ON ITS WAY IS NOT A NEW ONE. Mutation: draw the store's
// empty view while the transcript is read and the greeting and a live box
// stand where the reader's conversation is coming.
test("while a named conversation is read, the page names it and holds the composer", async () => {
  let release = () => {};
  const hold = new Promise<void>((resolve) => {
    release = resolve;
  });
  engine([summary(PAYROLL, "Payroll", 60_000)], undefined, { hold });
  mount(`#/ask/${PAYROLL}`);
  await screen.findByRole("heading", { name: "Payroll" });
  expect(screen.queryByRole("heading", { name: "New conversation" })).toBeNull();
  expect(screen.getByText("Reading this conversation")).toBeDefined();
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  expect(box.getAttribute("aria-disabled") ?? box.getAttribute("disabled")).not.toBeNull();
  await act(async () => {
    release();
  });
  await screen.findByText("About Payroll");
  expect(screen.queryByText("Reading this conversation")).toBeNull();
});

// RENAME, FROM THE ROW AND FROM THE HEAD: Enter saves, the engine's refusal
// is said in its words, and the list and the head show the new title.
test("Rename from a row saves on Enter, says a refusal in the engine's words, and retitles the row", async () => {
  const sent = engine([
    summary(PAYROLL, "Payroll", 60_000),
    summary(HIRING, "Hiring plan", 120_000),
  ]);
  mount(`#/ask/${PAYROLL}`);
  await screen.findByText("About Payroll");
  const hiring = await screen.findByText("Hiring plan");
  const row = hiring.closest("li") ?? hiring.parentElement!;
  await act(async () => {
    fireEvent.click(within(row as HTMLElement).getByRole("button", { name: "More actions" }));
  });
  fireEvent.click(await screen.findByRole("menuitem", { name: /Rename/ }));
  const dialog = await screen.findByRole("dialog", { name: "Rename conversation" });
  const field = within(dialog).getByRole("textbox", { name: "Title" });
  expect((field as HTMLInputElement).value).toBe("Hiring plan");

  fireEvent.change(field, { target: { value: "a refused title" } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  await within(dialog).findByText("That title is not taken here.");

  fireEvent.change(field, { target: { value: "x".repeat(61) } });
  expect(within(dialog).getAllByText("At most 60 characters; this is 61.").length).toBeGreaterThan(
    0,
  );

  fireEvent.change(field, { target: { value: "  Hiring for Q3  " } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  await waitFor(() =>
    expect(screen.queryByRole("dialog", { name: "Rename conversation" })).toBeNull(),
  );
  const patch = sent.filter((r) => r.init.method === "PATCH").at(-1);
  expect(patch?.url.pathname).toBe(`/assistant/conversations/${HIRING}`);
  expect(JSON.parse(String(patch!.init.body))).toMatchObject({ title: "Hiring for Q3" });
  expect(new Headers(patch!.init.headers).get("Idempotency-Key")).not.toBeNull();
  await screen.findByText("Hiring for Q3");
});

test("Rename from the head retitles the open conversation, and Escape cancels", async () => {
  engine([summary(PAYROLL, "Payroll", 60_000)]);
  mount(`#/ask/${PAYROLL}`);
  await screen.findByText("About Payroll");
  fireEvent.click(screen.getByRole("button", { name: "Rename this conversation" }));
  let dialog = await screen.findByRole("dialog", { name: "Rename conversation" });
  fireEvent.keyDown(within(dialog).getByRole("textbox", { name: "Title" }), { key: "Escape" });
  await waitFor(() =>
    expect(screen.queryByRole("dialog", { name: "Rename conversation" })).toBeNull(),
  );
  fireEvent.click(screen.getByRole("button", { name: "Rename this conversation" }));
  dialog = await screen.findByRole("dialog", { name: "Rename conversation" });
  const field = within(dialog).getByRole("textbox", { name: "Title" });
  fireEvent.change(field, { target: { value: "Payroll in March" } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  await screen.findByRole("heading", { name: "Payroll in March" });
  expect(conversation().title).toBe("Payroll in March");
});

/** Another tab of this browser, telling of a conversation. */
function otherTab(told: Record<string, unknown>) {
  const tab = new BroadcastChannel("crewlet-assistant-conversations");
  tab.postMessage(told);
  tab.close();
}

// A SECOND TAB FOLLOWS THE FIRST. Mutation: keep the store per tab with no
// channel and this tab never hears the question asked in the other.
test("a question asked in another tab is read into this one", async () => {
  const sent = engine([summary(PAYROLL, "Payroll", 60_000)]);
  mount(`#/ask/${PAYROLL}`);
  await screen.findByText("About Payroll");
  const reads = () =>
    sent.filter((r) => r.url.pathname === `/assistant/conversations/${PAYROLL}`).length;
  const before = reads();
  otherTab({ kind: "moved", conversation: PAYROLL, turn: "t2" });
  await waitFor(() => expect(reads()).toBe(before + 1));
  // A message this build cannot read is dropped.
  otherTab({ kind: "moved", conversation: "not-an-id" });
  otherTab({ kind: "erase", conversation: PAYROLL });
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(reads()).toBe(before + 1);
});

test("a conversation deleted in another tab says so, and takes no more questions", async () => {
  engine([summary(PAYROLL, "Payroll", 60_000)]);
  mount(`#/ask/${PAYROLL}`);
  await screen.findByText("About Payroll");
  otherTab({ kind: "deleted", conversation: PAYROLL });
  await screen.findAllByText("This conversation was deleted");
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  expect(box.getAttribute("aria-disabled") ?? box.getAttribute("disabled")).not.toBeNull();
});

test("a question sent into a conversation deleted elsewhere is told it was deleted, not that no record exists", async () => {
  const sent = engine([summary(PAYROLL, "Payroll", 60_000)]);
  mount(`#/ask/${PAYROLL}`);
  await screen.findByText("About Payroll");
  // DELETED ON ANOTHER DEVICE: no tab of this browser told of it.
  vi.mocked(fetch).mockImplementation(async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input));
    sent.push({ url, init: init ?? {} });
    return json({ error: "not_found", message: "There is no such record here." }, 404);
  });
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  fireEvent.change(box, { target: { value: "And next month?" } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  await screen.findByText("Your question was not sent: this conversation was deleted.");
  expect(document.body.textContent).not.toContain("There is no such record here.");
  await screen.findAllByText("This conversation was deleted");
});

// A READ THAT FAILED IS NOT KEPT AS THE ANSWER: coming back to the page reads
// the conversation again. Mutation: skip the read whenever the store holds an
// opening for the address, whatever came of it.
test("coming back to a conversation whose read failed reads it again", async () => {
  engine([summary(PAYROLL, "Payroll", 60_000)], undefined, { status: 500 });
  const first = mount(`#/ask/${PAYROLL}`);
  await screen.findByText("This conversation could not be read");
  first.unmount();
  vi.unstubAllGlobals();
  engine([summary(PAYROLL, "Payroll", 60_000)]);
  mount(`#/ask/${PAYROLL}`);
  await screen.findByText("About Payroll");
});
