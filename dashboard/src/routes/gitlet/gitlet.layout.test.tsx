/**
 * GITLET'S PAGES, LAID OUT: a comparison's title stays clear of the files'
 * sticky toolbar under it, and the clone panel shows its whole address.
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
import { ChangesGrid } from "./Changes.tsx";
import { BranchChip } from "./ui.tsx";
import { ComparePage } from "./commit/ComparePage.tsx";
import { RepositoryPage } from "./repo/RepositoryPage.tsx";

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

/** The engine, for one repository's comparison of feature into main. */
function answer(url: URL): Response {
  switch (url.pathname) {
    case "/query/code_repo":
      return json({
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

// THE CLONE PANEL SHOWS ITS WHOLE ADDRESS, with no sideways scroll once it
// focuses its copy button. Mutation: `.code-clone { width: 30rem }` fails it.
for (const width of [1440, 390]) {
  test(`the clone panel holds its whole address, inside its card and the viewport, at ${width}px`, async () => {
    await page.viewport(width, 900);
    location.hash = "#/gitlet/repos/web";
    mount(<RepositoryPage repo="web" />, width);
    const clone = await eventually(() => {
      const button = [...document.querySelectorAll("button")].find(
        (b) => b.textContent === "Clone",
      );
      expect(button).toBeTruthy();
      return button!;
    });
    const card = await eventually(() => {
      const listing = document.querySelector(".code-browser");
      expect(listing?.textContent).toContain("go.mod");
      return listing!;
    });
    clone.click();
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
      ".code-clone__head, .code-clone__url, .code-clone__note",
    )) {
      const at = part.getBoundingClientRect();
      expect(at.left).toBeGreaterThanOrEqual(box.left);
      expect(at.right).toBeLessThanOrEqual(box.right);
    }
    const head = panel.querySelector(".code-clone__head")!;
    expect(head.textContent).toBe("Clone with HTTPS");
    expect(head.scrollWidth).toBeLessThanOrEqual(head.clientWidth);
    expect(box.left).toBeGreaterThanOrEqual(0);
    expect(box.right).toBeLessThanOrEqual(window.innerWidth);
    const under = card.getBoundingClientRect();
    expect(box.left).toBeGreaterThanOrEqual(Math.floor(under.left));
    expect(box.right).toBeLessThanOrEqual(Math.ceil(under.right));
  });
}

// A CODED DIFF'S BAND HOLDS ITS NUMBERS AND STOPS BEFORE THE SIGN, and a changed
// run keeps a visible underline. Mutation: a narrower `--diff-gutter`, or a
// transparent run decoration, fails it.
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
  for (const layout of ["unified", "split"] as const) {
    mount(<DiffView hue="code" hunks={hunks} layout={layout} />);
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

// A CHIP ON A SELECTED ROW DRAWS NO GROUND (`aria-current` on its overlay or on
// the row), and a state pill paints the kit's fill. Mutation: drop either arm
// of the selected-row rule, or the merged pill's rule, and this fails.
test("a branch chip draws no ground on a selected row, and a state pill paints its fill", async () => {
  location.hash = "#/gitlet/changes?peek=change:web!42";
  mount(
    <ChangesGrid
      rows={[summary(42, "open"), summary(41, "merged")]}
      onLoadMore={undefined}
      loadedNote={undefined}
    />,
  );
  const rowOf = (ref: string) =>
    eventually(() => {
      const row = [...document.querySelectorAll<HTMLElement>('[role="row"]')].find((r) =>
        r.textContent?.includes(ref),
      );
      expect(row).toBeTruthy();
      return row!;
    });
  const picked = await rowOf("web!42");
  const other = await rowOf("web!41");
  const grounds = (row: HTMLElement) =>
    [...row.querySelectorAll(".code-branch-chip")].map((c) => getComputedStyle(c).backgroundColor);
  expect(grounds(picked)).toEqual([NONE, NONE]);
  for (const ground of grounds(other)) expect(ground).not.toBe(NONE);
  const probe = document.createElement("span");
  probe.style.background = "var(--color-change-merged-fill)";
  host!.append(probe);
  const pill = other.querySelector<HTMLElement>('.code-state[data-state="merged"]')!;
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

// A STATE PILL'S WORD IS CENTRED ON ITS GLYPH, the phone's stacked grid too,
// whose kit rule sets a pictured value's words on their baseline. Mutation:
// drop the pill word's `align-self`, and at 390px the word rides 4px high.
for (const width of [1440, 390]) {
  test(`a state pill's word is centred on its glyph, at ${width}px`, async () => {
    await page.viewport(width, 900);
    const states = ["open", "draft", "merged", "closed"];
    mount(
      <ChangesGrid
        rows={states.map((s, i) => summary(i + 1, s))}
        onLoadMore={undefined}
        loadedNote={undefined}
      />,
      width,
    );
    const pills = await eventually(() => {
      const found = [...document.querySelectorAll<HTMLElement>(".code-state")];
      expect(found).toHaveLength(states.length);
      if (width === 390) expect(document.querySelector("[data-stacked]")).not.toBeNull();
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

// A BRANCH'S CHIP KEEPS ITS NAME ON ONE LINE on the phone, its tags wrapping
// under it. Mutation: drop `.code-ref`'s `flex-wrap`, and a protected `main`
// stands one letter a line.
test("a branch's chip keeps its name on one line at 390px", async () => {
  await page.viewport(390, 900);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), location.origin);
      if (url.pathname !== "/query/code_repo") return answer(url);
      const read = (await answer(url).json()) as { repo: Record<string, unknown> };
      return json({
        ...read,
        repo: { ...read.repo, protections: [{ pattern: "refs/heads/main" }] },
      });
    }),
  );
  location.hash = "#/gitlet/repos/web?tab=branches";
  // The shell's column inside a phone's viewport.
  mount(<RepositoryPage repo="web" />, 358);
  const chips = await eventually(() => {
    const found = [...document.querySelectorAll<HTMLElement>(".code-ref .code-branch-chip")];
    expect(found.map((c) => c.textContent)).toEqual(["feature", "main"]);
    expect(found[1]!.closest(".code-ref")?.textContent).toBe("maindefaultprotected");
    return found;
  });
  for (const chip of chips) {
    const glyph = chip.querySelector("svg")!.getBoundingClientRect();
    expect(chip.getBoundingClientRect().height, chip.textContent!).toBeLessThan(2 * glyph.height);
  }
});
