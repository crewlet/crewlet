// @vitest-environment node

/**
 * A suite reaches the testing library — and React's `act` — only through
 * `inCase.ts`.
 *
 * The binding there ends every render, act scope, wait and event with the
 * case that made it — but only the ones that go through it. The library
 * imported anywhere else is the bare one again: an `act`, a `render` or a
 * `cleanup` that a case still running after its time ran out makes on the
 * next case's page, with nothing to refuse it. And loading the library is
 * what binds its waits and events (the binding wraps them as it loads), so a
 * suite that took the library from anywhere else would have its `findBy`
 * bound only if some other file happened to load the binding first. A second
 * `configure` of the library replaces the two wrappers its waits and events
 * are bound through, which unbinds every `findBy`, `waitFor` and `fireEvent`
 * in the run at once — and the binding hands out no `configure`.
 *
 * So the rules are where things COME FROM, since a bare `await act(` cannot
 * be told from a bound one by its text: nothing but `inCase.ts` takes a value
 * from the testing library; nothing takes React's own `act`; and every file
 * that calls `act` imports it from `inCase.ts`.
 *
 * And nothing polls through Vitest's own `vi.waitFor`, `vi.waitUntil` or
 * `expect.poll`, which nothing can bind: each goes on looking on real timers
 * after its case has ended, and the first two advance whatever fake clock is
 * installed — by then the next case's — before every look. A suite polls
 * through the `poll` `inCase.ts` hands out (`cases.ts` holds it), which ends
 * with its case.
 *
 * READ WITH A PARSER, NOT A PATTERN. There is no ESLint in this tree, and
 * this gate first read the source with comments blanked by a regular
 * expression and imports matched by another. That blanker cannot tell a
 * comment from a string, so a string holding a slash and a star — any glob,
 * `"./symbols/*\/*.svg"` — opened a "comment" that ran to the next star and
 * slash and blanked the real code between, an import included; and the import
 * pattern could not see a re-export, a `{ default as X }` or a dynamic
 * `import()`, and took an `act` from any file named `inCase.ts` for the
 * binding's. The tree's ONE reading (`./source.ts`: Vite's own parser over
 * the one walk every source gate shares) reads each file as the compiler
 * does, so a comment is never code and a string is never a comment, and every
 * shape below is a node rather than a spelling. Each shape the reading must
 * see is held by a case of its own ([SHAPES]), so the reading cannot go blind
 * to one and still pass the walk. The walk is that file's too — every suite
 * and helper, which is exactly this gate's subject — so what this gate calls
 * the tree is what every other source gate calls it.
 */

import { dirname, join } from "node:path";
import { expect, test } from "vitest";

import { everyModule, isNode, langOf, type Node, parse, SRC } from "./source.ts";

/** The one file that may take a value from the testing library. */
const BINDING = "test/inCase.ts";

/**
 * The WRITE SURFACE's `act` — the protocol's entry every change to the company's
 * work goes through, and the modules a caller takes it from. A different
 * function that shares the spelling: it names a tool, moves no page and has no
 * case to end with, so a file calling it is not calling an unbound React act.
 */
const WRITE_ACT = new Set(["protocol/act.ts", "protocol/index.ts"]);

/** The testing library: every entry a suite could take it from. */
const LIBRARY = new Set([
  "@testing-library/react",
  "@testing-library/react/pure",
  "@testing-library/dom",
]);
/** React's own `act`, and the test utilities that hand it out. */
const REACT_ACTS = new Set(["react", "react-dom/test-utils"]);
/** Vitest's own polls, by the export that carries them: none ends with its case. */
const POLLS = new Map([
  ["vi", new Set(["waitFor", "waitUntil"])],
  ["expect", new Set(["poll"])],
]);

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
 * The name a property key spells: `act`, or `["act"]` — a computed key
 * spelled by a variable is the variable's value, which no reading of the
 * source can know.
 */
function keyName(computed: unknown, key: unknown): string | null {
  return !computed || (isNode(key) && key.type === "Literal") ? nameOf(key) : null;
}

/** The name a member expression reaches: `X.act`, or `X["act"]` ([keyName]). */
function memberName(node: Node): string | null {
  return keyName(node.computed, node.property);
}

/** What one file reaches, as its parsed tree says. */
interface Reading {
  /**
   * What it reaches that only the binding may — the testing library, React's
   * own `act` — or that nothing may: Vitest's polls.
   */
  reaches: string[];
  /**
   * Whether it calls an identifier named `act` — other than the write
   * surface's ([WRITE_ACT]), imported under its own name and bound nowhere
   * else in the file.
   */
  callsAct: boolean;
  /**
   * Whether that `act` is the binding's: imported from it under its own name,
   * and the name bound nowhere else in the file — a local `act` shadows the
   * import, and a call of it is that local's.
   */
  actIsBinding: boolean;
}

/** The file under `src/` a module specifier written in `file` names, or null for a package. */
function targetOf(from: string, file: string): string | null {
  return from.startsWith("~/")
    ? join(SRC, from.slice(2))
    : from.startsWith(".")
      ? join(dirname(join(SRC, file)), from)
      : null;
}

/** Whether a module specifier written in `file` names the binding. */
function namesBinding(from: string, file: string): boolean {
  return targetOf(from, file) === join(SRC, BINDING);
}

/** Whether a module specifier written in `file` names the write surface ([WRITE_ACT]). */
function namesWriteAct(from: string, file: string): boolean {
  const target = targetOf(from, file);
  return target !== null && [...WRITE_ACT].some((module) => target === join(SRC, module));
}

/** Reads `text`, the source of `file` (a path under `src/`). */
function read(file: string, text: string): Reading {
  let program: Node;
  try {
    program = parse(text, langOf(file));
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
  let importsWriteAct = false;
  let callsAct = false;

  /**
   * What a destructuring takes off an import — `const { waitFor } = vi`,
   * `({ act: flush } = React)` — which reaches the member exactly as a `.`
   * does, under a name the member rule below never sees and the call rule
   * does not know.
   */
  const takeOff = (pattern: unknown, object: unknown): void => {
    if (!isNode(pattern) || pattern.type !== "ObjectPattern") return;
    if (!isNode(object) || object.type !== "Identifier") return;
    const name = object.name as string;
    const carrier = vitest.get(name);
    const from = wholes.get(name);
    for (const property of pattern.properties as Node[]) {
      if (property.type !== "Property") continue;
      const taken = keyName(property.computed, property.key);
      if (taken === null) continue;
      if (carrier !== undefined && POLLS.get(carrier)!.has(taken)) {
        reaches.push(`${carrier}.${taken}`);
      }
      if (from !== undefined && taken === "act" && REACT_ACTS.has(from)) {
        reaches.push(`${name}.act`);
      }
    }
  };

  // THE IMPORTS FIRST, which are all at the top level: a member is judged by
  // what its object was imported from, and the walk below meets a statement
  // in no particular order relative to the import it uses.
  for (const node of program.body as Node[]) {
    if (node.type !== "ImportDeclaration" || node.importKind === "type") continue;
    const from = (node.source as Node).value as string;
    const values = (node.specifiers as Node[]).filter((spec) => spec.importKind !== "type");
    // A TYPE moves nothing; a value — or a bare `import "x"`, which runs the
    // module — is the library itself.
    if (LIBRARY.has(from) && (values.length > 0 || (node.specifiers as Node[]).length === 0)) {
      reaches.push(`imports "${from}"`);
    }
    for (const spec of values) {
      const local = nameOf(spec.local)!;
      bind(local);
      const imported = spec.type === "ImportSpecifier" ? nameOf(spec.imported) : "default";
      if (imported === "act" && local === "act" && namesBinding(from, file)) {
        importsBindingAct = true;
      }
      if (imported === "act" && local === "act" && namesWriteAct(from, file)) {
        importsWriteAct = true;
      }
      if (spec.type !== "ImportSpecifier" || imported === "default") {
        wholes.set(local, from);
      } else if (from === "vitest" && imported !== null && POLLS.has(imported)) {
        vitest.set(local, imported);
      } else if (imported === "act" && REACT_ACTS.has(from)) {
        reaches.push(`act from "${from}"`);
      }
    }
  }

  for (const node of nodes(program)) {
    switch (node.type) {
      case "ExportNamedDeclaration": {
        if (!isNode(node.source) || node.exportKind === "type") break;
        const from = node.source.value as string;
        if (LIBRARY.has(from)) {
          reaches.push(`re-exports "${from}"`);
          break;
        }
        for (const spec of node.specifiers as Node[]) {
          if (nameOf(spec.local) === "act" && REACT_ACTS.has(from)) {
            reaches.push(`re-exports act from "${from}"`);
          }
        }
        break;
      }
      case "ExportAllDeclaration": {
        const from = (node.source as Node).value as string;
        if (node.exportKind !== "type" && (LIBRARY.has(from) || REACT_ACTS.has(from))) {
          reaches.push(`re-exports "${from}"`);
        }
        break;
      }
      case "ImportExpression": {
        const from = nameOf(node.source);
        if (from !== null && (LIBRARY.has(from) || REACT_ACTS.has(from))) {
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
          if (from !== undefined && member === "act" && REACT_ACTS.has(from)) {
            reaches.push(`${object}.act`);
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
      case "VariableDeclarator":
        bound(node.id).forEach(bind);
        takeOff(node.id, node.init);
        break;
      case "AssignmentExpression":
        takeOff(node.left, node.right);
        break;
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
  // THE WRITE SURFACE'S `act`, when it is the one `act` the file binds, is not
  // the testing act this rule is about.
  const writes = importsWriteAct && bindings.get("act") === 1;
  return {
    reaches,
    callsAct: callsAct && !writes,
    actIsBinding: importsBindingAct && bindings.get("act") === 1,
  };
}

/**
 * Every shape the reading must see, and the ones it must not: each is a way
 * a file once reached — or could reach — the library or an unbound `act`
 * unseen, so a reading that went blind to one fails here by name rather than
 * passing the walk below over a tree that happens not to hold it today.
 */
const SHAPES: { shape: string; file?: string; source: string; reading: Partial<Reading> }[] = [
  {
    shape: "the library's act",
    source: `import { act } from "@testing-library/react";`,
    reading: { reaches: [`imports "@testing-library/react"`] },
  },
  {
    shape: "the library's render, which moves the page as surely",
    source: `import { render, screen } from "@testing-library/react";`,
    reading: { reaches: [`imports "@testing-library/react"`] },
  },
  {
    shape: "the library through a namespace",
    source: `import * as RTL from "@testing-library/react/pure"; RTL.act(() => {});`,
    reading: { reaches: [`imports "@testing-library/react/pure"`] },
  },
  {
    shape: "the library's dom half, whose configure unbinds the waits",
    source: `import { configure } from "@testing-library/dom"; configure({});`,
    reading: { reaches: [`imports "@testing-library/dom"`] },
  },
  {
    shape: "the library for its side effects alone",
    source: `import "@testing-library/react";`,
    reading: { reaches: [`imports "@testing-library/react"`] },
  },
  {
    shape: "the library's types, which move nothing",
    source: `import type { RenderResult } from "@testing-library/react"; import { type RenderOptions } from "@testing-library/react";`,
    reading: { reaches: [] },
  },
  {
    shape: "a re-export of part of the library",
    source: `export { screen } from "@testing-library/react";`,
    reading: { reaches: [`re-exports "@testing-library/react"`] },
  },
  {
    shape: "a re-export of all of it",
    source: `export * from "@testing-library/react/pure";`,
    reading: { reaches: [`re-exports "@testing-library/react/pure"`] },
  },
  {
    shape: "a dynamic import of the library",
    source: `const m = await import("@testing-library/react");`,
    reading: { reaches: [`import("@testing-library/react")`] },
  },
  {
    shape: "React's act by an alias",
    source: `import { act as flush } from "react"; flush(() => {});`,
    reading: { reaches: [`act from "react"`], callsAct: false },
  },
  {
    shape: "React's act off its default import",
    source: `import React from "react"; React.act(() => {});`,
    reading: { reaches: ["React.act"] },
  },
  {
    shape: "React's act off a default imported by name, its member computed",
    source: `import { default as R } from "react"; R["act"](() => {});`,
    reading: { reaches: ["R.act"] },
  },
  {
    shape: "React's act taken off its default import under another name",
    source: `import React from "react"; const { act: flush } = React; flush(() => {});`,
    reading: { reaches: ["React.act"], callsAct: false },
  },
  {
    shape: "React's act taken off a namespace in an assignment",
    source: `import * as R from "react"; let flush; ({ ["act"]: flush } = R);`,
    reading: { reaches: ["R.act"] },
  },
  {
    shape: "the rest of React taken off its default import",
    source: `import React from "react"; const { useState, useEffect: effect } = React; ({ useMemo: effect } = React);`,
    reading: { reaches: [] },
  },
  {
    shape: "React's act re-exported from the test utilities",
    source: `export { act } from "react-dom/test-utils";`,
    reading: { reaches: [`re-exports act from "react-dom/test-utils"`] },
  },
  {
    shape: "the rest of React, which is not an act",
    source: `import React, { useState } from "react"; React.useEffect(() => {}); useState(0);`,
    reading: { reaches: [] },
  },
  {
    shape: "an import after a string holding a slash and a star",
    source: `const glob = "./symbols/*/*.svg";\nimport { act } from "react";\nconst done = "*/";`,
    reading: { reaches: [`act from "react"`] },
  },
  {
    shape: "an import after a string holding two slashes",
    source: `const at = "#//evil.example"; import { render } from "@testing-library/react";`,
    reading: { reaches: [`imports "@testing-library/react"`] },
  },
  {
    shape: "an import written in a comment, which is not one",
    source: `// import { act } from "react";\n/* import { render } from "@testing-library/react"; */`,
    reading: { reaches: [], callsAct: false },
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
    shape: "a call of the write surface's act, which is not the testing one",
    file: join("lib", "useAct.ts"),
    source: `import { act } from "~/protocol/act.ts"; await act(tool, { args });`,
    reading: { reaches: [], callsAct: false },
  },
  {
    shape: "a call of the write surface's act by a relative path, through its barrel",
    file: join("protocol", "x.test.ts"),
    source: `import { act } from "./index.ts"; await act("set_pins", { args: {} });`,
    reading: { reaches: [], callsAct: false },
  },
  {
    shape: "a call of a local act shadowing the write surface's",
    file: join("lib", "x.ts"),
    source: `import { act } from "~/protocol/act.ts"; function settle() { const act = (f: () => void) => f(); act(() => {}); }`,
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
    shape: "Vitest's waitFor taken off vi in an assignment",
    source: `import { vi } from "vitest"; let poll; ({ waitFor: poll } = vi);`,
    reading: { reaches: ["vi.waitFor"] },
  },
  {
    shape: "a key computed from a variable, which is not the name the variable has",
    source: `import { vi } from "vitest"; const waitFor = "useFakeTimers"; const { [waitFor]: fake } = vi; fake();`,
    reading: { reaches: [] },
  },
  {
    shape: "the rest of vi, which is not a poll",
    source: `import { vi } from "vitest"; vi.useFakeTimers(); vi.advanceTimersByTime(50);`,
    reading: { reaches: [] },
  },
];

test.each(SHAPES)("the reading sees $shape", ({ file = "x.test.tsx", source, reading }) => {
  const seen = read(file, source);
  for (const [key, value] of Object.entries(reading)) {
    expect(seen[key as keyof Reading], key).toEqual(value);
  }
});

/** Every TypeScript file under `src/`, suites and helpers alike, read. */
const files = everyModule().map(({ path, text }) => ({ path, reading: read(path, text) }));

test("the walk reads the suites, and finds the binding reaching the library itself", () => {
  // A walk rooted in the wrong place passes the rules below having checked
  // nothing. The binding is the one file that must take the library, so
  // finding it there is the proof the walk reached it.
  expect(files.length).toBeGreaterThan(400);
  const binding = files.find((f) => f.path === BINDING);
  expect(binding, `no ${BINDING} under ${SRC}`).toBeDefined();
  expect(binding!.reading.reaches).toContain(`imports "@testing-library/react"`);
});

test("nothing but the binding takes the testing library or React's act, and nothing polls through Vitest", () => {
  const offenders = files
    .filter((f) => f.path !== BINDING)
    .flatMap((f) => f.reading.reaches.map((what) => `${f.path} — ${what}`));
  expect(
    offenders,
    `take the testing library — \`act\`, \`render\`, \`screen\` and the rest — and \`poll\` from "~/test/inCase.ts", which ends what they do with the case that did it; the library taken anywhere else moves the next case's page for a case that timed out, its waits are bound only if something else loaded the binding, and Vitest's polls go on looking — and moving the fake clock — in the next case:\n${offenders.join("\n")}`,
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
