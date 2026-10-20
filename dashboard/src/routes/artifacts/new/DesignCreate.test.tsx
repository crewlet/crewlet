/**
 * A new design: a starting layout lays boards on the canvas, the panel follows
 * the selection, and Create sends the boards as the engine's design layout
 * starter and opens the design it made.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { DesignCreate } from "./DesignCreate.tsx";

vi.mock("~/lib/artifacts/access.ts", () => ({
  useArtifactAccess: () => ({ can: true }),
}));
const fileArtifactTask = vi.fn();
const refusal = { current: null as { sentence: string } | null };
vi.mock("./fileTask.ts", () => ({
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

afterEach(() => {
  cleanup();
  globalThis.ResizeObserver = realResizeObserver;
  location.hash = "";
});

function mount(hash = "#/artifacts/new/design") {
  location.hash = hash;
  return render(
    <Router>
      <LayerHost>
        <ToastProvider>
          <DesignCreate />
        </ToastProvider>
      </LayerHost>
    </Router>,
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
  fireEvent.change(screen.getByRole("textbox", { name: /^Name/ }), {
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

test("Command Enter creates from anywhere on the page", async () => {
  mount();
  pickLanding();
  fireEvent.change(screen.getByRole("textbox", { name: /^Name/ }), { target: { value: "Launch" } });
  await act(async () => {
    fireEvent.keyDown(screen.getByRole("textbox", { name: /^Brief/ }), {
      key: "Enter",
      metaKey: true,
    });
  });
  expect(run).toHaveBeenCalledOnce();
});

test("Cancel asks before discarding what was laid out", () => {
  mount();
  pickLanding();
  fireEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
  expect(screen.getByRole("alertdialog", { name: "Discard this design?" })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Keep working" }));
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
  fireEvent.change(screen.getByRole("textbox", { name: /^Name/ }), { target: { value: "Launch" } });
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
