/**
 * Ask Crewlet's page, over the engine's own routes, stubbed at `fetch`: the
 * reader's history beside the conversation the address names, a question
 * asked with no page attached, and Delete.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { AppAnnouncer } from "~/app/announcer.tsx";
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
const summary = (id: string, title: string, ago: number, state?: string) => ({
  id,
  title,
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

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

/** The engine as the page meets it: every request recorded, each route answered from `rows`. */
function engine(
  rows: ReturnType<typeof summary>[],
  pages?: Record<string, { rows: typeof rows; next?: string }>,
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
        return json({ conversations: held.filter((r) => r.title.toLowerCase().includes(q)) });
      }
      if (path === "/assistant/conversations" && method === "POST") {
        const made = summary(MADE, "", 0);
        held = [made, ...held];
        return json({ conversation: made }, 201);
      }
      const one = /^\/assistant\/conversations\/([^/]+)$/.exec(path);
      if (one && method === "GET") {
        const row = held.find((r) => r.id === one[1]);
        return row
          ? json(transcript(row, `About ${row.title}`))
          : json({ error: "not_found" }, 404);
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

function mount(address: string, reading: AskReading = READY) {
  vi.mocked(useAskReading).mockReturnValue(reading);
  window.location.hash = address;
  const store = new Store();
  store.applyOrg(withDerived({ name: "Nimbus", roles: [] }));
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve({});
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
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
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
  fireEvent.click(within(row as HTMLElement).getByRole("button", { name: "Delete" }));
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
