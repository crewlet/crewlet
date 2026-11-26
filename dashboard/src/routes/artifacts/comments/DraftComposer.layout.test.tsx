/**
 * The draft beside its pin stays inside the frame at every pin position, its
 * actions in view and its picture fitted whole. A measurement, so it runs in
 * a headless browser (`~/test/layout.tsx`).
 */
import "~/test/layout.tsx";
import "~/styles/comments.css";
import { afterEach, expect, test, vi } from "vitest";
import type { ReactNode } from "react";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { ArtifactDetail } from "~/protocol/index.ts";
import { DraftComposer } from "./DraftComposer.tsx";
import { DraftPin } from "./layer.tsx";
import { CommentsScope, type ArtifactComments, type CommentDraft } from "./state.tsx";

vi.mock("~/lib/artifacts/people.ts", () => ({
  useArtifactPeople: () => ({ seatName: (s: string) => s }),
}));

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

/** A picture as a page draws one, `w` by `h`. */
function picture(w: number, h: number): string {
  const canvas = document.createElement("canvas");
  canvas.width = w;
  canvas.height = h;
  const ctx = canvas.getContext("2d")!;
  ctx.fillStyle = "#3a6";
  ctx.fillRect(0, 0, w, h);
  return canvas.toDataURL("image/png");
}

function comments(draft: Omit<CommentDraft, "seq">): ArtifactComments {
  const noop = () => {};
  return {
    detail: { id: "a1", kind: "html", head_number: 12, role: "editor" } as ArtifactDetail,
    version: 12,
    answer: null,
    threads: [],
    failed: null,
    numberOf: () => 0,
    refetch: noop,
    filter: "open",
    setFilter: noop,
    selected: null,
    select: noop,
    openThread: noop,
    showPanel: noop,
    stepAside: noop,
    placing: true,
    setPlacing: noop,
    draft: { seq: 1, ...draft },
    startDraft: () => 1,
    setDraftCrop: noop,
    dockDraft: noop,
    clearDraft: noop,
    draftWords: { current: "" },
    can: true,
    reason: undefined,
    placeable: true,
    narrow: false,
    ownerName: "Product Lead",
    reveal: noop,
    setRevealer: noop,
    lendPage: () => noop,
    pageHost: () => null,
  };
}

/** The viewer the draft's box asks whether the engine can dictate, from a client that answers nothing. */
function WithViewer({ children }: { children: ReactNode }) {
  const store = new Store();
  const socket = new LiveSocket(store);
  socket.query = (() => Promise.resolve({})) as typeof socket.query;
  return (
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>{children}</ViewerProvider>
    </ClientContext.Provider>
  );
}

/** A frame's stage `w` by `h` with the draft's pin at `x`, `y`, as `PagePins` draws it. */
async function open(w: number, h: number, x: number, y: number, draft: Omit<CommentDraft, "seq">) {
  host = document.createElement("div");
  host.style.cssText = `position: fixed; left: 0; top: 0; width: ${w}px; height: ${h}px;`;
  host.className = "artifact-frame-stage";
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <WithViewer>
        <Router>
          <ToastProvider>
            <CommentsScope value={comments(draft)}>
              <div className="artifact-comment-layer">
                <LayerHost>
                  <DraftPin x={x} y={y} />
                </LayerHost>
              </div>
            </CommentsScope>
          </ToastProvider>
        </Router>
      </WithViewer>,
    ),
  );
  for (let i = 0; i < 60; i++) {
    const panel = document.querySelector<HTMLElement>(".crewlet-popover");
    const img = panel?.querySelector("img");
    if (panel?.style.top && (!img || img.complete)) {
      // MEASURED ONCE IT HAS ARRIVED: its entrance rises from below where it is placed.
      await Promise.all(panel.getAnimations().map((a) => a.finished));
      const pin = document.querySelector(".artifact-comment-pin")!.getBoundingClientRect();
      return { panel, pin, stage: host.getBoundingClientRect() };
    }
    await new Promise((resolve) => requestAnimationFrame(resolve));
  }
  throw new Error("the draft was never placed");
}

const within = (inner: DOMRect, outer: DOMRect) =>
  inner.left >= outer.left - 0.5 &&
  inner.right <= outer.right + 0.5 &&
  inner.top >= outer.top - 0.5 &&
  inner.bottom <= outer.bottom + 0.5;

const POINT = { anchor: { kind: "html", page: "index.html", point: { x: 0.5, y: 0.5 } } } as const;
const LONG = "A heading that runs long ".repeat(12).trim();

const STATES: [string, Omit<CommentDraft, "seq">][] = [
  ["a picture", { ...POINT, label: "A point on the page", crop: picture(480, 300) }],
  ["a tall picture", { ...POINT, label: "A point on the page", crop: picture(240, 900) }],
  ["a picture being drawn", { ...POINT, label: "A point on the page", crop: undefined }],
  ["no picture", { ...POINT, label: "A point on the page", crop: null }],
  [
    "a section's long words",
    {
      anchor: { kind: "html", page: "index.html", name: "results" },
      label: `Section: ${LONG}`,
      quote: LONG,
      crop: picture(480, 300),
    },
  ],
];

test("beside a pin at each corner and the middle of the frame, the draft is inside it, whole and fitted", async () => {
  const [w, h] = [834, 767];
  const spots = [
    [24, 24],
    [w - 24, 24],
    [24, h - 24],
    [w - 24, h - 24],
    [w / 2, h / 2],
  ] as const;
  for (const [name, draft] of STATES) {
    for (const [x, y] of spots) {
      const { panel, pin, stage } = await open(w, h, x, y, draft);
      const at = `${name} at ${x},${y}`;
      const box = panel.getBoundingClientRect();
      expect(within(box, stage), `${at}: inside the frame`).toBe(true);
      // BESIDE ITS PIN, never over it: the frame is wide enough for one on either side.
      const covers =
        box.left < pin.right &&
        box.right > pin.left &&
        box.top < pin.bottom &&
        box.bottom > pin.top;
      expect(covers, `${at}: off the pin`).toBe(false);
      // WHOLE: nothing scrolls out of view, sideways or down.
      expect(panel.scrollHeight, `${at}: no scroll`).toBeLessThanOrEqual(panel.clientHeight + 1);
      for (const el of panel.querySelectorAll<HTMLElement>("*")) {
        // A label kept for a screen reader alone is clipped to a pixel on purpose.
        if (
          getComputedStyle(el).overflowX === "visible" ||
          el.closest(".crewlet-visually-hidden")
        ) {
          continue;
        }
        expect(el.scrollWidth, `${at}: ${el.className} sideways`).toBeLessThanOrEqual(
          el.clientWidth + 1,
        );
      }
      const comment = [...panel.querySelectorAll("button")].find(
        (b) => b.textContent === "Comment",
      )!;
      expect(within(comment.getBoundingClientRect(), box), `${at}: Comment in view`).toBe(true);
      const well = panel.querySelector(".artifact-comment-preview")!.getBoundingClientRect();
      const img = panel.querySelector("img");
      if (img) {
        const shown = img.getBoundingClientRect();
        expect(within(shown, well), `${at}: fitted`).toBe(true);
        const off = Math.abs(shown.left + shown.width / 2 - (well.left + well.width / 2));
        expect(off, `${at}: in the well's middle`).toBeLessThanOrEqual(1);
      }
      flushSync(() => root?.unmount());
      host?.remove();
    }
  }
});

test("in a frame shorter than the draft, it stays inside and scrolls rather than lose its foot", async () => {
  const { panel, stage } = await open(834, 300, 40, 280, STATES[0]![1]);
  expect(within(panel.getBoundingClientRect(), stage)).toBe(true);
  expect(panel.scrollHeight).toBeGreaterThan(panel.clientHeight);
});

test("the picture's well is one height while the picture is drawn, shown or missing", async () => {
  const heights = [];
  for (const [, draft] of STATES.slice(0, 4)) {
    const { panel } = await open(834, 767, 200, 100, draft);
    heights.push(panel.querySelector(".artifact-comment-preview")!.getBoundingClientRect().height);
    flushSync(() => root?.unmount());
    host?.remove();
  }
  expect(new Set(heights).size).toBe(1);
});

test("in the panel on a phone, Cancel and Comment share one row", async () => {
  host = document.createElement("div");
  host.style.cssText = "position: fixed; left: 0; top: 0; width: 358px;";
  document.body.append(host);
  root = createRoot(host);
  const value = { ...comments(STATES[0]![1]), narrow: true };
  flushSync(() =>
    root!.render(
      <WithViewer>
        <Router>
          <ToastProvider>
            <CommentsScope value={value}>
              <DraftComposer />
            </CommentsScope>
          </ToastProvider>
        </Router>
      </WithViewer>,
    ),
  );
  const buttons = [...host.querySelectorAll("button")];
  const cancel = buttons.find((b) => b.textContent === "Cancel")!.getBoundingClientRect();
  const comment = buttons.find((b) => b.textContent === "Comment")!.getBoundingClientRect();
  expect(Math.abs(cancel.top - comment.top)).toBeLessThanOrEqual(1);
  expect(comment.right).toBeLessThanOrEqual(host.getBoundingClientRect().right + 0.5);
});
