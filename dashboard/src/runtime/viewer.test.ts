/**
 * Which links the runtime opens in its file viewer: every link to one of the
 * artifact's own files the frame cannot hold, and nothing the frame or the
 * page itself already handles.
 */
import { afterAll, afterEach, beforeAll, describe, expect, test } from "vitest";
import { breakpoint, color, font, radius, size, spacing } from "@crewlethq/tokens";
import {
  drawnAs,
  interceptFileLinks,
  KIT,
  kindOf,
  labelOf,
  linkedFiles,
  viewerFile,
  type ViewerItem,
} from "./viewer.ts";

const TICKET = "/t/tk_0123456789abcdef";
const ORIGIN = window.location.origin;
const at = (path: string) => new URL(path, ORIGIN);

describe("the file a link names", () => {
  test.each([
    ["a picture", `${TICKET}/img/before.png`, "img/before.png"],
    ["an SVG, which the frame would load with no runtime", `${TICKET}/diagram.svg`, "diagram.svg"],
    ["a PDF", `${TICKET}/docs/report.pdf`, "docs/report.pdf"],
    ["a video", `${TICKET}/clip.mp4`, "clip.mp4"],
    ["a file named with escapes, decoded", `${TICKET}/img/before%20fix.png`, "img/before fix.png"],
    [
      "an asset, which is never a page",
      `${TICKET}/_assets/0192f00d-0000-7000-8000-0000000000a5`,
      "_assets/0192f00d-0000-7000-8000-0000000000a5",
    ],
    [
      "a file the version may not hold, which the viewer says is not there",
      `${TICKET}/notes`,
      "notes",
    ],
  ])("is the viewer's for %s", (_, path, file) => {
    expect(viewerFile(at(path), ORIGIN, TICKET)).toBe(file);
  });

  test.each([
    ["a page", `${TICKET}/about.html`],
    ["a page in upper case", `${TICKET}/ABOUT.HTM`],
    ["a page with a query and a fragment", `${TICKET}/about.html?tab=2#team`],
    ["the ticket's own document", `${TICKET}/`],
    ["another ticket's file", "/t/tk_fedcba9876543210/img/before.png"],
    ["the runtime and kit beside the tickets", "/_crewlet/kit/0.159.0/kit.css"],
    ["a malformed escape", `${TICKET}/img/%E0%A4%A.png`],
  ])("is the frame's for %s", (_, path) => {
    expect(viewerFile(at(path), ORIGIN, TICKET)).toBeNull();
  });

  test("is another site's for a picture elsewhere", () => {
    expect(viewerFile(new URL("https://example.com/t/tk/img/a.png"), ORIGIN, TICKET)).toBeNull();
  });

  test("is any file of the origin's for a page served under no ticket", () => {
    expect(viewerFile(at("/img/a.png"), ORIGIN, "")).toBe("img/a.png");
    expect(viewerFile(at("/index.html"), ORIGIN, "")).toBeNull();
  });
});

describe("a link's label", () => {
  const link = (html: string) => {
    const holder = document.createElement("div");
    holder.innerHTML = html;
    return holder.firstElementChild!;
  };

  test("is its picture's alternative text first", () => {
    expect(
      labelOf(
        link(`<a href="a.png" title="t"><img alt=" Before  the fix " src="a.png"> Open</a>`),
        "a.png",
      ),
    ).toBe("Before the fix");
  });

  test("is its own label, then its title, then its words, past a decorative picture", () => {
    expect(labelOf(link(`<a href="a.png" aria-label="Chart" title="t">x</a>`), "a.png")).toBe(
      "Chart",
    );
    expect(labelOf(link(`<a href="a.png" title="Chart">x</a>`), "a.png")).toBe("Chart");
    expect(labelOf(link(`<a href="a.png"><img alt="" src="a.png"> After</a>`), "a.png")).toBe(
      "After",
    );
  });

  test("is the file's own name where the link says nothing", () => {
    expect(labelOf(link(`<a href="img/a.png"><img src="img/a.png"></a>`), "img/a.png")).toBe(
      "a.png",
    );
  });
});

describe("a click on a link", () => {
  // THE PAGE IS SERVED UNDER ITS TICKET, so its relative links resolve beside it.
  beforeAll(() => window.history.replaceState(null, "", `${TICKET}/index.html`));
  afterAll(() => window.history.replaceState(null, "", "/"));

  let stops: (() => void)[] = [];
  afterEach(() => {
    for (const stop of stops.splice(0)) stop();
    document.body.replaceChildren();
  });

  /**
   * The page's links, watched as the runtime watches them, with what each
   * click opened and whether it was kept from the frame. A listener after the
   * runtime's keeps jsdom from a navigation it does not implement.
   */
  function page(html: string) {
    document.body.innerHTML = html;
    const opened: { items: ViewerItem[]; at: number; opener: Element }[] = [];
    stops.push(
      interceptFileLinks({
        window,
        ticketBase: TICKET,
        open: (items, at, opener) => opened.push({ items, at, opener }),
      }),
    );
    let kept = false;
    const last = (event: Event) => {
      kept = event.defaultPrevented;
      event.preventDefault();
    };
    window.addEventListener("click", last);
    stops.push(() => window.removeEventListener("click", last));
    const click = (selector: string | Element, init: MouseEventInit = {}) => {
      const target = typeof selector === "string" ? document.querySelector(selector)! : selector;
      target.dispatchEvent(
        new MouseEvent("click", { bubbles: true, cancelable: true, composed: true, ...init }),
      );
      return kept;
    };
    return { opened, click };
  }

  const PAGE = `
    <h1>Before and after</h1>
    <a id="before" href="${TICKET}/img/before.png"><img alt="Before" src="${TICKET}/img/before.png"></a>
    <a id="page" href="${TICKET}/notes.html">Notes</a>
    <a id="after" href="img/after.png#zoom"><img alt="After" src="img/after.png"></a>
    <a id="again" href="${TICKET}/img/before.png">The first picture again</a>
    <a id="away" href="https://example.com/a.png">Elsewhere</a>
    <a id="report" href="${TICKET}/report.pdf"><span id="inside">The report</span></a>
    <a id="here" href="#top">Top</a>`;

  test("to a picture opens it in the viewer, beside the page's other files, and keeps the frame", () => {
    const { opened, click } = page(PAGE);
    expect(click("#after")).toBe(true);
    expect(opened).toHaveLength(1);
    expect(opened[0]!.items).toEqual([
      { url: `${ORIGIN}${TICKET}/img/before.png`, file: "img/before.png", label: "Before" },
      { url: `${ORIGIN}${TICKET}/img/after.png`, file: "img/after.png", label: "After" },
      { url: `${ORIGIN}${TICKET}/report.pdf`, file: "report.pdf", label: "The report" },
    ]);
    expect(opened[0]!.at).toBe(1);
    expect(opened[0]!.opener).toBe(document.getElementById("after"));
  });

  test("on something inside the link opens what the link names", () => {
    const { opened, click } = page(PAGE);
    expect(click("#inside")).toBe(true);
    expect(opened[0]!.items[opened[0]!.at]!.file).toBe("report.pdf");
  });

  test.each([
    ["a page, which the frame follows", "#page"],
    ["another site, as it always was", "#away"],
    ["a place on this page", "#here"],
  ])("to %s is left alone", (_, selector) => {
    const { opened, click } = page(PAGE);
    expect(click(selector)).toBe(false);
    expect(opened).toEqual([]);
  });

  test.each([
    ["Control", { ctrlKey: true }],
    ["Meta", { metaKey: true }],
    ["Shift", { shiftKey: true }],
    ["Alt", { altKey: true }],
    ["a button other than the main one", { button: 1 }],
  ])("with %s is the browser's", (_, init) => {
    const { opened, click } = page(PAGE);
    expect(click("#before", init)).toBe(false);
    expect(opened).toEqual([]);
  });

  test("the page handled itself is the page's", () => {
    const { opened, click } = page(PAGE);
    const own = (event: Event) => event.preventDefault();
    document.addEventListener("click", own);
    stops.push(() => document.removeEventListener("click", own));
    expect(click("#before")).toBe(true);
    expect(opened).toEqual([]);
  });

  test("to an image map's area or an SVG link opens the file it names", () => {
    const { opened, click } = page(`
      <map name="m"><area id="area" href="${TICKET}/img/a.png" alt="Region" shape="rect" coords="0,0,9,9"></map>
      <svg><a id="drawn" href="${TICKET}/img/b.png"><text>B</text></a></svg>`);
    expect(click("#area")).toBe(true);
    expect(click("#drawn")).toBe(true);
    expect(opened.map((o) => o.items[o.at]!.label)).toEqual(["Region", "B"]);
  });

  test("inside a shadow root opens that file alone", () => {
    const { opened, click } = page(`<div id="host"></div>`);
    const root = document.getElementById("host")!.attachShadow({ mode: "open" });
    root.innerHTML = `<a id="shadowed" href="${TICKET}/img/c.png">C</a>`;
    expect(click(root.getElementById("shadowed")!)).toBe(true);
    expect(opened).toHaveLength(1);
    expect(opened[0]!.items).toEqual([
      { url: `${ORIGIN}${TICKET}/img/c.png`, file: "img/c.png", label: "C" },
    ]);
  });
});

test("the files a page links are each listed once, in its order, pages and other sites left out", () => {
  document.body.innerHTML = `
    <a href="${TICKET}/b.png">B</a><a href="${TICKET}/a.html">A</a>
    <a href="${TICKET}/a.png">A</a><a href="${TICKET}/b.png">B again</a>
    <a href="mailto:someone@example.com">Mail</a><a>No address</a>`;
  expect(linkedFiles(document, ORIGIN, TICKET).map((i) => i.file)).toEqual(["b.png", "a.png"]);
  document.body.replaceChildren();
});

test.each([
  ["image/png", "image", "File"],
  ["image/svg+xml", "image", "File"],
  ["video/mp4", "video", "File"],
  ["audio/mpeg", "audio", "File"],
  ["application/pdf", "file", "PDF"],
  ["font/woff2", "file", "Font"],
  ["model/gltf-binary", "file", "3D model"],
  ["application/wasm", "file", "WebAssembly"],
  ["text/csv; charset=utf-8", "file", "Text"],
  ["application/json", "file", "Text"],
  ["text/html; charset=utf-8", "file", "Web page"],
])("a file served as %s is drawn as %s", (type, drawn, kind) => {
  expect(drawnAs(type)).toBe(drawn);
  if (drawn === "file") expect(kindOf(type)).toBe(kind);
});

test("the viewer is drawn with the design kit's own values", () => {
  expect(KIT).toEqual({
    surfaceGlass: color.surface.glass,
    surfaceElevated: color.surface.elevated,
    textPrimary: color.text.primary,
    textSecondary: color.text.secondary,
    textTertiary: color.text.tertiary,
    borderDefault: color.border.default,
    borderStrongHover: color.border.strongHover,
    focus: color.focus,
    fontSans: font.family.sans,
    size2xs: font.size["2xs"],
    sizeXs: font.size.xs,
    sizeSm: font.size.sm,
    sizeMd: font.size.md,
    weightSemibold: font.weight.semibold,
    radiusLg: radius.lg,
    radiusCircle: radius.circle,
    controlLg: size.control.lg,
    spacing2: spacing[2],
    spacing3: spacing[3],
    spacing6: spacing[6],
    breakpointPhone: breakpoint.phone,
  });
});
