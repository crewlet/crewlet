/**
 * Gitlet's section figures: one `code_counts` question for the company's changes, repositories and
 * queue, published on the header's tabs by every section page; a part not answered draws nothing.
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { useSectionCounts } from "~/app/Shell.tsx";
import { WORKSPACES } from "~/app/nav.ts";
import { session } from "~/protocol/session.ts";
import { engineJson } from "~/test/engineAnswer.ts";
import { settled } from "~/test/settle.ts";
import { modules, parse, walk, type Module, type Node } from "~/test/source.ts";
import { useGitletSectionCounts } from "./sectionCounts.ts";

vi.mock("~/app/Shell.tsx", () => ({ useSectionCounts: vi.fn() }));

beforeEach(() => {
  vi.mocked(useSectionCounts).mockClear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 9 };
const CHANGES = { live: 5, draft: 1, open: 4, closed: 3, merged: 54 };
const KIND = { live: 0, importing: 0, active: 0, archived: 0, deleted: 0, purging: 0 };
const REPOS = {
  project: { ...KIND, live: 4, active: 4 },
  scratch: { ...KIND, live: 1, active: 1 },
};
const STORY = { changes: CHANGES, repos: REPOS, queue: { live: 2 } };

const answered = (counts: unknown) =>
  engineJson({ counts, position: POSITION, level: "session" }, 200);
const refused = () =>
  engineJson({ error: "unavailable", detail: "this node does not answer it yet" }, 503);

/** What each request asked, answered by `respond`. */
function stub(respond: (url: URL) => Response | Promise<Response>) {
  const sent: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (raw: string) => {
      const url = new URL(raw);
      sent.push(url);
      return respond(url);
    }),
  );
  return sent;
}

const published = () => vi.mocked(useSectionCounts).mock.calls.map((c) => c[0]);

async function mounted(sent: URL[], options?: { pollMs?: number }) {
  const hook = renderHook(() => useGitletSectionCounts(options));
  await settled(() => sent.length);
  return hook;
}

// Mutation: ask a question per part.
test("the strip asks one question, the company's three parts, and publishes Changes 5, Repositories 5 and Queue 2", async () => {
  const sent = stub(() => answered(STORY));
  await mounted(sent);
  expect(sent).toHaveLength(1);
  expect(sent[0]!.pathname).toBe("/query/code_counts");
  expect(sent[0]!.searchParams.get("parts")).toBe("changes,repos,queue");
  expect(sent[0]!.searchParams.has("repo")).toBe(false);
  expect(published().at(-1)).toEqual({ changes: "5", repos: "5", queue: "2" });
});

// Mutation: key Repositories as `repositories`.
test("the figures are keyed by Gitlet's section keys", async () => {
  const sent = stub(() => answered(STORY));
  await mounted(sent);
  const row = WORKSPACES.find((w) => w.key === "gitlet")!;
  const keys = row.sections.map((s) => s.key);
  const last = published().at(-1)!;
  expect(Object.keys(last)).toHaveLength(3);
  for (const key of Object.keys(last)) expect(keys).toContain(key);
});

// Mutation: publish "0" for a refusal.
test("a refused question publishes no figure, never a zero", async () => {
  const sent = stub(refused);
  await mounted(sent);
  expect(published().at(-1)).toEqual({});
});

// Mutation: publish a part the answer lacks as zero.
test("a part the answer lacks draws no figure for it", async () => {
  const sent = stub(() => answered({ changes: CHANGES, repos: REPOS }));
  await mounted(sent);
  expect(published().at(-1)).toEqual({ changes: "5", repos: "5" });
});

// Mutation: drop the "+" of a count the engine stopped at its bound.
test("a queue counted to its bound says so", async () => {
  const sent = stub(() => answered({ ...STORY, queue: { live: 128, more: true } }));
  await mounted(sent);
  expect(published().at(-1)?.queue).toBe("128+");
});

// Mutation: hide an answered zero.
test("an answered zero is drawn", async () => {
  const sent = stub(() => answered({ ...STORY, queue: { live: 0 } }));
  await mounted(sent);
  expect(published().at(-1)?.queue).toBe("0");
});

// Mutation: drop the write listener.
test("the figures follow a write's floor, and a write to another log asks nothing", async () => {
  let counts: unknown = STORY;
  const sent = stub(() => answered(counts));
  await mounted(sent);
  counts = { ...STORY, changes: { ...CHANGES, live: 6, open: 5 } };
  const floor = `CREWLET_CODE_LOG_SECTION_COUNTS@1:${Date.now()}`;
  act(() => void session.written("code", floor, []));
  await settled(() => sent.length);
  expect(sent).toHaveLength(2);
  expect(sent[1]!.searchParams.getAll("min_position")).toContain(floor);
  expect(published().at(-1)).toEqual({ changes: "6", repos: "5", queue: "2" });
  act(() => void session.written("tracker", "CREWLET_TRACKER_LOG@1:5", []));
  await settled(() => sent.length);
  expect(sent).toHaveLength(2);
});

// Mutation: drop the poll option.
test("a page's poll asks again each minute, and a page without one asks once", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  const polled = stub(() => answered(STORY));
  const first = await mounted(polled, { pollMs: 60_000 });
  await act(async () => void vi.advanceTimersByTime(60_000));
  await settled(() => polled.length);
  expect(polled.map((u) => u.pathname)).toEqual(["/query/code_counts", "/query/code_counts"]);
  first.unmount();
  const once = stub(() => answered(STORY));
  await mounted(once);
  await act(async () => void vi.advanceTimersByTime(60_000));
  await settled(() => once.length);
  expect(once).toHaveLength(1);
});

// Mutation: drop the held figures, or keep them past a refusal.
test("a second page view publishes the last figures at once, and a refusal forgets them", async () => {
  const first = await mounted(stub(() => answered(STORY)));
  first.unmount();
  vi.mocked(useSectionCounts).mockClear();
  const unanswered = stub(() => new Promise<Response>(() => {}));
  const second = await mounted(unanswered);
  expect(published()[0]).toEqual({ changes: "5", repos: "5", queue: "2" });
  second.unmount();
  const third = await mounted(stub(refused));
  expect(published().at(-1)).toEqual({});
  third.unmount();
  vi.mocked(useSectionCounts).mockClear();
  await mounted(stub(() => new Promise<Response>(() => {})));
  expect(published()[0]).toEqual({});
});

const HOOK = "useGitletSectionCounts";

/** The screens `index.ts` exports whose function never calls the hook itself. */
function screensWithoutCounts(index: Module, byPath: ReadonlyMap<string, Module>): string[] {
  const missing: string[] = [];
  for (const node of parse(index.text, index.lang).body as Node[]) {
    if (node.type !== "ExportNamedDeclaration") continue;
    const from = (node.source as { value: string }).value.replace(/^\.\//, "routes/gitlet/");
    const screen = byPath.get(from);
    for (const spec of node.specifiers as { local: { name: string } }[]) {
      const name = spec.local.name;
      if (!screen || !callsHook(screen, name)) missing.push(name);
    }
  }
  return missing;
}

/** Whether the exported function `name` calls the hook in its own body, not a nested function's. */
function callsHook(screen: Module, name: string): boolean {
  let found = false;
  walk(parse(screen.text, screen.lang), (node) => {
    const fn = node.declaration as Node | undefined;
    if (node.type !== "ExportNamedDeclaration" || fn?.type !== "FunctionDeclaration") return;
    if ((fn.id as { name: string }).name !== name) return false;
    walk(fn.body as Node, (inner) => {
      if (inner.type.includes("Function")) return false;
      const callee = inner.callee as Node | undefined;
      if (inner.type === "CallExpression" && (callee as { name?: string })?.name === HOOK) {
        found = true;
      }
    });
    return false;
  });
  return found;
}

// Mutation: drop the call from `Pushes.tsx`.
test("every Gitlet section page asks its counts", () => {
  const all = modules();
  const index = all.find((m) => m.path === "routes/gitlet/index.ts")!;
  const byPath = new Map(all.map((m) => [m.path, m]));
  expect(screensWithoutCounts(index, byPath)).toEqual([]);
  expect((parse(index.text, index.lang).body as Node[]).length).toBeGreaterThanOrEqual(6);
});

test("the section pages' gate can tell a page that never asks, or asks only in a callback", () => {
  const index: Module = {
    path: "routes/gitlet/index.ts",
    lang: "ts",
    text: 'export { Asks } from "./Asks.tsx";\nexport { Never, Nested } from "./Never.tsx";',
  };
  const byPath = new Map<string, Module>([
    [
      "routes/gitlet/Asks.tsx",
      { path: "", lang: "tsx", text: `export function Asks() { ${HOOK}(); return null; }` },
    ],
    [
      "routes/gitlet/Never.tsx",
      {
        path: "",
        lang: "tsx",
        text: `export function Never() { return null; }\nexport function Nested() { const f = () => ${HOOK}(); return f; }`,
      },
    ],
  ]);
  expect(screensWithoutCounts(index, byPath)).toEqual(["Never", "Nested"]);
});
