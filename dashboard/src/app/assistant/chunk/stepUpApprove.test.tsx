/**
 * An approval that needed a step-up, was confirmed on the second try and then
 * failed leaves nothing behind: the next card the resumed turn prepares sends
 * its own approval the moment it is pressed.
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
import { StepUpHost } from "~/app/StepUp.tsx";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { resetPageContextForTest } from "../context.ts";
import { Drawer } from "./Drawer.tsx";
import { conversation, resetSessionForTest } from "./session.ts";

const GOLDEN = readFileSync(
  join(repoRoot(), "internal/assistant/wire/testdata/frames.sse"),
  "utf8",
);
const TURN = "3db96f52-a891-5953-9735-0ae522cf6347";
const CONVERSATION = "403212ed-5a4a-586d-8af6-c62411a17c2f";
const FIRST_PROPOSAL = "9035513c-6cd1-5b25-bbe2-c39df0a5a786";
const SECOND_PROPOSAL = "0b6c9a1e-2f3d-4e5a-8b7c-9d0e1f2a3b4c";
const BLOCKS = GOLDEN.split("\n\n").filter((block) => block.includes(`${TURN}.`));

/** A block of the golden turn, with its frame as JSON. */
interface Framed {
  block: string;
  frame: Record<string, unknown> & {
    kind: string;
    card?: { card: { type: string; proposal?: { scope: string } } };
  };
}
const FRAMES: Framed[] = BLOCKS.map((block) => {
  const data = block.split("\n").find((line) => line.startsWith("data: "))!;
  return { block, frame: JSON.parse(data.slice("data: ".length)) as Framed["frame"] };
});

/** The golden turn's proposal card of one scope. */
const proposalOf = (scope: string): Framed | undefined =>
  FRAMES.find((f) => f.frame.kind === "card" && f.frame.card?.card.proposal?.scope === scope);

// THE SERVER CARD ALONE: the golden turn's page change and its actions are
// another surface's. Found by what they are, never by their place in the
// stream, so a frame the vocabulary gains does not move them.
const PAGE_CHANGE = new Set(
  FRAMES.filter((f) => f.frame.kind === "action" || f === proposalOf("page")).map((f) => f.block),
);
const FIRST = BLOCKS.filter((block) => !PAGE_CHANGE.has(block))
  .map((block) => `${block}\n\n`)
  .join("");

/** The golden turn's last frame of a kind, as its JSON. */
function lastOf(kind: string): Record<string, unknown> {
  return structuredClone(FRAMES.filter((f) => f.frame.kind === kind).at(-1)!.frame);
}

function sse(frame: Record<string, unknown>): string {
  return `id: ${TURN}.${frame.seq as number}\nevent: ${frame.kind as string}\ndata: ${JSON.stringify(frame)}\n\n`;
}

/**
 * What the resumed turn says after the first change failed: a second card,
 * then it parks on it again.
 */
function resumed(): string {
  const card = structuredClone(proposalOf("server")!.frame) as unknown as {
    seq: number;
    at: string;
    card: { card_id: string; card: { proposal: Record<string, unknown> } };
  };
  card.seq = 41;
  card.at = "2026-10-24T09:42:00Z";
  card.card.card_id = "5d0f7c3a-1b2e-4c5d-8e9f-0a1b2c3d4e5f";
  card.card.card.proposal = {
    ...card.card.card.proposal,
    proposal_id: SECOND_PROPOSAL,
    approve_label: "Approve the retry",
  };
  const status = { ...lastOf("status"), seq: 42, at: "2026-10-24T09:42:00.1Z" };
  const done = { ...lastOf("done"), seq: 43, at: "2026-10-24T09:42:00.2Z" };
  return [card, status, done].map(sse).join("");
}

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

const streamOf = `/assistant/conversations/${CONVERSATION}/stream?turn=${TURN}`;

/**
 * The engine: the first approval asks for a step-up, the first code is wrong,
 * the replay fails and the turn resumes onto a second card.
 */
function engine() {
  const sent: { path: string; init: RequestInit }[] = [];
  let approvals = 0;
  let stepUps = 0;
  let streams = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      const at = new URL(url);
      sent.push({ path: at.pathname, init });
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
        return json({ turn_id: TURN, stream_url: streamOf, after_seq: 0 }, 202);
      }
      if (at.pathname === "/iam/credentials") {
        return json({ credentials: [{ method: "totp", revoked: false }] });
      }
      if (at.pathname === "/auth/step-up") {
        stepUps += 1;
        if (stepUps === 1) {
          return json(
            { error: "sign_in_refused", message: "Those details were not accepted." },
            401,
          );
        }
        return json({
          person: "p1",
          login: "ada",
          expires_at: "2026-10-24T21:41:00Z",
          position: "CREWLET_IAM_LOG@0:1",
          status: "active",
        });
      }
      if (at.pathname.endsWith("/approve")) {
        approvals += 1;
        if (approvals === 1) {
          return json({ error: "step_up_required", message: "Confirm it is you." }, 403);
        }
        if (approvals === 2) {
          return json(
            {
              proposal: {
                id: FIRST_PROPOSAL,
                state: "failed",
              },
              turn_id: TURN,
              stream_url: streamOf,
              after_seq: 23,
            },
            202,
          );
        }
        return json(
          { proposal: { id: SECOND_PROPOSAL, state: "applied" }, turn_id: TURN, stream_url: null },
          202,
        );
      }
      if (at.pathname.endsWith("/stream")) {
        streams += 1;
        const body = streams === 1 ? FIRST : resumed();
        return new Response(body, {
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
                model={null}
                admin={false}
                onClose={() => {}}
                initialFocus="composer"
                name="Ada Lovelace"
                shape="column"
              />
              <StepUpHost />
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

async function confirmIdentity(code: string) {
  await screen.findByRole("dialog", { name: "Confirm it’s you" });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "secret" } });
  fireEvent.change(screen.getByLabelText("Code"), { target: { value: code } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
  });
}

test("a failed approval after a step-up leaves the next card's Approve working", async () => {
  const sent = engine();
  mount();
  await ask("Fix the flaky sign-in test");
  await waitFor(() => expect(conversation().turns[0]?.phase).toBe("awaiting_approval"));

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Approve and save" }));
  });
  await waitFor(() =>
    expect(screen.getByRole("dialog", { name: "Confirm it’s you" })).toBeTruthy(),
  );
  await confirmIdentity("000000");
  await screen.findByText("Wrong password or code. Try again.");
  await confirmIdentity("123456");
  await waitFor(() =>
    expect(screen.queryByRole("dialog", { name: "Confirm it’s you" })).toBeNull(),
  );

  // THE TURN RESUMES ONTO A SECOND CARD, and the drawer waits on the person.
  const retry = await screen.findByRole("button", { name: "Approve the retry" });
  await waitFor(() => expect(conversation().turns[0]?.phase).toBe("awaiting_approval"));

  const before = sent.filter((r) => r.path.endsWith("/approve")).length;
  await act(async () => {
    fireEvent.click(retry);
  });
  await waitFor(() =>
    expect(sent.filter((r) => r.path.endsWith("/approve")).length).toBe(before + 1),
  );
  expect(sent.filter((r) => r.path.endsWith("/approve")).at(-1)!.path).toContain(SECOND_PROPOSAL);
});
