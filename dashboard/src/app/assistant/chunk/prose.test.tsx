/**
 * A model's words: nothing it WROTE is live.
 *
 * THE ASSISTANT'S PROSE IS WRITTEN BY A MODEL that may have read anything, so
 * an address it writes opens nothing: a link is its label and the host it
 * names, never an anchor and never focusable; an autolink is its address as
 * written; an app route it spells is its label; a picture is its alt text,
 * never fetched. What does link is what the dashboard built from checked
 * parts (`links: "checked"`, held in `components/InlineToken.test.tsx`).
 * Each case renders through the drawer's own `ReplyProse`, outside the frame,
 * so a renderer change that lets one through fails here.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { ReplyProse } from "./blocks.tsx";

afterEach(cleanup);

test("a model's link is its label and host, and no anchor at all", () => {
  const { container } = render(
    <ReplyProse text="See [the pricing page](https://docs.example.com/pricing) for details." />,
  );
  expect(container.querySelector("a")).toBeNull();
  expect(container.textContent).toContain("the pricing page (docs.example.com/pricing)");
  // NOTHING IN IT TAKES FOCUS: an inert link is text a reader reads.
  expect(container.querySelectorAll("[tabindex], button, a")).toHaveLength(0);
});

test("an autolink is its host and path, inert", () => {
  const { container } = render(<ReplyProse text="Open <https://evil.example/login?next=1> now." />);
  expect(container.querySelector("a")).toBeNull();
  expect(container.textContent).toContain("evil.example/login");
});

test("a picture is its alt text and nothing is fetched", () => {
  const { container } = render(
    <ReplyProse text="![a chart](https://tracker.example/pixel.png) and ![](data:image/png;base64,AAAA)" />,
  );
  expect(container.querySelector("img")).toBeNull();
  expect(container.querySelector("a")).toBeNull();
  expect(container.textContent).toContain("a chart");
});

test("a script link or an app route is drawn as its label alone", () => {
  const { container } = render(
    <ReplyProse text="[click me](javascript:alert(1)) or [this task](#/worklet/ENG-1)" />,
  );
  expect(container.querySelector("a")).toBeNull();
  expect(container.textContent).toContain("click me");
  expect(container.textContent).not.toContain("javascript");
  expect(container.textContent).toContain("this task");
});

test("markup in the text is text", () => {
  const { container } = render(<ReplyProse text={'<img src=x onerror="alert(1)"> <b>bold</b>'} />);
  expect(container.querySelector("img")).toBeNull();
  expect(container.querySelector("b")).toBeNull();
  expect(container.textContent).toContain("<img");
});
