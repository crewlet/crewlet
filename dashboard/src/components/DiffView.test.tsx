/**
 * The one diff the dashboard draws: what tells a line's kind, what it
 * underlines, the shapes it takes, and the window it mounts.
 *
 * Its colours are measured in `styles/diffs.test.ts`, against the installed
 * palette; what is held here is the half a rendered DOM can see — which rules
 * reach what the diff draws, and that none of them spends a hue.
 */

import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import { MAX_COMPARE_LINES, MAX_EDITS } from "~/lib/lineDiff.ts";
import { changedRuns } from "~/lib/wordDiff.ts";
import { sheetRules } from "~/test/sheets.ts";
import { DiffView, WINDOW_BLOCK_ROWS, WINDOW_FROM_ROWS, type DiffHunk } from "./DiffView.tsx";

// THE UNDERLINE'S READING, WATCHED: a pass-through, so every case below draws
// the real runs, and a long diff's cases can count which pairs were read.
vi.mock("~/lib/wordDiff.ts", async (importOriginal) => {
  const real = await importOriginal<typeof import("~/lib/wordDiff.ts")>();
  return { ...real, changedRuns: vi.fn(real.changedRuns) };
});

afterEach(cleanup);

/** A numbered line of a text: removed lines carry the older number, added the newer. */
const removed = (before: number, text: string) =>
  ({ kind: "remove", text, before, after: null }) as const;
const added = (after: number, text: string) =>
  ({ kind: "add", text, before: null, after }) as const;
const same = (before: number, after: number, text: string) =>
  ({ kind: "same", text, before, after }) as const;

/** The rendered lines as `[kind, text]`. */
const lines = (root: ParentNode) =>
  [...root.querySelectorAll<HTMLElement>(".diff-view__line")].map((line) => [
    line.dataset.kind,
    line.querySelector(".diff-view__text")?.textContent,
  ]);

/** Every underlined run, by the kind of the line it is in. */
const runs = (root: ParentNode) =>
  [...root.querySelectorAll<HTMLElement>(".diff-view__run")].map((run) => [
    run.closest<HTMLElement>(".diff-view__line")?.dataset.kind,
    run.textContent,
  ]);

const EDIT = {
  before: "# Settlement\n\nNets every balance first.\n\nEqual balances are paid in any order.\n",
  after:
    "# Settlement\n\nNets every balance first.\n\nEqual balances are ordered by join order.\n\nTies are rare.\n",
};

describe("a diff of a text", () => {
  test("marks each line's kind with a glyph and the word read in its place", () => {
    const { container } = render(<DiffView {...EDIT} />);
    const kinds = [...container.querySelectorAll<HTMLElement>(".diff-view__line")].map((line) => [
      line.dataset.kind,
      line.querySelector(".diff-view__mark")?.textContent,
    ]);
    // The glyph is hidden from assistive technology and the word read in its
    // place, so an unchanged line's mark is a space and nothing is said.
    expect(kinds).toEqual([
      ["same", " "],
      ["same", " "],
      ["same", " "],
      ["remove", "−Removed"],
      ["add", "+Added"],
      ["add", "+Added"],
      ["add", "+Added"],
    ]);
    for (const glyph of container.querySelectorAll(".diff-view__mark > [aria-hidden]")) {
      expect(glyph.getAttribute("aria-hidden")).toBe("true");
    }
  });

  // THE INVARIANT THE UNDERLINE IS FOR: the reader finds the edit in an
  // edited line without reading it twice — and nothing else is underlined:
  // not the unchanged lines, not a line only added.
  test("underlines only the changed run of a line edited", () => {
    const { container } = render(<DiffView {...EDIT} />);
    expect(runs(container)).toEqual([
      ["remove", "paid in any"],
      ["add", "ordered by join"],
    ]);
    // The run is inside the line, which still reads whole.
    expect(lines(container)).toContainEqual(["remove", "Equal balances are paid in any order."]);
  });

  test("numbers each line in the older text and the newer", () => {
    const { container } = render(<DiffView {...EDIT} context={0} />);
    const numbers = [...container.querySelectorAll<HTMLElement>(".diff-view__line")].map((line) =>
      [...line.querySelectorAll(".diff-view__number")].map((n) => n.textContent).join("|"),
    );
    expect(numbers).toEqual(["5|", "|5", "|6", "|7"]);
    const root = container.querySelector<HTMLElement>(".diff-view")!;
    expect(root.classList.contains("diff-view--numbered")).toBe(true);
    // The number tracks are the widest number's digits wide, on every row.
    expect(root.style.getPropertyValue("--diff-digits")).toBe("1");
  });

  // A GAP SAYS WHAT IT STANDS FOR: closed silently, a document edited at both
  // ends reads as one rewritten in the middle.
  test("opens a hunk on how many unchanged lines it skipped", () => {
    const hunks: DiffHunk[] = [
      { skipped: 12, lines: [same(13, 13, "kept"), removed(14, "gone")] },
      { skipped: 1, lines: [added(16, "new")] },
    ];
    const { container } = render(<DiffView hunks={hunks} />);
    expect(
      [...container.querySelectorAll(".diff-view__skip")].map((row) => row.textContent),
    ).toEqual(["12 unchanged lines", "1 unchanged line"]);
  });

  test("counts what was added and removed, said in words, against what it names", () => {
    render(<DiffView {...EDIT} against="rev 4" />);
    const group = screen.getByRole("group", { name: "Line changes" });
    const head = group.querySelector(".diff-view__head")!;
    expect(head.textContent).toBe("+3 −13 lines added, 1 removed against rev 4");
    // The drawn counts are hidden; the sentence is what is read.
    expect(head.querySelector("[aria-hidden='true']")?.textContent).toBe("+3 −1");
    expect(within(group).getByText("3 lines added, 1 removed")).toBeTruthy();
  });

  test("keeps an empty line's height", () => {
    const { container } = render(<DiffView before={"a\n\nb\n"} after={"a\nb\n"} />);
    expect(lines(container)).toEqual([
      ["same", "a"],
      ["remove", " "],
      ["same", "b"],
    ]);
  });

  // TWO IDENTICAL DOCUMENTS ARE A FACT: the host says what was compared, and
  // a pane of unmarked lines would read as one that failed to load.
  test("draws the host's sentence when nothing changed, and its own without one", () => {
    const { container, rerender } = render(
      <DiffView before={"same\n"} after={"same\n"} empty={<p>The body did not change</p>} />,
    );
    expect(container.textContent).toBe("The body did not change");
    expect(container.querySelector(".diff-view")).toBeNull();
    rerender(<DiffView before={"same\n"} after={"same\n"} />);
    expect(container.textContent).toBe("Nothing changed.");
  });
});

// AN UNDERLINE SAYS TWO LINES ARE ONE LINE EDITED, so it is drawn only where
// the change's shape lets position say which two: a text's lines carry
// nothing else to be paired by.
describe("which lines of a text are one line edited", () => {
  test("in a change that removes as many lines as it adds, each and the line in its place", () => {
    const hunks: DiffHunk[] = [
      {
        skipped: 0,
        lines: [
          removed(1, "- name: api"),
          removed(2, "- name: web"),
          removed(3, "- name: db"),
          added(1, "- name: gateway"),
          added(2, "- name: site"),
          added(3, "- name: store"),
        ],
      },
    ];
    const { container } = render(<DiffView hunks={hunks} />);
    expect(runs(container)).toEqual([
      ["remove", "api"],
      ["remove", "web"],
      ["remove", "db"],
      ["add", "gateway"],
      ["add", "site"],
      ["add", "store"],
    ]);
  });

  test.each([
    ["one line against several added after it", EDIT, "paid in any", "ordered by join"],
    [
      "several against one: lines deleted after the edited one",
      { before: EDIT.after, after: EDIT.before },
      "ordered by join",
      "paid in any",
    ],
  ])("in %s, the first line of each side", (_, texts, older, newer) => {
    const { container } = render(<DiffView {...texts} />);
    expect(runs(container)).toEqual([
      ["remove", older],
      ["add", newer],
    ]);
  });

  // TWO LINES OR MORE ON BOTH SIDES AND THE COUNTS UNEQUAL: the line one side
  // has beyond the other sits where position cannot see it, and every pair
  // below it is two neighbours, which share a key here.
  test("in nothing of an uneven change of two lines or more a side", () => {
    const hunks: DiffHunk[] = [
      {
        skipped: 0,
        lines: [
          removed(1, "- name: api"),
          removed(2, "- name: web"),
          added(1, "- name: queue"),
          added(2, "- name: api-v2"),
          added(3, "- name: web-v2"),
        ],
      },
    ];
    for (const layout of ["unified", "split"] as const) {
      const { container, unmount } = render(<DiffView hunks={hunks} layout={layout} />);
      // By position "api" would read as edited into "queue", and "web" into "api-v2".
      expect(runs(container), layout).toEqual([]);
      unmount();
    }
  });

  // A CHANGE'S LENGTH DECIDES NOTHING: a long run of lines each edited in
  // place is paired line by line exactly as a short one is.
  test("in a change of thousands of lines a side, as in a short one", () => {
    const restore = installObserver();
    try {
      const n = 2_001;
      const hunks: DiffHunk[] = [
        {
          skipped: 0,
          lines: [
            ...Array.from({ length: n }, (_, i) => removed(i + 1, `- name: item${i} old`)),
            ...Array.from({ length: n }, (_, i) => added(i + 1, `- name: item${i} new`)),
          ],
        },
      ];
      const { container } = render(<DiffView hunks={hunks} />);
      // The first block is the first removed lines, each underlined where it
      // differs from the line in its place.
      expect(runs(container)).toEqual(
        Array.from({ length: WINDOW_BLOCK_ROWS }, () => ["remove", "old"]),
      );
    } finally {
      restore();
    }
  });
});

// A TEXT IS COMPARED, NEVER REPLACED WHOLE. Past 2,000 lines a side the design
// system's line diff compares nothing and answers every older line removed and
// every newer one added, which drew a page edited on one line as a page
// rewritten; the dashboard's own line diff draws the edit the text had.
describe("a long text", () => {
  /** 2,001 lines: one past where the design system's line diff stops comparing. */
  const older = Array.from({ length: 2_001 }, (_, i) => `- name: item${i}`);

  test.each([
    [
      "edited in place",
      older.map((line, i) => (i === 1_000 ? `${line}-v2` : line)),
      [
        ["remove", "- name: item1000"],
        ["add", "- name: item1000-v2"],
      ],
      [["add", "-v2"]],
    ],
    ["inserted at the top", ["- name: queue", ...older], [["add", "- name: queue"]], []],
    [
      "moved to the end",
      [...older.slice(1), older[0]!],
      [
        ["remove", "- name: item0"],
        ["add", "- name: item0"],
      ],
      [],
    ],
  ])("draws one line %s as that line changed, among its context", (_, newer, changed, under) => {
    const { container } = render(
      <DiffView before={older.join("\n")} after={newer.join("\n")} against="rev 1" />,
    );
    const drawn = lines(container);
    expect(drawn.filter(([kind]) => kind !== "same")).toEqual(changed);
    // ONLY THE CONTEXT BESIDE IT: three lines either side, in at most two places.
    expect(drawn.length).toBeLessThanOrEqual(changed.length + 12);
    expect(runs(container)).toEqual(under);
    const count = (kind: string) => changed.filter(([k]) => k === kind).length;
    expect(container.querySelector(".diff-view__head [aria-hidden='true']")?.textContent).toBe(
      `+${count("add")} −${count("remove")}`,
    );
  });

  // A COMPARISON NOT MADE IS SAID, and no row is drawn: past the lines the
  // line diff reads, or the lines it marks, two texts are a sentence that says
  // which, never a diff that says something false.
  test.each([
    [
      "with more lines removed and added than the diff marks",
      Array.from({ length: MAX_EDITS / 2 + 1 }, (_, i) => `old ${i}`),
      Array.from({ length: MAX_EDITS / 2 + 1 }, (_, i) => `new ${i}`),
      "more than 4,000 lines were removed or added",
    ],
    [
      "whose change spans more lines than the diff reads",
      Array.from({ length: MAX_COMPARE_LINES + 1 }, (_, i) => `line ${i}`),
      [
        "first, edited",
        ...Array.from({ length: MAX_COMPARE_LINES - 1 }, (_, i) => `line ${i + 1}`),
        "last, edited",
      ],
      "what changed spans 20,001 lines, more than the 20,000 a comparison reads",
    ],
  ])("draws a sentence and no row for two texts %s", (_, before, after, why) => {
    const { container } = render(
      <DiffView
        before={before.join("\n")}
        after={after.join("\n")}
        against="rev 1"
        empty={<p>The body did not change</p>}
      />,
    );
    expect(container.textContent).toBe(`The two versions were not compared line by line: ${why}.`);
    expect(container.querySelector(".diff-view")).toBeNull();
    expect(container.querySelector(".diff-view__line")).toBeNull();
  });
});

describe("a split diff", () => {
  test("sets each removed line beside the line that replaced it", () => {
    const { container } = render(<DiffView {...EDIT} layout="split" context={1} />);
    const root = container.querySelector<HTMLElement>(".diff-view")!;
    expect(root.classList.contains("diff-view--split")).toBe(true);
    const pairs = [...container.querySelectorAll<HTMLElement>(".diff-view__pair")].map((pair) =>
      [...pair.children].map((side) =>
        side.classList.contains("diff-view__line")
          ? `${(side as HTMLElement).dataset.kind} ${side.querySelector(".diff-view__number")?.textContent}`
          : "none",
      ),
    );
    expect(pairs).toEqual([
      ["same 4", "same 4"],
      ["remove 5", "add 5"],
      ["none", "add 6"],
      ["none", "add 7"],
    ]);
    // An unchanged line is read once: its newer copy is hidden from a reader
    // who hears the diff.
    const [context] = container.querySelectorAll(".diff-view__pair");
    expect(context?.lastElementChild?.getAttribute("aria-hidden")).toBe("true");
    expect(context?.firstElementChild?.hasAttribute("aria-hidden")).toBe(false);
    // And the pair is still underlined where it changed.
    expect(runs(container)).toEqual([
      ["remove", "paid in any"],
      ["add", "ordered by join"],
    ]);
  });

  // TWO COLUMNS OF CODE UNDER 640PX ARE A WORD OR TWO EACH, so a diff that
  // narrow draws unified whatever it was asked for.
  test("is unified on a diff as narrow as a phone", () => {
    const width = vi.spyOn(Element.prototype, "clientWidth", "get").mockReturnValue(390);
    try {
      const { container } = render(<DiffView {...EDIT} layout="split" />);
      expect(container.querySelector(".diff-view__pair")).toBeNull();
      expect(container.querySelector(".diff-view--split")).toBeNull();
      expect(lines(container)).toContainEqual(["add", "Ties are rare."]);
    } finally {
      width.mockRestore();
    }
  });
});

describe("a diff of settings", () => {
  const hunks: DiffHunk[] = [
    {
      skipped: 0,
      lines: [{ kind: "remove", text: '"a"', before: null, after: null, path: "x.y" }],
    },
    {
      skipped: 0,
      lines: [
        {
          kind: "remove",
          text: '"https://a.example.com"',
          before: null,
          after: null,
          path: "x.url",
        },
        { kind: "add", text: '"https://a.example.org"', before: null, after: null, path: "x.url" },
      ],
    },
  ];

  test("draws each line's path where a text draws its numbers", () => {
    const { container } = render(<DiffView hunks={hunks} />);
    const root = container.querySelector<HTMLElement>(".diff-view")!;
    expect(root.classList.contains("diff-view--keyed")).toBe(true);
    expect(root.classList.contains("diff-view--numbered")).toBe(false);
    expect(container.querySelector(".diff-view__number")).toBeNull();
    expect(
      [...container.querySelectorAll(".diff-view__path")].map((path) => path.textContent),
    ).toEqual(["x.y", "x.url", "x.url"]);
    expect(root.style.getPropertyValue("--diff-path")).toBe(String("x.url".length));
    expect(runs(container)).toEqual([
      ["remove", "com"],
      ["add", "org"],
    ]);
  });

  // A CHANGE CAN HOLD ONE SETTING REMOVED AND ANOTHER EDITED: paired by
  // position, the removed setting would be underlined against the edited one.
  test("underlines a removed value only against the value of its own path", () => {
    const { container } = render(
      <DiffView
        hunks={[
          {
            skipped: 0,
            lines: [
              { kind: "remove", text: "port 80", before: null, after: null, path: "a" },
              { kind: "remove", text: "port 81", before: null, after: null, path: "b" },
              { kind: "add", text: "port 82", before: null, after: null, path: "b" },
            ],
          },
        ]}
      />,
    );
    expect(runs(container)).toEqual([
      ["remove", "81"],
      ["add", "82"],
    ]);
  });

  test("is never split: a setting's two values are one path read down", () => {
    const { container } = render(<DiffView hunks={hunks} layout="split" />);
    expect(container.querySelector(".diff-view__pair")).toBeNull();
  });
});

/** The token a sheet's rule for exactly `selector` paints its background with. */
function paintOf(selector: string): string | undefined {
  const rule = sheetRules().find((r) => r.selector === selector);
  return /background(?:-color)?:\s*var\((--[\w-]+)\)/.exec(rule?.body ?? "")?.[1];
}

// A HOST WRITES ON A DIFF: what it anchors to a line is drawn under that
// line, once however the diff is laid out, and a line is picked by its own
// number — its newer one, else its older — which is the number a finding
// names. Mutation: draw a note under every line, or put the pick on an added
// line's older slot, and this fails.
describe("a diff a host writes on", () => {
  const HUNKS: DiffHunk[] = [
    {
      skipped: 0,
      lines: [same(1, 1, "keep"), removed(2, "old"), added(2, "new"), same(3, 3, "end")],
    },
  ];

  test("draws a host's notes under the line they are on, and only there", () => {
    for (const layout of ["unified", "split"] as const) {
      const { container, unmount } = render(
        <DiffView
          hunks={HUNKS}
          layout={layout}
          notes={(line) => (line.after === 2 ? <p>finding on 2</p> : null)}
        />,
      );
      const notes = container.querySelectorAll(".diff-view__notes");
      expect(notes, layout).toHaveLength(1);
      expect(notes[0]!.textContent).toBe("finding on 2");
      unmount();
    }
  });

  test("picks a line by its own number, and marks what the host picked", () => {
    const pick = vi.fn();
    render(<DiffView hunks={HUNKS} onPick={pick} picked={(line) => line.after === 3} />);
    act(() => screen.getByRole("button", { name: "Line 2: pick it" }).click());
    expect(pick).toHaveBeenCalledWith(HUNKS[0]!.lines[2], false);
    act(() => screen.getByRole("button", { name: "Removed line 2: pick it" }).click());
    expect(pick).toHaveBeenLastCalledWith(HUNKS[0]!.lines[1], false);
    // ONE BUTTON A LINE, on its own number: four lines, four buttons.
    expect(screen.getAllByRole("button")).toHaveLength(4);
    const marked = document.querySelectorAll<HTMLElement>(".diff-view__line[data-picked]");
    expect([...marked].map((l) => l.querySelector(".diff-view__text")?.textContent)).toEqual([
      "end",
    ]);
  });

  test("draws no button where the host picks nothing", () => {
    render(<DiffView hunks={HUNKS} />);
    expect(screen.queryAllByRole("button")).toHaveLength(0);
  });
});

// NO HUE. The design system's diffs paint an added line in the success
// state's tint and a removed one in the danger state's; here colour carries
// state, and a diff is not one. Every rule that can reach what the diff draws
// is read, so a rule added later for any class it carries is held too. The
// accent is not in the list: it says where the reader is (a focus ring, a
// range they picked), which is never a line's kind.
describe("what a diff draws", () => {
  test("is reached by no rule that spends a state's, a series' or an identity's hue", () => {
    const { container } = render(<DiffView {...EDIT} layout="split" against="rev 4" />);
    const classes = new Set(
      [...container.querySelectorAll("[class]")].flatMap((el) => [...el.classList]),
    );
    expect(classes.has("diff-view__line")).toBe(true);
    const reaching = sheetRules().filter((rule) =>
      [...classes].some((name) => new RegExp(`\\.${name}(?![\\w-])`).test(rule.selector)),
    );
    // A VACUITY FLOOR: a walk that found no rule for the diff checked nothing.
    expect(reaching.length).toBeGreaterThan(10);
    const hued = reaching.filter((rule) => /--color-(?:feedback|data|node)-/.test(rule.body));
    expect(hued.map((rule) => `${rule.selector} { ${rule.body.trim()} }`)).toEqual([]);
    // AND NONE OF THE PACKAGE'S DIFFS IS IN IT.
    expect([...classes].filter((name) => name.startsWith("crewlet-diff"))).toEqual([]);
  });

  // THE TWO STEPS AND THEIR GROUND are read from the sheet and matched to what
  // is drawn: jsdom resolves no custom property and expands no shorthand, so
  // the measurement of the three colours is `styles/diffs.test.ts`'s.
  test("paints its two steps off the ground it paints itself", () => {
    const { container } = render(<DiffView {...EDIT} />);
    expect(paintOf(".diff-view__body")).toBe("--color-surface-subtle");
    const added = '.diff-view__line[data-kind="add"]';
    const removed = '.diff-view__line[data-kind="remove"]';
    expect(paintOf(added)).toBe("--color-surface-elevated");
    expect(paintOf(removed)).toBe("--color-surface-background");
    for (const line of container.querySelectorAll<HTMLElement>(".diff-view__line")) {
      expect(line.matches(added), line.textContent ?? "").toBe(line.dataset.kind === "add");
      expect(line.matches(removed), line.textContent ?? "").toBe(line.dataset.kind === "remove");
      expect(line.closest(".diff-view__body")).not.toBeNull();
    }
    const run = sheetRules().find((r) => r.selector === ".diff-view__run");
    expect(run?.body).toMatch(/text-decoration-line:\s*underline/);
  });
});

/**
 * An IntersectionObserver the case drives: jsdom has none, and the window's
 * whole behaviour is what the observer reports.
 */
class Observer {
  static made: Observer[] = [];
  readonly watched = new Set<Element>();
  constructor(
    readonly report: IntersectionObserverCallback,
    readonly options: IntersectionObserverInit | undefined,
  ) {
    Observer.made.push(this);
  }
  observe(el: Element) {
    this.watched.add(el);
  }
  unobserve(el: Element) {
    this.watched.delete(el);
  }
  disconnect() {
    this.watched.clear();
  }
  takeRecords() {
    return [];
  }
  /** Says which blocks are near the view now, with the height each had. */
  say(blocks: { block: number; near: boolean; height?: number }[]) {
    const entries = blocks.map(({ block, near, height = 0 }) => ({
      target: [...this.watched].find((el) => (el as HTMLElement).dataset.block === String(block)),
      isIntersecting: near,
      boundingClientRect: { height },
    }));
    act(() => this.report(entries as never, this as never));
  }
}

/** Puts `value` in place of the global observer, and hands back what puts the old one back. */
function replaceObserver(value: unknown): () => void {
  const had = Object.getOwnPropertyDescriptor(globalThis, "IntersectionObserver");
  if (value === undefined)
    delete (globalThis as { IntersectionObserver?: unknown }).IntersectionObserver;
  else
    Object.defineProperty(globalThis, "IntersectionObserver", {
      configurable: true,
      writable: true,
      value,
    });
  return () => {
    if (had) Object.defineProperty(globalThis, "IntersectionObserver", had);
    else delete (globalThis as { IntersectionObserver?: unknown }).IntersectionObserver;
  };
}

function installObserver(): () => void {
  Observer.made = [];
  return replaceObserver(Observer);
}

/** One hunk of `n` added lines. */
const longDiff = (n: number): DiffHunk[] => [
  { skipped: 0, lines: Array.from({ length: n }, (_, i) => added(i + 1, `line ${i + 1}`)) },
];

/** `n` lines each edited in place: a hunk each, its old line removed and its new one added. */
const editedLines = (n: number): DiffHunk[] =>
  Array.from({ length: n }, (_, i) => ({
    skipped: 0,
    lines: [removed(i + 1, `line ${i + 1} was`), added(i + 1, `line ${i + 1} is`)],
  }));

describe("a long diff", () => {
  test("mounts only the blocks near the view, and keeps the others' height", () => {
    const restore = installObserver();
    try {
      const rows = 30 * WINDOW_BLOCK_ROWS;
      const { container } = render(
        <div style={{ overflowY: "auto" }} data-testid="scroller">
          <DiffView hunks={longDiff(rows)} />
        </div>,
      );
      const blocks = () => [...container.querySelectorAll<HTMLElement>(".diff-view__block")];
      // BEFORE THE OBSERVER HAS SPOKEN, the first block alone.
      expect(container.querySelectorAll(".diff-view__line")).toHaveLength(WINDOW_BLOCK_ROWS);
      expect(blocks()).toHaveLength(30);
      const [observer, ...more] = Observer.made;
      expect(more).toEqual([]);
      // ITS ROOT IS THE DIFF'S OWN SCROLLER, with one view's margin either
      // side: a margin on the viewport never reaches past the scroller's clip.
      expect(observer!.options?.root).toBe(screen.getByTestId("scroller"));
      expect(observer!.options?.rootMargin).toBe("100% 0px");
      expect(observer!.watched.size).toBe(30);

      // The reader scrolls: the first block leaves, measured as it goes, and
      // two blocks further down come within a view.
      observer!.say([
        { block: 0, near: false, height: 2400 },
        { block: 7, near: true },
        { block: 8, near: true },
      ]);
      expect(container.querySelectorAll(".diff-view__line")).toHaveLength(2 * WINDOW_BLOCK_ROWS);
      expect(lines(container)[0]).toEqual(["add", `line ${7 * WINDOW_BLOCK_ROWS + 1}`]);
      const first = blocks()[0]!;
      expect(first.childElementCount).toBe(0);
      expect(first.getAttribute("aria-hidden")).toBe("true");
      // THE HEIGHT IT HAD, so nothing above the reader's place moves; and a
      // block never drawn takes the measured block's height per row.
      expect(first.style.height).toBe("2400px");
      expect(blocks()[20]!.style.height).toBe("2400px");

      // A BLOCK TAKEN IN AND LET GO BEFORE IT WAS DRAWN is still its empty
      // box, so what it measured as it left is that box's height — an
      // estimate, never kept as the block's own.
      observer!.say([
        { block: 12, near: true },
        { block: 12, near: false, height: 999 },
      ]);
      expect(blocks()[12]!.childElementCount).toBe(0);
      expect(blocks()[12]!.style.height).toBe("2400px");
    } finally {
      restore();
    }
  });

  // A HIDDEN PANEL LAYS NOTHING OUT: its zeros are no row's height, and taken
  // as one they would bring every block within the window when it is shown.
  test("takes no measurement from a box with no height", () => {
    const restore = installObserver();
    try {
      const { container } = render(<DiffView hunks={longDiff(30 * WINDOW_BLOCK_ROWS)} />);
      Observer.made[0]!.say([{ block: 0, near: false, height: 0 }]);
      const [first, second] = container.querySelectorAll<HTMLElement>(".diff-view__block");
      expect(first!.childElementCount).toBe(0);
      expect(first!.style.height).not.toBe("0px");
      expect(first!.style.height).toBe(second!.style.height);
    } finally {
      restore();
    }
  });

  // THE WINDOW BOUNDS THE READING, not only the mounting: a pair's words are
  // read once one of its lines is drawn, so a diff of any length reads, before
  // its first paint, the pairs it draws and no others.
  test("reads only the pairs of the blocks it holds, and each of them once", () => {
    const restore = installObserver();
    try {
      const read = vi.mocked(changedRuns);
      read.mockClear();
      // Unified, an edited line is two rows: 30 blocks of 50 pairs each.
      const { container } = render(<DiffView hunks={editedLines(15 * WINDOW_BLOCK_ROWS)} />);
      const pairsABlock = WINDOW_BLOCK_ROWS / 2;
      expect(container.querySelectorAll(".diff-view__block")).toHaveLength(30);
      expect(read).toHaveBeenCalledTimes(pairsABlock);
      expect(runs(container).slice(0, 2)).toEqual([
        ["remove", "was"],
        ["add", "is"],
      ]);

      const observer = Observer.made[0]!;
      observer.say([
        { block: 0, near: false, height: 2400 },
        { block: 7, near: true },
        { block: 8, near: true },
      ]);
      expect(read).toHaveBeenCalledTimes(3 * pairsABlock);
      // A BLOCK TAKEN IN AGAIN reads nothing twice, and underlines as it did.
      observer.say([{ block: 0, near: true }]);
      expect(read).toHaveBeenCalledTimes(3 * pairsABlock);
      expect(runs(container)).toHaveLength(2 * 3 * pairsABlock);
    } finally {
      restore();
    }
  });

  test("reads a split row's pair once for both of its sides", () => {
    const restore = installObserver();
    try {
      const read = vi.mocked(changedRuns);
      read.mockClear();
      // Split, an edited line is one row: the first block is 100 pairs.
      const { container } = render(
        <DiffView hunks={editedLines(15 * WINDOW_BLOCK_ROWS)} layout="split" />,
      );
      expect(container.querySelectorAll(".diff-view__pair")).toHaveLength(WINDOW_BLOCK_ROWS);
      expect(runs(container)).toHaveLength(2 * WINDOW_BLOCK_ROWS);
      expect(read).toHaveBeenCalledTimes(WINDOW_BLOCK_ROWS);
    } finally {
      restore();
    }
  });

  test("is mounted whole up to the windowing threshold", () => {
    const restore = installObserver();
    try {
      const { container } = render(<DiffView hunks={longDiff(WINDOW_FROM_ROWS)} />);
      expect(container.querySelectorAll(".diff-view__line")).toHaveLength(WINDOW_FROM_ROWS);
      // Below it no observer is asked for at all.
      expect(Observer.made).toEqual([]);
    } finally {
      restore();
    }
  });

  // NO OBSERVER, NO WINDOW: slower, and never wrong.
  test("is mounted whole where the browser has no observer", () => {
    const restore = replaceObserver(undefined);
    try {
      const rows = WINDOW_FROM_ROWS + WINDOW_BLOCK_ROWS;
      const { container } = render(<DiffView hunks={longDiff(rows)} />);
      expect(container.querySelectorAll(".diff-view__line")).toHaveLength(rows);
    } finally {
      restore();
    }
  });
});
