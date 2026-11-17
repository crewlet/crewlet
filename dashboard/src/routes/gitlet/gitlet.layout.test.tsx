/**
 * GITLET'S PAGES, LAID OUT: a comparison's title stays clear of the files'
 * sticky toolbar under it, the revision picker and the clone panel stay inside
 * the viewport, and code coloured by kind keeps the box it had plain.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { InertWebSocket, LayoutClient } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { DiffView, type DiffHunk } from "~/components/DiffView.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import type { CodeChangeSummary } from "~/protocol/index.ts";
import { ChangeRows, ChangeStateTag } from "./ChangeRow.tsx";
import { Changes } from "./Changes.tsx";
import { BranchChip } from "./ui.tsx";
import { ComparePage } from "./commit/ComparePage.tsx";
import { RepositoryPage } from "./repo/RepositoryPage.tsx";
import { SYNTAX_CLASS } from "./syntax.tsx";

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const BASE = "3a3ecae0123456789abcdef0123456789abcdef0";
const HEAD = "74066b10123456789abcdef0123456789abcdef0";

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [{ name: "Jane Founder", handle: "jane", kind: "human" }],
  units: [],
};

const json = (payload: unknown) =>
  new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

/** A Go file whose third line holds a keyword, a call, a string and a comment, wider than a phone. */
const MAIN_GO = [
  "package main",
  "",
  'func main() { serve(":8080") } // the storefront, on the port the shell and the health check both read',
  "",
];

/** Ten branches by when each moved, one named past any panel's width. */
const MOVED = [
  "main",
  `feature/${"a-very-long-branch-name-".repeat(5)}end`,
  ...Array.from({ length: 8 }, (_, i) => `topic/${i}`),
].map((name, i) => ({
  ref: `refs/heads/${name}`,
  oid: BASE,
  position: POSITION,
  moved_at: new Date(Date.UTC(2026, 10, 1, 12 - i)).toISOString(),
  ...(name === "main"
    ? { protection: { patterns: ["refs/heads/main"], direct_push: "none" } }
    : {}),
}));

/** The changes Gitlet › Changes lists and the repositories its pickers offer, where a case sets them. */
let listed: CodeChangeSummary[] | null = null;
let repos: string[] | null = null;

/** The engine, for one repository's comparison of feature into main, and one file. */
function answer(url: URL): Response {
  switch (url.pathname) {
    case "/query/code_changes":
      if (!listed) break;
      return json({
        changes: url.searchParams.has("limit") ? [] : listed,
        position: POSITION,
        level: "session",
      });
    case "/query/code_repos":
      if (!repos) break;
      return json({ repos: repos.map((name) => ({ name })), position: POSITION, level: "session" });
    case "/query/code_blob":
      if (url.searchParams.get("path") !== "cmd/main.go") break;
      return json({
        repo: "web",
        ref: url.searchParams.get("ref") ?? "main",
        commit: BASE,
        kind: "blob",
        blob: {
          path: "cmd/main.go",
          oid: HEAD,
          size: MAIN_GO.join("\n").length,
          lines: MAIN_GO.length - 1,
          start_line: 1,
          end_line: MAIN_GO.length - 1,
          text: MAIN_GO.join("\n"),
        },
        position: POSITION,
        level: "session",
      });
    case "/query/code_repo":
      return json({
        clone_url: "https://crewlet.example.com/git/web.git",
        repo: {
          id: "repo-web",
          name: "web",
          kind: "project",
          status: "active",
          managed_by: "tools",
          owner: { kind: "unit", ref: "engineering" },
          visibility: "company",
          search: "grep",
          default_branch: "main",
          refs: 2,
          live_packs: 1,
          live_bytes: 2048,
          live_objects: 9,
          position: POSITION,
          head: BASE,
          pushes: 3,
          branches: 2,
          tags: 0,
          tombstones: 0,
          created_at: "2026-11-01T10:00:00Z",
        },
        position: POSITION,
        level: "session",
      });
    case "/query/code_refs":
      if (url.searchParams.get("detail") === "true") {
        return json({ refs_detail: MOVED, position: POSITION, level: "session" });
      }
      return json({
        refs: [
          { ref: "refs/heads/feature", oid: HEAD, position: POSITION },
          { ref: "refs/heads/main", oid: BASE, position: POSITION },
        ],
        position: POSITION,
        level: "session",
      });
    case "/query/code_compare":
      return json({
        repo: "web",
        compare: {
          base: BASE,
          head: HEAD,
          merge_base: BASE,
          ahead: 1,
          behind: 0,
          total: 1,
          added: 4,
          deleted: 0,
        },
        preview: { state: "clean" },
        position: POSITION,
        level: "session",
      });
    case "/query/code_tree":
      return json({
        repo: "web",
        ref: url.searchParams.get("ref") ?? BASE,
        commit: BASE,
        kind: "tree",
        tree: {
          tree: BASE,
          entries: [
            { mode: "040000", type: "tree", oid: HEAD, path: "cmd" },
            { mode: "100644", type: "blob", oid: HEAD, path: "go.mod", size: 1200 },
          ],
          total: 2,
        },
        position: POSITION,
        level: "session",
      });
    case "/query/code_diff":
      return json({
        repo: "web",
        diff: {
          total: 1,
          added: 4,
          deleted: 0,
          files: [{ path: "src/remember.go", status: "A", added: 4, deleted: 0 }],
        },
        position: POSITION,
        level: "session",
      });
  }
  return json({ position: POSITION, level: "session" });
}

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => answer(new URL(String(input), location.origin))),
  );
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  listed = null;
  repos = null;
  vi.unstubAllGlobals();
  location.hash = "";
});

function mount(node: React.ReactNode, width = 1200) {
  host = document.createElement("div");
  // THE SCREEN'S COLUMN, as the shell gives a view: the kit's gapped content.
  host.className = "app";
  host.innerHTML = `<div class="crewlet-app-shell__content" style="display:flex;flex-direction:column;width:${width}px"></div>`;
  document.body.append(host);
  root = createRoot(host.firstElementChild!);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={ORG}>
        {node}
      </LayoutClient>,
    ),
  );
}

// A DIFF'S TITLE IS NEVER UNDER ITS TOOLBAR. The files' toolbar is a sticky,
// opaque band that hands its top padding back to the gap above it; drawn
// right under the comparison's caption in a plain section, it covered the
// caption's lower half ("What feature changed since 3a3ecae" cut in two).
// Mutation: draw the title in a section with no gap again, and the band's
// top rises above the title's bottom.
test("a comparison's title stays clear of the files' toolbar", async () => {
  location.hash = "#/gitlet/repos/web/compare?base=main&head=feature";
  mount(<ComparePage repo="web" />);
  const title = await eventually(() => {
    const el = [...document.querySelectorAll(".code-section__title, p.t-caption")].find((e) =>
      e.textContent?.startsWith("What feature changed since"),
    );
    expect(el).toBeTruthy();
    return el!;
  });
  const toolbar = await eventually(() => {
    const bar = document.querySelector('[aria-label="Layout"]')?.closest(".toolbar");
    expect(bar).toBeTruthy();
    return bar!;
  });
  const under = title.getBoundingClientRect();
  const band = toolbar.getBoundingClientRect();
  expect(band.top).toBeGreaterThanOrEqual(Math.floor(under.bottom));
});

// THE CODE MENU SHOWS ITS WHOLE ADDRESS and its actions, with no sideways
// scroll once it focuses its copy button. Mutation: `.code-clone { width: 30rem }` fails it.
for (const width of [1440, 390]) {
  test(`the clone panel holds its whole address, inside its card and the viewport, at ${width}px`, async () => {
    await page.viewport(width, 900);
    location.hash = "#/gitlet/repos/web";
    mount(<RepositoryPage repo="web" />, width);
    const code = await eventually(() => {
      const button = [...document.querySelectorAll('button[aria-haspopup="dialog"]')].find(
        (b) => b.textContent === "Code",
      );
      expect(button).toBeTruthy();
      return button as HTMLElement;
    });
    const card = await eventually(() => {
      const listing = document.querySelector(".code-browser");
      expect(listing?.textContent).toContain("go.mod");
      return listing!;
    });
    code.click();
    // Measured once placed and focused: the focus is what scrolled it.
    const panel = await eventually(() => {
      const el = document.querySelector<HTMLElement>('.crewlet-popover[aria-label="Clone web"]');
      expect(el).toBeTruthy();
      expect(el!.style.visibility).not.toBe("hidden");
      expect(el!.contains(document.activeElement)).toBe(true);
      return el!;
    });
    expect(panel.scrollWidth).toBeLessThanOrEqual(panel.clientWidth);
    expect(panel.scrollLeft).toBe(0);
    const box = panel.getBoundingClientRect();
    for (const part of panel.querySelectorAll(
      ".code-clone__head, .code-clone__url, .code-clone__note, .code-clone__act",
    )) {
      const at = part.getBoundingClientRect();
      expect(at.left).toBeGreaterThanOrEqual(box.left);
      expect(at.right).toBeLessThanOrEqual(box.right);
    }
    const head = panel.querySelector(".code-clone__head")!;
    expect(head.textContent).toBe("Clone");
    expect(head.scrollWidth).toBeLessThanOrEqual(head.clientWidth);
    const command = [...panel.querySelectorAll<HTMLElement>(".code-clone__act")].find(
      (a) => a.textContent === "Copy the clone command",
    )!;
    expect(command.scrollWidth).toBeLessThanOrEqual(command.clientWidth);
    expect(command.getBoundingClientRect().height).toBeLessThan(40);
    expect(box.left).toBeGreaterThanOrEqual(0);
    expect(box.right).toBeLessThanOrEqual(window.innerWidth);
    const under = card.getBoundingClientRect();
    expect(box.left).toBeGreaterThanOrEqual(Math.floor(under.left));
    expect(box.right).toBeLessThanOrEqual(Math.ceil(under.right));
  });
}

// THE PICKER'S PANEL STAYS IN THE VIEWPORT with no sideways scroll, a long
// branch name ellipsed on one line. Mutation: a name that wraps, or
// `.code-picker { width: 24rem }`, fails it.
for (const width of [1440, 1024, 390]) {
  test(`the branch picker's panel stays inside the viewport and does not scroll sideways at ${width}px`, async () => {
    await page.viewport(width, 900);
    location.hash = "#/gitlet/repos/web";
    mount(<RepositoryPage repo="web" />, width);
    const picker = await eventually(() => {
      const button = document.querySelector<HTMLElement>('[aria-label="Branch or tag: main"]');
      expect(button).toBeTruthy();
      return button!;
    });
    picker.click();
    const panel = await eventually(() => {
      const el = document.querySelector<HTMLElement>(
        '.crewlet-popover[aria-label="Switch branches or tags"]',
      );
      expect(el).toBeTruthy();
      expect(el!.style.visibility).not.toBe("hidden");
      expect(document.activeElement?.getAttribute("aria-label")).toBe("Find a branch");
      expect(el!.querySelectorAll("[role=option]")).toHaveLength(10);
      return el!;
    });
    const list = panel.querySelector<HTMLElement>("[role=listbox]")!;
    for (const box of [panel, list]) {
      expect(box.scrollWidth).toBeLessThanOrEqual(box.clientWidth);
      expect(box.scrollLeft).toBe(0);
    }
    const at = panel.getBoundingClientRect();
    expect(at.left).toBeGreaterThanOrEqual(0);
    expect(at.right).toBeLessThanOrEqual(window.innerWidth);
    expect(at.width).toBeGreaterThanOrEqual(260);
    for (const row of panel.querySelectorAll("[role=option], .code-picker__foot")) {
      const r = row.getBoundingClientRect();
      expect(r.left).toBeGreaterThanOrEqual(at.left);
      expect(r.right).toBeLessThanOrEqual(at.right);
    }
    const long = [...panel.querySelectorAll<HTMLElement>(".code-picker__name")].find((n) =>
      n.textContent?.startsWith("feature/"),
    )!;
    expect(long.scrollWidth).toBeGreaterThan(long.clientWidth);
    const main = panel.querySelector<HTMLElement>(".code-picker__name")!;
    expect(long.getBoundingClientRect().height).toBe(main.getBoundingClientRect().height);
  });
}

// A CODED DIFF'S BAND HOLDS ITS NUMBERS AND STOPS BEFORE THE SIGN, and a changed
// run keeps a visible underline. Mutation: a narrower `--diff-gutter`, or a
// transparent run decoration, fails it.
// A CODED LINE KEEPS THE PLAIN LINE'S HEIGHT, and no ink is drawn inside a
// run. Mutation: `font-size: 1.2em` on the comment rule fails it.
test("a coloured diff's band holds its numbers, and a changed run keeps its underline", async () => {
  const hunks: DiffHunk[] = [
    {
      skipped: 9,
      lines: [
        { kind: "same", text: "keep", before: 10, after: 10 },
        { kind: "remove", text: "paid in any order", before: 11, after: null },
        { kind: "add", text: "paid in join order", before: null, after: 11 },
        { kind: "add", text: "ties are rare", before: null, after: 12 },
      ],
    },
  ];
  // Each line's first word a keyword and the rest a comment, by its side and number.
  const texts = { before: new Map<number, string>(), after: new Map<number, string>() };
  for (const line of hunks[0]!.lines) {
    if (line.before !== null) texts.before.set(line.before, line.text);
    if (line.after !== null) texts.after.set(line.after, line.text);
  }
  const tokens = (side: "before" | "after", n: number) => {
    const text = texts[side].get(n) ?? "";
    const word = Math.max(text.indexOf(" "), 0);
    return [
      { end: word, className: SYNTAX_CLASS.keyword },
      { end: text.length, className: SYNTAX_CLASS.comment },
    ];
  };
  const heights = () =>
    [...document.querySelectorAll<HTMLElement>(".diff-view__line")].map(
      (line) => line.getBoundingClientRect().height,
    );
  for (const layout of ["unified", "split"] as const) {
    mount(<DiffView hue="code" hunks={hunks} layout={layout} />);
    const plain = await eventually(() => {
      if (layout === "split") expect(document.querySelector(".diff-view--split")).not.toBeNull();
      const drawn = heights();
      expect(drawn.length).toBeGreaterThan(0);
      return drawn;
    });
    flushSync(() => root?.unmount());
    host?.remove();
    mount(<DiffView hue="code" hunks={hunks} layout={layout} tokens={tokens} />);
    const changed = await eventually(() => {
      const found = [
        ...document.querySelectorAll<HTMLElement>(
          '.diff-view--code .diff-view__line:is([data-kind="add"], [data-kind="remove"])',
        ),
      ];
      expect(found).toHaveLength(3);
      if (layout === "split") expect(document.querySelector(".diff-view--split")).not.toBeNull();
      return found;
    });
    for (const line of changed) {
      const style = getComputedStyle(line);
      const band = Number(/(-?[\d.]+)px/.exec(style.boxShadow)?.[1]);
      expect(band, `${layout}: ${style.boxShadow}`).toBeGreaterThan(0);
      const left = line.getBoundingClientRect().left + parseFloat(style.borderLeftWidth);
      const numbers = line.querySelectorAll(".diff-view__number");
      const last = numbers[numbers.length - 1]!.getBoundingClientRect();
      const mark = line.querySelector(".diff-view__mark")!.getBoundingClientRect();
      expect(left + band, `${layout}: the band holds the numbers`).toBeGreaterThanOrEqual(
        last.right - 0.5,
      );
      expect(left + band, `${layout}: the band stops before the sign`).toBeLessThanOrEqual(
        mark.left + 0.5,
      );
    }
    expect(document.querySelectorAll(".code-syn-comment").length).toBeGreaterThan(0);
    expect(heights(), `${layout}: each line keeps its plain height`).toEqual(plain);
    expect(document.querySelector('.diff-view__run [class*="code-syn-"]')).toBeNull();
    const runs = document.querySelectorAll(".diff-view--code .diff-view__run");
    expect(runs.length, `${layout}: the edited line draws its runs`).toBeGreaterThan(0);
    for (const run of runs) {
      const style = getComputedStyle(run);
      expect(style.textDecorationLine, `${layout}: a run's underline`).toContain("underline");
      expect(style.textDecorationColor, `${layout}: a run's underline colour`).not.toBe(
        "rgba(0, 0, 0, 0)",
      );
    }
    flushSync(() => root?.unmount());
    host?.remove();
    root = null;
    host = null;
  }
});

/** A change as the listing answers it. */
function summary(number: number, state: string): CodeChangeSummary {
  return {
    id: `id-web-${number}`,
    repo_id: "repo-web",
    number,
    title: `Change ${number}`,
    state,
    source_ref: "refs/heads/feature",
    target_ref: "refs/heads/main",
    head: HEAD,
    revision: 1,
    owner: "jane",
    owner_kind: "human",
    opened_at: "2026-11-01T10:00:00Z",
    opened_position: POSITION,
    version: 1,
    position: POSITION,
    repo: "web",
    ref: `web!${number}`,
    access: "maintain",
  } as unknown as CodeChangeSummary;
}

const NONE = "rgba(0, 0, 0, 0)";

// A CHIP ON A SELECTED ROW DRAWS NO GROUND (the list's peeked row, or a grid's
// `aria-current` row), and a state pill paints the kit's fill. Mutation: drop either
// selected-row rule, or the merged pill's rule, and this fails.
test("a branch chip draws no ground on a selected row, and a state pill paints its fill", async () => {
  location.hash = "#/gitlet?peek=change:web!42";
  mount(
    <>
      <ChangeRows rows={[summary(42, "open"), summary(41, "merged")]} label="Changes" />
      <ChangeStateTag state="merged" />
    </>,
  );
  const rowOf = (ref: string) =>
    eventually(() => {
      const row = [...document.querySelectorAll<HTMLElement>(".code-list__row")].find((r) =>
        r.textContent?.includes(ref),
      );
      expect(row).toBeTruthy();
      return row!;
    });
  const picked = await rowOf("web!42");
  const other = await rowOf("web!41");
  expect(picked.hasAttribute("data-current")).toBe(true);
  const grounds = (row: HTMLElement) =>
    [...row.querySelectorAll(".code-branch-chip")].map((c) => getComputedStyle(c).backgroundColor);
  expect(grounds(picked)).toEqual([NONE, NONE]);
  for (const ground of grounds(other)) expect(ground).not.toBe(NONE);
  const probe = document.createElement("span");
  probe.style.background = "var(--color-change-merged-fill)";
  host!.append(probe);
  const pill = document.querySelector<HTMLElement>('.code-state[data-state="merged"]')!;
  expect(getComputedStyle(pill).backgroundColor).toBe(getComputedStyle(probe).backgroundColor);
  expect(getComputedStyle(probe).backgroundColor).not.toBe(NONE);
  flushSync(() => root?.unmount());
  host?.remove();
  location.hash = "";

  mount(
    <DataGrid<{ id: string }>
      rows={[{ id: "a" }]}
      rowKey={(r) => r.id}
      isSelected={() => true}
      aria-label="Branches"
      columns={[{ key: "branch", header: "Branch", cell: () => <BranchChip name="main" /> }]}
    />,
  );
  const chip = await eventually(() => {
    const el = document.querySelector<HTMLElement>(".code-branch-chip");
    expect(el).toBeTruthy();
    return el!;
  });
  expect(chip.closest('[role="row"]')?.getAttribute("aria-current")).toBe("true");
  expect(getComputedStyle(chip).backgroundColor).toBe(NONE);
});

// A STATE PILL'S WORD IS CENTRED ON ITS GLYPH at either width, where the kit's
// stacked grid sets a pictured value's words on their baseline. Mutation:
// drop the pill word's `align-self`, and the word rides high.
for (const width of [1440, 390]) {
  test(`a state pill's word is centred on its glyph, at ${width}px`, async () => {
    await page.viewport(width, 900);
    const states = ["open", "draft", "merged", "closed"] as const;
    mount(
      <span>
        {states.map((s) => (
          <ChangeStateTag key={s} state={s} />
        ))}
      </span>,
      width,
    );
    const pills = await eventually(() => {
      const found = [...document.querySelectorAll<HTMLElement>(".code-state")];
      expect(found).toHaveLength(states.length);
      return found;
    });
    const middle = (el: Element) => {
      const box = el.getBoundingClientRect();
      return box.top + box.height / 2;
    };
    for (const pill of pills) {
      const glyph = pill.querySelector("svg")!;
      const word = pill.lastElementChild!;
      expect(word.textContent, pill.dataset.state).not.toBe("");
      expect(Math.abs(middle(word) - middle(glyph)), pill.dataset.state).toBeLessThanOrEqual(1);
    }
  });
}

/** A 120-character branch name, which has to wrap inside its row. */
const LONG_BRANCH = "frontend/".padEnd(120, "stream-the-shell-");

/** The repository's branches in detail: a protected main, a long name with its change, a protected release. */
function withBranches() {
  const row = (name: string, extra: Record<string, unknown>) => ({
    ref: `refs/heads/${name}`,
    oid: HEAD,
    position: POSITION,
    moved_at: "2026-11-15T10:00:00Z",
    moved_by: {
      push_id: "p1",
      actor: "jane",
      actor_kind: "person",
      actor_seat: "jane",
      via: "git",
    },
    ...extra,
  });
  const rule = (pattern: string) => ({ patterns: [pattern], direct_push: "none" });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), location.origin);
      if (url.pathname === "/query/code_tips") {
        const oids = (url.searchParams.get("oids") ?? "").split(",");
        return json({
          repo: "web",
          kind: "tips",
          tips: {
            base: BASE,
            tips: oids.map((oid) => ({
              oid,
              type: "commit",
              peeled: oid,
              peeled_type: "commit",
              behind: 12,
              ahead: 3,
            })),
          },
        });
      }
      if (url.pathname !== "/query/code_refs" || url.searchParams.get("detail") !== "true") {
        return answer(url);
      }
      return json({
        refs_detail: [
          row("main", {
            oid: BASE,
            protection: rule("refs/heads/main"),
            delete_refusal: "default_branch",
            delete_detail: "main is web's default branch",
          }),
          row(LONG_BRANCH, {
            change: { number: 42, title: "Stream the shell", state: "open" },
            may_delete: true,
          }),
          row("release/2026.11", {
            protection: rule("refs/heads/release/*"),
            delete_refusal: "protected_delete",
            delete_detail: "release/2026.11 is protected: its rule refuses a delete",
          }),
        ],
        position: POSITION,
        level: "session",
      });
    }),
  );
}

const overlaps = (a: DOMRect, b: DOMRect) =>
  a.left < b.right - 0.5 &&
  b.left < a.right - 0.5 &&
  a.top < b.bottom - 0.5 &&
  b.top < a.bottom - 0.5;

// A BRANCH ROW KEEPS EVERY CONTROL CLEAR of its words, a long name wrapping inside
// its row, on a phone, beside a peek and at full width. Mutation: drop the phone
// stack, or the name's wrap, and a control lands on the name or leaves the box.
for (const [viewport, column] of [
  [1440, 1200],
  [390, 358],
  [1440, 600],
] as const) {
  test(`a branch row keeps every control clear and its name inside it, at a ${column}px column`, async () => {
    await page.viewport(viewport, 900);
    withBranches();
    location.hash = "#/gitlet/repos/web?tab=branches&view=all";
    mount(<RepositoryPage repo="web" />, column);
    const rows = await eventually(() => {
      const found = [...document.querySelectorAll<HTMLElement>(".code-refs__table tbody tr")];
      expect(found).toHaveLength(3);
      return found;
    });
    const box = document.querySelector<HTMLElement>(".code-refs")!;
    expect(box.scrollWidth).toBeLessThanOrEqual(box.clientWidth);
    const edge = box.getBoundingClientRect();
    for (const row of rows) {
      expect(row.scrollWidth, row.dataset.key).toBeLessThanOrEqual(row.clientWidth);
      const words = [
        ...row.querySelectorAll<HTMLElement>(".code-refs__marks, .code-byline, .code-change-ref"),
      ].map((w) => w.getBoundingClientRect());
      for (const control of row.querySelectorAll<HTMLElement>(
        ".code-refs__actions a, .code-refs__actions button",
      )) {
        const at = control.getBoundingClientRect();
        const name = control.getAttribute("aria-label") ?? control.textContent ?? "";
        for (const w of words) expect(overlaps(at, w), name).toBe(false);
        expect(at.right, name).toBeLessThanOrEqual(edge.right);
      }
      // A STACKED ROW: the name over who moved it, the controls under both.
      if (column < 640) {
        const cell = (c: string) => row.querySelector(`.code-refs__${c}`)!.getBoundingClientRect();
        expect(cell("updated").top).toBeGreaterThanOrEqual(cell("name").bottom - 0.5);
        expect(cell("actions").top).toBeGreaterThanOrEqual(cell("updated").bottom - 0.5);
      }
    }
    // HOW FAR A BRANCH IS FROM main: bars beside the columns, words where the row stacks.
    const drift = await eventually(() => {
      const found = document.querySelector<HTMLElement>(".code-drift");
      expect(found).not.toBeNull();
      return found!;
    });
    const shown = (el: Element) => getComputedStyle(el).display !== "none";
    expect(shown(drift.querySelector(".code-drift__words")!)).toBe(column < 640);
    expect(shown(drift.querySelector(".code-drift__bars")!)).toBe(column >= 640);
    if (column < 640) return;
    // THE TABLE FILLS ITS BOX: the name on the title's inset, the controls ending on the box's.
    const title = box.querySelector(".code-refs__heading")!.getBoundingClientRect();
    const marks = rows[0]!.querySelector(".code-refs__marks")!.getBoundingClientRect();
    expect(Math.abs(marks.left - title.left)).toBeLessThan(1);
    const controls = rows[1]!.querySelector(".code-refs__controls")!.getBoundingClientRect();
    expect(edge.right - controls.right).toBeCloseTo(marks.left - edge.left, 0);
  });
}

/** A history page of three commits, the first with a 200-character subject. */
function withHistory() {
  const commit = (oid: string, subject: string, at: string) => ({
    oid,
    author: "Jane Founder",
    authored_at: at,
    committed_at: at,
    subject,
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), location.origin);
      if (url.pathname !== "/query/code_log") return answer(url);
      return json({
        repo: "web",
        kind: "log",
        log: {
          commits: [
            commit(HEAD, "render: ".padEnd(200, "stream the shell "), "2026-11-15T10:00:00Z"),
            commit(BASE, "cart: cache the badge count for 30s", "2026-11-15T09:00:00Z"),
            commit("1".repeat(40), "go.mod: bump to go 1.27", "2026-11-14T09:00:00Z"),
          ],
          scanned: 3,
          complete: true,
        },
        pushed_by: {
          [BASE]: {
            push_id: "p1",
            position: POSITION,
            at: "2026-11-15T09:00:00Z",
            principal: { kind: "agent", seat: "jane", run: { turn_id: "t", work_key: "ENG-399" } },
            via: "engine",
          },
        },
        position: POSITION,
        level: "session",
      });
    }),
  );
}

// A COMMIT ROW KEEPS ITS ID AND BROWSE ON ITS TITLE'S LINE where there is room, and stacks
// them under its words on a phone or beside a peek, above the row's stretched link.
// Mutation: drop the phone fold, or the controls' lift over the link.
for (const [viewport, column] of [
  [1440, 1200],
  [390, 358],
  [1440, 600],
] as const) {
  test(`a commit row's facts sit by its title or stack under it, at a ${column}px column`, async () => {
    await page.viewport(viewport, 900);
    withHistory();
    location.hash = "#/gitlet/repos/web?tab=commits";
    mount(<RepositoryPage repo="web" />, column);
    const rows = await eventually(() => {
      const found = [...document.querySelectorAll<HTMLElement>(".code-list__row")];
      expect(found).toHaveLength(3);
      return found;
    });
    const list = document.querySelector<HTMLElement>(".code-list")!;
    expect(list.scrollWidth).toBeLessThanOrEqual(list.clientWidth);
    for (const row of rows) {
      expect(row.scrollWidth, row.dataset.key).toBeLessThanOrEqual(row.clientWidth);
      const title = row.querySelector(".code-list__title")!.getBoundingClientRect();
      const meta = row.querySelector(".code-list__meta")!.getBoundingClientRect();
      const body = row.querySelector(".code-list__body")!.getBoundingClientRect();
      const facts = row.querySelector(".code-list__facts")!.getBoundingClientRect();
      const box = row.getBoundingClientRect();
      expect(
        row.querySelector(".code-list__link")!.getBoundingClientRect().right,
      ).toBeLessThanOrEqual(box.right);
      if (column >= 640) {
        expect(facts.top).toBeLessThan(title.bottom);
        expect(facts.left).toBeGreaterThanOrEqual(body.right);
      } else {
        expect(facts.top).toBeGreaterThanOrEqual(meta.bottom - 0.5);
      }
      expect(facts.right).toBeLessThanOrEqual(box.right + 0.5);
      // THE ROW'S CONTROLS ARE WHAT A PRESS ON THEM HITS, not the link under them.
      for (const control of row.querySelectorAll<HTMLElement>(
        ".code-sha, a[aria-label^='Browse']",
      )) {
        const at = control.getBoundingClientRect();
        const hit = document.elementFromPoint(at.left + at.width / 2, at.top + at.height / 2);
        expect(control.contains(hit), control.getAttribute("aria-label") ?? "").toBe(true);
      }
    }
  });
}

// A COMMIT'S ID STAYS PUT WHEN ITS CHECK ARRIVES: the check's slot keeps its width while the
// marks are read, and at a phone's width a row's pill, change and tags wrap inside it.
// Mutation: draw the slot only once the marks answer.
const THIRD = "1".repeat(40);
for (const [viewport, column] of [
  [1440, 1200],
  [390, 358],
] as const) {
  test(`a commit's id stays put when its check arrives, at ${viewport}px`, async () => {
    await page.viewport(viewport, 900);
    withHistory();
    const history = vi.mocked(fetch).getMockImplementation()!;
    let release = () => {};
    const held = new Promise<void>((r) => (release = r));
    const tags = ["refs/tags/v2026.11.2", "refs/tags/v2026.11.1"];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = new URL(String(input), location.origin);
        if (url.pathname === "/query/code_log") {
          const answer = (await (await history(input)).json()) as Record<string, unknown>;
          return json({ ...answer, tagged: { commits: { [THIRD]: { refs: tags } } } });
        }
        if (url.pathname !== "/query/code_commit_marks") return history(input);
        await held;
        const change = { repo: "web", title: "cache the badge", state: "merged" };
        const passed = { state: "passed", counts: { passed: 2 }, required: 2, required_passed: 2 };
        return json({
          commit_marks: [
            { commit: BASE, checks: [{ ...passed, target: "refs/heads/main" }] },
            {
              commit: THIRD,
              merged: [{ ...change, ref: "web!41", merge_oid: THIRD }],
              heads: [{ change: { ...change, ref: "web!40" }, n: 2 }],
            },
          ],
          position: POSITION,
          level: "session",
        });
      }),
    );
    location.hash = "#/gitlet/repos/web?tab=commits";
    mount(<RepositoryPage repo="web" />, column);
    const rows = await eventually(() => {
      const found = [...document.querySelectorAll<HTMLElement>(".code-list__row")];
      expect(found).toHaveLength(3);
      return found;
    });
    const sha = () => rows[1]!.querySelector(".code-sha")!.getBoundingClientRect();
    const before = sha();
    release();
    await eventually(() => expect(rows[1]!.querySelector(".code-check-mark")).toBeTruthy());
    await eventually(() => expect(rows[2]!.querySelector(".code-change-ref")).toBeTruthy());
    expect([sha().x, sha().y]).toEqual([before.x, before.y]);
    const last = rows[2]!;
    expect(last.scrollWidth).toBeLessThanOrEqual(last.clientWidth);
    const box = last.getBoundingClientRect();
    for (const el of last.querySelectorAll<HTMLElement>(
      ".code-change-ref, .code-tag-chip, .code-commit-landed",
    )) {
      expect(el.getBoundingClientRect().right, el.textContent!).toBeLessThanOrEqual(
        box.right + 0.5,
      );
    }
  });
}

/** The file view of `cmd/main.go` at `width`, once its third line is coloured. */
async function colouredLine(width: number): Promise<HTMLElement> {
  await page.viewport(width, 900);
  location.hash = "#/gitlet/repos/web?path=cmd%2Fmain.go";
  mount(<RepositoryPage repo="web" />, width);
  return eventually(() => {
    const line = document.querySelector<HTMLElement>("#L3");
    expect(line?.querySelector(".code-syn-comment")).toBeTruthy();
    return line!;
  });
}

// A COLOURED LINE KEEPS THE PLAIN LINE'S BOX: an ink changes no advance, and
// a line too wide still scrolls the card, never the page. Mutation:
// `letter-spacing: 1px` on the keyword rule fails it.
for (const width of [1440, 390]) {
  test(`a coloured line keeps the plain line's box, at ${width}px`, async () => {
    const line = await colouredLine(width);
    const text = line.querySelector(".code-source__text")!;
    expect(text.textContent).toBe(MAIN_GO[2]);
    for (const kind of ["keyword", "function", "string", "comment"])
      expect(text.querySelector(`.code-syn-${kind}`), kind).toBeTruthy();
    // THE SAME TEXT PLAIN, a line drawn beside it with no spans.
    const plain = line.cloneNode(true) as HTMLElement;
    plain.removeAttribute("id");
    plain.querySelector(".code-source__text")!.textContent = MAIN_GO[2]!;
    line.after(plain);
    const box = (el: HTMLElement, part: string) => el.querySelector(part)!.getBoundingClientRect();
    const [coloured, drawn] = [box(line, ".code-source__text"), box(plain, ".code-source__text")];
    expect(Math.abs(coloured.width - drawn.width)).toBeLessThanOrEqual(0.5);
    expect(Math.abs(coloured.height - drawn.height)).toBeLessThanOrEqual(0.5);
    expect(box(line, ".code-source__number").left).toBe(box(plain, ".code-source__number").left);
    plain.remove();
    if (width === 390) {
      const source = line.closest<HTMLElement>(".code-source")!;
      expect(source.scrollWidth).toBeGreaterThan(source.clientWidth);
      expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(window.innerWidth);
    }
  });
}

// A COMMENT IS ITALIC AND IN ITS INK, as the kit draws one. Mutation: drop
// the comment rule's `font-style`, or read another ink, and this fails.
test("a comment is italic and in its ink", async () => {
  const line = await colouredLine(1440);
  const comment = line.querySelector(".code-syn-comment")!;
  const probe = document.createElement("span");
  probe.style.color = "var(--color-syntax-comment)";
  host!.append(probe);
  expect(getComputedStyle(comment).fontStyle).toBe("italic");
  expect(getComputedStyle(comment).color).toBe(getComputedStyle(probe).color);
  expect(getComputedStyle(comment).color).not.toBe(getComputedStyle(line).color);
});

/** A repository's root whose head row and first entry carry subjects far wider than a row. */
function withHome() {
  const at = "2026-11-15T10:00:00Z";
  const commit = (oid: string, subject: string) => ({
    oid,
    author: "Jane Founder",
    authored_at: at,
    committed_at: at,
    subject,
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), location.origin);
      switch (url.pathname) {
        case "/query/code_tree": {
          const listed = (await answer(url).json()) as Record<string, unknown>;
          return json({ ...listed, resolved: { ref: { ref: "refs/heads/main", oid: BASE } } });
        }
        case "/query/code_log":
          return json({
            repo: "web",
            kind: "log",
            log: { commits: [commit(BASE, "Merge: ".padEnd(160, "cache the badge "))], scanned: 1 },
          });
        case "/query/code_tree_commits":
          return json({
            repo: "web",
            kind: "tree_commits",
            tree_commits: {
              tree: BASE,
              entries: [
                { path: "cmd", commit: HEAD },
                { path: "go.mod", commit: BASE },
              ],
              commits: [
                commit(HEAD, "render: ".padEnd(200, "stream the shell ")),
                commit(BASE, "go.mod: bump to go 1.27"),
              ],
              total: 2,
            },
          });
        case "/query/code_find":
          return json({
            repo: "web",
            kind: "find",
            find: {
              tree: BASE,
              matches: [
                { path: "cmd/web/main.go", oid: HEAD, mode: "100644", spans: [[8, 12]] },
                {
                  path: "internal/render/main_test.go",
                  oid: HEAD,
                  mode: "100644",
                  spans: [[16, 20]],
                },
              ],
              total: 2,
              files: 1204,
            },
          });
      }
      return answer(url);
    }),
  );
}

// A REPOSITORY'S HOME HOLDS ITS WIDTH: subjects give way, an entry's age stays one line, and on
// a phone each row stacks. Mutation: drop the subject's `overflow: hidden`, the phone rule or
// the colgroup: this fails.
for (const [viewport, column] of [
  [1024, 740],
  [640, 608],
] as const) {
  test(`a repository's home holds its width, its subjects giving way, at ${viewport}px`, async () => {
    await page.viewport(viewport, 900);
    withHome();
    location.hash = "#/gitlet/repos/web";
    mount(<RepositoryPage repo="web" />, column);
    const table = await eventually(() => {
      const found = document.querySelector<HTMLElement>(".code-entries");
      expect(found?.querySelectorAll(".code-entries__subject")).toHaveLength(2);
      expect(document.querySelector(".code-latest__subject")).toBeTruthy();
      return found!;
    });
    const card = document.querySelector<HTMLElement>(".code-browser")!;
    const content = document.querySelector<HTMLElement>(".crewlet-app-shell__content")!;
    for (const el of [content, card, table]) {
      expect(el.scrollWidth, el.className).toBeLessThanOrEqual(el.clientWidth);
    }
    const end = document.querySelector<HTMLElement>(".code-bar-end")!;
    const bar = end.getBoundingClientRect();
    expect(bar.right).toBeLessThanOrEqual(content.getBoundingClientRect().right + 0.5);
    // THE END GROUP IS ONE LINE where there is room: Go to file never takes the whole group.
    if (column >= 640) {
      const tops = [...end.children].map((el) => Math.round(el.getBoundingClientRect().top));
      expect(new Set(tops).size, tops.join(",")).toBe(1);
    }
    for (const row of table.querySelectorAll<HTMLElement>("tbody tr")) {
      const box = row.getBoundingClientRect();
      const name = row.querySelector(".code-entries__name")!.getBoundingClientRect();
      const when = row.querySelector(".code-entries__when")!.getBoundingClientRect();
      const subject = row.querySelector(".code-entries__subject")!.getBoundingClientRect();
      expect(subject.right, row.dataset.key).toBeLessThanOrEqual(box.right + 0.5);
      expect(row.querySelector("time")!.getClientRects()).toHaveLength(1);
      if (column < 640) {
        expect(Math.abs(name.top - when.top)).toBeLessThan(6);
        expect(subject.top).toBeGreaterThanOrEqual(name.bottom - 0.5);
      } else {
        expect(subject.top).toBeLessThan(name.bottom);
        // THE SUBJECT'S COLUMN IS THE WIDEST: the columns are not equal thirds.
        const commit = row.querySelector(".code-entries__commit")!.getBoundingClientRect();
        expect(commit.width, row.dataset.key).toBeGreaterThan(name.width);
        expect(commit.width, row.dataset.key).toBeGreaterThan(when.width);
      }
    }
    const latest = card.querySelector<HTMLElement>(".code-latest")!;
    const head = latest.querySelector(".code-latest__subject")!.getBoundingClientRect();
    const frame = card.getBoundingClientRect();
    for (const part of [".code-sha", ".code-latest__when", "a[href*='tab=commits']"]) {
      const at = latest.querySelector(part)!.getBoundingClientRect();
      expect(at.right, part).toBeLessThanOrEqual(frame.right + 0.5);
      if (column < 640) expect(at.top, part).toBeGreaterThanOrEqual(head.bottom - 0.5);
      else expect(at.top, part).toBeLessThan(head.bottom);
    }
  });

  // GO TO FILE'S LIST STAYS IN THE VIEWPORT. Mutation: a fixed width on the field, and this fails.
  test(`Go to file's list stays inside the viewport at ${viewport}px`, async () => {
    await page.viewport(viewport, 900);
    withHome();
    location.hash = "#/gitlet/repos/web";
    mount(<RepositoryPage repo="web" />, column);
    await page.getByRole("combobox", { name: "Go to file" }).fill("main");
    const list = await eventually(() => {
      const found = document.querySelector<HTMLElement>("[role=listbox]");
      expect(found?.textContent).toContain("cmd/web/main.go");
      return found!;
    });
    const box = list.getBoundingClientRect();
    expect(box.left).toBeGreaterThanOrEqual(0);
    expect(box.right).toBeLessThanOrEqual(window.innerWidth);
  });
}

/** A change with its card, its title as long as `title` says. */
function carded(number: number, title = `Change ${number}`): CodeChangeSummary {
  return {
    ...summary(number, "open"),
    title,
    source_ref: `refs/heads/feature/${"stream-".repeat(number % 3)}shell`,
    revision: 3,
    open_findings: number % 2,
    work_item: { backend: "worklet", id: `t-${number}`, key: `ENG-${number}` },
    readiness: {
      met: 2,
      total: 3,
      first: "",
      preconditions: [
        { kind: "facts", state: "met", detail: "" },
        {
          kind: "approval",
          state: "unmet",
          detail: "",
          waits_on: [{ handle: "jane", kind: "person" }],
        },
        { kind: "mergeable", state: "met", detail: "" },
      ],
    },
    card: {
      comments: number * 7,
      reviews: 1,
      changes_requested: 0,
      reviewers: [{ handle: "jane", kind: "person", verdict: "approve", verdict_revision: 1 }],
      checks: { state: "passed", counts: { passed: 2 }, required: 2, required_passed: 2 },
    },
  } as CodeChangeSummary;
}

/** A change whose branch is long enough that its meta wraps on a wide screen. */
const WRAPPED = {
  ...carded(105),
  source_ref: `refs/heads/feature/${"stream-the-product-page-".repeat(6)}shell`,
} as CodeChangeSummary;

const ROWS = [
  carded(42, "render: stream the product page's shell before its data"),
  carded(7),
  WRAPPED,
];

/** Each row's body and facts boxes, once the list has drawn. */
const rowBoxes = () =>
  eventually(() => {
    const rows = [...document.querySelectorAll<HTMLElement>(".code-list__row")];
    expect(rows).toHaveLength(ROWS.length);
    return rows.map((row) => ({
      row,
      body: row.querySelector(".code-list__body")!.getBoundingClientRect(),
      facts: row.querySelector(".code-list__facts")!.getBoundingClientRect(),
      slots: [...row.querySelectorAll(".code-change-fact")].map(
        (s) => s.getBoundingClientRect().left,
      ),
    }));
  });

// A CHANGE ROW'S FACTS LINE UP DOWN THE LIST, on the title's line however the meta wraps.
// Mutation: size the slots by their content, or centre the facts on the row.
test("a change row keeps its facts on its title's line at 1440 px, in columns down the card", async () => {
  await page.viewport(1440, 900);
  mount(<ChangeRows rows={ROWS} label="Changes" />, 1200);
  const boxes = await rowBoxes();
  for (const { row } of boxes) {
    const title = row.querySelector(".code-list__title")!.getBoundingClientRect();
    const facts = row.querySelector(".code-change-facts")!.getBoundingClientRect();
    expect(Math.abs(facts.top - title.top)).toBeLessThanOrEqual(2);
  }
  const wrapped = boxes.at(-1)!.row.querySelector(".code-list__meta")!.getBoundingClientRect();
  expect(wrapped.height).toBeGreaterThan(40);
  const [first, ...rest] = boxes;
  for (const other of rest) expect(other.slots).toEqual(first!.slots);
});

// A CHANGE ROW FOLDS WHERE ITS FACTS WOULD CRUSH ITS WORDS, a list under 820 px of its own.
// Mutation: fold change rows at the phone's width alone, and a 720 px list keeps them beside.
test("a change row stacks its facts under its words in a 720 px list", async () => {
  await page.viewport(1440, 900);
  mount(<ChangeRows rows={ROWS} label="Changes" />, 720);
  const boxes = await rowBoxes();
  for (const { row, body, facts } of boxes) {
    expect(facts.top).toBeGreaterThanOrEqual(body.bottom - 1);
    expect(row.scrollWidth).toBeLessThanOrEqual(row.clientWidth);
  }
});

// ON A PHONE THE FACTS GO UNDER THE WORDS, and nothing is wider than the column.
// Mutation: drop the fold's facts rules, and the slots' fixed widths overflow the row.
test("a change row stacks its facts under its words at 390 px, and nothing overflows", async () => {
  await page.viewport(390, 900);
  const long = carded(9, `render: ${"stream the product page's shell before its data ".repeat(4)}`);
  mount(<ChangeRows rows={[...ROWS.slice(0, 2), long]} label="Changes" />, 358);
  const boxes = await rowBoxes();
  for (const { row, body, facts } of boxes) {
    expect(facts.top).toBeGreaterThanOrEqual(body.bottom - 1);
    expect(row.scrollWidth).toBeLessThanOrEqual(row.clientWidth);
  }
  const list = document.querySelector<HTMLElement>(".code-list__rows")!;
  expect(list.scrollWidth).toBeLessThanOrEqual(list.clientWidth);
  // A MIDDLE DOT ENDS THE LINE ITS PART ENDS, never a line of its own.
  const dots = [...document.querySelectorAll<HTMLElement>(".code-change-meta__dot")];
  expect(dots.length).toBeGreaterThan(0);
  for (const dot of dots) {
    const range = document.createRange();
    range.selectNodeContents(dot.parentElement!);
    range.setEndBefore(dot);
    const before = [...range.getClientRects()].filter((r) => r.width > 0).at(-1)!;
    const at = dot.getBoundingClientRect();
    expect(Math.abs(before.top + before.height / 2 - (at.top + at.height / 2))).toBeLessThan(
      at.height / 2,
    );
  }
});

// THE ROW FOLDS BY ITS OWN LIST'S WIDTH, so Home's narrow card stacks it on a wide screen.
// Mutation: fold on the page's container, and a 370 px card at 1440 keeps its facts beside.
test("a change row folds by its own width in a 370 px card at 1440 px", async () => {
  await page.viewport(1440, 900);
  mount(<ChangeRows rows={ROWS} label="Changes" density="compact" />, 370);
  const boxes = await rowBoxes();
  for (const { row, body, facts } of boxes) {
    expect(facts.top).toBeGreaterThanOrEqual(body.bottom - 1);
    expect(row.scrollWidth).toBeLessThanOrEqual(row.clientWidth);
  }
});

/** Every option of the open list, once it holds `count`. */
const optionsOf = (count: number) =>
  eventually(() => {
    const found = [...document.querySelectorAll<HTMLElement>('[role="option"]')];
    expect(found).toHaveLength(count);
    return found;
  });

/** Whether a point on the element's middle lands on it, nothing clipping or covering it. */
function reachable(el: HTMLElement): boolean {
  const box = el.getBoundingClientRect();
  return el.contains(document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2));
}

const FIVE = ["api", "docs", "infra", "mobile", "web"];

// THE HEAD'S PICKER HANGS ITS LIST PAST A SHORT CARD: only the rows clip.
// Mutation: clip the card that holds the head, and the list is cut off under one row.
test("the Repository picker's list hangs whole past a card of one row", async () => {
  await page.viewport(1440, 900);
  listed = [carded(42)];
  // FOUR AND EVERY REPOSITORY, as many rows as the bounded list shows without scrolling.
  repos = FIVE.slice(0, 4);
  location.hash = "#/gitlet";
  mount(<Changes />, 1200);
  await eventually(() => expect(document.querySelectorAll(".code-list__row")).toHaveLength(1));
  const field = document.querySelector<HTMLInputElement>(
    '.code-list__head input[aria-label="Repository"]',
  )!;
  field.click();
  const options = await optionsOf(5);
  for (const option of options) expect(reachable(option), option.textContent ?? "").toBe(true);
});

// NEW CHANGE'S REPOSITORIES STAND IN ITS POPOVER, in the flow, never under its scroll box.
// Mutation: draw the picker's popup list inside the popover, and its rows are past its bottom.
test("New change's repository list stands inside its popover", async () => {
  await page.viewport(1440, 900);
  listed = [carded(42)];
  repos = FIVE;
  location.hash = "#/gitlet";
  mount(<Changes />, 1200);
  const button = await eventually(() => {
    const found = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
      (b) => b.textContent === "New change",
    );
    expect(found).toBeTruthy();
    return found!;
  });
  button.click();
  const options = await optionsOf(FIVE.length);
  for (const option of options) expect(reachable(option), option.textContent ?? "").toBe(true);
});
