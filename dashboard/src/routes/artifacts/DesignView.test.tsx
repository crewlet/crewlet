/**
 * A design on screen: its boards framed one document each, and a person's
 * rearrangement saved as a version of `canvas.json` against its base, merged
 * onto a newer version when somebody published first. The frames themselves
 * are FrameView's to hold; here each is a stand-in that names its board.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import { Router } from "~/app/router.tsx";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import type { ArtifactDetail } from "~/protocol/index.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { DesignView, PRINT_TEARDOWN_MS } from "./DesignView.tsx";
import { context, draftField, openFrame, setDraft } from "./draftTesting.ts";

vi.mock("./FrameView.tsx", async () => {
  const { useEffect } = await import("react");
  return {
    // A stand-in that names its board, says hello at once, and hands its
    // print to the test.
    FrameView: ({
      document,
      title,
      print,
      onReady,
    }: {
      document?: string;
      title?: string;
      print?: () => Promise<boolean>;
      onReady?: () => void;
    }) => {
      useEffect(() => onReady?.(), [onReady]);
      return (
        <div data-board-frame={document}>
          {title}
          <button type="button" onClick={() => void print?.()}>
            {`Page print ${document}`}
          </button>
        </div>
      );
    },
  };
});
vi.mock("~/lib/artifacts/access.ts", () => ({
  useArtifactAccess: () => ({ can: true }),
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

const ID = "0192f2c4-2222-7000-8000-000000000002";

const canvas = (pricingY: number, heroX = 0) =>
  `${JSON.stringify(
    {
      v: 1,
      title: "Launch",
      boards: {
        "hero.html": { x: heroX, y: 0, w: 1440, h: 900, title: "Hero" },
        "pricing.html": { x: 0, y: pricingY, w: 1440, h: 900, title: "Pricing" },
      },
      order: ["hero.html", "pricing.html"],
    },
    null,
    2,
  )}\n`;

const SOURCES: Record<number, string> = { 3: canvas(1000), 4: canvas(2000) };

const detail = {
  id: ID,
  kind: "design",
  title: "Launch page",
  head_number: 3,
  role: "editor",
} as unknown as ArtifactDetail;

const run = vi.fn();
const realResizeObserver = globalThis.ResizeObserver;

beforeEach(() => {
  FakeResizeObserver.all = [];
  globalThis.ResizeObserver = FakeResizeObserver as unknown as typeof ResizeObserver;
  location.hash = `#/artifacts/${ID}`;
  run.mockReset();
  vi.mocked(useKeyedGesture).mockReturnValue({ run } as never);
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      const n = Number(/\/versions\/(\d+)\/source$/.exec(path)?.[1]);
      const text = SOURCES[n];
      return text === undefined
        ? new Response("{}", { status: 404, headers: { "Content-Type": "application/json" } })
        : new Response(
            JSON.stringify({ artifact: ID, kind: "design", version: n, path: "canvas.json", text }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          );
    }),
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  globalThis.ResizeObserver = realResizeObserver;
  location.hash = "";
});

async function mount(onPublished = vi.fn()) {
  render(
    <Router>
      <ToastProvider>
        <DesignView
          detail={detail}
          version={3}
          room={{} as never}
          presence={{ at: () => {} }}
          bridgeStores={new Set()}
          onPublished={onPublished}
        />
      </ToastProvider>
    </Router>,
  );
  await screen.findByRole("navigation", { name: "Boards" });
  FakeResizeObserver.all.at(-1)!.resize(1200, 800);
  return onPublished;
}

test("every board is listed, and each live board is a frame of its own document", async () => {
  await mount();
  expect(
    screen.getByRole("button", { name: "Hero, press Enter or double-click to open" }),
  ).toBeTruthy();
  expect(
    screen.getByRole("button", { name: "Pricing, press Enter or double-click to open" }),
  ).toBeTruthy();
  const frames = [...document.querySelectorAll("[data-board-frame]")].map((el) =>
    el.getAttribute("data-board-frame"),
  );
  expect(frames).toEqual(["hero.html", "pricing.html"]);
  expect(screen.getByText("Launch page, Hero")).toBeTruthy();
});

test("a rearrangement saves the canvas alone against the version it started from", async () => {
  const onPublished = await mount();
  fireEvent.click(screen.getByRole("button", { name: "Edit layout" }));
  fireEvent.keyDown(screen.getByRole("button", { name: "Move Hero" }), { key: "ArrowRight" });
  run.mockResolvedValueOnce({ kind: "done", pending: false, body: { number: 4 } });
  fireEvent.click(screen.getByRole("button", { name: "Save version" }));
  await waitFor(() => expect(onPublished).toHaveBeenCalledWith(4));
  const req = run.mock.calls[0]![0] as {
    method: string;
    path: string;
    headers: Record<string, string>;
    body: {
      mode: string;
      files: unknown[];
      edits: { path: string; old_str: string; new_str: string }[];
    };
  };
  expect(req.method).toBe("POST");
  expect(req.path).toBe(`/artifacts/${ID}/versions`);
  expect(req.headers["If-Match"]).toBe('"3"');
  expect(req.body.mode).toBe("patch");
  expect(req.body.files).toEqual([]);
  expect(req.body.edits).toEqual([
    { path: "canvas.json", old_str: SOURCES[3], new_str: canvas(1000, 8) },
  ]);
});

test("the Sticky tool places a note where the canvas is clicked, opens it for typing, and saves it", async () => {
  await mount();
  // A viewer has Move and Hand alone; editing adds the tools that place notes.
  expect(screen.queryByRole("button", { name: "Sticky note" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Edit layout" }));
  fireEvent.click(screen.getByRole("button", { name: "Sticky note" }));
  const viewport = document.querySelector(".crewlet-canvas__viewport")!;
  fireEvent.click(viewport, { clientX: 0, clientY: 0 });
  const field = screen.getByRole("textbox", { name: "Note text" });
  expect(field).toBe(document.activeElement);
  fireEvent.change(field, { target: { value: "Check the copy" } });
  fireEvent.blur(field);
  run.mockResolvedValueOnce({ kind: "done", pending: false, body: { number: 4 } });
  fireEvent.click(screen.getByRole("button", { name: "Save version" }));
  await waitFor(() => expect(run).toHaveBeenCalledTimes(1));
  const req = run.mock.calls[0]![0] as { body: { edits: { new_str: string }[] } };
  const saved = JSON.parse(req.body.edits[0]!.new_str) as {
    notes: { id: string; kind: string; x: number; y: number; text: string }[];
  };
  expect(saved.notes).toEqual([
    expect.objectContaining({ id: "note-1", kind: "sticky", text: "Check the copy" }),
  ]);
  // Placed at the canvas point under the click: the top left of the view.
  const layer = document.querySelector(".crewlet-canvas__world") as HTMLElement;
  const [, x, y, k] = /translate\((-?[\d.e+-]+)px, (-?[\d.e+-]+)px\) scale\(([\d.e+-]+)\)/
    .exec(layer.style.transform)!
    .map(Number);
  expect(saved.notes[0]!.x).toBe(Math.round(-x! / k!));
  expect(saved.notes[0]!.y).toBe(Math.round(-y! / k!));
});

test("a save refused because somebody published is merged onto their version and saved there", async () => {
  await mount();
  fireEvent.click(screen.getByRole("button", { name: "Edit layout" }));
  fireEvent.keyDown(screen.getByRole("button", { name: "Move Hero" }), { key: "ArrowRight" });
  run.mockResolvedValueOnce({
    kind: "refused",
    text: "The artifact moved.",
    error: { code: "version_moved", body: { live: { number: 4 } } },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save version" }));
  expect(await screen.findByText("Moved onto version 4")).toBeTruthy();

  run.mockResolvedValueOnce({ kind: "done", pending: false, body: { number: 5 } });
  fireEvent.click(screen.getByRole("button", { name: "Save version" }));
  await waitFor(() => expect(run).toHaveBeenCalledTimes(2));
  const second = run.mock.calls[1]![0] as {
    headers: Record<string, string>;
    body: { edits: { old_str: string; new_str: string }[] };
  };
  // BASED ON THEIR VERSION, carrying their move of the pricing board and the
  // person's move of the hero.
  expect(second.headers["If-Match"]).toBe('"4"');
  expect(second.body.edits[0]!.old_str).toBe(SOURCES[4]);
  expect(second.body.edits[0]!.new_str).toBe(canvas(2000, 8));
});

test("a board's print shows that board alone at its own size, prints, and goes after the print", async () => {
  const printed = vi.spyOn(window, "print").mockImplementation(() => {});
  // jsdom constructs no stylesheet a document adopts: a stand-in that keeps
  // the text the print view writes.
  const written = new WeakMap<object, string>();
  Object.defineProperty(document, "adoptedStyleSheets", {
    value: [],
    writable: true,
    configurable: true,
  });
  const realSheet = globalThis.CSSStyleSheet;
  globalThis.CSSStyleSheet = class {
    replaceSync(text: string) {
      written.set(this, text);
    }
  } as unknown as typeof CSSStyleSheet;
  await mount();
  fireEvent.click(screen.getAllByRole("button", { name: "Page print pricing.html" })[0]!);
  const view = document.querySelector(".design-print-view") as HTMLElement;
  expect(view?.dataset.board).toBe("pricing.html");
  expect(view.querySelectorAll("[data-board-frame]")).toHaveLength(1);
  expect((view.firstElementChild as HTMLElement).style.width).toBe("1440px");
  await waitFor(() => expect(printed).toHaveBeenCalledTimes(1));
  // THE PAGE SIZE IS THE BOARD'S, for as long as the print lasts.
  const sizes = () =>
    document.adoptedStyleSheets
      .map((sheet) => written.get(sheet) ?? "")
      .filter((t) => t.includes("@page"));
  expect(sizes().join()).toContain("1440px 900px");
  // THE PAGE SIZE GOES WITH THE VIEW, in the commit that removes it: read at
  // the instant the view leaves the document, not after whatever task runs
  // next, which may or may not have run an effect's cleanup by then.
  let atRemoval: string[] | undefined;
  const removal = new MutationObserver(() => {
    if (atRemoval === undefined && !document.querySelector(".design-print-view")) {
      atRemoval = sizes();
    }
  });
  removal.observe(document.body, { childList: true });
  // THE MOMENT IT OUTLIVES THE PRINT is the view's own timer, run on the
  // case's clock: not gone a millisecond early, and gone on time.
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  act(() => {
    window.dispatchEvent(new Event("afterprint"));
  });
  // NOT AT ONCE: the browser is still taking the board's frame into the print.
  act(() => {
    vi.advanceTimersByTime(PRINT_TEARDOWN_MS - 1);
  });
  expect(document.querySelector(".design-print-view")).not.toBeNull();
  act(() => {
    vi.advanceTimersByTime(1);
  });
  expect(document.querySelector(".design-print-view")).toBeNull();
  // The observer's record of the removal is delivered in a microtask after
  // it.
  await afterMicrotasks();
  removal.disconnect();
  expect(atRemoval).toEqual([]);
  printed.mockRestore();
  globalThis.CSSStyleSheet = realSheet;
});

test("a click on a listed board only selects it; Enter or a double-click opens it", async () => {
  await mount();
  const row = screen.getByRole("button", { name: "Pricing, press Enter or double-click to open" });
  fireEvent.click(row);
  expect(screen.queryByRole("dialog")).toBeNull();
  // Selected on the canvas too, and marked in the list.
  expect(
    screen.getByRole("region", { name: "Pricing, 1440 by 900" }).hasAttribute("data-selected"),
  ).toBe(true);
  expect(row.getAttribute("aria-current")).toBe("true");
  fireEvent.keyDown(row, { key: "Enter" });
  expect(await screen.findByRole("dialog")).toBeTruthy();
  fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  fireEvent.doubleClick(
    screen.getByRole("button", { name: "Hero, press Enter or double-click to open" }),
  );
  expect(await screen.findByRole("dialog")).toBeTruthy();
});

test("the board picked on the canvas is the one marked in the list", async () => {
  await mount();
  const hero = screen.getByRole("region", { name: "Hero, 1440 by 900" });
  fireEvent.click(hero.querySelector(".crewlet-canvas-board__shield")!);
  const row = (name: string) =>
    screen.getByRole("button", { name: `${name}, press Enter or double-click to open` });
  expect(row("Hero").getAttribute("aria-current")).toBe("true");
  expect(row("Pricing").hasAttribute("aria-current")).toBe(false);
  // A click on the field clears the selection, and the mark with it.
  fireEvent.click(document.querySelector(".crewlet-canvas__viewport")!);
  expect(row("Hero").hasAttribute("aria-current")).toBe(false);
});

test("an opened board fits the dialog, letterboxed, and shows at its own size on request", async () => {
  await mount();
  fireEvent.keyDown(
    screen.getByRole("button", { name: "Pricing, press Enter or double-click to open" }),
    { key: "Enter" },
  );
  const dialog = await screen.findByRole("dialog");
  // FITTED BY DEFAULT: the board's page is laid out at 1440 by 900 on a stage
  // that scales it into the dialog.
  const stage = dialog.querySelector(".design-opened--fit .crewlet-slide-stage--contain");
  expect(stage).not.toBeNull();
  expect(stage!.getAttribute("aria-label")).toBe("Pricing, 1440 by 900");
  expect((stage!.querySelector(".crewlet-slide-stage__frame") as HTMLElement).style.width).toBe(
    "1440px",
  );
  // The stage takes the board's proportions, so a narrow window draws it
  // short rather than letterboxed in a tall box.
  expect((dialog.querySelector(".design-opened--fit") as HTMLElement).style.aspectRatio).toBe(
    "1440 / 900",
  );
  fireEvent.click(within(dialog).getByRole("radio", { name: "Actual size" }));
  const actual = within(dialog).getByRole("region", { name: "Pricing at actual size" });
  expect(actual.classList.contains("design-opened--actual")).toBe(true);
  expect((actual.firstElementChild as HTMLElement).style.width).toBe("1440px");
  expect(dialog.querySelector(".crewlet-slide-stage")).toBeNull();
});

// THE CANVAS BEING EDITED IS THE ASSISTANT'S TO READ, and a change it proposes
// retypes a note as typing on the canvas would; read, it holds no draft.
test("the canvas being edited is the assistant's to read, and a draft.set retypes a note", async () => {
  openFrame("artifact");
  await mount();
  expect(context().draft).toBeUndefined();
  fireEvent.click(screen.getByRole("button", { name: "Edit layout" }));
  expect(context().draft).toMatchObject({ kind: "client", id: ID, revision: "3" });
  expect(draftField("boards")).toMatchObject({
    kind: "list",
    value: expect.stringMatching(/Hero/),
  });
  fireEvent.click(screen.getByRole("button", { name: "Sticky note" }));
  fireEvent.click(document.querySelector(".crewlet-canvas__viewport")!, { clientX: 0, clientY: 0 });
  fireEvent.blur(screen.getByRole("textbox", { name: "Note text" }));
  await waitFor(() => expect(draftField("notes.1")).toMatchObject({ editable: true }));
  expect(setDraft({ "notes.1": "Check the pricing copy" })).toMatchObject({ outcome: "applied" });
  run.mockResolvedValueOnce({ kind: "done", pending: false, body: { number: 4 } });
  fireEvent.click(screen.getByRole("button", { name: "Save version" }));
  await waitFor(() => expect(run).toHaveBeenCalledTimes(1));
  const req = run.mock.calls[0]![0] as { body: { edits: { new_str: string }[] } };
  expect(JSON.parse(req.body.edits[0]!.new_str).notes[0].text).toBe("Check the pricing copy");
});
