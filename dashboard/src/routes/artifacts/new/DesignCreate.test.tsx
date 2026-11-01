/**
 * A new design: a starting layout lays boards on the canvas, the panel follows
 * the selection, and Create sends the boards as the engine's design layout
 * starter and opens the design it made.
 */

import { TestClient } from "~/test/client.tsx";
import { AppAnnouncer } from "~/app/announcer.tsx";
import { installWindow } from "~/testing.tsx";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { DesignCreate } from "./DesignCreate.tsx";
import { draftField, openFrame, setDraft } from "../draftTesting.ts";

vi.mock("~/lib/artifacts/access.ts", () => ({
  useArtifactAccess: () => ({ can: true }),
}));
const fileArtifactTask = vi.fn();
const refusal = { current: null as { sentence: string } | null };
vi.mock("./fileTask.ts", async (original) => ({
  ...(await original<typeof import("./fileTask.ts")>()),
  useFileArtifactTask: () => ({
    fileArtifactTask,
    busy: false,
    refusal: refusal.current,
    held: null,
  }),
}));
vi.mock("./WhoBuildsIt.tsx", () => ({
  useWhoBuildsIt: () => ({
    builders: [{ seat: { handle: "ava", name: "Ava Lindqvist" }, suggested: true }],
    seat: "ava",
    setSeat: () => {},
    project: "DES",
    setProject: () => {},
    projects: [],
    projectsLoading: false,
  }),
  WhoBuildsIt: () => <p>Ava builds it</p>,
}));
vi.mock("~/lib/keyedWrite.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/keyedWrite.ts")>("~/lib/keyedWrite.ts");
  return { ...actual, useKeyedGesture: vi.fn() };
});

type Callback = (entries: { contentRect: { width: number; height: number } }[]) => void;
class FakeResizeObserver {
  static all: FakeResizeObserver[] = [];
  constructor(private callback: Callback) {
    FakeResizeObserver.all.push(this);
  }
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
  resize(width: number, height: number) {
    act(() => this.callback([{ contentRect: { width, height } }]));
  }
}

const ID = "0192f2c4-2222-7000-8000-000000000009";
const run = vi.fn();
const realResizeObserver = globalThis.ResizeObserver;

beforeEach(() => {
  FakeResizeObserver.all = [];
  globalThis.ResizeObserver = FakeResizeObserver as unknown as typeof ResizeObserver;
  run.mockReset();
  run.mockResolvedValue({ kind: "done", pending: false, body: { artifact: ID } });
  vi.mocked(useKeyedGesture).mockReturnValue({ run, busy: false, answer: null } as never);
  fileArtifactTask.mockReset();
  fileArtifactTask.mockResolvedValue({ item: "DES-1", key: "DES-1", linked: true });
  refusal.current = null;
});

let win: ReturnType<typeof installWindow>;
beforeEach(() => {
  // A LAPTOP'S WIDTH, where the hand-off stands beside the page as a column
  // and so arrives open.
  win = installWindow(1440);
});
afterEach(() => {
  win.restore();
  cleanup();
  globalThis.ResizeObserver = realResizeObserver;
  location.hash = "";
});

function mount(hash = "#/artifacts/new/design") {
  location.hash = hash;
  return render(
    <TestClient>
      <Router>
        <LayerHost>
          <ToastProvider>
            <AppAnnouncer />
            <DesignCreate />
          </ToastProvider>
        </LayerHost>
      </Router>
    </TestClient>,
  );
}

const boards = () =>
  within(screen.getByRole("navigation", { name: "Boards" }))
    .getAllByRole("button")
    .filter((b) => b.getAttribute("aria-label") !== "Add board")
    .map((o) => o.textContent);

function pickLanding() {
  fireEvent.click(screen.getByRole("button", { name: "Landing page" }));
  FakeResizeObserver.all.forEach((o) => o.resize(1000, 700));
}

test("the page opens on the starting layouts, the first one focused", () => {
  mount();
  const names = [
    "Landing page",
    "App screens",
    "Responsive screen",
    "Presentation",
    "Blank canvas",
  ];
  for (const name of names) expect(screen.getByRole("button", { name })).toBeTruthy();
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Landing page" }));
  expect(
    screen
      .getByRole("button", { name: "Create and ask Ava Lindqvist" })
      .getAttribute("aria-disabled"),
  ).toBe("true");
});

test("a layout lays its boards on the canvas and the list beside it", () => {
  mount();
  pickLanding();
  expect(boards()).toEqual(["Home", "Pricing", "Home on phone"]);
  expect(screen.getByRole("heading", { name: "Design" })).toBeTruthy();
});

test("the panel follows the selection: a board's frame preset sets its size", () => {
  mount();
  pickLanding();
  fireEvent.click(within(screen.getByRole("navigation", { name: "Boards" })).getByText("Pricing"));
  expect(screen.getByRole("heading", { name: "Board 2 of 3" })).toBeTruthy();
  fireEvent.change(screen.getByRole("textbox", { name: /^Name/ }), { target: { value: "Plans" } });
  expect(boards()).toEqual(["Home", "Plans", "Home on phone"]);
  fireEvent.change(screen.getByRole("textbox", { name: "Width" }), { target: { value: "1280" } });
  fireEvent.change(screen.getByRole("textbox", { name: "Height" }), { target: { value: "832" } });
  expect(screen.getByRole("combobox", { name: /Frame/ }).textContent).toContain("Laptop");
  fireEvent.click(screen.getByRole("button", { name: "Delete board" }));
  expect(boards()).toEqual(["Home", "Home on phone"]);
  expect(screen.getByRole("heading", { name: "Design" })).toBeTruthy();
});

test("Create is held until the design is named, then makes it, asks the agent and opens the design", async () => {
  mount("#/artifacts/new/design?item=ENG-7");
  pickLanding();
  const create = screen.getByRole("button", { name: "Create and ask Ava Lindqvist" });
  expect(create.getAttribute("aria-disabled")).toBe("true");
  fireEvent.change(screen.getByRole("textbox", { name: /^Design name/ }), {
    target: { value: " Launch " },
  });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Create and ask Ava Lindqvist" }));
  });
  expect(run).toHaveBeenCalledOnce();
  const req = run.mock.calls[0]![0];
  expect(req).toMatchObject({ method: "POST", path: "/artifacts" });
  expect(req.body).toMatchObject({
    kind: "design",
    title: "Launch",
    starter: true,
    work_item: "ENG-7",
  });
  expect(req.body.layout.boards.map((b: { title: string }) => b.title)).toEqual([
    "Home",
    "Pricing",
    "Home on phone",
  ]);
  // THE AGENT MAY PUBLISH OVER THE PLACEHOLDERS, and its task names the design.
  expect(req.body.shares).toEqual([{ audience: { kind: "seat", id: "ava" }, role: "editor" }]);
  expect(fileArtifactTask).toHaveBeenCalledWith(
    expect.objectContaining({
      kind: "design",
      title: "Launch",
      seat: "ava",
      project: "DES",
      artifact: ID,
    }),
  );
  expect(fileArtifactTask.mock.calls[0]![0].brief).toContain("1. Home (1440 by 1024, Desktop)");
  expect(location.hash).toBe(`#/artifacts/${ID}`);
});

test("Command Enter in the hand-off creates and asks", async () => {
  mount();
  pickLanding();
  fireEvent.change(screen.getByRole("textbox", { name: /^Design name/ }), {
    target: { value: "Launch" },
  });
  await act(async () => {
    fireEvent.keyDown(screen.getByRole("textbox", { name: "What is the design for?" }), {
      key: "Enter",
      metaKey: true,
    });
  });
  expect(run).toHaveBeenCalledOnce();
});

// THE HAND-OFF IS THE PAGE'S ONE WAY TO MAKE A DESIGN: it arrives open, and
// closed, the bar's Ask an agent is the primary that opens it again, as is
// Command Enter from the canvas.
test("closed, Ask an agent is the bar's primary, and Command Enter opens it again", () => {
  mount();
  pickLanding();
  const panel = screen.getByRole("complementary", { name: "Ask an agent" });
  expect(within(panel).getByText("3 boards")).toBeTruthy();
  fireEvent.click(within(panel).getByRole("button", { name: "Close ask an agent" }));
  expect(screen.queryByRole("complementary", { name: "Ask an agent" })).toBeNull();
  const tool = screen.getAllByRole("button", { name: "Ask an agent" })[0]!;
  expect(tool.className).toContain("primary");
  fireEvent.keyDown(document.body, { key: "Enter", metaKey: true });
  expect(screen.getByRole("complementary", { name: "Ask an agent" })).toBeTruthy();
  expect(run).not.toHaveBeenCalled();
});

test("Cancel asks before discarding what was laid out", () => {
  mount();
  pickLanding();
  fireEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
  expect(screen.getByRole("alertdialog", { name: "Discard this design?" })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
  expect(location.hash).toBe("#/artifacts/new/design");
});

test("a reference that is not a web address is refused on the field", () => {
  mount();
  pickLanding();
  const field = screen.getByRole("textbox", { name: /new link/i });
  fireEvent.change(field, { target: { value: "javascript:alert(1)" } });
  fireEvent.keyDown(field, { key: "Enter" });
  expect(screen.getByText("A reference is a web address starting with https://.")).toBeTruthy();
});

test("a task refused after the design was made keeps the page, and the next press files only the task", async () => {
  fileArtifactTask.mockResolvedValueOnce(null);
  mount();
  pickLanding();
  fireEvent.change(screen.getByRole("textbox", { name: /^Design name/ }), {
    target: { value: "Launch" },
  });
  refusal.current = { sentence: "The project DES does not take new tasks." };
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Create and ask Ava Lindqvist" }));
  });
  expect(location.hash).toBe("#/artifacts/new/design");
  expect(screen.getByText("The project DES does not take new tasks.")).toBeTruthy();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Ask Ava Lindqvist" }));
  });
  expect(run).toHaveBeenCalledOnce();
  expect(fileArtifactTask).toHaveBeenCalledTimes(2);
  expect(location.hash).toBe(`#/artifacts/${ID}`);
});

// NARROWER, THE PANEL WOULD BE A DRAWER over the layouts, the page's first
// step, so it waits for the press there.
test("below a laptop's width the hand-off waits for the press rather than covering the layouts", () => {
  win.set(1024);
  mount();
  expect(screen.queryByRole("complementary", { name: "Ask an agent" })).toBeNull();
  fireEvent.click(screen.getAllByRole("button", { name: "Ask an agent" })[0]!);
  expect(screen.getByRole("complementary", { name: "Ask an agent" })).toBeTruthy();
});

// ADD BOARD IS ALWAYS IN REACH: beside the list of boards, and beside the
// panel's heading for the widths where that list is folded away.
test("the design's panel carries Add board, for widths where the list of boards is folded away", () => {
  mount();
  pickLanding();
  const head = screen.getByRole("heading", { name: "Design" }).parentElement!;
  expect(within(head).getByRole("button", { name: "Add board" })).toBeTruthy();
});

// WHAT IS LAID OUT IS THE ASSISTANT'S TO READ, and a change it proposes picks a
// layout before any board is laid out, then names the boards and says what
// each is for, through the page's own rules; the canvas stays the person's.
test("the design being laid out is the assistant's to read, and a draft.set fills its boards", async () => {
  openFrame("artifacts-new");
  mount();
  expect(draftField("layout")).toMatchObject({ value: "", editable: true });
  expect(setDraft({ title: "Launch", layout: "landing" })).toMatchObject({ outcome: "applied" });
  FakeResizeObserver.all.forEach((o) => o.resize(1000, 700));
  expect(boards()).toEqual(["Home", "Pricing", "Home on phone"]);
  expect(draftField("boards.2.name")).toMatchObject({ value: "Pricing", editable: true });
  expect(
    setDraft({ "boards.2.name": "Plans", "boards.2.purpose": "The three plans side by side." }),
  ).toMatchObject({ outcome: "applied" });
  expect(boards()).toEqual(["Home", "Plans", "Home on phone"]);
  // A PICK OVER BOARDS ALREADY LAID OUT IS THE PERSON'S, and so is a board without a name.
  expect(setDraft({ layout: "app" })).toMatchObject({ outcome: "invalid" });
  expect(setDraft({ "boards.1.name": " " })).toMatchObject({
    outcome: "invalid",
    reason: "Give the board a name.",
  });
  expect(setDraft({ references: ["ftp://example.com"] })).toMatchObject({ outcome: "invalid" });
});
