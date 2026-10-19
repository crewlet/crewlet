/**
 * A design on screen: its boards framed one document each, and a person's
 * rearrangement saved as a version of `canvas.json` against its base, merged
 * onto a newer version when somebody published first. The frames themselves
 * are FrameView's to hold; here each is a stand-in that names its board.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import { Router } from "~/app/router.tsx";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import type { ArtifactDetail } from "~/protocol/index.ts";
import { DesignView } from "./DesignView.tsx";

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
          versions={[]}
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
  expect(screen.getByRole("button", { name: "Hero" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Pricing" })).toBeTruthy();
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
  act(() => {
    window.dispatchEvent(new Event("afterprint"));
  });
  // NOT AT ONCE: the browser is still taking the board's frame into the print.
  expect(document.querySelector(".design-print-view")).not.toBeNull();
  await waitFor(() => expect(document.querySelector(".design-print-view")).toBeNull(), {
    timeout: 3000,
  });
  expect(sizes()).toEqual([]);
  printed.mockRestore();
  globalThis.CSSStyleSheet = realSheet;
});
