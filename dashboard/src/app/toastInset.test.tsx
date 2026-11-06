/**
 * The toast stack keeps clear of the assistant: beside it while it is a
 * column or over the page, above its foot while it is the whole window.
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
  ToastRoom,
  roomOf,
  useToastInset,
  useToastTop,
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

/** A bar whose bottom edge stands `bottom` px down the window. */
function Bar({ bottom }: { bottom: number }) {
  const bar = useRef<HTMLDivElement | null>(null);
  useToastTop(bar);
  return (
    <div
      ref={(el) => {
        if (el) el.getBoundingClientRect = () => ({ bottom }) as DOMRect;
        bar.current = el;
      }}
    />
  );
}

/** The edge the stack stands on, as its class says. */
const edge = () =>
  document
    .querySelector<HTMLElement>("[class*='crewlet-toaster--']")
    ?.className.match(/crewlet-toaster--(\S+)/)?.[1];

test("a screen's bar stands the stack under it, and lets go with it", () => {
  // THE BAR'S BOTTOM HOLDS THE STACK OFF THE TOP; a panel's width still keeps
  // it beside the panel, and a foot's height, the other edge's, no longer
  // applies. Unmounted, the stack goes back to the foot with the foot's room.
  const view = render(
    <InsetToastProvider>
      <Ask owner="frame" inline={428} />
      <Ask owner="panel" block={120} />
      <Bar bottom={56} />
      <Toast />
    </InsetToastProvider>,
  );
  act(() => view.getByRole("button", { name: "raise" }).click());
  expect(edge()).toBe("top-right");
  expect(Object.values(published()).sort()).toEqual(["428px", "56px"].sort());

  view.rerender(
    <InsetToastProvider>
      <Ask owner="frame" inline={428} />
      <Ask owner="panel" block={120} />
      <Toast />
    </InsetToastProvider>,
  );
  expect(edge()).toBe("bottom-right");
  expect(Object.values(published()).sort()).toEqual(["120px", "428px"].sort());
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

// Mutation: measure nothing, and the page's end keeps no room.
test("the page's end holds the room the stack covers, and none while it stands at the top", () => {
  stackAt(window.innerHeight - 120, 96);
  const view = render(
    <InsetToastProvider>
      <ToastRoom />
    </InsetToastProvider>,
  );
  expect(roomDrawn(view)).toBe("120px");
  view.rerender(
    <InsetToastProvider>
      <Bar bottom={56} />
      <ToastRoom />
    </InsetToastProvider>,
  );
  expect(roomDrawn(view)).toBeNull();
});

// Mutation: leave the scroll where it was, and the end is behind the stack.
test.each([
  ["a reader at the end stays there", 500, 1100],
  ["a reader above it stays put", 200, 200],
])("%s as the room comes", (_name, scrolled, after) => {
  const scroller = document.createElement("div");
  scroller.id = SCREEN_SCROLL_ID;
  document.body.append(scroller);
  // 600 px of page under a 500 px view, the room's 100 px added.
  Object.defineProperty(scroller, "scrollHeight", { value: 1100 });
  Object.defineProperty(scroller, "clientHeight", { value: 500 });
  scroller.scrollTop = scrolled;
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(100);
  stackAt(window.innerHeight - 100, 80);
  render(
    <InsetToastProvider>
      <ToastRoom />
    </InsetToastProvider>,
  );
  expect(scroller.scrollTop).toBe(after);
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
  expect(roomDrawn(view)).toBeNull();
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
