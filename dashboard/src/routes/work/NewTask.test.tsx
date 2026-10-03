/**
 * The New task sheet: what it files, where, and what it says when it cannot.
 *
 * Four invariants, each a way this sheet could file something the person did
 * not ask for or fail without saying why: only what was SET is sent (an unset
 * field is the engine's default, never the sheet's guess of it); the door's
 * status and project reach the wire in the one record; `applied` opens the
 * task by the key the engine minted; and a refusal stays in the sheet, in the
 * engine's own sentence naming the argument.
 */

import { act, cleanup, fireEvent, render, screen, within } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { ReactNode } from "react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { NewTaskSheet, blockedBy, createArgs, startingProject, textBudget } from "./NewTask.tsx";
import { TASK_BODY_MAX_BYTES, TASK_TITLE_MAX_BYTES } from "~/contract/work.ts";
import { presetForLane } from "~/app/newTask.ts";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { indexOrg } from "~/lib/seats.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import type { WorkProjectDetail, WorkProjectRow } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const JANE = {
  login: "jane.founder",
  grants: ["state:read", "work:write"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["create_work_item"],
  project: "OPS",
};

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Agent SWE", handle: "swe", kind: "agent" },
  ],
  units: [],
};

const row = (key: string, name: string): WorkProjectRow =>
  ({
    key,
    name,
    unit: { resolved: true },
    lead: { handle: "jane" },
    task_counts: { todo: 1, active: 0, done: 0, closed: 0 },
  }) as WorkProjectRow;

const ENG: WorkProjectDetail = {
  ...row("ENG", "Core platform"),
  statuses: [],
  types: [
    { slug: "task", name: "Task" },
    { slug: "bug", name: "Bug" },
  ],
  fields: [],
  tags: [{ slug: "api", label: "api" }],
  policy_stamp: 1,
  complete: true,
} as WorkProjectDetail;

type Reply = { status: number; body: unknown };
let posted: { tool: string; args: Record<string, unknown> }[];
let reply: Reply;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  posted = [];
  reply = {
    status: 200,
    body: {
      tool: "create_work_item",
      outcome: "applied",
      position: "CREWLET_TRACKER_LOG@1:9",
      receipt: { item: "ENG-9", key: "ENG-9" },
    },
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
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  reloadForTest();
  location.hash = "#/";
});

function mount(
  children: ReactNode,
  viewer: Record<string, unknown> = JANE,
  {
    org = true,
    projects = [row("ENG", "Core platform"), row("OPS", "Operations")],
    colleague = { match: null, candidates: [] },
    project = ENG,
  }: {
    org?: boolean;
    projects?: WorkProjectRow[];
    colleague?: unknown;
    project?: WorkProjectDetail;
  } = {},
) {
  const store = new Store();
  store.applyHealth({ status: "healthy", nodes: 1 } as never);
  if (org) store.applyOrg(ORG as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string) => {
    const answers: Record<string, unknown> = {
      viewer,
      work_projects: { projects },
      work_project: project,
      colleague,
      work_inbox: { handle: "jane", notices: [], unread: 0, primary: 0 },
    };
    return Promise.resolve(answers[what] ?? {});
  }) as typeof socket.query;
  const view = render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>{children}</Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return { ...view, store };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}

// AN UNSET FIELD IS THE ENGINE'S DEFAULT, never the sheet's guess of it: no
// type is the catalogue's `task`, no status is `todo`, no assignee is the
// project's own routing. Sending the sheet's idea of each would file the same
// task today and a different one the day a company changes its own.
test("only what the person set is sent", () => {
  const bare = createArgs({
    project: "ENG",
    title: "  Fix the race  ",
    body: " ",
    type: "",
    status: "",
    assignee: "",
    assigneeText: "",
    priority: "none",
    due: "",
    labels: [],
    ask: "",
  });
  expect(bare).toEqual({ title: "Fix the race", project: "ENG" });

  const whole = createArgs({
    project: "ENG",
    title: "Fix the race",
    body: "Repro inside.",
    type: "bug",
    status: "in_progress",
    assignee: "swe",
    assigneeText: "Agent SWE",
    priority: "high",
    due: "2031-05-01",
    labels: ["api"],
    ask: "swe",
  });
  expect(whole).toEqual({
    title: "Fix the race",
    project: "ENG",
    body: "Repro inside.",
    type: "bug",
    status: "in_progress",
    assignee: "swe",
    priority: "high",
    due: "2031-05-01",
    labels: ["api"],
    ask: "swe",
  });
});

// A LANE'S `+` FILES INTO THAT LANE: it presets exactly the value that puts a
// task there, and a lane whose value the sheet cannot hold as one takes no `+`
// (`undefined`) — a `+` on it filed the task into some other lane.
test("a board lane presets what files into it, or takes no +", () => {
  expect(presetForLane("status", "in_review")).toEqual({ status: "in_review" });
  // A GROUP'S FIRST STATUS, which is any of the group's and so the lane.
  expect(presetForLane("status_group", "not_started")).toEqual({ status: "todo" });
  expect(presetForLane("status_group", "active")).toEqual({ status: "in_progress" });
  expect(presetForLane("status_group", "done")).toEqual({ status: "done" });
  // NOTHING IS FILED AS ALREADY ABANDONED: the closed group, and the
  // Cancelled and Closed statuses, take no `+`. Done keeps its.
  expect(presetForLane("status_group", "closed")).toBeUndefined();
  expect(presetForLane("status", "cancelled")).toBeUndefined();
  expect(presetForLane("status", "closed")).toBeUndefined();
  expect(presetForLane("status", "done")).toEqual({ status: "done" });
  expect(presetForLane("type", "bug")).toEqual({ type: "bug" });
  expect(presetForLane("assignee", "")).toEqual({ assignee: "" });
  expect(presetForLane("assignee", "swe")).toEqual({ assignee: "swe" });
  expect(presetForLane("priority", "urgent")).toEqual({ priority: "urgent" });
  expect(presetForLane("project", "OPS")).toEqual({ project: "OPS" });
  expect(presetForLane("tag", "api")).toEqual({ labels: ["api"] });
  expect(presetForLane("tag", "")).toEqual({});
  // A DATE BAND IS A SPAN, and the undated lane is the one a create lands in.
  expect(presetForLane("due:bucket", "this_week")).toBeUndefined();
  expect(presetForLane("due:bucket", "today")).toBeUndefined();
  expect(presetForLane("due:bucket", "")).toEqual({});
  // THE SHEET HAS NO UNIT: a task is filed into its project's team.
  expect(presetForLane("unit", "core")).toBeUndefined();
  expect(presetForLane("parent", "ENG-1")).toBeUndefined();
});

// A TYPE LANE'S `+` FILES THE TYPE, and a label lane's the label — through the
// one record, each checked against the project's own words.
test("a type or a label from a lane reaches the wire", async () => {
  mount(
    <NewTaskSheet preset={{ project: "ENG", type: "bug", labels: ["api"] }} onClose={() => {}} />,
  );
  await settle();
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Flaky boot" } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Create task" }));
  });
  await settle();
  expect(posted).toEqual([
    {
      tool: "create_work_item",
      args: { title: "Flaky boot", project: "ENG", type: "bug", labels: ["api"] },
    },
  ]);
});

// A FIELD'S HELP LINE IS READ WITH ITS CONTROL. The assignee's and the labels'
// were drawn beside controls that did not name them, so where an empty
// Assignee sends the task, and which labels a project takes, were never read
// to anybody who could not see them.
test("the assignee's and the labels' help lines describe their controls", async () => {
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  const describedBy = (el: Element) =>
    (el.getAttribute("aria-describedby") ?? "")
      .split(/\s+/)
      .map((id) => document.getElementById(id)?.textContent ?? "")
      .join(" ");
  expect(describedBy(screen.getByPlaceholderText("Type a name or a handle"))).toContain(
    "Left empty",
  );
  const labels = document.getElementById("new-task-labels")!;
  expect(describedBy(labels.matches("input") ? labels : labels.querySelector("input")!)).toContain(
    "The project's own labels",
  );
});

test("the project is the door's, else the person's own, else the first", () => {
  const listed = [row("ENG", "Core"), row("OPS", "Ops")];
  expect(startingProject({ project: "ENG" }, "OPS", listed)).toBe("ENG");
  expect(startingProject({}, "OPS", listed)).toBe("OPS");
  expect(startingProject({ project: "GONE" }, "", listed)).toBe("ENG");
  // Before the list answers, the door's key is trusted as given.
  expect(startingProject({ project: "ENG" }, "", [])).toBe("ENG");
});

// A SEAT'S MESSAGE FILES ON THAT SEAT'S BOARD. The Agent CEO's ask opened on
// ENG — the first project — while the CEO's unit files its work under LEAD.
test("a door naming only a seat opens on the project of that seat's own unit", () => {
  const index = indexOrg(CHART_ORG);
  const filed = (key: string, unit: string): WorkProjectRow =>
    ({ ...row(key, unit), unit: { name: unit, resolved: true } }) as WorkProjectRow;
  const listed = [filed("ENG", "Core"), filed("LEAD", "Executives"), filed("PROD", "Product")];
  expect(startingProject({ assignee: "ceo", ask: "ceo" }, "ENG", listed, index)).toBe("LEAD");
  // The nearest unit ABOVE the seat's own, when its own files nothing.
  expect(startingProject({ assignee: "devrel" }, "ENG", listed, index)).toBe("PROD");
  // A seat above every unit, or none of whose units files work: the person's own.
  expect(startingProject({ assignee: "jane" }, "PROD", listed, index)).toBe("PROD");
  // The door's own project still wins.
  expect(startingProject({ project: "ENG", assignee: "ceo" }, "", listed, index)).toBe("ENG");
});

// ONE RECORD CARRIES THE WHOLE TASK, the lane's status included — and
// `applied` opens the task by the key the ENGINE minted.
test("a lane's status reaches the wire and applied opens the new task", async () => {
  let closed = 0;
  mount(
    <NewTaskSheet preset={{ project: "ENG", status: "in_progress" }} onClose={() => closed++} />,
  );
  await settle();
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Retry PXE boot" } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Create task" }));
  });
  await settle();
  expect(posted).toEqual([
    {
      tool: "create_work_item",
      args: { title: "Retry PXE boot", project: "ENG", status: "in_progress" },
    },
  ]);
  expect(closed).toBe(1);
  expect(location.hash).toBe("#/work/ENG-9");
});

// A NEW TASK OPENS BY THE ADDRESS ITS RECEIPT NAMES. A counter restored beside
// work minted after it hands a new task a key an older one claimed first, and
// that key opens the OLDER task: the engine names the new one by its id there
// (`item`), with the key beside it, and the sheet follows the address.
test("a create whose key another task holds opens the new task by its id", async () => {
  reply = {
    status: 200,
    body: {
      tool: "create_work_item",
      outcome: "applied",
      position: "CREWLET_TRACKER_LOG@1:9",
      receipt: { item: "t-new", key: "ENG-9", key_collision: true },
    },
  };
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Retry PXE boot" } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Create task" }));
  });
  await settle();
  expect(location.hash).toBe("#/work/t-new");
});

// A REFUSAL NAMES THE FIELD, in the engine's own sentence, and the sheet stays
// open with everything the person typed — the one place they can fix it.
test("a refusal is said in the sheet and the sheet stays open", async () => {
  reply = {
    status: 422,
    body: {
      error: "invalid",
      tool: "create_work_item",
      detail: "`labels`: ENG declares no label “wontfix”.",
    },
  };
  let closed = 0;
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => closed++} />);
  await settle();
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Won't fix" } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Create task" }));
  });
  await settle();
  // THE ARGUMENT IS DRAWN AS A VALUE, not wrapped in the backticks the
  // engine marks it with.
  const said = screen.getAllByRole("alert").find((a) => a.textContent?.includes("wontfix"));
  expect(said?.querySelector("code")?.textContent).toBe("labels");
  expect(said?.textContent).not.toContain("`");
  expect(closed).toBe(0);
  expect((screen.getByLabelText("Title") as HTMLInputElement).value).toBe("Won't fix");
  expect(location.hash).not.toContain("ENG-");
});

// THE WRITE CONTROL IS NEVER HIDDEN: for every reader who cannot file, Create
// is drawn, disabled, with the sentence that says what would let them — and a
// press sends nothing.
test.each([
  {
    who: "anonymous",
    viewer: { login: "", grants: [], handle: "", owner: "", name: "", acts: [] },
    reason: WRITE_REASONS.anonymous,
  },
  // UNBOUND IS NOT A BLOCK: an unbound reader files under their own login, so
  // what holds them is what holds anybody — the tool the engine will not make.
  {
    who: "unbound and not served the create",
    viewer: { login: "ci.release", grants: [], handle: "", owner: "ci.release", acts: [] },
    reason: WRITE_REASONS.not_served,
  },
  {
    who: "not served the create",
    viewer: { ...JANE, acts: ["update_work_item"] },
    reason: WRITE_REASONS.not_served,
  },
])("a reader who is $who sees Create disabled with the reason", async ({ viewer, reason }) => {
  mount(<NewTaskSheet preset={{}} onClose={() => {}} />, viewer);
  await settle();
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Something" } });
  const create = screen.getByRole("button", { name: "Create task" });
  expect(create.getAttribute("aria-disabled")).toBe("true");
  const described = (create.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
  expect(described).toContain(reason);
  // AND IT IS WRITTEN WHERE A SIGHTED READER LOOKS: the kit reads a held
  // button's reason to a screen reader only.
  expect(document.querySelector(".new-task-hold")?.textContent).toBe(reason);
  fireEvent.click(create);
  expect(posted).toEqual([]);
});

// UNHELD, THE FOOT SAYS WHO IT IS FILED AS — beside the press that files it,
// rather than as a second line in a head sized for a title alone.
test("a draft that can be filed says who files it, beside Create", async () => {
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Retry PXE boot" } });
  expect(document.querySelector(".new-task-hold")).toBeNull();
  const foot = screen
    .getByRole("button", { name: "Create task" })
    .closest(".crewlet-modal__footer");
  expect(foot?.textContent).toContain("Filed as Jane Founder");
  // THE HEAD IS THE TITLE ALONE.
  const dialog = screen.getByRole("dialog", { name: "New task" });
  expect(dialog.querySelector(".crewlet-modal__subtitle")).toBeNull();
});

// A TEXT IS COUNTED IN THE ENGINE'S UNIT. The engine refuses a title past
// `MaxTitle` BYTES rather than cutting it; counting characters would let a
// non-Latin title through that the engine then refuses after the press.
test("a text is measured in bytes against the engine's cap, and counts down near it", () => {
  expect(textBudget("x".repeat(TASK_TITLE_MAX_BYTES), TASK_TITLE_MAX_BYTES, "a title")).toEqual({
    bytes: TASK_TITLE_MAX_BYTES,
    limit: TASK_TITLE_MAX_BYTES,
    over: false,
    line: `${TASK_TITLE_MAX_BYTES} of ${TASK_TITLE_MAX_BYTES} bytes`,
  });
  // 86 characters, 258 bytes: over, though far short of 256 characters.
  const wide = textBudget("日".repeat(86), TASK_TITLE_MAX_BYTES, "a title");
  expect(wide.over).toBe(true);
  expect(wide.line).toContain("258 bytes — a title holds at most 256");
  // Far from the cap there is nothing to count.
  expect(textBudget("Fix the race", TASK_TITLE_MAX_BYTES, "a title").line).toBeUndefined();
  // WHAT IS SENT is what is measured: the trimmed title.
  expect(textBudget(`  ${"x".repeat(10)}  `, TASK_TITLE_MAX_BYTES, "a title").bytes).toBe(10);
});

test("the hold names the first field in the form that is holding it", () => {
  const draft = {
    project: "ENG",
    title: "ok",
    body: "",
    type: "",
    status: "",
    assignee: "",
    assigneeText: "",
    priority: "",
    due: "",
    labels: [],
    ask: "",
  };
  expect(blockedBy(draft)).toBeUndefined();
  expect(blockedBy({ ...draft, project: "" })?.field).toBe("project");
  expect(blockedBy({ ...draft, title: "  " })?.field).toBe("title");
  expect(blockedBy({ ...draft, title: "x".repeat(TASK_TITLE_MAX_BYTES + 1) })).toEqual({
    field: "title",
    reason: `Shorten the title: it is ${TASK_TITLE_MAX_BYTES + 1} bytes and a title holds at most ${TASK_TITLE_MAX_BYTES}.`,
  });
  expect(blockedBy({ ...draft, body: "b".repeat(TASK_BODY_MAX_BYTES + 1) })?.field).toBe("body");
  // WORDS NO SEAT WAS TAKEN FOR hold it, quoted back — and a chosen seat's
  // name, or nothing, does not.
  expect(blockedBy({ ...draft, assigneeText: "Agent SWE" })).toEqual({
    field: "assignee",
    reason: "Choose who “Agent SWE” is from the Assignee list, or clear it.",
  });
  expect(blockedBy({ ...draft, assignee: "swe", assigneeText: "Agent SWE" })).toBeUndefined();
  expect(blockedBy({ ...draft, assigneeText: "   " })).toBeUndefined();
  // A PASTED PARAGRAPH IS NOT THE SENTENCE: the quote stops at a line's worth.
  expect(blockedBy({ ...draft, assigneeText: "x".repeat(200) })?.reason).toBe(
    `Choose who “${"x".repeat(39)}…” is from the Assignee list, or clear it.`,
  );
});

// A TITLE THE ENGINE WOULD REFUSE IS REFUSED ON ITS FIELD, before the press:
// marked invalid, saying by how much, with Create held and the reason written
// where a sighted reader can see it — and nothing is sent.
test("a title past the cap is marked on its field and Create is held, visibly", async () => {
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  const title = screen.getByLabelText("Title") as HTMLInputElement;
  fireEvent.change(title, { target: { value: "t".repeat(TASK_TITLE_MAX_BYTES + 344) } });
  expect(title.getAttribute("aria-invalid")).toBe("true");
  const description = (title.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
  expect(description).toContain("600 bytes — a title holds at most 256");
  const create = screen.getByRole("button", { name: "Create task" });
  expect(create.getAttribute("aria-disabled")).toBe("true");
  // THE REASON IS ON THE PAGE, not only in the button's hidden description.
  const hold = document.querySelector(".new-task-hold");
  expect(hold?.textContent).toBe(
    "Shorten the title: it is 600 bytes and a title holds at most 256.",
  );
  fireEvent.click(create);
  expect(posted).toEqual([]);
});

test("an empty title says so beside Create, and Enter takes the reader to it", async () => {
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  expect(document.querySelector(".new-task-hold")?.textContent).toBe(
    "Give the task a title — one line saying what the work is.",
  );
  const title = screen.getByLabelText("Title");
  // ENTER IN ANOTHER SINGLE-LINE FIELD — the due date — is the press.
  const due = screen.getByLabelText(/Due/);
  due.focus();
  fireEvent.keyDown(due, { key: "Enter" });
  expect(document.activeElement).toBe(title);
  expect(posted).toEqual([]);
});

// ENTER FILES THE TASK. The sheet's form has no submit button — Create is a
// write control — so a browser's implicit submission never ran and Enter did
// nothing at all, while a suite firing `submit` on the form passed. The key is
// the sheet's own, in a single-line field and nowhere else: a description
// takes its newline.
test("Enter in a field files the task, and in the description is a newline", async () => {
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  const title = screen.getByLabelText("Title");
  fireEvent.change(title, { target: { value: "Retry PXE boot" } });
  fireEvent.keyDown(screen.getByLabelText(/Description/), { key: "Enter" });
  await settle();
  expect(posted).toEqual([]);
  await act(async () => {
    fireEvent.keyDown(title, { key: "Enter" });
  });
  await settle();
  expect(posted).toEqual([
    { tool: "create_work_item", args: { title: "Retry PXE boot", project: "ENG" } },
  ]);
});

// A LANE GROUPED BY HOLDER hands the sheet a HANDLE, possibly before the chart
// has loaded; the field shows the seat's name once it has, and never a slug
// left behind.
test("a preset holder reads as their name once the chart arrives", async () => {
  const { store } = mount(
    <NewTaskSheet preset={{ project: "ENG", assignee: "swe" }} onClose={() => {}} />,
    JANE,
    { org: false },
  );
  await settle();
  const field = screen.getByPlaceholderText("Type a name or a handle") as HTMLInputElement;
  expect(field.value).toBe("swe");
  await act(async () => {
    store.applyOrg(ORG as never);
  });
  await settle();
  expect(field.value).toBe("Agent SWE");
});

// A NAME THAT MATCHES NOBODY IS SAID, not a list that closes on the reader.
test("the assignee list says when no seat matches", async () => {
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  const field = screen.getByPlaceholderText("Type a name or a handle");
  fireEvent.change(field, { target: { value: "zzzz" } });
  await settle();
  expect(document.querySelector(".crewlet-listbox__empty")?.textContent).toBe(
    "No seat is called “zzzz”",
  );
  // AND THE CLEAR CONTROL SAYS WHAT IT CLEARS.
  fireEvent.change(field, { target: { value: "Agent" } });
  await settle();
  // THE LIST IS NAMED FOR THE FIELD IT COMPLETES.
  const list = screen.getByRole("listbox", { name: "Assignee suggestions" });
  fireEvent.mouseDown(within(list).getByRole("option", { name: /Agent SWE/ }));
  await settle();
  expect(screen.getByRole("button", { name: "Clear assignee" })).toBeTruthy();
});

// WORDS IN THE ASSIGNEE FIELD ARE NOT A SEAT, and they are never dropped: a
// name typed and not taken from the list held nothing, so the task went to
// the project's routing with the name still showing above "Left empty…". It
// holds Create now, says why beside it, and Enter takes the reader to the list
// — where taking the seat sends it.
test("an assignee typed and not chosen holds Create until a seat is taken", async () => {
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Fix it" } });
  const field = screen.getByPlaceholderText("Type a name or a handle") as HTMLInputElement;
  fireEvent.change(field, { target: { value: "Agent SWE" } });
  await settle();
  // THE FIELD SAYS IT IS NOT CHOSEN — never "Left empty" under words.
  const described = (field.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
  expect(described).toContain("Not chosen yet");
  expect(described).not.toContain("Left empty");
  // CREATE IS HELD, visibly, and a press sends nothing.
  expect(document.querySelector(".new-task-hold")?.textContent).toBe(
    "Choose who “Agent SWE” is from the Assignee list, or clear it.",
  );
  const create = screen.getByRole("button", { name: "Create task" });
  expect(create.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(create);
  expect(posted).toEqual([]);
  // ENTER WHILE HELD — here in the title — opens the list on the field
  // holding it.
  fireEvent.keyDown(field, { key: "Escape" });
  const title = screen.getByLabelText("Title");
  title.focus();
  await act(async () => {
    fireEvent.keyDown(title, { key: "Enter" });
  });
  await settle();
  expect(document.activeElement).toBe(field);
  const list = screen.getByRole("listbox", { name: "Assignee suggestions" });
  expect(within(list).getAllByRole("option")[0]?.textContent).toContain("Agent SWE");
  // ENTER ON THE OPEN LIST TAKES THE SEAT, and files nothing: the list took
  // the key.
  await act(async () => {
    fireEvent.keyDown(field, { key: "Enter" });
  });
  await settle();
  expect(posted).toEqual([]);
  expect(document.querySelector(".new-task-hold")).toBeNull();
  // AND THE NEXT ENTER FILES IT, to the seat taken.
  await act(async () => {
    fireEvent.keyDown(field, { key: "Enter" });
  });
  await settle();
  expect(posted).toEqual([
    { tool: "create_work_item", args: { title: "Fix it", project: "ENG", assignee: "swe" } },
  ]);
});

// A KEY FROM A POPUP IS NOT THE FORM'S. The project picker's search box is
// portalled out of the sheet, and its Enter bubbles to the sheet through
// React's tree: a search that matched no project would otherwise file the task.
test("Enter in the project picker's search files nothing", async () => {
  const many = Array.from({ length: 10 }, (_, i) => row(`P${i}`, `Project ${i}`));
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />, JANE, {
    projects: [row("ENG", "Core platform"), ...many],
  });
  await settle();
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Fix it" } });
  fireEvent.click(document.getElementById("new-task-project")!);
  await settle();
  const search = screen.getByPlaceholderText("Find a project");
  fireEvent.change(search, { target: { value: "zzzz" } });
  await settle();
  await act(async () => {
    fireEvent.keyDown(search, { key: "Enter" });
  });
  await settle();
  expect(posted).toEqual([]);
});

// ENTER THAT A LIST TOOK IS NOT A PRESS. An empty Assignee holds nothing, so
// Enter on the list the arrow opened there would otherwise take the seat AND
// file the task in the same keystroke — without the seat just taken.
test("Enter taking a seat from the list files nothing", async () => {
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Fix it" } });
  const field = screen.getByPlaceholderText("Type a name or a handle");
  field.focus();
  fireEvent.keyDown(field, { key: "ArrowDown" });
  await settle();
  expect(screen.getByRole("listbox", { name: "Assignee suggestions" })).toBeTruthy();
  await act(async () => {
    fireEvent.keyDown(field, { key: "Enter" });
  });
  await settle();
  expect(posted).toEqual([]);
  expect((field as HTMLInputElement).value).not.toBe("");
});

// AND CLEARING THE WORDS IS THE OTHER WAY OUT: the hold lifts, and the task
// goes where the project sends unassigned work, which the field says again.
test("clearing typed words lifts the hold and the field says where it lands", async () => {
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Fix it" } });
  const field = screen.getByPlaceholderText("Type a name or a handle") as HTMLInputElement;
  fireEvent.change(field, { target: { value: "zzzz" } });
  await settle();
  expect(document.querySelector(".new-task-hold")).not.toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Clear assignee" }));
  await settle();
  expect(field.value).toBe("");
  expect(document.activeElement).toBe(field);
  expect(document.querySelector(".new-task-hold")).toBeNull();
  const described = (field.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
  expect(described).toContain("Left empty");
});

// THE ENGINE'S MATCH IS READ BY ITS NAME. The kit's hint never shrinks and its
// label does, so the tier written into the hint ("the engine's match: part of
// the name matches") drew the seat as "Age…" — or as nothing — in the one case
// the row is there for. The hint is as short as every other row's; the tier
// is in the row's accessible name, and on the help line once the seat is
// taken, and only for the seat it was said about.
test("the engine's match leads the list by its name, and says why once taken", async () => {
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />, JANE, {
    colleague: {
      match: { handle: "swe", method: "substring", why: "part of the name matches" },
      candidates: [],
    },
  });
  await settle();
  const field = screen.getByPlaceholderText("Type a name or a handle") as HTMLInputElement;
  fireEvent.change(field, { target: { value: "sw" } });
  await settle();
  const list = screen.getByRole("listbox", { name: "Assignee suggestions" });
  const first = within(list).getAllByRole("option")[0]!;
  expect(first.querySelector(".crewlet-combobox__label")?.textContent).toBe("Agent SWE");
  const hint = first.querySelector(".crewlet-listbox__hint")!;
  // WHAT IS DRAWN is as short as a local row's "@swe · agent"…
  const drawn = [...hint.childNodes]
    .filter((n) => !(n instanceof HTMLElement && n.classList.contains("sr-only")))
    .map((n) => n.textContent)
    .join("");
  expect(drawn).toBe("@swe · best match");
  // …and the tier is still read with the row.
  expect(first.textContent).toContain("the engine's match: part of the name matches");
  fireEvent.mouseDown(first);
  await settle();
  expect(field.value).toBe("Agent SWE");
  const described = () =>
    (field.getAttribute("aria-describedby") ?? "")
      .split(/\s+/)
      .map((id) => document.getElementById(id)?.textContent ?? "")
      .join(" ");
  expect(described()).toBe(
    "Agent SWE is woken with it and follows it. The engine's match for “sw”: part of the name matches.",
  );
  // A SEAT TAKEN FROM THE CHART'S OWN ROWS is told no engine's tier.
  fireEvent.change(field, { target: { value: "Jane" } });
  await settle();
  fireEvent.mouseDown(
    within(screen.getByRole("listbox", { name: "Assignee suggestions" })).getByRole("option", {
      name: /Jane Founder/,
    }),
  );
  await settle();
  expect(described()).toBe("Jane Founder is woken with it and follows it.");
});

// THE CLEAR CONTROL IS INSIDE THE FIELD, so the completion list — anchored to
// the field's own box — spans everything it hangs from. Beside the field it
// was a column the list did not cover, and the help line's last words showed
// next to the open list.
test("the assignee's clear control is inside the field the list spans", async () => {
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  const field = screen.getByPlaceholderText("Type a name or a handle");
  fireEvent.change(field, { target: { value: "Agent" } });
  await settle();
  const clear = screen.getByRole("button", { name: "Clear assignee" });
  const box = field.closest(".crewlet-combobox")!;
  expect(box.contains(clear)).toBe(true);
  expect(box.contains(screen.getByRole("listbox", { name: "Assignee suggestions" }))).toBe(true);
});

// WHERE AN EMPTY ASSIGNEE SENDS THE TASK IS THE ENGINE'S ANSWER, said before
// the press. A default assignee the chart no longer holds is not applied —
// the engine files the task to nobody and puts it in triage — so the sheet
// promising that seat said the one thing the create would contradict.
test("an empty assignee lands where the engine will file it", async () => {
  const helpOfAssignee = () =>
    (screen.getByPlaceholderText("Type a name or a handle").getAttribute("aria-describedby") ?? "")
      .split(/\s+/)
      .map((id) => document.getElementById(id)?.textContent ?? "")
      .join(" ");

  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />, JANE, {
    project: { ...ENG, default_assignee: "swe" } as WorkProjectDetail,
  });
  await settle();
  expect(helpOfAssignee()).toContain("it goes to Agent SWE, ENG's default assignee");
  cleanup();

  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />, JANE, {
    project: { ...ENG, default_assignee: "ana" } as WorkProjectDetail,
  });
  await settle();
  expect(helpOfAssignee()).toContain("it lands in triage");
  expect(helpOfAssignee()).toContain("ana, is no longer on the org chart");
  expect(helpOfAssignee()).not.toContain("it goes to");
});

// AND AN APPLIED CREATE THAT CAME BACK WITH A CAVEAT SAYS IT: "Filed" over a
// task the engine sent to triage instead of the default assignee is the
// opposite of what happened.
test("an applied create's warnings are said, not dropped", async () => {
  reply = {
    status: 200,
    body: {
      tool: "create_work_item",
      outcome: "applied",
      position: "CREWLET_TRACKER_LOG@1:9",
      receipt: {
        key: "ENG-9",
        warnings: ['ENG\'s default assignee "ana" is not a seat on the org chart any more.'],
      },
    },
  };
  mount(<NewTaskSheet preset={{ project: "ENG" }} onClose={() => {}} />);
  await settle();
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Triage me" } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Create task" }));
  });
  await settle();
  expect(
    (await screen.findAllByText(/is not a seat on the org chart any more/)).length,
  ).toBeGreaterThan(0);
});
