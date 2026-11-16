/**
 * The page's half of the comment layer: anchors declared, layout reported only
 * while asked, a pick kept from the page's own handlers, and the host's asks.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { ANCHOR_ATTRIBUTE, PageComments, anchorAround, clipBytes, fractionOf } from "./comments.ts";

type Sent = { method: string; params: Record<string, unknown> };

let sent: Sent[];
let comments: PageComments;

/** A refusal as the runtime makes one: an Error carrying its code. */
const refuse = (code: string, message: string) => Object.assign(new Error(message), { code });

/** Sets an element's box as the page draws it, in the viewport. */
function box(el: Element, x: number, y: number, w: number, h: number) {
  el.getBoundingClientRect = () =>
    ({ left: x, top: y, width: w, height: h, right: x + w, bottom: y + h }) as DOMRect;
}

const frame = () => new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
const layouts = () => sent.filter((s) => s.method === "comments.layout");

beforeEach(() => {
  sent = [];
  document.body.innerHTML = `
    <section data-crewlet-anchor="pricing">
      <h2>Plans</h2>
      <div data-crewlet-anchor="Not A Name"><button id="buy">Buy annual</button></div>
    </section>
    <p id="loose">Between sections</p>`;
  box(document.querySelector("section")!, 0, 100, 800, 300);
  comments = new PageComments({
    call: (method, params) => {
      sent.push({ method, params });
      return Promise.resolve(null);
    },
    refuse,
  });
});

afterEach(() => {
  comments.receive({ mode: "off" });
  document.body.innerHTML = "";
});

describe("a part", () => {
  test("is declared by its attribute, set and let go by comments.anchor", () => {
    const el = document.getElementById("loose")!;
    const stop = comments.anchor("aside", el);
    expect(el.getAttribute(ANCHOR_ATTRIBUTE)).toBe("aside");
    stop();
    expect(el.hasAttribute(ANCHOR_ATTRIBUTE)).toBe(false);
  });

  test("is refused a name the engine would not index, or no element, with invalid", () => {
    const el = document.getElementById("loose")!;
    for (const name of ["Pricing", "", "a b", "x".repeat(65)]) {
      expect(() => comments.anchor(name, el)).toThrow(expect.objectContaining({ code: "invalid" }));
    }
    expect(() => comments.anchor("aside", "loose" as unknown as Element)).toThrow(
      expect.objectContaining({ code: "invalid" }),
    );
    expect(el.hasAttribute(ANCHOR_ATTRIBUTE)).toBe(false);
  });

  test("is the innermost declared anchor around a target, past a name the grammar refuses", () => {
    expect(anchorAround(document.getElementById("buy"))?.getAttribute(ANCHOR_ATTRIBUTE)).toBe(
      "pricing",
    );
    expect(anchorAround(document.getElementById("loose"))).toBeNull();
  });
});

describe("the layout", () => {
  test("is reported only while the host asks, with each anchor's box in the document", async () => {
    window.dispatchEvent(new Event("resize"));
    await frame();
    expect(sent).toEqual([]);

    comments.receive({ mode: "show" });
    expect(layouts()).toHaveLength(1);
    const reported = layouts()[0]!.params;
    expect(reported["anchors"]).toEqual([{ name: "pricing", x: 0, y: 100, w: 800, h: 300 }]);
    expect(reported).toMatchObject({ scroll: { x: 0, y: 0 } });

    window.dispatchEvent(new Event("resize"));
    await frame();
    expect(layouts()).toHaveLength(2);

    comments.receive({ mode: "off" });
    window.dispatchEvent(new Event("resize"));
    await frame();
    expect(layouts()).toHaveLength(2);
  });

  test("moves with a part declared from script, once per frame however many asked", async () => {
    comments.receive({ mode: "show" });
    const el = document.getElementById("loose")!;
    box(el, 0, 500, 800, 40);
    comments.anchor("aside", el);
    window.dispatchEvent(new Event("scroll"));
    await frame();
    expect(layouts()).toHaveLength(2);
    expect(layouts()[1]!.params["anchors"]).toContainEqual({
      name: "aside",
      x: 0,
      y: 500,
      w: 800,
      h: 40,
    });
  });
});

describe("placing", () => {
  test("a click is the reader's pick, kept from the page's own handlers", () => {
    const pressed = vi.fn();
    document.getElementById("buy")!.addEventListener("click", pressed);
    comments.receive({ mode: "place" });
    const click = new MouseEvent("click", { bubbles: true, cancelable: true });
    document.getElementById("buy")!.dispatchEvent(click);
    expect(pressed).not.toHaveBeenCalled();
    expect(click.defaultPrevented).toBe(true);
    const pick = sent.find((s) => s.method === "comments.pick")!.params;
    expect(pick["name"]).toBe("pricing");
    expect(pick["quote"]).toBe("Plans Buy annual");
    expect(pick["point"]).toEqual({ x: 0, y: 0 });
  });

  test("a click outside every part is a point, with no words", () => {
    comments.receive({ mode: "place" });
    document.getElementById("loose")!.click();
    expect(sent.find((s) => s.method === "comments.pick")!.params).toMatchObject({
      name: null,
      quote: "",
    });
  });

  test("is not a pick while the host only shows pins", () => {
    comments.receive({ mode: "show" });
    const pressed = vi.fn();
    document.getElementById("buy")!.addEventListener("click", pressed);
    document.getElementById("buy")!.click();
    expect(pressed).toHaveBeenCalled();
    expect(sent.some((s) => s.method === "comments.pick")).toBe(false);
  });

  test("names the part under the pointer in the next layout", async () => {
    comments.receive({ mode: "place" });
    document
      .getElementById("buy")!
      .dispatchEvent(new MouseEvent("pointermove", { bubbles: true }) as PointerEvent);
    await frame();
    expect(layouts().at(-1)!.params["hover"]).toBe("pricing");
  });

  test("Escape tells the host, and only while placing", () => {
    comments.receive({ mode: "show" });
    comments.onKey(new KeyboardEvent("keydown", { key: "Escape", cancelable: true }));
    expect(sent.some((s) => s.method === "comments.escape")).toBe(false);
    comments.receive({ mode: "place" });
    const esc = new KeyboardEvent("keydown", { key: "Escape", cancelable: true });
    comments.onKey(esc);
    expect(esc.defaultPrevented).toBe(true);
    expect(sent.filter((s) => s.method === "comments.escape")).toHaveLength(1);
    comments.onKey(new KeyboardEvent("keydown", { key: "Escape", ctrlKey: true }));
    expect(sent.filter((s) => s.method === "comments.escape")).toHaveLength(1);
  });
});

describe("the host's requests", () => {
  test("a reveal brings a part or a point into view", () => {
    const section = document.querySelector("section")!;
    section.scrollIntoView = vi.fn();
    comments.receive({ reveal: { name: "pricing" } });
    expect(section.scrollIntoView).toHaveBeenCalledWith(
      expect.objectContaining({ block: "center" }),
    );
    const scrollTo = vi.spyOn(window, "scrollTo").mockImplementation(() => {});
    comments.receive({ reveal: { point: { x: 0.5, y: 0.5 } } });
    expect(scrollTo).toHaveBeenCalled();
    scrollTo.mockRestore();
  });

  test("a picture of a part is its box with a margin; one the page cannot draw is answered as none", async () => {
    expect(comments.regionOf({ name: "pricing" })).toMatchObject({ x: 0, y: 76 });
    comments.receive({ capture: { id: 4, name: "pricing" } });
    await vi.waitFor(() => expect(sent.some((s) => s.method === "comments.crop")).toBe(true));
    expect(sent.find((s) => s.method === "comments.crop")!.params).toEqual({ id: 4, image: null });
  });
});

describe("the words and the point", () => {
  test("a quote is cut at 1 KiB, never inside a character", () => {
    expect(clipBytes("abc", 1024)).toBe("abc");
    const cut = clipBytes("€".repeat(400), 1024);
    expect(cut).toBe("€".repeat(341));
    expect(new TextEncoder().encode(cut).length).toBeLessThanOrEqual(1024);
  });

  test("a point is a fraction of the document, held to it", () => {
    expect(fractionOf({ x: 640, y: 1000 }, { w: 1280, h: 4000 })).toEqual({ x: 0.5, y: 0.25 });
    expect(fractionOf({ x: -5, y: 9000 }, { w: 1280, h: 4000 })).toEqual({ x: 0, y: 1 });
  });
});
