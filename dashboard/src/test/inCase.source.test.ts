// @vitest-environment node

/**
 * A suite reaches `act`, and configures the testing library, only through
 * `inCase.ts`.
 *
 * The binding there ends every act scope, wait and event with the case that
 * made it — but only the ones that go through it. The library's own `act`,
 * imported anywhere else, is the bare one again: a `settle()` every case in a
 * file shares, or a line in a case body, that a case still running after its
 * time ran out opens beside the next case, with nothing to refuse it. And a
 * second `configure` of the library replaces the two wrappers its waits and
 * events are bound through, which unbinds every `findBy`, `waitFor` and
 * `fireEvent` in the run at once.
 *
 * A bare `await act(` cannot be told from a bound one by its text — both are
 * spelled the same — so what is held is where `act` COMES FROM: every file
 * that calls it imports it from `inCase.ts`, and nothing but `inCase.ts`
 * reaches the library's.
 *
 * And nothing polls through Vitest's own `vi.waitFor`, `vi.waitUntil` or
 * `expect.poll`, which nothing can bind: each goes on looking on real timers
 * after its case has ended, and the first two advance whatever fake clock is
 * installed — by then the next case's — before every look. A suite polls
 * through the `poll` `inCase.ts` hands out (`cases.ts` holds it), which ends
 * with its case.
 *
 * READ WITH A PARSER, NOT A PATTERN. There is no ESLint in this tree, and
 * this gate first read the source the way `app/source.test.ts` does: comments
 * blanked by a regular expression, imports matched by another. That blanker
 * cannot tell a comment from a string, so a string holding a slash and a star
 * — any glob, `"./symbols/*\/*.svg"` — opened a "comment" that ran to the
 * next star and slash and blanked the real code between, an import included;
 * and the import pattern could not see a re-export, a `{ default as X }` or a
 * dynamic `import()`, and took an `act` from any file named `inCase.ts` for
 * the binding's. Vite's own parser (`parseAst`, the one the build runs)
 * reads the file as the compiler does, so a comment is never code and a
 * string is never a comment, and every shape below is a node rather than a
 * spelling. Each shape the reading must see is held by a case of its own
 * ([SHAPES]), so the reading cannot go blind to one and still pass the walk.
 */

import { readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { parseAst } from "vite";
import { expect, test } from "vitest";

const SRC = fileURLToPath(new URL("..", import.meta.url));
/** The one file that may reach the library's own `act` and `configure`. */
const BINDING = join("test", "inCase.ts");

/** The modules a bare `act` comes from. */
const ACTS = new Set([
  "@testing-library/react",
  "@testing-library/react/pure",
  "react",
  "react-dom/test-utils",
]);
/** The modules whose `configure` replaces the library's wrappers. */
const CONFIGURES = new Set([
  "@testing-library/react",
  "@testing-library/react/pure",
  "@testing-library/dom",
]);
/** Vitest's own polls, by the export that carries them: none ends with its case. */
const POLLS = new Map([
  ["vi", new Set(["waitFor", "waitUntil"])],
  ["expect", new Set(["poll"])],
]);

/** One node of the parsed tree: a `type`, and whatever that type carries. */
type Node = { type: string; [key: string]: unknown };

function isNode(value: unknown): value is Node {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as { type?: unknown }).type === "string"
  );
}

/** Every node under `root`, itself included. */
function* nodes(root: Node): Generator<Node> {
  const stack: Node[] = [root];
  for (let node = stack.pop(); node; node = stack.pop()) {
    yield node;
    for (const value of Object.values(node)) {
      if (Array.isArray(value)) {
        for (const item of value) if (isNode(item)) stack.push(item);
      } else if (isNode(value)) {
        stack.push(value);
      }
    }
  }
}

/** A name a specifier or a property spells: `act`, or the string `"act"`. */
function nameOf(node: unknown): string | null {
  if (!isNode(node)) return null;
  if (node.type === "Identifier") return node.name as string;
  if (node.type === "Literal" && typeof node.value === "string") return node.value;
  return null;
}

/** The identifiers a declaration pattern binds: `act`, `{ act }`, `[act]`, `act = x`, `...act`. */
function bound(pattern: unknown): string[] {
  if (!isNode(pattern)) return [];
  switch (pattern.type) {
    case "Identifier":
      return [pattern.name as string];
    case "ObjectPattern":
      return (pattern.properties as Node[]).flatMap((p) =>
        p.type === "RestElement" ? bound(p.argument) : bound(p.value),
      );
    case "ArrayPattern":
      return (pattern.elements as unknown[]).flatMap(bound);
    case "AssignmentPattern":
      return bound(pattern.left);
    case "RestElement":
      return bound(pattern.argument);
    case "TSParameterProperty":
      return bound(pattern.parameter);
    default:
      return [];
  }
}

/**
 * The name a member expression reaches: `X.act`, or `X["act"]` — a computed
 * member spelled by a variable is the variable's value, which no reading of
 * the source can know.
 */
function memberName(node: Node): string | null {
  return !node.computed || (isNode(node.property) && node.property.type === "Literal")
    ? nameOf(node.property)
    : null;
}

/** What one file does with `act`, as its parsed tree says. */
interface Reading {
  /** What it reaches that only the binding may — the library's `act` and `configure` — or nothing may: Vitest's polls. */
  reaches: string[];
  /** Whether it calls an identifier named `act`. */
  callsAct: boolean;
  /**
   * Whether that `act` is the binding's: imported from it under its own name,
   * and the name bound nowhere else in the file — a local `act` shadows the
   * import, and a call of it is that local's.
   */
  actIsBinding: boolean;
}

/** Whether a module specifier written in `file` names the binding. */
function namesBinding(from: string, file: string): boolean {
  const target = from.startsWith("~/")
    ? join(SRC, from.slice(2))
    : from.startsWith(".")
      ? join(dirname(join(SRC, file)), from)
      : null;
  return target === join(SRC, BINDING);
}

/** Reads `text`, the source of `file` (a path under `src/`). */
function read(file: string, text: string): Reading {
  let program: Node;
  try {
    program = parseAst(text, { lang: file.endsWith(".tsx") ? "tsx" : "ts" }) as unknown as Node;
  } catch (cause) {
    throw new Error(`${file} does not parse`, { cause });
  }
  const reaches: string[] = [];
  /** Default and namespace bindings of a module, whose members are reached by `.`. */
  const wholes = new Map<string, string>();
  /** Local names of vitest's poll carriers (`vi`, `expect`), to the export each is. */
  const vitest = new Map<string, string>();
  /** Every name bound in the file, and how many times. */
  const bindings = new Map<string, number>();
  const bind = (name: string) => bindings.set(name, (bindings.get(name) ?? 0) + 1);
  let importsBindingAct = false;
  let callsAct = false;

  // THE IMPORTS FIRST, which are all at the top level: a member is judged by
  // what its object was imported from, and the walk below meets a statement
  // in no particular order relative to the import it uses.
  for (const node of program.body as Node[]) {
    if (node.type !== "ImportDeclaration" || node.importKind === "type") continue;
    const from = (node.source as Node).value as string;
    for (const spec of node.specifiers as Node[]) {
      if (spec.importKind === "type") continue;
      const local = nameOf(spec.local)!;
      bind(local);
      const imported = spec.type === "ImportSpecifier" ? nameOf(spec.imported) : "default";
      if (imported === "act" && local === "act" && namesBinding(from, file)) {
        importsBindingAct = true;
      }
      if (spec.type !== "ImportSpecifier" || imported === "default") {
        wholes.set(local, from);
      } else if (from === "vitest" && imported !== null && POLLS.has(imported)) {
        vitest.set(local, imported);
      } else if (imported === "act" && ACTS.has(from)) {
        reaches.push(`act from "${from}"`);
      } else if (imported === "configure" && CONFIGURES.has(from)) {
        reaches.push(`configure from "${from}"`);
      }
    }
  }

  for (const node of nodes(program)) {
    switch (node.type) {
      case "ExportNamedDeclaration": {
        if (!isNode(node.source) || node.exportKind === "type") break;
        const from = node.source.value as string;
        for (const spec of node.specifiers as Node[]) {
          const name = nameOf(spec.local);
          if (
            (name === "act" && ACTS.has(from)) ||
            (name === "configure" && CONFIGURES.has(from))
          ) {
            reaches.push(`export ${name} from "${from}"`);
          }
        }
        break;
      }
      case "ExportAllDeclaration": {
        const from = (node.source as Node).value as string;
        if (node.exportKind !== "type" && (ACTS.has(from) || CONFIGURES.has(from))) {
          reaches.push(`export * from "${from}"`);
        }
        break;
      }
      case "ImportExpression": {
        const from = nameOf(node.source);
        if (from !== null && (ACTS.has(from) || CONFIGURES.has(from))) {
          reaches.push(`import("${from}")`);
        }
        break;
      }
      case "MemberExpression": {
        const member = memberName(node);
        if (member === null || !isNode(node.object)) break;
        if (node.object.type === "Identifier") {
          const object = node.object.name as string;
          const from = wholes.get(object);
          if (
            from !== undefined &&
            ((member === "act" && ACTS.has(from)) ||
              (member === "configure" && CONFIGURES.has(from)))
          ) {
            reaches.push(`${object}.${member}`);
          }
          const carrier = vitest.get(object);
          if (carrier !== undefined && POLLS.get(carrier)!.has(member)) {
            reaches.push(`${carrier}.${member}`);
          }
        } else if (
          node.object.type === "MemberExpression" &&
          isNode(node.object.object) &&
          node.object.object.type === "Identifier" &&
          wholes.get(node.object.object.name as string) === "vitest"
        ) {
          // `V.vi.waitFor`, through a namespace import of vitest.
          const carrier = memberName(node.object);
          if (carrier !== null && POLLS.get(carrier)?.has(member)) {
            reaches.push(`${carrier}.${member}`);
          }
        }
        break;
      }
      case "CallExpression":
        if (
          isNode(node.callee) &&
          node.callee.type === "Identifier" &&
          node.callee.name === "act"
        ) {
          callsAct = true;
        }
        break;
      case "VariableDeclarator": {
        bound(node.id).forEach(bind);
        // `const { waitFor } = vi`, which takes the poll off its carrier.
        const carrier =
          isNode(node.init) && node.init.type === "Identifier"
            ? vitest.get(node.init.name as string)
            : undefined;
        if (carrier === undefined || !isNode(node.id) || node.id.type !== "ObjectPattern") break;
        for (const property of node.id.properties as Node[]) {
          const taken = property.type === "Property" ? nameOf(property.key) : null;
          if (taken !== null && POLLS.get(carrier)!.has(taken)) {
            reaches.push(`${carrier}.${taken}`);
          }
        }
        break;
      }
      case "FunctionDeclaration":
      case "FunctionExpression":
      case "ArrowFunctionExpression":
        if (node.type !== "ArrowFunctionExpression") bound(node.id).forEach(bind);
        (node.params as unknown[]).flatMap(bound).forEach(bind);
        break;
      case "ClassDeclaration":
      case "ClassExpression":
        bound(node.id).forEach(bind);
        break;
      case "CatchClause":
        bound(node.param).forEach(bind);
        break;
    }
  }
  return { reaches, callsAct, actIsBinding: importsBindingAct && bindings.get("act") === 1 };
}

/**
 * Every shape the reading must see, and one it must not: each is a way a
 * file once reached — or could reach — the library's `act` unseen, so a
 * reading that went blind to one fails here by name rather than passing the
 * walk below over a tree that happens not to hold it today.
 */
const SHAPES: { shape: string; file?: string; source: string; reading: Partial<Reading> }[] = [
  {
    shape: "a named import",
    source: `import { act } from "@testing-library/react";`,
    reading: { reaches: [`act from "@testing-library/react"`] },
  },
  {
    shape: "an aliased import from react",
    source: `import { act as flush } from "react"; flush(() => {});`,
    reading: { reaches: [`act from "react"`], callsAct: false },
  },
  {
    shape: "a namespace's member",
    source: `import * as RTL from "@testing-library/react"; RTL.act(() => {});`,
    reading: { reaches: ["RTL.act"] },
  },
  {
    shape: "a default import's member",
    source: `import React from "react"; React.act(() => {});`,
    reading: { reaches: ["React.act"] },
  },
  {
    shape: "a default imported by name, its member computed",
    source: `import { default as R } from "react"; R["act"](() => {});`,
    reading: { reaches: ["R.act"] },
  },
  {
    shape: "a re-export",
    source: `export { act } from "react-dom/test-utils";`,
    reading: { reaches: [`export act from "react-dom/test-utils"`] },
  },
  {
    shape: "a re-export of everything",
    source: `export * from "@testing-library/react/pure";`,
    reading: { reaches: [`export * from "@testing-library/react/pure"`] },
  },
  {
    shape: "a dynamic import",
    source: `const m = await import("@testing-library/react");`,
    reading: { reaches: [`import("@testing-library/react")`] },
  },
  {
    shape: "a configure of the library's dom half",
    source: `import { configure } from "@testing-library/dom"; configure({});`,
    reading: { reaches: [`configure from "@testing-library/dom"`] },
  },
  {
    shape: "a configure through a namespace",
    source: `import * as DTL from "@testing-library/dom"; DTL.configure({});`,
    reading: { reaches: ["DTL.configure"] },
  },
  {
    shape: "an import after a string holding a slash and a star",
    source: `const glob = "./symbols/*/*.svg";\nimport { act } from "react";\nconst done = "*/";`,
    reading: { reaches: [`act from "react"`] },
  },
  {
    shape: "an import after a string holding two slashes",
    source: `const at = "#//evil.example"; import { act } from "react";`,
    reading: { reaches: [`act from "react"`] },
  },
  {
    shape: "an import written in a comment, which is not one",
    source: `// import { act } from "react";\n/* import { act } from "react"; */`,
    reading: { reaches: [], callsAct: false },
  },
  {
    shape: "a type-only import, which cannot be called",
    source: `import type { act } from "@testing-library/react";`,
    reading: { reaches: [] },
  },
  {
    shape: "a call of the binding's act",
    file: join("routes", "x.test.tsx"),
    source: `import { act } from "~/test/inCase.ts"; await act(async () => {});`,
    reading: { reaches: [], callsAct: true, actIsBinding: true },
  },
  {
    shape: "a call of the binding's act by a relative path",
    file: join("test", "x.test.tsx"),
    source: `import { act } from "./inCase.ts"; act(() => {});`,
    reading: { callsAct: true, actIsBinding: true },
  },
  {
    shape: "a call of an act from another file named inCase.ts",
    file: join("routes", "x.test.tsx"),
    source: `import { act } from "./inCase.ts"; act(() => {});`,
    reading: { callsAct: true, actIsBinding: false },
  },
  {
    shape: "a call of an act imported from nowhere",
    source: `function act(f: () => void) { f(); } act(() => {});`,
    reading: { callsAct: true, actIsBinding: false },
  },
  {
    shape: "a call of a local act shadowing the binding's",
    file: join("routes", "x.test.tsx"),
    source: `import { act } from "~/test/inCase.ts"; function settle() { const act = (f: () => void) => f(); act(() => {}); }`,
    reading: { callsAct: true, actIsBinding: false },
  },
  {
    shape: "Vitest's waitFor",
    source: `import { vi } from "vitest"; await vi.waitFor(() => {});`,
    reading: { reaches: ["vi.waitFor"] },
  },
  {
    shape: "Vitest's waitUntil, through an alias",
    source: `import { vi as v } from "vitest"; await v.waitUntil(() => true);`,
    reading: { reaches: ["vi.waitUntil"] },
  },
  {
    shape: "Vitest's expect.poll",
    source: `import { expect } from "vitest"; await expect.poll(() => 1).toBe(1);`,
    reading: { reaches: ["expect.poll"] },
  },
  {
    shape: "Vitest's waitFor through a namespace",
    source: `import * as V from "vitest"; await V.vi["waitFor"](() => {});`,
    reading: { reaches: ["vi.waitFor"] },
  },
  {
    shape: "Vitest's waitFor taken off vi",
    source: `import { vi } from "vitest"; const { waitFor } = vi; await waitFor(() => {});`,
    reading: { reaches: ["vi.waitFor"] },
  },
  {
    shape: "the rest of vi, which is not a poll",
    source: `import { vi } from "vitest"; vi.useFakeTimers(); vi.advanceTimersByTime(50);`,
    reading: { reaches: [] },
  },
];

test.each(SHAPES)("the reading sees $shape", ({ file = "x.test.tsx", source, reading }) => {
  const read_ = read(file, source);
  for (const [key, value] of Object.entries(reading)) {
    expect(read_[key as keyof Reading], key).toEqual(value);
  }
});

/** Every TypeScript file under `src/`, suites and helpers alike, read. */
function sources(): { path: string; reading: Reading }[] {
  const out: { path: string; reading: Reading }[] = [];
  (function walk(dir: string): void {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!/\.tsx?$/.test(full)) continue;
      const path = relative(SRC, full);
      out.push({ path, reading: read(path, readFileSync(full, "utf8")) });
    }
  })(SRC);
  return out;
}

const files = sources();

test("the walk reads the suites, and finds the binding reaching the library itself", () => {
  // A walk rooted in the wrong place passes the two rules below having
  // checked nothing. The binding is the one file that must import the
  // library's `act` and `configure`, so finding both there is the proof the
  // walk reached it.
  expect(files.length).toBeGreaterThan(400);
  const binding = files.find((f) => f.path === BINDING);
  expect(binding, `no ${BINDING} under ${SRC}`).toBeDefined();
  expect(binding!.reading.reaches.sort()).toEqual([
    `act from "@testing-library/react"`,
    `configure from "@testing-library/react"`,
  ]);
});

test("nothing but the binding reaches the library's act or configures the library, and nothing polls through Vitest", () => {
  const offenders = files
    .filter((f) => f.path !== BINDING)
    .flatMap((f) => f.reading.reaches.map((what) => `${f.path} — ${what}`));
  expect(
    offenders,
    `import \`act\` and \`poll\` from "~/test/inCase.ts", which end with the case that called them; the library's \`act\` is opened beside the next case by a case that timed out, a second \`configure\` unbinds every wait and event, and Vitest's polls go on looking — and moving the fake clock — in the next case:\n${offenders.join("\n")}`,
  ).toEqual([]);
});

test("every file that calls act takes it from the binding", () => {
  const calls = files.filter((f) => f.path !== BINDING && f.reading.callsAct);
  // The suites that act are many; a reading that saw no call would pass the
  // rule below vacuously.
  expect(calls.length).toBeGreaterThan(50);
  const offenders = calls.filter((f) => !f.reading.actIsBinding).map((f) => f.path);
  expect(
    offenders,
    `these call an \`act\` that is not the case-bound one from "~/test/inCase.ts":\n${offenders.join("\n")}`,
  ).toEqual([]);
});
