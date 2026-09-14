/**
 * The three affordances a JSON panel is: copy it, save it, and select it.
 *
 * All three are asserted here rather than on a screen because each was WRONG
 * in the same way before, or would be — silently. A copy that reached no
 * clipboard clicked exactly like one that did, select-all took the whole
 * document while looking like it had done something, and a download whose
 * anchor carries no `download` attribute opens the JSON in a tab rather than
 * saving it, which reads as success to everyone including the reader. A test
 * that only rendered the buttons would have passed through all of them.
 */

import { StrictMode, type ReactElement } from "react";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { Code, CopyButton, DownloadButton } from "./primitives.tsx";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  // `vi.unstubAllGlobals` does not reach a property defined on `document`, so
  // a stub left here is a stub the next test inherits — and the clipboard's
  // fallback branch turns on whether this exists at all.
  delete (document as Partial<Document>).execCommand;
});

/** Give the component the Clipboard API, and report what it was handed. */
function withClipboard(): { written: string[]; fail?: boolean } {
  const state: { written: string[]; fail?: boolean } = { written: [] };
  vi.stubGlobal("navigator", {
    ...globalThis.navigator,
    clipboard: {
      writeText: (text: string) => {
        if (state.fail) return Promise.reject(new Error("refused"));
        state.written.push(text);
        return Promise.resolve();
      },
    },
  });
  return state;
}

async function click(label: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: new RegExp(label, "i") }));
  });
}

describe("CopyButton", () => {
  test("puts the text on the clipboard and says it did", async () => {
    const clipboard = withClipboard();
    render(<CopyButton text='{"turn_id":"t-1"}' />);

    await click("copy");

    expect(clipboard.written).toEqual(['{"turn_id":"t-1"}']);
    // THE FEEDBACK IS THE FEATURE. The clipboard is invisible; without the
    // control saying so, a working copy and a dead button are the same
    // event.
    expect(screen.getByRole("button").textContent).toContain("Copied");
  });

  test("falls back to execCommand where the Clipboard API is absent", async () => {
    // Which is not a hypothetical: the API is gated on a secure context, so
    // it is simply undefined on the http://<lan-ip>:8000 anyone reads the
    // dashboard of a node that is not their laptop at.
    vi.stubGlobal("navigator", { ...globalThis.navigator, clipboard: undefined });
    const copied: string[] = [];
    const exec = vi.fn(() => {
      const field = document.querySelector("textarea");
      copied.push(field?.value ?? "");
      return true;
    });
    Object.defineProperty(document, "execCommand", {
      writable: true,
      configurable: true,
      value: exec,
    });

    render(<CopyButton text="fallback text" />);
    await click("copy");

    expect(exec).toHaveBeenCalledWith("copy");
    expect(copied).toEqual(["fallback text"]);
    expect(screen.getByRole("button").textContent).toContain("Copied");
    // The scratch field is not left behind for the next reader to tab into.
    expect(document.querySelector("textarea")).toBeNull();
  });

  test("says so when the browser refuses, rather than looking like it worked", async () => {
    const clipboard = withClipboard();
    clipboard.fail = true;
    Object.defineProperty(document, "execCommand", {
      writable: true,
      configurable: true,
      value: () => false,
    });

    render(<CopyButton text="anything" />);
    await click("copy");

    expect(screen.getByRole("button").textContent).toContain("Copy failed");
  });

  test("settles back to Copy so the label is never a stale claim", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    withClipboard();
    render(<CopyButton text="x" />);

    await click("copy");
    expect(screen.getByRole("button").textContent).toContain("Copied");

    await act(async () => {
      vi.advanceTimersByTime(2500);
    });
    const label = screen.getByRole("button").textContent ?? "";
    expect(label).toContain("Copy");
    expect(label).not.toContain("Copied");
  });
  test("a refusal does NOT settle back, because the reader may not be watching", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const clipboard = withClipboard();
    clipboard.fail = true;
    Object.defineProperty(document, "execCommand", {
      writable: true,
      configurable: true,
      value: () => false,
    });
    render(<CopyButton text="x" />);

    await click("copy");
    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });

    // Reverting this would put the failure back into the state the control
    // exists to leave: a button offering its action again is
    // indistinguishable from one that was never pressed.
    expect(screen.getByRole("button").textContent).toContain("Copy failed");
    expect(screen.getByRole("status").textContent).toBe("the browser refused the clipboard");
  });

  test("an answer that arrives after the screen is gone arms nothing", async () => {
    vi.useFakeTimers();
    let settle: (() => void) | undefined;
    vi.stubGlobal("navigator", {
      ...globalThis.navigator,
      clipboard: {
        writeText: () =>
          new Promise<void>((resolve) => {
            settle = resolve;
          }),
      },
    });

    const view = render(<CopyButton text="x" />);
    await click("copy");
    // Chrome can hold `clipboard-write` behind a permission prompt, so the
    // promise is still outstanding when the reader navigates away.
    view.unmount();
    const armed = vi.getTimerCount();

    await act(async () => {
      settle?.();
      await Promise.resolve();
    });

    // The unmount cleanup has already run; a late answer that armed the reset
    // timer would arm the one timer nothing can clear.
    expect(vi.getTimerCount()).toBe(armed);
  });

  test("resolves a thunk on click, so a live record is not serialized per frame", async () => {
    const clipboard = withClipboard();
    let calls = 0;
    const produce = () => {
      calls += 1;
      return "assembled once";
    };
    const { rerender } = render(<CopyButton text={produce} />);
    // Re-rendered as a streamed frame would: the thunk is not called.
    rerender(<CopyButton text={produce} />);
    rerender(<CopyButton text={produce} />);
    expect(calls).toBe(0);

    await click("copy");
    expect(calls).toBe(1);
    expect(clipboard.written).toEqual(["assembled once"]);
  });

  test("a StrictMode remount leaves the control still able to answer", async () => {
    // The other half of the `live` flag. StrictMode mounts, unmounts and
    // mounts again, so a flag only ever CLEARED on the way out stays cleared,
    // and every click after that would resolve into a component that has
    // decided it is gone.
    const clipboard = withClipboard();
    render(
      <StrictMode>
        <CopyButton text="x" />
      </StrictMode>,
    );

    await click("copy");

    expect(clipboard.written).toEqual(["x"]);
    expect(screen.getByRole("button").textContent).toContain("Copied");
  });

  test("a refusal replaces the offer in the tooltip with the reason", async () => {
    // The at-rest `title` describes what the button WILL do, which is the one
    // thing that just did not happen. Every route passes one.
    const clipboard = withClipboard();
    clipboard.fail = true;
    Object.defineProperty(document, "execCommand", {
      writable: true,
      configurable: true,
      value: () => false,
    });
    render(<CopyButton text="anything" title="copy this record" />);

    const button = screen.getByRole("button");
    expect(button.getAttribute("title")).toBe("copy this record");
    await click("copy");
    expect(button.getAttribute("title")).toBe("the browser refused the clipboard");
  });

  test("the status text is not part of the button's accessible name", async () => {
    withClipboard();
    render(<CopyButton text="x" />);
    await click("copy");

    // getByRole matches on the accessible NAME, so an exact-name query is
    // the assertion: with the live region inside the button, the control
    // was named "Copied copied to the clipboard".
    expect(screen.getByRole("button", { name: "Copied" })).toBeDefined();
    expect(screen.getByRole("status").textContent).toBe("copied to the clipboard");
  });
});

describe("DownloadButton", () => {
  /**
   * What the primitive handed the browser: the blob it built, the anchor it
   * clicked, and the object URL it freed afterwards.
   *
   * THE BLOB IS THE ASSERTION POINT, never the `blob:` URL that comes back.
   * Vitest 5's jsdom shim builds that URL by reading `blob[impl]._buffer`,
   * which jsdom 30 renamed to `_bytes` — so resolving one yields the nine-byte
   * string "undefined" for every payload, silently. The blob object itself is
   * jsdom's own and its `text()` is exact.
   *
   * The click is intercepted in the CAPTURE phase and cancelled, which does
   * two jobs at once: it reads the anchor at the instant the primitive clicked
   * it — href and download already set, still in the document — and it stops
   * jsdom following the hyperlink, which it does regardless of `download` and
   * reports as an unattributed "Not implemented: navigation to another
   * Document" line at the end of the run.
   */
  let seen: {
    blobs: Blob[];
    clicked: { href: string; download: string; connected: boolean }[];
    revoked: string[];
  };
  let watchAnchors: (event: Event) => void;

  beforeEach(() => {
    seen = { blobs: [], clicked: [], revoked: [] };
    vi.spyOn(URL, "createObjectURL").mockImplementation((blob: Blob | MediaSource) => {
      seen.blobs.push(blob as Blob);
      return `blob:test/${seen.blobs.length}`;
    });
    vi.spyOn(URL, "revokeObjectURL").mockImplementation((url: string) => {
      seen.revoked.push(url);
    });
    watchAnchors = (event: Event) => {
      const target = event.target as HTMLElement;
      if (target instanceof HTMLAnchorElement) {
        seen.clicked.push({
          href: target.href,
          download: target.download,
          connected: target.isConnected,
        });
        event.preventDefault();
      }
    };
    document.addEventListener("click", watchAnchors, true);
  });

  afterEach(async () => {
    // DRAIN THE REVOKE the primitive armed for the next macrotask, before the
    // spies that record it are torn down.
    //
    // A test that leaves it pending hands it to the NEXT test's spies, which is
    // not a hypothetical: the counter restarts at 1 each time, so the leaked
    // call recorded `blob:test/1` into a ledger whose own click had not
    // happened yet, and "frees the object URL" went red about one run in
    // fourteen. This afterEach is registered after the file's own, and hooks
    // run last-registered-first, so it drains while the spies and the fake
    // clock are still installed.
    await act(async () => {
      if (vi.isFakeTimers()) vi.advanceTimersByTime(1);
      else await new Promise((resolve) => setTimeout(resolve, 0));
    });
    document.removeEventListener("click", watchAnchors, true);
  });

  test("hands the browser the text, under the name it was given", async () => {
    render(<DownloadButton text='{"turn_id":"t-1"}' filename="turn-t-1.json" />);

    await click("download");

    expect(seen.blobs).toHaveLength(1);
    expect(await seen.blobs[0]!.text()).toBe('{"turn_id":"t-1"}');
    expect(seen.blobs[0]!.type).toBe("application/json;charset=utf-8");
    expect(seen.clicked).toEqual([
      { href: "blob:test/1", download: "turn-t-1.json", connected: true },
    ]);
    // THE FEEDBACK IS THE FEATURE, as it is for the clipboard: a download
    // lands in a folder the reader is not looking at.
    expect(screen.getByRole("button").textContent).toContain("Downloading");
    // And the announcement NAMES the file, since "Downloaded" tells a reader
    // who cannot see the download shelf nothing about what to go and open.
    expect(screen.getByRole("status").textContent).toBe("download started — turn-t-1.json");
    // The scratch anchor is not left behind for the next reader to tab into.
    expect(document.querySelector("a")).toBeNull();
  });

  test("frees the object URL, but only after the click that reads it", async () => {
    // WITHOUT `shouldAdvanceTime`, unlike the reset case below: this timer is
    // armed for zero, so a clock that also advances on its own fires it
    // before the assertion that it has not.
    vi.useFakeTimers();
    render(<DownloadButton text="body" filename="x.json" />);

    await click("download");
    // Not yet: the click QUEUES the download, and revoking inside the same
    // task races the fetch that is about to read the entry.
    expect(seen.revoked).toEqual([]);

    await act(async () => {
      vi.advanceTimersByTime(1);
    });
    // …and not never, either: the blob is a second copy of the whole turn,
    // and a reader comparing turns clicks this several times.
    expect(seen.revoked).toEqual(["blob:test/1"]);
  });

  test("a second identical refusal still reaches the live region", async () => {
    // An `aria-live` region announces CHANGES, and a refusal holds rather
    // than settling back — so with the text alone, the second click and every
    // one after it was silence for a reader who cannot see the button.
    vi.spyOn(URL, "createObjectURL").mockImplementation(() => {
      throw new Error("refused");
    });
    render(<DownloadButton text="x" filename="x.json" />);

    await click("download");
    const first = screen.getByRole("status").firstElementChild;
    await click("download");

    expect(screen.getByRole("status").textContent).toBe("the browser refused the download");
    // A NEW NODE, not the same one re-rendered: that is the change.
    expect(screen.getByRole("status").firstElementChild).not.toBe(first);
  });

  test("resolves a thunk on click, so a live turn is not serialized per frame", async () => {
    let calls = 0;
    const produce = () => {
      calls += 1;
      return "assembled once";
    };
    const { rerender } = render(<DownloadButton text={produce} filename="x.json" />);
    // Re-rendered as a streamed frame would: the thunk is not called.
    rerender(<DownloadButton text={produce} filename="x.json" />);
    rerender(<DownloadButton text={produce} filename="x.json" />);
    expect(calls).toBe(0);

    await click("download");
    expect(calls).toBe(1);
    expect(await seen.blobs[0]!.text()).toBe("assembled once");
  });

  test("says so when the browser refuses, rather than looking like it worked", async () => {
    vi.spyOn(URL, "createObjectURL").mockImplementation(() => {
      throw new Error("refused");
    });

    render(<DownloadButton text="anything" filename="x.json" />);
    await click("download");

    expect(screen.getByRole("button").textContent).toContain("Download failed");
    expect(screen.getByRole("status").textContent).toBe("the browser refused the download");
  });

  test("refuses outright where an anchor cannot carry a download", async () => {
    // The one failure worse than a dead button. Without the attribute the
    // same click NAVIGATES to the JSON — a tab full of text, which looks
    // enough like something happening that nobody checks it saved.
    const proto = HTMLAnchorElement.prototype;
    const had = Object.getOwnPropertyDescriptor(proto, "download")!;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    delete (proto as any).download;
    try {
      render(<DownloadButton text="anything" filename="x.json" />);
      await click("download");
    } finally {
      Object.defineProperty(proto, "download", had);
    }

    expect(seen.clicked).toEqual([]);
    expect(seen.blobs).toEqual([]);
    expect(screen.getByRole("button").textContent).toContain("Download failed");
  });

  test("offers a name a filesystem will take, whatever it was handed", async () => {
    // A turn id comes off the URL, so the name is composed from data. The
    // `download` attribute is only a SUGGESTION — each engine sanitises it
    // its own way — so it is decided here instead, once.
    render(<DownloadButton text="x" filename={"../etc/pa ss\nwd"} />);

    await click("download");

    expect(seen.clicked[0]!.download).toBe("etc-pa-ss-wd");
    expect(screen.getByRole("status").textContent).toBe("download started — etc-pa-ss-wd");
  });

  test("keeps a name whose letters are not Latin, extension and all", async () => {
    // `\w` is ASCII-only, so an ASCII class collapsed the whole stem to one
    // `-` and the leading-strip then took that hyphen AND the extension's own
    // dot with it: this arrived as a file called `json`.
    render(<DownloadButton text="x" filename={"日本語-résumé.json"} />);

    await click("download");

    expect(seen.clicked[0]!.download).toBe("日本語-résumé.json");
  });

  test("cuts a long name to a byte budget, not a code-unit one", async () => {
    // MAX_FILENAME is 120 BYTES — every filesystem in its comment counts
    // bytes — and `slice` counts UTF-16 code units, which agree only for
    // ASCII. The sanitizer deliberately keeps letters in every script (the
    // case above exists for that), so this is reachable rather than
    // hypothetical: 120 units of Japanese is 360 bytes, past ext4's 255 and
    // well past eCryptfs's 143.
    render(<DownloadButton text="x" filename={"日".repeat(400) + ".json"} />);

    await click("download");

    const name = seen.clicked[0]!.download as string;
    expect(new TextEncoder().encode(name).length).toBeLessThanOrEqual(120);
    // The extension SURVIVES the cut — a name that loses `.json` opens in the
    // wrong application — and what was cut is the stem.
    expect(name.endsWith(".json")).toBe(true);
    expect(name.startsWith("日")).toBe(true);
  });

  // A cut between the halves of a surrogate pair leaves a lone surrogate, which
  // is not valid UTF-8: it reaches the disk as U+FFFD, so the name the reader
  // sees is not the name that was cut.
  //
  // SEVERAL EXTENSIONS, because one is not a test. `𠮷` is two UTF-16 units and
  // four UTF-8 bytes, so a cut that walks units lands between the halves only
  // when the budget divides to an ODD count — with `.json` it happens to come
  // out even and reassembles into whole characters by luck. Varying the
  // extension varies the budget, so the boundary falls both ways.
  test.each([".json", ".md", ".txt", ".yaml"])(
    "never cuts a character in half (%s)",
    async (ext) => {
      render(<DownloadButton text="x" filename={"𠮷".repeat(200) + ext} />);

      await click("download");

      const name = seen.clicked[0]!.download as string;
      expect(new TextEncoder().encode(name).length).toBeLessThanOrEqual(120);
      // With the `u` flag a well-formed pair is ONE code point, so this class
      // matches only a surrogate left on its own.
      expect(/\p{Surrogate}/u.test(name)).toBe(false);
      // And the round trip is lossless, which a lone surrogate would not be:
      // encoding one yields U+FFFD and decoding gives back a different string.
      expect(new TextDecoder().decode(new TextEncoder().encode(name))).toBe(name);
    },
  );

  test("gives a name with no stem left one, rather than a bare extension", async () => {
    render(<DownloadButton text="x" filename={"../.json"} />);

    await click("download");

    expect(seen.clicked[0]!.download).toBe("download.json");
  });

  test("collapses runs of separators rather than emitting them", async () => {
    render(<DownloadButton text="x" filename={"turn - 1 -- draft.json"} />);

    await click("download");

    expect(seen.clicked[0]!.download).toBe("turn-1-draft.json");
  });

  test("keeps the extension when a name is too long to keep whole", async () => {
    render(<DownloadButton text="x" filename={`${"t".repeat(400)}.json`} />);

    const name = await click("download").then(() => seen.clicked[0]!.download);
    expect(name).toHaveLength(120);
    // A name trimmed to fit that loses its `.json` opens in the wrong
    // application on every desktop there is.
    expect(name.endsWith(".json")).toBe(true);
  });
});

describe("Code", () => {
  test("a selectable block takes ⌘A / Ctrl+A for itself", () => {
    render(
      <div>
        <p>page furniture nobody asked to select</p>
        <Code selectable label="The turn record, as JSON">
          {'{"turn_id":"t-1"}'}
        </Code>
      </div>,
    );
    const block = screen.getByRole("region", { name: "The turn record, as JSON" });

    // A CYRILLIC LAYOUT: the physical A key reports `key: "ф"`. Browsers
    // resolve select-all from the key's POSITION, so a handler matching only
    // `e.key` declines here and the page-wide select-all it exists to
    // replace happens instead.
    const event = new KeyboardEvent("keydown", {
      key: "ф",
      code: "KeyA",
      ctrlKey: true,
      bubbles: true,
      cancelable: true,
    });
    block.dispatchEvent(event);

    // Handled: the browser's document-wide select-all never runs.
    expect(event.defaultPrevented).toBe(true);
    const selection = window.getSelection();
    expect(selection?.toString()).toBe('{"turn_id":"t-1"}');
    // Scoped: the paragraph beside it is not in the range.
    expect(selection?.toString()).not.toContain("page furniture");
  });

  test("plain ⌘A elsewhere in the block's own keys is left alone", () => {
    render(
      <Code selectable label="A block">
        {"body"}
      </Code>,
    );
    const block = screen.getByRole("region");

    for (const init of [
      { key: "a", code: "KeyA" }, // no modifier: typing, not selecting
      { key: "a", code: "KeyA", ctrlKey: true, altKey: true }, // a different chord
      // Ctrl+Shift+A is Chrome's tab search. Swallowing a chord the browser
      // owns takes it away and puts nothing in its place.
      { key: "A", code: "KeyA", ctrlKey: true, shiftKey: true },
      { key: "c", code: "KeyC", ctrlKey: true }, // copy, which the browser must keep
    ]) {
      const event = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, ...init });
      block.dispatchEvent(event);
      expect(event.defaultPrevented).toBe(false);
    }
  });

  test("a short block is not a tab stop and takes no keys", () => {
    // The ordinary case — a tool call's arguments that fit in the box — must
    // not become one. There are dozens of them on one phase card, and a
    // keyboard reader would have to step through every one.
    const { container } = render(<Code label="Arguments">{"body"}</Code>);
    const block = container.querySelector("pre")!;
    expect(block.getAttribute("tabindex")).toBeNull();
    expect(block.getAttribute("role")).toBeNull();

    const event = new KeyboardEvent("keydown", {
      key: "a",
      ctrlKey: true,
      bubbles: true,
      cancelable: true,
    });
    block.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  });

  // A BLOCK THAT SCROLLS IS REACHABLE, WHETHER OR NOT IT OWNS ⌘A.
  //
  // `.code` is `overflow: auto` with a 460px cap, so any block taller than
  // that is a scroll container — and in Chrome and Safari a scroll container
  // with no tabindex cannot be scrolled by keyboard at all. The blocks that
  // hit the cap are the ones that matter most: a phase card's verbatim system
  // prompt runs to tens of kilobytes, and it was unreachable.
  test("a block tall enough to scroll becomes a named, focusable region", () => {
    const tall = overflowing(<Code label="The execute phase's system prompt">{"body"}</Code>);
    expect(tall.getAttribute("tabindex")).toBe("0");
    expect(tall.getAttribute("role")).toBe("region");
    expect(tall.getAttribute("aria-label")).toBe("The execute phase's system prompt");
  });

  // BOTH AXES. `plain` sets `white-space: pre`, so a wide line scrolls
  // sideways inside a box that is nowhere near tall enough to scroll down —
  // measuring only the height leaves exactly the code blocks unreachable.
  test("a block that only scrolls SIDEWAYS is reachable too", () => {
    const wide = overflowing(
      <Code plain label="Arguments">
        {"{}"}
      </Code>,
      "width",
    );
    expect(wide.getAttribute("tabindex")).toBe("0");
  });

  // …AND A SCROLLING BLOCK STILL DOES NOT SWALLOW ⌘A unless it asked to. The
  // focus is for scrolling; select-all is a separate opt-in, and taking the
  // key on every tall block would change what ⌘A means on any screen with one.
  test("a scrolling block that did not ask for select-all still declines it", () => {
    const tall = overflowing(<Code label="Arguments">{"body"}</Code>);
    const event = new KeyboardEvent("keydown", {
      code: "KeyA",
      key: "a",
      ctrlKey: true,
      bubbles: true,
      cancelable: true,
    });
    tall.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  });
});

/**
 * Render a Code block whose box overflows, and hand back the `pre`.
 *
 * jsdom lays nothing out, so every scroll and client dimension it reports is
 * 0 and no block can ever overflow on its own. Overriding the pair the
 * component compares is what makes the branch reachable at all — and it is
 * the real branch: the same measurement a browser answers differently.
 */

function overflowing(node: ReactElement, axis: "height" | "width" = "height"): HTMLElement {
  const scroll = axis === "height" ? "scrollHeight" : "scrollWidth";
  const client = axis === "height" ? "clientHeight" : "clientWidth";
  const original = {
    scroll: Object.getOwnPropertyDescriptor(HTMLElement.prototype, scroll),
    client: Object.getOwnPropertyDescriptor(HTMLElement.prototype, client),
  };
  Object.defineProperty(HTMLElement.prototype, scroll, { configurable: true, value: 4000 });
  Object.defineProperty(HTMLElement.prototype, client, { configurable: true, value: 460 });
  try {
    const { container } = render(node);
    return container.querySelector("pre")!;
  } finally {
    // Restored before the assertions run, so one case cannot leave every
    // later one measuring a box jsdom never laid out.
    if (original.scroll) Object.defineProperty(HTMLElement.prototype, scroll, original.scroll);
    else delete (HTMLElement.prototype as unknown as Record<string, unknown>)[scroll];
    if (original.client) Object.defineProperty(HTMLElement.prototype, client, original.client);
    else delete (HTMLElement.prototype as unknown as Record<string, unknown>)[client];
  }
}
