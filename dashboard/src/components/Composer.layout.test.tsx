/**
 * A PICK'S KEY IS WHOLE. Link a task lists its hits as a key beside a title,
 * and both were free to shrink, so a long title squeezed the key until it
 * broke at its hyphen ("ENG-" over "11"): the part a reader matches on, cut
 * in two. Laid out in a real browser, in a dialog's width.
 */
import "~/test/layout.tsx";
import { afterEach, expect, test } from "vitest";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { PickList } from "./Composer.tsx";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  root?.unmount();
  host?.remove();
  root = null;
  host = null;
});

test("a pick's key stays on one line beside a title too long for the row", () => {
  host = document.createElement("div");
  host.style.width = "320px";
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <PickList
        label="Tasks"
        chosen=""
        onChoose={() => {}}
        empty="No task matches."
        items={[
          {
            id: "t-11",
            meta: "ENG-11",
            title:
              "Investigate memory growth in the reconciler after 72 hours of uptime with thousands of clusters",
          },
        ]}
      />,
    ),
  );
  const key = host.querySelector<HTMLElement>(".composer-pick-key");
  const title = host.querySelector<HTMLElement>(".composer-pick-title");
  expect(key?.textContent).toBe("ENG-11");
  // ONE LINE: a key broken at its hyphen is two client rects, one per line.
  const range = document.createRange();
  range.selectNodeContents(key!);
  expect(range.getClientRects().length).toBe(1);
  // AND THE TITLE IS WHAT GAVE WAY: cut with an ellipsis inside the row.
  expect(title!.scrollWidth).toBeGreaterThan(title!.clientWidth);
  expect(title!.getBoundingClientRect().right).toBeLessThanOrEqual(
    host.getBoundingClientRect().right,
  );
});
