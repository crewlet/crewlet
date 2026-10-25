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
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { repoRoot } from "~/test/engineFiles.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { FrameReadings } from "~/app/Shell.tsx";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { resetPageContextForTest, setFramePage } from "../context.ts";
import { assistantActivity } from "../status.ts";
import { queueQuestion, takeQueuedQuestion } from "../control.ts";
import { Drawer } from "./Drawer.tsx";
import { conversation, resetSessionForTest } from "./session.ts";

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

/** The engine, as the drawer meets it: each request recorded and answered. */
function engine() {
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
            },
          },
          201,
        );
      }
      if (path.endsWith("/turns")) {
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

function mount() {
  const store = new Store();
  store.applyOrg(withDerived({ name: "Nimbus", roles: [] }));
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve({});
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <FrameReadings>
          <ToastProvider>
            <LayerHost>
              <Drawer
                unavailable={null}
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
    screen: "work-tasks",
    title: "Work › Tasks",
    path: ["work", "tasks"],
    params: {},
    query: {},
  });
  mount();
  expect(
    screen.getByRole("heading", { name: /^Good (morning|afternoon|evening), Ada$/ }),
  ).toBeDefined();

  const box = screen.getByRole("textbox", { name: "Ask the assistant" });
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
    protocol_version: 1,
    input: { text: "Fix the flaky sign-in test" },
  });
  expect(body.page_context).toMatchObject({ screen: "work-tasks", title: "Work › Tasks" });

  // The question, the page it carried, and the engine's cards.
  expect(screen.getByRole("article", { name: "You said" }).textContent).toContain(
    "Fix the flaky sign-in test",
  );
  expect(screen.getAllByText("Work › Tasks").length).toBeGreaterThan(0);
  expect(screen.getAllByText("Matching tasks").length).toBeGreaterThan(0);
  expect(screen.getAllByText("No open task covers it yet.").length).toBeGreaterThan(0);
  // A CHANGE WAITS ON THE PERSON, and the Ask button would say so.
  expect(assistantActivity()).toBe("waiting");
});

test("a question past the length a turn takes is not sent", async () => {
  const sent = engine();
  mount();
  const box = screen.getByRole("textbox", { name: "Ask the assistant" });
  fireEvent.change(box, { target: { value: "x".repeat(4001) } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
  expect(sent).toEqual([]);
});

test("the page goes with a question until the reader removes it", async () => {
  const sent = engine();
  setFramePage({
    screen: "work-tasks",
    title: "Work › Tasks",
    path: ["work", "tasks"],
    params: {},
    query: {},
  });
  mount();
  fireEvent.click(screen.getByRole("button", { name: "Remove Work › Tasks from this question" }));
  expect(screen.getByRole("button", { name: "Add Work › Tasks to this question" })).toBeDefined();
  const box = screen.getByRole("textbox", { name: "Ask the assistant" });
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
