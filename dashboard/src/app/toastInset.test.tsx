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
import { afterEach, expect, test } from "vitest";
import { toasterInsetStyle, useToast } from "@crewlethq/ui";
import { InsetToastProvider, useToastInset, useToastTop, type InsetOwner } from "./toastInset.tsx";

afterEach(cleanup);

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
