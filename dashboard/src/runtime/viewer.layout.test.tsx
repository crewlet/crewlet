/**
 * A page that links its pictures, in the design system's own sandboxed frame,
 * running the runtime: selecting a picture shows it full size over the page,
 * and the frame stays open. Before the viewer, the frame navigated to the
 * picture, loaded a document that never said hello, and was closed as a page
 * that left its frame.
 */
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, test } from "vitest";
import { page, userEvent } from "vitest/browser";
import { createRoot, type Root } from "react-dom/client";
import { act } from "react";
import { DEFAULT_FRAME_THEME } from "@crewlethq/ui/FrameProtocol";
import { SandboxedFrame, type SandboxedFrameState } from "@crewlethq/ui";
import { eventually } from "~/test/settle.ts";
import { FileViewer, type Probe } from "./viewer.ts";

const PAGE = new URL("/src/runtime/testdata/pictures.html", window.location.origin).href;

let root: Root;
let region: HTMLElement;
let frame: HTMLIFrameElement;
let states: SandboxedFrameState[] = [];

beforeAll(async () => {
  await page.viewport(1280, 860);
  const host = document.createElement("div");
  host.style.cssText = "width:1200px;height:800px";
  document.body.append(host);
  root = createRoot(host);
  await act(async () => {
    root.render(
      <SandboxedFrame
        style={{ width: "100%", height: "100%" }}
        src={PAGE}
        origin={window.location.origin}
        mode="per_artifact"
        sandbox="allow-scripts allow-forms allow-same-origin"
        title="Before and after"
        theme={DEFAULT_FRAME_THEME}
        nonce="viewer-suite-0123456789"
        onStateChange={(state) => states.push(state)}
      />,
    );
  });
  region = host.querySelector<HTMLElement>(".crewlet-sandboxed-frame")!;
  await eventually(() => expect(region.dataset["state"]).toBe("ready"));
  frame = region.querySelector("iframe")!;
  frame.style.cssText = "display:block;width:1200px;height:800px;border:0";
  await eventually(() => expect(inPage().querySelector("#before img")).not.toBeNull());
});

afterAll(() => {
  act(() => root.unmount());
});

beforeEach(async () => {
  if (open()) {
    viewer().querySelector("dialog")!.close();
    // CLOSED ONCE THE BROWSER SAYS SO, which it does in a task of its own.
    await eventually(() => expect(figure().childElementCount).toBe(0));
  }
  states = [];
});

const inPage = () => frame.contentDocument!;
const viewer = () => inPage().querySelector("crewlet-file-viewer")!.shadowRoot!;
// NULL-SAFE, since a frame the dashboard closed has no document left to ask.
const open = () =>
  frame.contentDocument?.querySelector("crewlet-file-viewer")?.shadowRoot?.querySelector("dialog")
    ?.open ?? false;
const figure = () => viewer().querySelector("figure")!;
const button = (label: string) =>
  viewer().querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`)!;
const picture = () => figure().querySelector<HTMLImageElement>("img.media");

/** A click on one of the page's links; true where the page's runtime kept it from the frame. */
function select(id: string): boolean {
  return !inPage()
    .getElementById(id)!
    .dispatchEvent(
      new MouseEvent("click", { bubbles: true, cancelable: true, view: frame.contentWindow }),
    );
}

/** Waits for the viewer to open, or for the frame to be closed as a page that left it. */
async function opened(): Promise<void> {
  await eventually(() => expect(open() || region.dataset["state"] !== "ready").toBe(true));
  expect(region.dataset["state"], "the frame was closed instead of opening the viewer").toBe(
    "ready",
  );
  expect(open()).toBe(true);
}

describe("selecting a picture the page links", () => {
  test("shows it full size over the page, and the frame stays open on the page", async () => {
    const document = inPage();
    const kept = select("before");
    await opened();
    expect(kept).toBe(true);
    await eventually(() => expect(picture()?.complete && picture()!.naturalWidth).toBe(320));
    expect(picture()!.alt).toBe("Before the fix");
    expect(new URL(picture()!.src).pathname).toBe("/src/runtime/testdata/before.png");
    expect(figure().querySelector("figcaption")!.textContent).toBe("Before the fix");
    // LARGER THAN THE PAGE DREW IT: the page shows a 160 pixel thumbnail.
    expect(picture()!.getBoundingClientRect().width).toBeGreaterThan(160);
    expect(viewer().querySelector(".count")!.textContent).toBe("1 of 3");
    expect(inPage()).toBe(document);
    expect(frame.contentWindow!.location.pathname).toBe("/src/runtime/testdata/pictures.html");
    expect(states).toEqual([]);
  });

  test("from the keyboard opens it too, moves focus in, and gives it back on Escape", async () => {
    const link = inPage().getElementById("after")!;
    link.focus();
    await userEvent.keyboard("{Enter}");
    await opened();
    expect(picture()!.alt).toBe("After the fix");
    expect(viewer().activeElement).toBe(button("Close"));
    // Moved there by the viewer itself: an artifact's frame is another
    // origin's, where the browser refuses `autofocus` with a console error.
    expect(button("Close").autofocus).toBe(false);
    await userEvent.keyboard("{Escape}");
    await eventually(() => expect(open()).toBe(false));
    expect(inPage().activeElement).toBe(link);
    expect(region.dataset["state"]).toBe("ready");
  });

  test("closes from its button and from the dark around the picture, but not from the picture", async () => {
    select("before");
    await opened();
    await eventually(() => expect(picture()).not.toBeNull());
    picture()!.click();
    expect(open()).toBe(true);
    viewer().querySelector<HTMLElement>(".stage")!.click();
    expect(open()).toBe(false);
    select("before");
    await opened();
    button("Close").click();
    expect(open()).toBe(false);
  });

  test("steps through the page's other files, past either end, by button and arrow key", async () => {
    select("after");
    await opened();
    const count = () => viewer().querySelector(".count")!.textContent;
    expect(count()).toBe("2 of 3");
    button("Next file").click();
    expect(count()).toBe("3 of 3");
    await eventually(() => expect(figure().querySelector(".kind")?.textContent).toBe("PDF"));
    expect(figure().querySelector(".name")!.textContent).toBe("report.pdf");
    expect(viewer().querySelector('[role="status"]')!.textContent).toBe("The full report, 3 of 3");
    button("Next file").focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(count()).toBe("1 of 3");
    await eventually(() => expect(picture()?.alt).toBe("Before the fix"));
    await userEvent.keyboard("{ArrowLeft}");
    expect(count()).toBe("3 of 3");
    button("Previous file").click();
    expect(count()).toBe("2 of 3");
  });

  test("on a phone-wide frame fills its width, with the steps under it", async () => {
    frame.style.width = "390px";
    try {
      await eventually(() => expect(frame.contentWindow!.innerWidth).toBe(390));
      select("before");
      await opened();
      await eventually(() => expect(picture()?.complete && picture()!.naturalWidth).toBe(320));
      // ITS OWN FULL WIDTH, 320 pixels, which the frame's 390 fits: between the
      // two buttons it would have about 254.
      expect(picture()!.getBoundingClientRect().width).toBe(320);
      const below = picture()!.getBoundingClientRect().bottom;
      expect(button("Next file").getBoundingClientRect().top).toBeGreaterThan(below);
    } finally {
      frame.style.width = "1200px";
    }
  });

  test("is drawn by the kit's values, whatever the page's own styles say", async () => {
    select("before");
    await opened();
    const close = getComputedStyle(button("Close"));
    expect(close.display).not.toBe("none");
    expect(close.width).toBe("44px");
    expect(getComputedStyle(figure()).display).toBe("flex");
  });
});

test("a link to one of the page's own pages is still followed by the frame", async () => {
  const window = frame.contentWindow!;
  expect(select("self")).toBe(false);
  await eventually(() => expect(window.location.hash).toBe("#top"));
  expect(open()).toBe(false);
  expect(region.dataset["state"]).toBe("ready");
});

describe("a file the origin will not give", () => {
  afterEach(() => {
    for (const host of document.querySelectorAll("crewlet-file-viewer")) host.remove();
  });

  /** A viewer over this suite's own document, told what the origin answers. */
  function viewing(answer: Probe | Error, url = PAGE.replace("pictures.html", "gone.png")) {
    const own = new FileViewer({
      window,
      probe: () => (answer instanceof Error ? Promise.reject(answer) : Promise.resolve(answer)),
    });
    own.open([{ url, file: "gone.png", label: "A picture this page lost" }], 0, null);
    const shadow = document.querySelector("crewlet-file-viewer")!.shadowRoot!;
    return { own, said: () => shadow.querySelector(".kind")?.textContent };
  }

  test("is said to be missing where the origin has none, and the viewer stays open", async () => {
    const { own, said } = viewing({ ok: false, status: 404 });
    await eventually(() => expect(said()).toBe("Not found"));
    expect(own.isOpen).toBe(true);
    own.close();
  });

  test("is said to be unavailable where the origin refused or could not be reached", async () => {
    for (const answer of [{ ok: false, status: 401 } as const, new TypeError("Failed to fetch")]) {
      const { own, said } = viewing(answer);
      await eventually(() => expect(said()).toBe("Unavailable"));
      own.close();
      document.querySelector("crewlet-file-viewer")!.remove();
    }
  });

  test("is said to be unavailable where a picture's bytes do not draw", async () => {
    const { own, said } = viewing(
      { ok: true, type: "image/png" },
      PAGE.replace("pictures.html", "report.pdf"),
    );
    await eventually(() => expect(said()).toBe("Unavailable"));
    own.close();
  });
});
