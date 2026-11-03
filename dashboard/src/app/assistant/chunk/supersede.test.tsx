/**
 * A question asked while a change waits on the person supersedes the waiting
 * turn rather than being refused: the drawer offers Send (never Stop) while a
 * card waits, the waiting turn reads as finished once the next question is
 * accepted, and its card stays approvable, its receipt read beside the turn
 * now running rather than in place of it.
 */

import { readFileSync } from "node:fs";
import { AppAnnouncer } from "~/app/announcer.tsx";
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
import { resetPageContextForTest } from "../context.ts";
import { Drawer } from "./Drawer.tsx";
import { approve, conversation, resetSessionForTest } from "./session.ts";

const GOLDEN = readFileSync(
  join(repoRoot(), "internal/assistant/wire/testdata/frames.sse"),
  "utf8",
);
const FIRST_TURN = "3db96f52-a891-5953-9735-0ae522cf6347";
const SECOND_TURN = "9a0c6f1e-2b3d-4e5f-8a9b-0c1d2e3f4a5b";
const CONVERSATION = "403212ed-5a4a-586d-8af6-c62411a17c2f";
const FIRST = GOLDEN.split("\n\n")
  .filter((block) => block.includes(`${FIRST_TURN}.`))
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

const streamOf = (turn: string) => `/assistant/conversations/${CONVERSATION}/stream?turn=${turn}`;

/** A body that never ends, as a turn still running holds its stream open. */
const open = () =>
  new Response(new ReadableStream({ pull: () => new Promise(() => {}) }), {
    status: 200,
    headers: { "Content-Type": "text/event-stream" },
  });

/** The engine: the first question parks on a card, the second runs on. */
function engine() {
  const sent: { url: string; init: RequestInit }[] = [];
  let turns = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      sent.push({ url, init });
      const at = new URL(url);
      if (at.pathname === "/assistant/conversations" && init.method === "POST") {
        return json(
          {
            conversation: {
              id: CONVERSATION,
              title: "",
              created_at: "2026-10-24T09:41:00Z",
              updated_at: "2026-10-24T09:41:00Z",
              last_activity_at: "2026-10-24T09:41:00Z",
            },
          },
          201,
        );
      }
      if (at.pathname.endsWith("/turns")) {
        turns += 1;
        const turn = turns === 1 ? FIRST_TURN : SECOND_TURN;
        return json({ turn_id: turn, stream_url: streamOf(turn), after_seq: 0 }, 202);
      }
      if (at.pathname.endsWith("/approve")) {
        return json(
          {
            proposal: { id: at.pathname.split("/")[3], state: "applied" },
            turn_id: FIRST_TURN,
            stream_url: streamOf(FIRST_TURN),
            after_seq: 40,
          },
          202,
        );
      }
      if (at.pathname.endsWith("/stream")) {
        return at.searchParams.get("turn") === FIRST_TURN && turns === 1
          ? new Response(FIRST, { status: 200, headers: { "Content-Type": "text/event-stream" } })
          : open();
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
              <AppAnnouncer />
              <Drawer
                unavailable={null}
                model={null}
                admin={false}
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

async function ask(text: string) {
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  fireEvent.change(box, { target: { value: text } });
  await act(async () => {
    fireEvent.keyDown(box, { key: "Enter" });
  });
}

test("a question asked while a card waits supersedes the waiting turn", async () => {
  const sent = engine();
  mount();
  await ask("Fix the flaky sign-in test");
  await waitFor(() => expect(conversation().turns[0]?.phase).toBe("awaiting_approval"));

  // SEND, NEVER STOP, while a card waits on the person.
  const box = screen.getByRole("textbox", { name: "Ask Crewlet" });
  fireEvent.change(box, { target: { value: "And who owns it?" } });
  expect(screen.queryByRole("button", { name: "Stop answering" })).toBeNull();
  expect(screen.getByRole("button", { name: "Send" }).getAttribute("aria-disabled")).not.toBe(
    "true",
  );

  await ask("And who owns it?");
  await waitFor(() => expect(conversation().turns[1]?.id).toBe(SECOND_TURN));
  expect(conversation().turns[0]?.phase).toBe("completed");
  expect(sent.filter((r) => new URL(r.url).pathname.endsWith("/turns"))).toHaveLength(2);
});

test("a superseded turn's card is decided beside the turn now running", async () => {
  const sent = engine();
  mount();
  await ask("Fix the flaky sign-in test");
  await waitFor(() => expect(conversation().turns[0]?.phase).toBe("awaiting_approval"));
  await ask("And who owns it?");
  await waitFor(() => expect(conversation().turns[1]?.id).toBe(SECOND_TURN));
  const running = sent.find((r) => r.url.includes(`turn=${SECOND_TURN}`))!;

  const card = conversation()
    .turns[0]!.cards.map((c) => (c.card.type === "proposal" ? c.card.proposal : undefined))
    .find((p) => p !== undefined)!;
  await act(async () => {
    await approve(card);
  });

  // THE RECEIPT IS READ BESIDE THE RUNNING TURN: its stream is dialled again,
  // and the running turn's stream is left open.
  await waitFor(() =>
    expect(sent.filter((r) => r.url.includes(`turn=${FIRST_TURN}`))).toHaveLength(2),
  );
  expect(running.init.signal?.aborted).toBe(false);
});
