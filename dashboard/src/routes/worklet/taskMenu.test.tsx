/**
 * A task into the trash, back out of it, and out of the company for good, from
 * a row's menu and a selection's bar.
 *
 * The failure modes: a task with subtasks removed without asking whether they
 * go too (they stand alone on every board, or vanish with it, and the reader
 * chose neither); an Undo that restores something other than what the removal
 * took; a refusal said in the dashboard's words rather than the engine's; a
 * permanent delete reachable without the key typed, without a reason, or by a
 * reader the engine never lets purge; and a control hidden from a reader who
 * cannot act, rather than drawn and held with the sentence that says why.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { TaskRowMenu, TaskSelectionBar, taskTargetOf, useTaskActions } from "./taskMenu.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { AppAnnouncer } from "~/app/announcer.tsx";
import { ViewerProvider } from "~/lib/viewer.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { WorkSummary } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** A lead with Worklet, an administrator: every gesture is theirs to try. */
const JANE = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["remove_work_item", "restore_work_item", "move_work_item"],
};

/** A colleague with Worklet and no admin: the trash is theirs, a purge is not. */
const MAYA = { ...JANE, login: "U0MAYA", grants: ["worklet"], handle: "maya", owner: "maya" };

const row = (id: string, key: string, title: string): WorkSummary => ({
  id,
  key,
  project: "ENG",
  title,
  type: "task",
  status: "todo",
  updated: "2031-04-16T00:00:00Z",
  version: 3,
});

const ENG1 = row("eng-1", "ENG-1", "Retry PXE boot");
const ENG2 = row("eng-2", "ENG-2", "Rotate the build keys");

interface Sent {
  method: string;
  url: string;
  key: string | null;
  body: Record<string, unknown>;
}

let sent: Sent[];
let asked: { what: string; params: Record<string, unknown> }[];
/** The ids the subtask question answers as having children. */
let parents: string[];
/** What each tool answers, by name; the default is an applied write. */
let replies: Record<string, { status: number; body: unknown }>;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  sent = [];
  asked = [];
  parents = [];
  replies = {};
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const headers = new Headers(init.headers);
      sent.push({
        method: init.method ?? "GET",
        url: String(url),
        key: headers.get("Idempotency-Key"),
        body: init.body ? (JSON.parse(init.body as string) as Record<string, unknown>) : {},
      });
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const reply = replies[tool] ?? {
        status: 200,
        body: { tool, outcome: "applied", position: "CREWLET_TRACKER_LOG@1:4" },
      };
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/** A list of rows, each with its menu, a bar over a selection of them, and the dialogs. */
function Rows({
  rows,
  picked = [],
  inTrash = false,
}: {
  rows: WorkSummary[];
  picked?: WorkSummary[];
  inTrash?: boolean;
}) {
  const actions = useTaskActions();
  return (
    <>
      <TaskSelectionBar
        selected={picked.map(taskTargetOf)}
        onClear={() => {}}
        actions={actions}
        inTrash={inTrash}
      />
      {rows.map((r) => (
        <div key={r.id} role="row">
          {r.key}
          <TaskRowMenu row={r} actions={actions} inTrash={inTrash} />
        </div>
      ))}
      {actions.dialog}
    </>
  );
}

function mount(
  viewer: Record<string, unknown> = JANE,
  props: { rows?: WorkSummary[]; picked?: WorkSummary[]; inTrash?: boolean } = {},
) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  const socket = new LiveSocket(store);
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ what, params: params ?? {} });
    if (what === "viewer") return Promise.resolve(viewer);
    if (what === "work_projects") {
      return Promise.resolve({
        projects: [
          { key: "ENG", name: "Engineering", task_counts: {} },
          { key: "OPS", name: "Operations", task_counts: {} },
        ],
        total: 2,
        census: { active: 2, archived: 0 },
        complete: true,
      });
    }
    if (what === "work_items") {
      const items = [ENG1, ENG2].filter((r) => parents.includes(r.id));
      return Promise.resolve({ items, complete: true });
    }
    return Promise.resolve({});
  }) as typeof socket.query;
  render(
    <ToastProvider>
      <AppAnnouncer />
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <ViewerProvider>
            <Rows rows={props.rows ?? [ENG1]} picked={props.picked} inTrash={props.inTrash} />
          </ViewerProvider>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
}

async function settle() {
  await afterMicrotasks();
  await afterMicrotasks();
}

/** Opens a row's menu and presses one of its entries. */
async function press(key: string, entry: string) {
  fireEvent.click(screen.getByRole("button", { name: `More for ${key}` }));
  fireEvent.click(screen.getByRole("menuitem", { name: new RegExp(`^${entry}`) }));
  await settle();
}

const acts = () =>
  sent
    .filter((s) => s.url.includes("/operator/act/"))
    .map((s) => ({
      tool: decodeURIComponent(s.url.split("/operator/act/")[1]!),
      args: (s.body as { args: Record<string, unknown> }).args,
    }));

describe("moving a task to the trash", () => {
  test("a task with no subtasks goes at once, alone, and Undo restores it", async () => {
    mount();
    await settle();
    await press("ENG-1", "Move to trash");
    // WHETHER IT HAS SUBTASKS WAS ASKED OF THE ENGINE, by its key, every
    // status, each task on its own.
    const probe = asked.find((q) => q.what === "work_items")!;
    expect(probe.params).toMatchObject({
      key: "ENG-1",
      has_children: true,
      subtasks: "separate",
      show_closed: "true",
    });
    expect(acts()).toEqual([{ tool: "remove_work_item", args: { item: "ENG-1" } }]);
    expect(await screen.findAllByText("Moved ENG-1 to the trash")).not.toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Undo" }));
    await settle();
    expect(acts().at(-1)).toEqual({ tool: "restore_work_item", args: { item: "ENG-1" } });
    expect(await screen.findAllByText("Restored ENG-1")).not.toHaveLength(0);
  });

  test("a task with subtasks asks first, and takes them only when told to", async () => {
    parents = ["eng-1"];
    mount();
    await settle();
    await press("ENG-1", "Move to trash");
    expect(acts()).toEqual([]);
    expect(screen.getByText(/ENG-1 has subtasks\./)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "With subtasks" }));
    await settle();
    expect(acts()).toEqual([{ tool: "remove_work_item", args: { item: "ENG-1", subtree: true } }]);

    await press("ENG-1", "Move to trash");
    fireEvent.click(screen.getByRole("button", { name: "Only this task" }));
    await settle();
    expect(acts().at(-1)).toEqual({ tool: "remove_work_item", args: { item: "ENG-1" } });
  });

  test("a refusal is said in the engine's own words, and stays", async () => {
    replies.remove_work_item = {
      status: 403,
      body: {
        error: "forbidden",
        tool: "remove_work_item",
        detail: "Only whoever filed ENG-1, its project's lead or an admin may put it in the trash.",
      },
    };
    mount(MAYA);
    await settle();
    await press("ENG-1", "Move to trash");
    expect(await screen.findAllByText(/Not moved to the trash/)).not.toHaveLength(0);
    expect(
      screen.getAllByText(/Only whoever filed ENG-1, its project's lead or an admin may put it/),
    ).not.toHaveLength(0);
  });

  test("a selection goes in one gesture, asking once about the ones with subtasks", async () => {
    parents = ["eng-2"];
    mount(JANE, { rows: [ENG1, ENG2], picked: [ENG1, ENG2] });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Move 2 to trash" }));
    await settle();
    expect(screen.getByText(/1 of these 2 tasks has subtasks\./)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "With subtasks" }));
    await settle();
    await settle();
    // THE SUBTREE ONLY WHERE THERE IS ONE: ENG-1 has nothing under it.
    expect(acts()).toEqual([
      { tool: "remove_work_item", args: { item: "ENG-1" } },
      { tool: "remove_work_item", args: { item: "ENG-2", subtree: true } },
    ]);
    expect(await screen.findAllByText("Moved 2 tasks to the trash")).not.toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Undo" }));
    await settle();
    await settle();
    expect(acts().slice(-2)).toEqual([
      { tool: "restore_work_item", args: { item: "ENG-1" } },
      { tool: "restore_work_item", args: { item: "ENG-2" } },
    ]);
  });
});

describe("moving a task to another project", () => {
  test("one top-level task moves to the project chosen, and its new key is said", async () => {
    replies.move_work_item = {
      status: 200,
      body: {
        tool: "move_work_item",
        outcome: "applied",
        position: "CREWLET_TRACKER_LOG@1:9",
        receipt: { key: "OPS-3", project: "OPS", moved_from: "ENG" },
      },
    };
    mount();
    await settle();
    await press("ENG-1", "Move to project");
    // THE PROJECT IT IS IN IS NOT OFFERED: a move into it is no move.
    const move = () => screen.getByRole("button", { name: /^Move$/ });
    expect(move().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(screen.getByRole("combobox", { name: "Move to" }));
    expect(screen.queryByRole("option", { name: /Engineering/ })).toBeNull();
    fireEvent.mouseDown(screen.getByRole("option", { name: /Operations/ }));
    await settle();
    fireEvent.click(move());
    await settle();
    expect(acts()).toEqual([{ tool: "move_work_item", args: { item: "ENG-1", project: "OPS" } }]);
    expect(await screen.findAllByText("Moved ENG-1 to OPS")).not.toHaveLength(0);
  });

  test("a subtask's Move is held: it moves with its parent", async () => {
    mount(JANE, { rows: [{ ...ENG2, parent: "eng-1" }] });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "More for ENG-2" }));
    const entry = screen.getByRole("menuitem", { name: /^Move to project/ });
    expect(entry.getAttribute("aria-disabled")).toBe("true");
    expect(entry.textContent).toContain("A subtask moves with its parent");
  });

  test("a refusal stays in the dialog, in the engine's words", async () => {
    replies.move_work_item = {
      status: 403,
      body: {
        error: "forbidden",
        tool: "move_work_item",
        detail: "Moving ENG-1 out of ENG is its project's lead's.",
      },
    };
    mount(MAYA);
    await settle();
    await press("ENG-1", "Move to project");
    fireEvent.click(screen.getByRole("combobox", { name: "Move to" }));
    fireEvent.mouseDown(screen.getByRole("option", { name: /Operations/ }));
    await settle();
    fireEvent.click(screen.getByRole("button", { name: /^Move$/ }));
    await settle();
    expect(
      await screen.findAllByText(/Moving ENG-1 out of ENG is its project.s lead.s/),
    ).not.toHaveLength(0);
  });
});

describe("in the trash", () => {
  test("a row restores, and a selection restores together", async () => {
    mount(JANE, { rows: [ENG1, ENG2], picked: [ENG1, ENG2], inTrash: true });
    await settle();
    await press("ENG-1", "Restore");
    expect(acts()).toEqual([{ tool: "restore_work_item", args: { item: "ENG-1" } }]);
    fireEvent.click(screen.getByRole("button", { name: "Restore 2" }));
    await settle();
    await settle();
    expect(acts().slice(-2)).toEqual([
      { tool: "restore_work_item", args: { item: "ENG-1" } },
      { tool: "restore_work_item", args: { item: "ENG-2" } },
    ]);
  });

  test("deleting permanently says what goes, and asks for a reason and the key", async () => {
    mount(JANE, { inTrash: true });
    await settle();
    await press("ENG-1", "Delete permanently");
    expect(screen.getByText(/is deleted on every node/)).toBeTruthy();
    expect(screen.getByText(/Its subtasks stay, moved onto its parent/)).toBeTruthy();
    const confirm = () => screen.getByRole("button", { name: /^Delete permanently/ });
    expect(confirm().getAttribute("aria-disabled")).toBe("true");
    fireEvent.change(screen.getByRole("textbox", { name: /Reason/ }), {
      target: { value: "Filed with a customer's address in it" },
    });
    fireEvent.change(screen.getByRole("textbox", { name: /Type/ }), { target: { value: "ENG-1" } });
    expect(confirm().getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(confirm());
    await settle();
    const purge = sent.find((s) => s.url.includes("/purge"))!;
    expect(purge.method).toBe("POST");
    const url = new URL(purge.url, "http://engine.example.com");
    expect(url.pathname).toBe("/worklet/items/ENG-1/purge");
    // THE KEY IS THE ENGINE'S CONFIRMATION, and the reason the lead is told.
    expect(url.searchParams.get("confirm")).toBe("ENG-1");
    expect(url.searchParams.get("reason")).toBe("Filed with a customer's address in it");
    // A KEYED WRITE: a retry of an unknown answer is the same operation.
    expect(purge.key).toMatch(/\S/);
    expect(await screen.findAllByText("Deleted ENG-1 permanently")).not.toHaveLength(0);
  });

  test("deleting permanently is held for a reader who is not an administrator", async () => {
    mount(MAYA, { inTrash: true });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "More for ENG-1" }));
    const entry = screen.getByRole("menuitem", { name: /^Delete permanently/ });
    expect(entry.getAttribute("aria-disabled")).toBe("true");
    expect(entry.textContent).toContain("Deleting a task permanently needs Admin access");
  });
});

// A READER WHO CANNOT CHANGE THE TASK SEES EVERY ENTRY, held with the sentence
// every other write control says, rather than a menu that hides them.
describe.each([
  { who: "an anonymous reader", viewer: { anonymous: true }, reason: WRITE_REASONS.anonymous },
  {
    who: "a person the engine does not trash tasks for",
    viewer: { ...JANE, acts: [] },
    reason: WRITE_REASONS.not_served,
  },
])("$who", ({ viewer, reason }) => {
  test("is shown Move to trash, held, and the bar's control held too", async () => {
    mount(viewer, { picked: [ENG1] });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "More for ENG-1" }));
    const entry = screen.getByRole("menuitem", { name: /^Move to trash/ });
    expect(entry.getAttribute("aria-disabled")).toBe("true");
    expect(entry.textContent).toContain(reason);
    expect(
      screen.getByRole("button", { name: /Move 1 to trash/ }).getAttribute("aria-disabled"),
    ).toBe("true");
    expect(acts()).toEqual([]);
  });
});
