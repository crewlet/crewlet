/**
 * Copy link says what the browser did, including when it said no.
 *
 * The engine serves the dashboard over plain HTTP on `api.port` unless
 * something terminates TLS in front of it, and an insecure origin has NO
 * `navigator.clipboard` at all — so the ordinary deployment is the one where
 * the write cannot even be attempted. The button used to call it through an
 * optional chain, which skipped the whole thing: no copy, no word, a button
 * that looked broken.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import { Breadcrumb, CopyLink, StarPage } from "./PageHeader.tsx";

const had = Object.getOwnPropertyDescriptor(navigator, "clipboard");

function clipboard(value: unknown): void {
  Object.defineProperty(navigator, "clipboard", { configurable: true, value });
}

afterEach(() => {
  cleanup();
  if (had) Object.defineProperty(navigator, "clipboard", had);
  else delete (navigator as unknown as Record<string, unknown>).clipboard;
});

async function press(): Promise<void> {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Copy link|Copied|Blocked/ }));
  });
}

describe("Copy link", () => {
  test("an insecure origin, with no clipboard at all, says Blocked", async () => {
    clipboard(undefined);
    render(<CopyLink />);
    await press();
    expect(screen.getByRole("button").textContent).toBe("Blocked");
  });

  test("a write the browser refuses says Blocked", async () => {
    clipboard({ writeText: vi.fn().mockRejectedValue(new Error("NotAllowedError")) });
    render(<CopyLink />);
    await press();
    expect(screen.getByRole("button").textContent).toBe("Blocked");
  });

  test("a write that lands copies the whole URL and says Copied", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    clipboard({ writeText });
    render(<CopyLink />);
    await press();
    expect(writeText).toHaveBeenCalledWith(location.href);
    expect(screen.getByRole("button").textContent).toBe("Copied");
  });
});

// A STAR IS A SHORTCUT TO A PAGE, so it is offered on a page and nowhere else.
describe("the star", () => {
  test("is offered on an object's page", () => {
    render(<StarPage path={["work", "ENG-42"]} label="ENG-42" workspace="work" />);
    expect(screen.getByRole("button", { name: "Keep in Starred" })).toBeTruthy();
  });

  // The store drops a kept path that does not resolve, so a star pressed on
  // Not Found filled and then vanished on the next read, with nothing said.
  test("is not offered on an address that is no page", () => {
    render(<StarPage path={["work", "tasks"]} label="Not found" workspace="work" />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  // A WORKSPACE'S OWN PAGE is a sidebar row already, so the star cannot keep
  // it — but it is drawn there, unavailable and saying why, because it was
  // drawn on every section of a workspace but the first and the header's
  // controls moved between two tabs of one workspace.
  test("is drawn on a workspace's own page, unavailable, with the reason", () => {
    render(<StarPage path={["me"]} label="My work" workspace="me" />);
    const star = screen.getByRole("button", { name: "Keep in Starred" });
    expect(star.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getByText("Already in the sidebar")).toBeTruthy();
    fireEvent.click(star);
    expect(localStorage.getItem("crewlet_starred") ?? "[]").toBe("[]");
  });
});

// THE TRAIL IS A TAB STOP ONLY WHILE IT HAS SOMEWHERE TO SCROLL. It was one
// on every page, and a trail that fits (nearly every trail) took a Tab press
// that did nothing.
describe("the breadcrumb", () => {
  const crumbs = [
    { label: "Live", path: ["live"] },
    { label: "Turns", path: ["live", "turns"] },
    { label: "Replied in the thread about the observability pipeline." },
  ];
  const had = {
    scroll: Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollWidth"),
    client: Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth"),
  };
  function widths(scroll: number, client: number): void {
    Object.defineProperty(HTMLElement.prototype, "scrollWidth", {
      configurable: true,
      get: () => scroll,
    });
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get: () => client,
    });
  }
  afterEach(() => {
    for (const [key, d] of [
      ["scrollWidth", had.scroll],
      ["clientWidth", had.client],
    ] as const) {
      if (d) Object.defineProperty(HTMLElement.prototype, key, d);
      else delete (HTMLElement.prototype as unknown as Record<string, unknown>)[key];
    }
  });
  const trail = () => screen.getByRole("navigation", { name: "Breadcrumb" });

  test("a trail that fits takes no tab stop", () => {
    widths(400, 400);
    render(<Breadcrumb crumbs={crumbs} />);
    expect(trail().hasAttribute("tabindex")).toBe(false);
  });

  test("a trail wider than its box is a tab stop, so the keyboard can scroll it", () => {
    widths(697, 494);
    render(<Breadcrumb crumbs={crumbs} />);
    expect(trail().getAttribute("tabindex")).toBe("0");
  });

  // EACH LABEL IS ITS OWN BOX, the one `.crumb-text` ellipsises: text straight
  // inside the flex part is an anonymous flex item no ellipsis reaches.
  test("every crumb's words sit in the box that ellipsises, the page's h1 included", () => {
    widths(400, 400);
    render(<Breadcrumb crumbs={crumbs} />);
    const h1 = screen.getByRole("heading", { level: 1 });
    expect(h1.querySelector(":scope > .crumb-text")?.textContent).toBe(crumbs[2]!.label);
    for (const link of trail().querySelectorAll("a.crumb-link")) {
      expect(link.querySelector(":scope > .crumb-text")).not.toBeNull();
    }
  });
});
