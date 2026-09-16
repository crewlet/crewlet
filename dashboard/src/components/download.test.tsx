/**
 * Saving a record as a file, which is the one affordance the design system
 * does not draw.
 *
 * Asserted here rather than on a screen because every failure this control has
 * is SILENT. A download whose anchor carries no `download` attribute opens the
 * JSON in a tab instead of saving it, which reads as success to everyone
 * including the reader; a blob URL freed in the same task as the click races
 * the fetch that is about to read it, and loses on a slow machine; and a name
 * composed from a turn id off the URL reaches the disk however the browser
 * feels like sanitising it. A test that only rendered the button would pass
 * through all of them.
 *
 * Its sibling, uilet's `CopyButton`, is asserted in `uilet.test.tsx` over the
 * same bytes. The two are a pair by design: one gesture for a thread, one for
 * a bug report.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { DownloadButton } from "./common.tsx";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

async function click(label: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: new RegExp(label, "i") }));
  });
}

/**
 * What the control handed the browser: the blob it built, the anchor it
 * clicked, and the object URL it freed afterwards.
 *
 * THE BLOB IS THE ASSERTION POINT, never the `blob:` URL that comes back.
 * Vitest 5's jsdom shim builds that URL by reading `blob[impl]._buffer`, which
 * jsdom 30 renamed to `_bytes`, so resolving one yields the nine-byte string
 * "undefined" for every payload, silently. The blob object itself is jsdom's
 * own and its `text()` is exact.
 *
 * The click is intercepted in the CAPTURE phase and cancelled, which does two
 * jobs at once: it reads the anchor at the instant the control clicked it,
 * with href and download already set and still in the document, and it stops
 * jsdom following the hyperlink, which it does regardless of `download` and
 * reports as an unattributed "Not implemented: navigation to another Document"
 * line at the end of the run.
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
  // DRAIN THE REVOKE the control armed for the next macrotask, before the
  // spies that record it are torn down.
  //
  // A test that leaves it pending hands it to the NEXT test's spies, which is
  // not a hypothetical: the counter restarts at 1 each time, so the leaked
  // call recorded `blob:test/1` into a ledger whose own click had not happened
  // yet, and "frees the object URL" went red about one run in fourteen. This
  // hook is registered after the file's own, and hooks run
  // last-registered-first, so it drains while the spies and the fake clock are
  // still installed.
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
  // THE FEEDBACK IS THE FEATURE, as it is for the clipboard: a download lands
  // in a folder the reader is not looking at.
  expect(screen.getByRole("button").textContent).toContain("Downloading");
  // And the announcement NAMES the file, since "Downloaded" tells a reader who
  // cannot see the download shelf nothing about what to go and open.
  expect(screen.getByRole("status").textContent).toBe("download started, turn-t-1.json");
  // The scratch anchor is not left behind for the next reader to tab into.
  expect(document.querySelector("a")).toBeNull();
});

test("frees the object URL, but only after the click that reads it", async () => {
  // WITHOUT `shouldAdvanceTime`: this timer is armed for zero, so a clock that
  // also advances on its own fires it before the assertion that it has not.
  vi.useFakeTimers();
  render(<DownloadButton text="body" filename="x.json" />);

  await click("download");
  // Not yet: the click QUEUES the download, and revoking inside the same task
  // races the fetch that is about to read the entry.
  expect(seen.revoked).toEqual([]);

  await act(async () => {
    vi.advanceTimersByTime(1);
  });
  // …and not never, either: the blob is a second copy of the whole record, and
  // a reader comparing two of them clicks this several times.
  expect(seen.revoked).toEqual(["blob:test/1"]);
});

test("a second identical refusal still reaches the live region", async () => {
  // An `aria-live` region announces CHANGES, and a refusal holds rather than
  // settling back, so with the text alone the second click and every one after
  // it was silence for a reader who cannot see the button.
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

test("resolves a thunk on click, so a live record is not serialized per frame", async () => {
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

test("a refusal replaces the offer in the tooltip with the reason", async () => {
  // The at-rest `title` describes what the control WILL do, which is the one
  // thing that just did not happen. Every call site passes one, so a tooltip
  // left saying "the same JSON, saved as a file" over a control that has just
  // refused is the screen telling a reader the opposite of what happened.
  vi.spyOn(URL, "createObjectURL").mockImplementation(() => {
    throw new Error("refused");
  });
  render(<DownloadButton text="anything" filename="x.json" title="the record, saved as a file" />);

  const button = screen.getByRole("button");
  expect(button.getAttribute("title")).toBe("the record, saved as a file");
  await click("download");
  expect(button.getAttribute("title")).toBe("the browser refused the download");
});

test("the status text is not part of the control's accessible name", async () => {
  render(<DownloadButton text="x" filename="turn-t-1.json" />);
  await click("download");

  // getByRole matches on the accessible NAME, so an exact-name query is the
  // assertion: a live region rendered INSIDE the button rather than beside it
  // names the control "Downloading download started, turn-t-1.json", which is
  // read out in full every time a reader lands on it afterwards.
  expect(screen.getByRole("button", { name: "Downloading" })).toBeDefined();
  expect(screen.getByRole("status").textContent).toBe("download started, turn-t-1.json");
});

test("refuses outright where an anchor cannot carry a download", async () => {
  // The one failure worse than a dead button. Without the attribute the same
  // click NAVIGATES to the payload, a tab full of text, which looks enough
  // like something happening that nobody checks it saved.
  const proto = HTMLAnchorElement.prototype;
  const had = Object.getOwnPropertyDescriptor(proto, "download")!;
  delete (proto as Partial<HTMLAnchorElement>).download;
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
  // `download` attribute is only a SUGGESTION, sanitised its own way by each
  // engine, so it is decided here instead, once.
  render(<DownloadButton text="x" filename={"../etc/pa ss\nwd"} />);

  await click("download");

  expect(seen.clicked[0]!.download).toBe("etc-pa-ss-wd");
  expect(screen.getByRole("status").textContent).toBe("download started, etc-pa-ss-wd");
});

test("keeps a name whose letters are not Latin, extension and all", async () => {
  // `\w` is ASCII-only, so an ASCII class collapsed the whole stem to one `-`
  // and the leading strip then took that hyphen AND the extension's own dot
  // with it: this arrived as a file called `json`.
  render(<DownloadButton text="x" filename={"日本語-résumé.json"} />);

  await click("download");

  expect(seen.clicked[0]!.download).toBe("日本語-résumé.json");
});

test("cuts a long name to a byte budget, not a code-unit one", async () => {
  // MAX_FILENAME is 120 BYTES, since every filesystem in its comment counts
  // bytes, and `slice` counts UTF-16 code units, which agree only for ASCII.
  // The sanitizer deliberately keeps letters in every script (the case above
  // exists for that), so this is reachable rather than hypothetical: 120 units
  // of Japanese is 360 bytes, past ext4's 255 and well past eCryptfs's 143.
  render(<DownloadButton text="x" filename={"日".repeat(400) + ".json"} />);

  await click("download");

  const name = seen.clicked[0]!.download;
  expect(new TextEncoder().encode(name).length).toBeLessThanOrEqual(120);
  // The extension SURVIVES the cut, since a name that loses `.json` opens in
  // the wrong application, and what was cut is the stem.
  expect(name.endsWith(".json")).toBe(true);
  expect(name.startsWith("日")).toBe(true);
});

// A cut between the halves of a surrogate pair leaves a lone surrogate, which
// is not valid UTF-8: it reaches the disk as U+FFFD, so the name the reader
// sees is not the name that was cut.
//
// SEVERAL EXTENSIONS, because one is not a test. `𠮷` is two UTF-16 units and
// four UTF-8 bytes, so a cut that walks units lands between the halves only
// when the budget divides to an ODD count; with `.json` it happens to come out
// even and reassembles into whole characters by luck. Varying the extension
// varies the budget, so the boundary falls both ways.
test.each([".json", ".md", ".txt", ".yaml"])("never cuts a character in half (%s)", async (ext) => {
  render(<DownloadButton text="x" filename={"𠮷".repeat(200) + ext} />);

  await click("download");

  const name = seen.clicked[0]!.download;
  expect(new TextEncoder().encode(name).length).toBeLessThanOrEqual(120);
  // With the `u` flag a well-formed pair is ONE code point, so this class
  // matches only a surrogate left on its own.
  expect(/\p{Surrogate}/u.test(name)).toBe(false);
  // And the round trip is lossless, which a lone surrogate would not be:
  // encoding one yields U+FFFD and decoding gives back a different string.
  expect(new TextDecoder().decode(new TextEncoder().encode(name))).toBe(name);
});

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

test("the confirmation settles back, and a refusal holds until the next press", async () => {
  // Two seconds after a download the reader is looking at the browser's shelf
  // rather than at the button, and a control that has reverted to offering its
  // action is indistinguishable from one that was never pressed. A REFUSAL is
  // not on that clock: it holds until the click a reader who wants to retry
  // makes anyway.
  vi.useFakeTimers({ shouldAdvanceTime: true });
  render(<DownloadButton text="x" filename="x.json" />);

  await click("download");
  expect(screen.getByRole("button").textContent).toContain("Downloading");
  await act(async () => {
    vi.advanceTimersByTime(2000);
  });
  expect(screen.getByRole("button").textContent).toContain("Download");
  expect(screen.getByRole("status").textContent).toBe("");

  vi.spyOn(URL, "createObjectURL").mockImplementation(() => {
    throw new Error("refused");
  });
  await click("download");
  await act(async () => {
    vi.advanceTimersByTime(10_000);
  });
  expect(screen.getByRole("button").textContent).toContain("Download failed");
});

test("the label, the tooltip and the type are the caller's to set", async () => {
  // Turn's control says "Download turn" beside a copy control that says
  // "Copy", because two bare verbs on one head name neither of the two things
  // an operator does with a record.
  render(
    <DownloadButton
      text="a,b\n1,2"
      filename="spend.csv"
      mime="text/csv;charset=utf-8"
      label="Download turn"
      title="the same JSON, saved as a file"
    />,
  );
  const button = screen.getByRole("button", { name: "Download turn" });
  expect(button.getAttribute("title")).toBe("the same JSON, saved as a file");

  await click("download turn");
  expect(seen.blobs[0]!.type).toBe("text/csv;charset=utf-8");
});
