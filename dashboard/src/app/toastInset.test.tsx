/**
 * The toast stack stands at the window's foot on every screen and keeps clear
 * of what stands there: beside the assistant while it is a column or over the
 * page, above its foot while it is the whole window, beside a screen's own pane
 * at the end edge, and above a bar docked at the foot.
 *
 * TWO OWNERS ASK, each for its own part, and the host keeps the largest of
 * each: the frame knows the width a panel takes and the panel knows its own
 * foot. Asserted on the stack's own published inset, which is what the
 * design system's stylesheet reads.
 */

import { act, cleanup, render } from "@testing-library/react";
import { useRef } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost, toasterInsetStyle, useToast } from "@crewlethq/ui";
import {
  InsetToastProvider,
  SurfaceToastProvider,
  TOAST_REST_ROOM,
  ToastRoom,
  roomOf,
  clearOf,
  useToastBeside,
  useToastClearOf,
  useToastInset,
  wasAtEnd,
  type InsetOwner,
} from "./toastInset.tsx";
import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  document.getElementById(SCREEN_SCROLL_ID)?.remove();
});

function Ask({ owner, inline, block }: { owner: InsetOwner; inline?: number; block?: number }) {
  useToastInset(owner, inline === undefined && block === undefined ? null : { inline, block });
  return null;
}

function Toast() {
  const toast = useToast();
  return (
    <button
      type="button"
      onClick={() => toast.show({ variant: "success", title: "Conversation cleared" })}
    >
      raise
    </button>
  );
}

/** The custom properties the stack carries now, by name. */
function published(): Record<string, string> {
  const names = Object.keys(toasterInsetStyle({ inline: 1, block: 1 }) ?? {});
  const out: Record<string, string> = {};
  for (const el of Array.from(document.querySelectorAll<HTMLElement>("[style]"))) {
    for (const name of names) {
      const value = el.style.getPropertyValue(name);
      // ZERO IS NO ROOM, the same as not asking.
      if (value && value !== "0px") out[name] = value;
    }
  }
  return out;
}

test("each owner's part is kept, and withdrawn with it", () => {
  const view = render(
    <InsetToastProvider>
      <Ask owner="frame" inline={428} />
      <Ask owner="panel" block={120} />
      <Toast />
    </InsetToastProvider>,
  );
  act(() => view.getByRole("button", { name: "raise" }).click());
  const both = published();
  expect(Object.values(both).sort()).toEqual(["120px", "428px"].sort());

  view.rerender(
    <InsetToastProvider>
      <Ask owner="frame" inline={428} />
      <Toast />
    </InsetToastProvider>,
  );
  expect(Object.values(published())).toEqual(["428px"]);
});

test("with nobody asking, the stack keeps only its own window inset", () => {
  const view = render(
    <InsetToastProvider>
      <Toast />
    </InsetToastProvider>,
  );
  act(() => view.getByRole("button", { name: "raise" }).click());
  expect(published()).toEqual({});
});

/** The edge the stack stands on, as its class says. */
const edge = () =>
  document
    .querySelector<HTMLElement>("[class*='crewlet-toaster--']")
    ?.className.match(/crewlet-toaster--(\S+)/)?.[1];

/** A panel at the window's end edge, `width` px wide. */
function Pane({ width, on = true }: { width: number; on?: boolean }) {
  const pane = useRef<HTMLDivElement>(null);
  useToastBeside(() => pane.current, on);
  return (
    <div
      ref={(el) => {
        if (el)
          el.getBoundingClientRect = () =>
            ({ left: window.innerWidth - width, width, height: 600 }) as DOMRect;
        pane.current = el;
      }}
    />
  );
}

// ONE PLACE ON EVERY SCREEN, the window's foot: Chatlet stood the stack at the
// top right under its bar, where it lay over an open pane. A panel at the end
// edge keeps the stack beside it instead, and lets go when it closes.
// Mutation: drop the pane's ask, and the stack stands over the pane.
test("a pane at the end edge keeps the stack beside it, at the foot, and lets go with it", () => {
  const view = render(
    <InsetToastProvider>
      <Pane width={400} />
      <Toast />
    </InsetToastProvider>,
  );
  act(() => view.getByRole("button", { name: "raise" }).click());
  expect(edge()).toBe("bottom-right");
  expect(published()).toMatchObject({ "--crewlet-toaster-inset-inline": "400px" });
  view.rerender(
    <InsetToastProvider>
      <Pane width={400} on={false} />
      <Toast />
    </InsetToastProvider>,
  );
  expect(published()).toEqual({});
});

// TWO ASKERS OF ONE KIND ARE TWO PARTS, the largest kept: a room's foot and a
// thread's reply box both ask as bars at the foot, and keyed by kind the one
// that asked last undid the other.
test("two askers of one kind each keep their part, and the larger holds", () => {
  const view = render(
    <InsetToastProvider>
      <Ask owner="bar" block={150} />
      <Ask owner="bar" block={90} />
      <Toast />
    </InsetToastProvider>,
  );
  act(() => view.getByRole("button", { name: "raise" }).click());
  expect(published()).toMatchObject({ "--crewlet-toaster-inset-block": "150px" });
  view.rerender(
    <InsetToastProvider>
      <Ask owner="bar" block={90} />
      <Toast />
    </InsetToastProvider>,
  );
  expect(published()).toMatchObject({ "--crewlet-toaster-inset-block": "90px" });
});

// ONLY A BAR THAT REACHES THE FOOT ASKS, and only one with a box: a reply box
// following a short thread stands mid-pane, and a column folded away has no box.
test("a bar asks for room only where it is docked at the foot and drawn", () => {
  const vh = 900;
  expect(clearOf({ top: 750, bottom: 892, width: 600, height: 142 } as DOMRect, vh)).toBe(150);
  expect(clearOf({ top: 300, bottom: 440, width: 600, height: 140 } as DOMRect, vh)).toBeNull();
  expect(clearOf({ top: 0, bottom: 0, width: 0, height: 0 } as DOMRect, vh)).toBeNull();
});

/*
 * THE PAGE KEEPS ROOM FOR A TOAST AT ITS FOOT. A toast stands over the page,
 * and after a press on a table's last row it covered that row's controls for
 * as long as it showed. While the stack holds a toast at the window's foot,
 * the page's end grows by the height it covers, and a reader at the end
 * stays there, above it.
 */
test("the room is what the stack covers to the window's foot, and none while it is empty", () => {
  expect(roomOf({ top: 780, height: 96 } as DOMRect, 900)).toBe(120);
  expect(roomOf({ top: 900, height: 0 } as DOMRect, 900)).toBe(0);
  // A reader at the end before the page grew is the growth away from it now.
  expect(wasAtEnd(120, 120)).toBe(true);
  expect(wasAtEnd(400, 120)).toBe(false);
  expect(wasAtEnd(0, 0)).toBe(false);
});

/** Stands the host's toaster `top` px down the window and `height` tall. */
function stackAt(top: number, height: number) {
  const real = HTMLElement.prototype.getBoundingClientRect;
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    return this.classList.contains("crewlet-toaster")
      ? ({ top, height } as DOMRect)
      : real.call(this);
  });
}

/** The room drawn at the page's end, in px, or null for none. */
const roomDrawn = (view: { container: HTMLElement }) => {
  const el = view.container.querySelector<HTMLElement>("[aria-hidden='true'][style*='height']");
  return el ? el.style.height : null;
};

// THE ROOM IS ALWAYS THERE, so a toast lies over it and moves nothing: made as a
// toast came, the page rose 93px under the pointer and fell back as it went.
// Only a taller stack grows it. Mutation: draw the room only while a toast shows,
// and the first read goes red; measure nothing, and the taller stack's does.
test("the page's end keeps one toast's room, and a taller stack's", () => {
  stackAt(window.innerHeight, 0);
  const view = render(
    <InsetToastProvider>
      <ToastRoom />
    </InsetToastProvider>,
  );
  expect(roomDrawn(view)).toBe(`${TOAST_REST_ROOM}px`);
  view.unmount();

  stackAt(window.innerHeight - TOAST_REST_ROOM + 3, 69);
  const one = render(
    <InsetToastProvider>
      <ToastRoom />
    </InsetToastProvider>,
  );
  expect(roomDrawn(one)).toBe(`${TOAST_REST_ROOM}px`);
  one.unmount();

  stackAt(window.innerHeight - 200, 176);
  const tall = render(
    <InsetToastProvider>
      <ToastRoom />
    </InsetToastProvider>,
  );
  expect(roomDrawn(tall)).toBe("200px");
});

/** A scroller of 1100px under a 500px view, read as the page's, scrolled to `at`. */
function scrolledTo(at: number) {
  const scroller = document.createElement("div");
  scroller.id = SCREEN_SCROLL_ID;
  document.body.append(scroller);
  Object.defineProperty(scroller, "scrollHeight", { value: 1100 });
  Object.defineProperty(scroller, "clientHeight", { value: 500 });
  scroller.scrollTop = at;
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockImplementation(function (
    this: HTMLElement,
  ) {
    return parseFloat(this.style.height) || 0;
  });
  return scroller;
}

// A TOAST THAT FITS THE ROOM MOVES NOTHING, and a reader at the end is not taken
// anywhere when the page opens with it. Mutation: scroll to the end whenever the
// room is drawn, and the reader is moved.
test("the room a page opens with moves no reader", () => {
  const scroller = scrolledTo(500);
  stackAt(window.innerHeight - 80, 60);
  render(
    <InsetToastProvider>
      <ToastRoom />
    </InsetToastProvider>,
  );
  expect(scroller.scrollTop).toBe(500);
});

// Mutation: leave the scroll where it was, and the end is behind the stack.
test.each([
  ["a reader at the end stays there", 500, 1100],
  ["a reader above it stays put", 200, 200],
])("%s as a taller stack grows the room", (_name, scrolled, after) => {
  const scroller = scrolledTo(scrolled);
  const real = HTMLElement.prototype.getBoundingClientRect;
  let stack = { top: window.innerHeight, height: 0 };
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    return this.classList.contains("crewlet-toaster") ? (stack as DOMRect) : real.call(this);
  });
  render(
    <InsetToastProvider>
      <ToastRoom />
    </InsetToastProvider>,
  );
  expect(scroller.scrollTop).toBe(scrolled);
  // GROWN BY THE 100PX BETWEEN THE SCROLLER'S END AND ITS VIEW, whatever the rest room is.
  stack = { top: window.innerHeight - TOAST_REST_ROOM - 100, height: TOAST_REST_ROOM + 76 };
  act(() => void window.dispatchEvent(new Event("resize")));
  expect(scroller.scrollTop).toBe(after);
});

// A BAR AT THE FOOT KEEPS THE STACK ABOVE IT, and the page's end keeps room for
// both. At 390px the toast (y 783 to 832) covered the selection bar's controls
// (y 795 to 821). Mutation: ask for nothing, and the stack stays on the bar.
function DockedBar({ on }: { on: boolean }) {
  const bar = useRef<HTMLDivElement>(null);
  useToastClearOf(() => bar.current, on);
  return <div ref={bar} data-docked="" />;
}

test("a bar docked at the foot stands the stack above it, while it is docked", () => {
  const real = HTMLElement.prototype.getBoundingClientRect;
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    return this.hasAttribute("data-docked")
      ? ({
          top: window.innerHeight - 54,
          bottom: window.innerHeight,
          width: 390,
          height: 54,
        } as DOMRect)
      : real.call(this);
  });
  const tree = (on: boolean) => (
    <InsetToastProvider>
      <DockedBar on={on} />
      <ToastRoom />
    </InsetToastProvider>
  );
  const view = render(tree(true));
  expect(published()).toMatchObject({ "--crewlet-toaster-inset-block": "54px" });
  expect(roomDrawn(view)).toBe(`${TOAST_REST_ROOM + 54}px`);
  view.rerender(tree(false));
  expect(published()).toEqual({});
  expect(roomDrawn(view)).toBe(`${TOAST_REST_ROOM}px`);
});

// A BAR DRAWN AFTER ITS ASKER MOUNTS IS FOUND WHEN IT IS ADDED: a room's foot
// is drawn under its first read, after the hook that keeps the stack above it
// has run, and was never measured. Mutation: find only on mount, and the stack
// stays on the bar.
function LateBar({ drawn }: { drawn: boolean }) {
  useToastClearOf(() => document.querySelector<HTMLElement>("[data-late]"), true);
  return drawn ? <div data-late="" /> : null;
}

test("a bar drawn after its asker mounted is measured once it is on the page", async () => {
  const real = HTMLElement.prototype.getBoundingClientRect;
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    return this.hasAttribute("data-late")
      ? ({
          top: window.innerHeight - 150,
          bottom: window.innerHeight - 8,
          width: 800,
          height: 142,
        } as DOMRect)
      : real.call(this);
  });
  const tree = (drawn: boolean) => (
    <InsetToastProvider>
      <LateBar drawn={drawn} />
      <ToastRoom />
    </InsetToastProvider>
  );
  const view = render(tree(false));
  expect(published()).toEqual({});
  view.rerender(tree(true));
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  expect(published()).toMatchObject({ "--crewlet-toaster-inset-block": "150px" });
});

/*
 * A SURFACE'S OWN STACK IS HOSTED AS THE APP'S. The org builder draws its
 * toasts inside its own layer, for its fullscreen; a bare provider there kept
 * clear of nothing the app's host was asked for, and the page made no room
 * for its stack. Mutation: host the surface with a bare `ToastProvider`, and
 * its stack keeps no inset and the page no room.
 */
test("a surface's stack keeps the app's inset, and the page keeps room for it", async () => {
  // A STACK HOLDING A TOAST is 96 px tall, standing 120 px over the foot; an
  // empty one holds only its live regions.
  const real = HTMLElement.prototype.getBoundingClientRect;
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    if (!this.classList.contains("crewlet-toaster")) return real.call(this);
    return (
      this.children.length > 1
        ? { top: window.innerHeight - 120, height: 96 }
        : { top: window.innerHeight, height: 0 }
    ) as DOMRect;
  });
  const view = render(
    <InsetToastProvider>
      <Ask owner="frame" inline={428} />
      <LayerHost>
        <SurfaceToastProvider>
          <Toast />
        </SurfaceToastProvider>
      </LayerHost>
      <ToastRoom />
    </InsetToastProvider>,
  );
  await act(async () => {});
  expect(roomDrawn(view)).toBe(`${TOAST_REST_ROOM}px`);
  act(() => view.getByRole("button", { name: "raise" }).click());
  // The resize the stack's toast makes, which the observer reports.
  await act(async () => window.dispatchEvent(new Event("resize")));
  expect(roomDrawn(view)).toBe("120px");
  const surface = [...document.querySelectorAll<HTMLElement>(".crewlet-toaster")].find(
    (el) => el.parentElement !== document.body,
  )!;
  const names = Object.keys(toasterInsetStyle({ inline: 1, block: 1 }) ?? {});
  expect(names.map((name) => surface.style.getPropertyValue(name))).toContain("428px");
});

// THE ROOM FITS THE TOAST A SEAT'S LONG NAME MAKES: three lines, 87px, over the
// kit's 24px inset. Mutation: 96, and a press at the page's end moves the page 15px.
test("the page's end keeps room for a three-line toast over the window inset", () => {
  expect(TOAST_REST_ROOM).toBeGreaterThanOrEqual(87 + 24);
});
