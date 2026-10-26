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
import { afterEach, expect, test } from "vitest";
import { toasterInsetStyle, useToast } from "@crewlethq/ui";
import { InsetToastProvider, useToastInset, type InsetOwner } from "./toastInset.tsx";

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
