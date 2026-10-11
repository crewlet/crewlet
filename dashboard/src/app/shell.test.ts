/**
 * THE PERSON SHELL ASKS ONLY WHAT A MEMBER IS SERVED — as a property of the
 * code, not a hope about it.
 *
 * The person shell is the one every key opens, a member's and an admin's
 * alike (ADR-0031), so each screen in it is drawn for a member and must be
 * built from what a member is served. A screen that asks above that does not
 * fail loudly: the engine refuses the question and the screen draws a refusal
 * nobody understands, or a slice that a member's socket is never pushed stays
 * empty and the screen reports a quiet nothing — the Inbox drew "no seat is
 * stopped" for every member because the queue it was built from read the
 * admin's meter and the admin's coding runs. So this walks the IMPORT GRAPH
 * from every person-shell chunk's entry (`lazyScreen.ts`'s loaders, narrowed
 * by `chunksOf("person")`) and refuses, in any module it reaches:
 *
 *  - a QUESTION above `member` — every `useQuery`, `query` and
 *    `useSearchQuery` literal, read exactly as the engine's gates read them
 *    (`namedCalls`), judged by `QUESTION_REACH`, which `internal/api/queries`
 *    holds against the registry;
 *  - a PUSH above `member` — every store slice the module reads, through a
 *    `lib/store-hooks.ts` hook, `useSlice`, `subscribe` or `state.<slice>`,
 *    judged by `protocol/store.ts`'s `SLICES`, which is held below to
 *    `PUSH_REACH`, which `internal/api/stream` holds against the hub;
 *  - an ACT above `member` — every `act`/`useAct` literal, judged by
 *    `ACTIONS`' own `reach`, which `internal/api/operator` holds;
 *  - a CONSOLE MODULE — anything under `routes/admin/` or `app/console/`,
 *    imported statically or by `import()`, or a console chunk drawn through
 *    `lazyScreen`: the console is a chunk a member never downloads, and a
 *    person screen that reached into it would put it back in theirs.
 *
 * THE MODULE IS THE UNIT, which is the conservative reading: a module a person
 * screen imports for one harmless helper is judged by every read it makes —
 * the frame's own modules included, when a person screen imports one — so a
 * hook that asks an admin's question lives in a module no person screen
 * imports (`routes/admin/agents/seatSetup.ts` left `lib/seats.ts` for exactly
 * that). The two modules judged otherwise are the STORE's: `protocol/store.ts`
 * fills every slice and `lib/store-hooks.ts` binds each to a hook that names
 * its slices in a literal, so neither is charged for the store it holds, and a
 * screen is charged for the hooks it calls.
 *
 * An exemption is a named entry in `EXEMPT` with its reason, and one that
 * stops matching is stale and fails — an exemption nobody checks is how a gate
 * quietly stops covering what it was written for.
 */

// @vitest-environment node

import { describe, expect, test } from "vitest";
import {
  BOUND,
  SPELLED,
  isLocalSource,
  lineOf,
  memberName,
  modules,
  namedCalls,
  parse,
  stringValue,
  walk,
  type Lang,
  type Module,
  type Node,
} from "~/test/source.ts";
import { PUSH_REACH, QUESTION_REACH } from "~/contract/reach.ts";
import { ACTIONS } from "~/contract/actions.ts";
import type { Reach } from "~/contract/access.ts";
import { SLICES, type Slice } from "~/protocol/store.ts";
import { reaches } from "~/lib/viewer.ts";
import { chunksOf, shellOfChunk, type Chunk } from "./lazyScreen.ts";

/** What the person shell is served: every key that opens it reaches this far. */
const PERSON_REACH: Reach = "member";

const STORE_HOOKS = "lib/store-hooks.ts";
const LAZY = "app/lazyScreen.ts";

/** The store itself and its binding: they hold every slice and read none for a screen. */
const STORE_MODULES = new Set(["protocol/store.ts", STORE_HOOKS]);

/** Whether a module is the console's: its screens and its frame chunk. */
function isConsoleModule(path: string): boolean {
  return path.startsWith("routes/admin/") || path.startsWith("app/console/");
}

/** One read a module makes, with the reach it is served at. */
interface Read {
  line: number;
  kind: "question" | "slice" | "act" | "console import" | "console chunk";
  name: string;
  /** Null where the name is not one the engine declares at all. */
  reach: Reach | null;
}

/**
 * The sites that reach above `member` on purpose, and why — each one a SWITCH
 * the frame makes at run time, held by a suite of its own. `name: null` is
 * every name of that kind in the module, for a module whose whole job is the
 * table the switch reads.
 */
const EXEMPT: readonly { path: string; kind: Read["kind"]; name: string | null; why: string }[] = [
  {
    path: "protocol/socket.ts",
    kind: "question",
    name: "live_call",
    why: "the socket asks for a running call's detail only when an agents push names a version of it beyond what the tab holds, and only an admin's push carries a live call at all — a member's rows never name one",
  },
  {
    path: LAZY,
    kind: "console import",
    name: null,
    why: "the loader table names every chunk's entry by `import()`, and each is fetched only for a shell `chunksOf` serves it to — `app/lazy.test.tsx` holds that a reader who is not an admin fetches no console chunk",
  },
  {
    path: "app/frame/peeks.tsx",
    kind: "console chunk",
    name: null,
    why: "the console's peek bodies, drawn only from `PEEKS.admin` — the person shell closes a console object's peek rather than fetching its body (`app/frame/peeks.test.tsx`)",
  },
];

const exempt = (path: string, read: Read) =>
  EXEMPT.some(
    (e) => e.path === path && e.kind === read.kind && (e.name === null || e.name === read.name),
  );

/**
 * Where an import lands, as a module path under `src/` — or null for a
 * package, or a file that is not a module (a stylesheet).
 */
function target(from: string, source: string, tree: ReadonlyMap<string, unknown>): string | null {
  if (!isLocalSource(source)) return null;
  let path: string;
  if (source.startsWith("~/")) {
    path = source.slice(2);
  } else {
    const parts = from.split("/").slice(0, -1);
    for (const seg of source.split("/")) {
      if (seg === "..") parts.pop();
      else if (seg !== ".") parts.push(seg);
    }
    path = parts.join("/");
  }
  for (const candidate of [path, `${path}.ts`, `${path}.tsx`, `${path}/index.ts`]) {
    if (tree.has(candidate)) return candidate;
  }
  // A STYLESHEET, an image: a file the bundler takes and no module.
  if (/\.[a-z0-9]+$/i.test(path) && !/\.tsx?$/.test(path)) return null;
  throw new Error(`${from} imports ${source}, which is no module of this tree`);
}

/** A module's imports: the static ones the bundler follows, and the `import()`s it splits on. */
function importsOf(
  mod: Module,
  program: Node,
  tree: ReadonlyMap<string, unknown>,
): { statics: { to: string; line: number }[]; dynamics: { to: string; line: number }[] } {
  const line = lineOf(mod.text);
  const statics: { to: string; line: number }[] = [];
  const dynamics: { to: string; line: number }[] = [];
  walk(program, (node) => {
    const decl = node as Node & { importKind?: string; exportKind?: string; source?: Node };
    if (
      (node.type === "ImportDeclaration" ||
        node.type === "ExportNamedDeclaration" ||
        node.type === "ExportAllDeclaration") &&
      decl.source &&
      decl.importKind !== "type" &&
      decl.exportKind !== "type"
    ) {
      const to = target(mod.path, String(stringValue(decl.source)), tree);
      if (to) statics.push({ to, line: line(node.start) });
    }
    if (node.type === "ImportExpression") {
      const source = stringValue(node.source as Node);
      const to = source === null ? null : target(mod.path, source, tree);
      if (to) dynamics.push({ to, line: line(node.start) });
    }
  });
  return { statics, dynamics };
}

/** The string literals of an array literal, or null when it is anything else. */
function literalArray(node: Node | undefined): string[] | null {
  if (node?.type !== "ArrayExpression") return null;
  const out: string[] = [];
  for (const el of node.elements as (Node | null)[]) {
    const value = el ? stringValue(el) : null;
    if (value === null) return null;
    out.push(value);
  }
  return out;
}

/**
 * Each hook `lib/store-hooks.ts` exports, and the slices it reads — from the
 * literal arrays its body hands `useSlice`, which is the only way the module
 * reads the store. A hook whose array is not literal is refused by name: its
 * slices would be invisible here, so every screen calling it would pass.
 */
function hookSlices(text: string, lang: Lang): Map<string, Slice[]> {
  const out = new Map<string, Slice[]>();
  for (const statement of parse(text, lang).body as Node[]) {
    if (statement.type !== "ExportNamedDeclaration") continue;
    const fn = statement.declaration as Node | null;
    if (fn?.type !== "FunctionDeclaration") continue;
    const name = String((fn.id as Node).name);
    const slices: Slice[] = [];
    walk(fn, (node) => {
      if (node.type !== "CallExpression") return;
      const callee = node.callee as Node;
      if (callee.type !== "Identifier" || callee.name !== "useSlice") return;
      const first = (node.arguments as Node[])[0];
      const named = literalArray(first);
      // useSlice's own definition passes its parameter on, and is not a hook
      // that reads anything of its own.
      if (named === null) {
        if (name === "useSlice") return;
        throw new Error(`${STORE_HOOKS}: ${name} reads slices this gate cannot name`);
      }
      for (const s of named) slices.push(s as Slice);
    });
    if (slices.length > 0) out.set(name, slices);
  }
  return out;
}

/**
 * Every read one module makes that has a reach: its questions, its writes, the
 * slices it reads, and its reach into the console.
 *
 * `hooks` is [hookSlices]' map, charged where a hook IMPORTED FROM
 * `lib/store-hooks.ts` is called; the hook library itself is charged nothing,
 * since what it defines is read only where a screen calls it.
 */
function readsOf(
  mod: Module,
  hooks: ReadonlyMap<string, readonly Slice[]>,
  tree: ReadonlyMap<string, unknown>,
): Read[] {
  const line = lineOf(mod.text);
  const program = parse(mod.text, mod.lang);
  const reads: Read[] = [];
  const reachOfSlice = (s: string): Reach | null => (s in SLICES ? SLICES[s as Slice].reach : null);

  // QUESTIONS AND WRITES, read as the engine's gates read them.
  for (const call of namedCalls(mod.text, mod.lang).calls) {
    if (call.name === null) continue;
    if (SPELLED.has(call.callee)) {
      const reach = (QUESTION_REACH as Record<string, Reach>)[call.name] ?? null;
      reads.push({ line: call.line, kind: "question", name: call.name, reach });
    } else if (BOUND.has(call.callee)) {
      const row = (ACTIONS as Record<string, { reach: Reach }>)[call.name];
      reads.push({ line: call.line, kind: "act", name: call.name, reach: row?.reach ?? null });
    }
  }

  // THE STORE'S HOOKS, by the name each is imported under.
  const bound = new Map<string, string>();
  for (const statement of program.body as Node[]) {
    if (statement.type !== "ImportDeclaration" || statement.importKind === "type") continue;
    const source = String((statement.source as Node).value);
    if (target(mod.path, source, tree) !== STORE_HOOKS) continue;
    for (const spec of statement.specifiers as Node[]) {
      if (spec.type !== "ImportSpecifier" || spec.importKind === "type") continue;
      const imported = spec.imported as Node;
      const name = String(imported.type === "Identifier" ? imported.name : imported.value);
      if (hooks.has(name)) bound.set(String((spec.local as Node).name), name);
    }
  }

  const library = STORE_MODULES.has(mod.path);
  walk(program, (node) => {
    if (node.type === "CallExpression") {
      const callee = node.callee as Node;
      const args = node.arguments as Node[];
      if (callee.type === "Identifier" && bound.has(String(callee.name))) {
        for (const s of hooks.get(bound.get(String(callee.name))!)!) {
          reads.push({ line: line(node.start), kind: "slice", name: s, reach: reachOfSlice(s) });
        }
      }
      // A SLICE NAMED IN PLACE: `useSlice([...])`, `store.subscribe([...])`.
      const fn = callee.type === "Identifier" ? String(callee.name) : memberName(callee);
      if (!library && (fn === "useSlice" || fn === "subscribe")) {
        for (const s of literalArray(args[0]) ?? []) {
          reads.push({ line: line(node.start), kind: "slice", name: s, reach: reachOfSlice(s) });
        }
      }
      // A CHUNK DRAWN BY NAME: the console's, from a person screen, is the
      // console's code downloaded for a member.
      if (
        callee.type === "Identifier" &&
        (callee.name === "lazyScreen" || callee.name === "loadChunk")
      ) {
        const chunk = stringValue(args[0]);
        if (chunk !== null && shellOfChunk(chunk as Chunk) === "admin") {
          reads.push({
            line: line(node.start),
            kind: "console chunk",
            name: chunk,
            reach: "admin",
          });
        }
      }
    }
    // A SLICE READ OFF THE STATE: `store.state.events`.
    if (!library && node.type === "MemberExpression") {
      const object = node.object as Node;
      const name = memberName(node);
      if (name !== null && name in SLICES && memberName(object) === "state") {
        reads.push({ line: line(node.start), kind: "slice", name, reach: reachOfSlice(name) });
      }
    }
  });

  // THE CONSOLE'S MODULES, however they are reached.
  const { statics, dynamics } = importsOf(mod, program, tree);
  for (const edge of [...statics, ...dynamics]) {
    if (isConsoleModule(edge.to)) {
      reads.push({ line: edge.line, kind: "console import", name: edge.to, reach: "admin" });
    }
  }
  return reads;
}

/**
 * Each person-shell chunk and the module its loader imports, read off
 * `lazyScreen.ts`'s `LOADERS` — the `import()` literals the bundler splits on.
 */
function personEntries(tree: ReadonlyMap<string, Module>): Map<Chunk, string> {
  const lazy = tree.get(LAZY);
  if (!lazy) throw new Error(`${LAZY} is not in the tree`);
  const loaders = new Map<string, string>();
  walk(parse(lazy.text, lazy.lang), (node) => {
    if (node.type !== "VariableDeclarator" || (node.id as Node).name !== "LOADERS") return;
    let init = node.init as Node;
    while (init.type === "TSSatisfiesExpression" || init.type === "TSAsExpression") {
      init = init.expression as Node;
    }
    for (const prop of init.properties as Node[]) {
      const key = prop.key as Node;
      const name = key.type === "Identifier" ? String(key.name) : stringValue(key);
      const body = (prop.value as Node).body as Node;
      const source = body.type === "ImportExpression" ? stringValue(body.source as Node) : null;
      const to = source === null ? null : target(LAZY, source, tree);
      if (name !== null && to !== null) loaders.set(name, to);
    }
    return false;
  });
  const out = new Map<Chunk, string>();
  for (const chunk of chunksOf("person")) {
    const entry = loaders.get(chunk);
    if (!entry) throw new Error(`the person chunk “${chunk}” has no loader in ${LAZY}`);
    out.set(chunk, entry);
  }
  return out;
}

/** Every module the person chunks reach statically, each with the module that first reached it. */
function personGraph(tree: ReadonlyMap<string, Module>): Map<string, string | null> {
  const from = new Map<string, string | null>();
  const queue: string[] = [];
  for (const entry of personEntries(tree).values()) {
    if (!from.has(entry)) {
      from.set(entry, null);
      queue.push(entry);
    }
  }
  while (queue.length > 0) {
    const path = queue.shift()!;
    const mod = tree.get(path)!;
    for (const edge of importsOf(mod, parse(mod.text, mod.lang), tree).statics) {
      if (from.has(edge.to)) continue;
      from.set(edge.to, path);
      queue.push(edge.to);
    }
  }
  return from;
}

/** How a module was reached: `routes/inbox/Inbox.tsx ← routes/inbox/index.ts`. */
function chain(from: ReadonlyMap<string, string | null>, path: string): string {
  const out = [path];
  for (let at = from.get(path); at; at = from.get(at)) out.push(at);
  return out.join(" ← ");
}

describe("the person shell", () => {
  const tree = new Map(modules().map((m) => [m.path, m]));
  const store = tree.get(STORE_HOOKS)!;
  const hooks = hookSlices(store.text, store.lang);
  const graph = personGraph(tree);
  const reads = [...graph.keys()].map((path) => ({
    path,
    reads: readsOf(tree.get(path)!, hooks, tree),
  }));

  test("asks, reads and writes only what a member is served, and nothing of the console", () => {
    const offences = reads.flatMap(({ path, reads }) =>
      reads
        .filter((r) => (r.reach === null || !reaches(PERSON_REACH, r.reach)) && !exempt(path, r))
        .map(
          (r) =>
            `${path}:${r.line} — ${r.kind} “${r.name}” (${r.reach ?? "not the engine's"}), reached from ${chain(graph, path)}`,
        ),
    );
    expect(
      offences,
      "a person screen is drawn for a member: take the read to the console, or to a module no person screen imports",
    ).toEqual([]);
  });

  test("and every exemption it names still matches", () => {
    const stale = EXEMPT.filter(
      (e) =>
        !reads.some(({ path, reads }) => path === e.path && reads.some((r) => exempt(path, r))),
    ).map((e) => `${e.path} — ${e.kind} “${e.name}”`);
    expect(stale, "this exemption no longer matches anything: delete it").toEqual([]);
  });

  // NOT VACUOUS: a parser that stopped seeing imports, calls or hooks would
  // pass the rule above over nothing.
  test("and it reads the tree, so it cannot pass by reading nothing", () => {
    expect(personEntries(tree).size).toBe(chunksOf("person").length);
    expect(chunksOf("person").length).toBeGreaterThanOrEqual(6);
    expect(graph.size).toBeGreaterThan(100);
    const kinds = (kind: Read["kind"]) =>
      new Set(
        reads.flatMap(({ reads }) => reads.filter((r) => r.kind === kind).map((r) => r.name)),
      );
    expect(kinds("question").size).toBeGreaterThan(20);
    expect(kinds("act").size).toBeGreaterThan(5);
    expect([...kinds("slice")]).toEqual(expect.arrayContaining(["agents", "org"]));
    expect(hooks.get("useAgents")).toEqual(["agents"]);
    expect(hooks.size).toBeGreaterThanOrEqual(10);
    // THE CONSOLE IS NOT IN IT: the walk stops at the person chunks.
    expect([...graph.keys()].filter(isConsoleModule)).toEqual([]);
  });
});

describe("the slices", () => {
  // A SLICE IS A PUSH HELD, so it is served no lower than the pushes that fill
  // it: a slice marked `member` that an admin's push fills would pass every
  // person screen that reads it, and draw nothing for a member.
  test("are served no lower than the pushes that fill them", () => {
    const low = Object.entries(SLICES).flatMap(([slice, row]) =>
      row.from
        .filter((kind) => {
          const pushed = (PUSH_REACH as Partial<Record<string, Reach>>)[kind];
          return pushed === undefined || !reaches(row.reach, pushed);
        })
        .map((kind) => `${slice} (${row.reach}) is filled by ${kind}`),
    );
    expect(low).toEqual([]);
  });

  test("hold every kind the engine broadcasts", () => {
    const filled = new Set(Object.values(SLICES).flatMap((row) => row.from as readonly string[]));
    expect(Object.keys(PUSH_REACH).filter((kind) => !filled.has(kind))).toEqual([]);
  });
});

// THE RULE'S RED HALF: every way a person module reaches above `member` must
// come back as a read the rule refuses.
describe("the reader", () => {
  const tree = new Map(modules().map((m) => [m.path, m]));
  const store = tree.get(STORE_HOOKS)!;
  const hooks = hookSlices(store.text, store.lang);
  const read = (text: string) =>
    readsOf({ path: "routes/today/Probe.tsx", text, lang: "tsx" }, hooks, tree);

  test.each([
    [
      "a question",
      'import { useQuery } from "~/lib/useQuery.ts";\nuseQuery("turns");',
      "question",
      "turns",
    ],
    [
      "a search",
      'import { useSearchQuery } from "~/lib/useSearchQuery.ts";\nuseSearchQuery("events");',
      "question",
      "events",
    ],
    ["a socket's query", 'socket.query("trace", { id });', "question", "trace"],
    [
      "a store hook",
      'import { useEvents } from "~/lib/store-hooks.ts";\nconst e = useEvents();',
      "slice",
      "events",
    ],
    [
      "a store hook under another name",
      'import { useOrgBudget as meter } from "~/lib/store-hooks.ts";\nmeter();',
      "slice",
      "budget",
    ],
    [
      "a slice named in place",
      'useSlice(["sandboxes"], (s) => s.sandboxes);',
      "slice",
      "sandboxes",
    ],
    ["a subscription", 'store.subscribe(["tokens"], fn);', "slice", "tokens"],
    ["a slice read off the state", "const t = client.store.state.tools;", "slice", "tools"],
    [
      "a console module",
      'import { Overview } from "~/routes/admin/overview/Overview.tsx";',
      "console import",
      "routes/admin/overview/Overview.tsx",
    ],
    [
      "a console module by import()",
      'const m = import("~/app/console/index.ts");',
      "console import",
      "app/console/index.ts",
    ],
    [
      "a console chunk",
      'const X = lazyScreen("overview", (m) => m.Overview);',
      "console chunk",
      "overview",
    ],
  ])("refuses %s", (_name, text, kind, name) => {
    const found = read(text).filter((r) => r.kind === kind && r.name === name);
    expect(found, `nothing read in ${text}`).toHaveLength(1);
    expect(reaches(PERSON_REACH, found[0]!.reach ?? "admin")).toBe(false);
  });

  test("and a question nobody answers is refused as no reach at all", () => {
    const [r] = read('import { useQuery } from "~/lib/useQuery.ts";\nuseQuery("work_item_turns");');
    expect(r).toMatchObject({ kind: "question", name: "work_item_turns", reach: null });
  });

  // AND ITS GREEN HALF: what a person screen does make passes.
  test.each([
    [
      "a member's question",
      'import { useQuery } from "~/lib/useQuery.ts";\nuseQuery("work_item");',
    ],
    ["the roster", 'import { useAgents } from "~/lib/store-hooks.ts";\nuseAgents();'],
    ["the chart", 'import { useOrg } from "~/lib/store-hooks.ts";\nuseOrg();'],
    ["a person's write", 'import { useAct } from "~/lib/useAct.ts";\nuseAct("mark_inbox");'],
    ["a person chunk", 'const X = lazyScreen("work", (m) => m.Work);'],
    ["a shared module", 'import { href } from "~/app/router.tsx";'],
  ])("passes %s", (_name, text) => {
    const above = read(text).filter((r) => r.reach === null || !reaches(PERSON_REACH, r.reach));
    expect(above).toEqual([]);
  });
});
