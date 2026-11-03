/**
 * The one walk of the tree every source gate shares, held to what each gate
 * relies on it for.
 *
 * Two promises, each of which a gate depends on without saying so: a module is
 * parsed once however many gates in a suite read it (a gate over the tree is
 * CPU-bound work inside the runner's per-test budget, and one that parsed the
 * tree again ran out of it on a loaded machine), and a node's children come
 * out in SOURCE order whatever order its fields are listed in — the order a
 * gate reports offenders in, and the order `money.test.tsx` assembles a run of
 * text by.
 */

import { expect, test } from "vitest";

import { childrenOf, modules, parse, suites, walk, type Node } from "./source.ts";

test("a module is parsed once however many gates read it", () => {
  const { text, lang } = modules()[0]!;
  expect(parse(text, lang)).toBe(parse(text, lang));
  // The key is the text, so a different text is a different tree, and the same
  // text read as another language is parsed as that language.
  expect(parse(`${text}\n`, lang)).not.toBe(parse(text, lang));
  expect(parse("const a = 1;", "ts")).not.toBe(parse("const a = 1;", "tsx"));
});

// A BENCHMARK IS RUN, NOT SHIPPED: read as a module, `lib/lineDiff.bench.ts`
// would be held to every rule over the code a reader's browser runs, and
// left out of the gates over the suites, whose budgets bind its cases too.
test("a benchmark is read with the suites, never as a module that ships", () => {
  const paths = (list: readonly { path: string }[]) => list.map(({ path }) => path);
  expect(paths(suites())).toContain("lib/lineDiff.bench.ts");
  expect(paths(modules())).not.toContain("lib/lineDiff.bench.ts");
  expect(paths(modules())).toContain("lib/lineDiff.ts");
});

/** A node whose fields are listed in the opposite order to their text. */
function backwards(): Node {
  const node = (type: string, start: number): Node => ({ type, start, end: start + 1 });
  return {
    type: "Outer",
    start: 0,
    end: 30,
    late: node("Late", 20),
    many: [node("Middle", 10), node("Later", 25)],
    early: node("Early", 1),
  };
}

test("a node's children come out in source order whatever order its fields are listed in", () => {
  expect(childrenOf(backwards()).map((child) => child.type)).toEqual([
    "Early",
    "Middle",
    "Late",
    "Later",
  ]);
  // And with the field each is held under, which is what tells a callee from an
  // argument: the walk hands both on in the same order.
  const visited: string[] = [];
  walk(backwards(), (child, _parent, key) => void visited.push(`${key}:${child.type}`));
  expect(visited).toEqual([":Outer", "early:Early", "many:Middle", "late:Late", "many:Later"]);
});

test("a node whose fields are already in order keeps that order", () => {
  const program = parse("const first = 1;\nfunction second(a: number) { return a; }\n", "ts");
  const kinds = childrenOf(program).map((child) => child.type);
  expect(kinds).toEqual(["VariableDeclaration", "FunctionDeclaration"]);
});
