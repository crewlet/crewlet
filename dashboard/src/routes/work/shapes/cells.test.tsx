/**
 * A row's status, priority and holder, changed in place — as the reader, and
 * conditional on the row they were looking at.
 *
 * The failure modes: an edit sent without the version the row was drawn at,
 * which silently overwrites whatever somebody else did a second ago; a lost
 * race reported as "somebody else", when the one fact a person can act on is
 * WHO; and a reader who cannot change the task handed a hundred pickers that
 * each refuse, or none and no word about why.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { AssigneeCell, InlineEdits, PriorityCell, StatusCell } from "./cells.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { WorkSummary } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const JANE = {
  operator_id: "U0FOUNDER",
  operator: true,
  handle: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["update_work_item"],
};

const NAMES: Record<string, string> = { jane: "Jane Founder", maya: "Maya Ops", swe: "SWE" };
const seatName = (handle: string) => NAMES[handle] ?? handle;

const ROW: WorkSummary = {
  id: "eng-1",
  key: "ENG-1",
  project: "ENG",
  title: "Retry PXE boot",
  type: "task",
  status: "todo",
  priority: "normal",
  assignee: "swe",
  updated: "2031-04-16T00:00:00Z",
  version: 12,
};

let posted: { tool: string; args: Record<string, unknown> }[];
let reply: { status: number; body: unknown };

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  posted = [];
  reply = {
    status: 200,
    body: { tool: "update_work_item", outcome: "applied", position: "CREWLET_TRACKER_LOG@1:4" },
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function mount(viewer: Record<string, unknown> = JANE, answers: Record<string, unknown> = {}) {
  const store = new Store();
  store.applyHealth({ status: "healthy", nodes: 1 } as never);
  const socket = new LiveSocket(store);
  const all: Record<string, unknown> = { viewer, ...answers };
  socket.query = ((what: string) => Promise.resolve(all[what] ?? {})) as typeof socket.query;
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <ViewerProvider>
            <InlineEdits seatName={seatName}>
              <div role="row">
                <StatusCell row={ROW} />
                <PriorityCell row={ROW} word />
                <AssigneeCell
                  row={ROW}
                  chrome={{ seatName }}
                  seats={[
                    { handle: "jane", name: "Jane Founder", human: true },
                    { handle: "maya", name: "Maya Ops", human: true },
                  ]}
                  named
                  readOnly={<span>{seatName(ROW.assignee!)}</span>}
                />
              </div>
            </InlineEdits>
          </ViewerProvider>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

/** Open one cell's picker and choose an option by its label. */
function choose(cell: string, option: string) {
  fireEvent.click(screen.getByRole("button", { name: new RegExp(`^${cell} of ENG-1`) }));
  fireEvent.click(screen.getByRole("menuitemradio", { name: option }));
}

describe("a writer", () => {
  // CONDITIONAL ON THE ROW AS DRAWN: `if_match` is the version the list read,
  // so a change somebody made a second ago is refused rather than overwritten.
  test("an inline edit carries if_match for each of the three fields", async () => {
    mount();
    await settle();
    choose("Status", "In progress");
    await settle();
    choose("Priority", "High");
    await settle();
    choose("Assignee", "Maya Ops");
    await settle();
    expect(posted).toEqual([
      { tool: "update_work_item", args: { item: "ENG-1", if_match: 12, status: "in_progress" } },
      { tool: "update_work_item", args: { item: "ENG-1", if_match: 12, priority: "high" } },
      { tool: "update_work_item", args: { item: "ENG-1", if_match: 12, assignee: "maya" } },
    ]);
  });

  // AND "NOBODY" IS A VALUE: the empty handle is the tool's own unassignment.
  test("choosing nobody unassigns", async () => {
    mount();
    await settle();
    choose("Assignee", "Nobody");
    await settle();
    expect(posted.at(-1)?.args).toEqual({ item: "ENG-1", if_match: 12, assignee: "" });
  });

  // A LOST RACE NAMES WHO WON IT, read from the task's own newest history
  // entry — "somebody else" is not somebody a person can talk to.
  test("a lost race reports the conflict by the actor who won it", async () => {
    reply = { status: 409, body: { error: "stale_version", tool: "update_work_item", detail: "" } };
    mount(JANE, {
      work_activity: {
        records: [{ id: "h-9", kind: "status_changed", actor: "U0MAYA", actor_seat: "maya" }],
        complete: true,
      },
    });
    await settle();
    choose("Status", "Done");
    await settle();
    await settle();
    expect(
      await screen.findByText(
        "ENG-1 was changed by Maya Ops since you opened it. Look again, then retry.",
      ),
    ).toBeTruthy();
  });
});

// A READER WHO CANNOT CHANGE THE TASK SEES THE VALUES, and the reason once —
// the sentence every other write control uses — rather than a hundred pickers
// that each refuse.
describe.each([
  { who: "an anonymous reader", viewer: { anonymous: true }, reason: WRITE_REASONS.anonymous },
  {
    who: "an unbound token",
    viewer: { operator_id: "ci", operator: true, handle: "", unbound: true },
    reason: WRITE_REASONS.unbound,
  },
  {
    who: "a person the engine does not change tasks for",
    viewer: { ...JANE, acts: [] },
    reason: WRITE_REASONS.not_served,
  },
])("$who", ({ viewer, reason }) => {
  test("reads the values and is told once why they are read-only", async () => {
    mount(viewer);
    await settle();
    expect(screen.queryByRole("button", { name: /of ENG-1/ })).toBeNull();
    expect(screen.getByText("To do")).toBeTruthy();
    expect(screen.getByText("SWE")).toBeTruthy();
    expect(screen.getByText(/read-only here/).textContent).toContain(reason);
  });
});
