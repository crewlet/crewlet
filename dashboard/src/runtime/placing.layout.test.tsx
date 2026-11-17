/**
 * While a reader places a comment, the page's whole surface takes the
 * placing cursor, its links' and buttons' own cursors included, and gives
 * them back after.
 */
import { afterEach, expect, test } from "vitest";
import { commentPinCursor } from "@crewlethq/ui";
import { PageComments } from "./comments.ts";

const PAGE = `<!doctype html><html><head><style>
a{cursor:pointer} button{cursor:grab} .text{cursor:text}
</style></head><body><a href="#x" id="link">Pricing</a><button id="press">Buy</button>
<p class="text" id="words">Words</p></body></html>`;

let frame: HTMLIFrameElement | null = null;

afterEach(() => {
  frame?.remove();
  frame = null;
});

async function open(): Promise<{ win: Window; comments: PageComments }> {
  frame = document.createElement("iframe");
  const loaded = new Promise((resolve) => frame!.addEventListener("load", resolve, { once: true }));
  frame.srcdoc = PAGE;
  document.body.append(frame);
  await loaded;
  const win = frame.contentWindow!;
  const comments = new PageComments({
    call: () => Promise.resolve(null),
    refuse: (code, message) => Object.assign(new Error(message), { code }),
    window: win,
  });
  return { win, comments };
}

const cursorOf = (win: Window, id: string) =>
  win.getComputedStyle(win.document.getElementById(id)!).cursor;

test("placing draws the host's cursor over every element, and stopping gives each its own back", async () => {
  const { win, comments } = await open();
  const pin = commentPinCursor();
  comments.receive({ mode: "place", cursor: pin });
  for (const id of ["link", "press", "words"]) {
    expect(cursorOf(win, id), id).toMatch(/^url\("data:image\/svg\+xml,.+"\) 3 21, crosshair$/);
  }
  comments.receive({ mode: "show" });
  expect(cursorOf(win, "link")).toBe("pointer");
  expect(cursorOf(win, "press")).toBe("grab");
  expect(cursorOf(win, "words")).toBe("text");
  expect(win.document.adoptedStyleSheets).toHaveLength(0);
});

test("a cursor the browser refuses, or none, places by the crosshair", async () => {
  const { win, comments } = await open();
  comments.receive({ mode: "place", cursor: "url(nope" });
  expect(cursorOf(win, "link")).toBe("crosshair");
  comments.receive({ mode: "off" });
  comments.receive({ mode: "place" });
  expect(cursorOf(win, "press")).toBe("crosshair");
});
