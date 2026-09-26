// @vitest-environment node
import { describe, expect, test } from "vitest";

import { type Lang, type Node, lineOf, modules, parse, stringValue, walk } from "../test/source.ts";

/**
 * A query whose parameter is not chosen yet must not be ASKED.
 *
 * # Why this is a test and not a convention
 *
 * `useQuery(what, params)` sends the frame whether `params` is an object or
 * `undefined` — an absent params object is an EMPTY one on the wire, not a
 * skipped question. So the idiom every screen reaches for,
 *
 *     useQuery("work_project", project ? { key: project } : undefined, …)
 *
 * reads like a guard and is not one. The engine refuses a question missing a
 * parameter it has no default for (`queries: bad parameters: key is
 * required`), and the refusal is invisible where it is made: the screen keeps
 * its last answer and renders an error for a question nobody asked, while the
 * ENGINE logs it once per poll — for the whole time a board sits on its
 * default view. The guard that works is `enabled`, and it is one word away
 * from the guard that does not.
 *
 * Three of the tree's nine conditional call sites had it wrong when this was
 * written — `work_project` on the board (which polls at a minute and whose
 * parameter is empty until somebody picks a project, so it fired for ever),
 * and `work_my_work` among them — while six had it right. That ratio is
 * what makes this a test: the correct and incorrect forms differ by an option
 * nobody misses in review, and nothing else in the build can tell them apart.
 *
 * # What it cannot see
 *
 * A parameter that is required by the engine but always non-empty at the call
 * site needs no guard and gets none — `{ id }` on a screen the router only
 * reaches with an id. This checks the shape that ADMITS it may be empty, which
 * is the shape that is wrong without `enabled`.
 */

interface Call {
  what: string;
  where: string;
  guarded: boolean;
}

/**
 * Every `useQuery` call whose params may be `undefined`.
 *
 * READ FROM THE SYNTAX TREE (`test/source.ts`), not the text. Prettier breaks
 * exactly these calls over several lines, and the options object is optional
 * on the hook — a conditional params argument passed with NO options at all is
 * the shape that cannot possibly be guarded, and it is the one a scan that had
 * to be told how far to look skipped. It was a bracket count over the text
 * once, and the text could not say WHICH argument anything was in: it read a
 * kind only in double quotes, took a `: undefined` anywhere in the call as a
 * conditional params argument (an option's `pollMs: waiting ? … : undefined`
 * put a params-less call in the list), and took an `enabled` anywhere as the
 * guard, the params object included — where `{ enabled: key }` guards
 * nothing, since the engine receives it as a parameter. A call's arguments in
 * the tree are its arguments, whatever their layout and whichever quote the
 * kind is written in, and the guard is the option.
 *
 * A kind that is not a constant string is not read here; `app/source.test.ts`
 * refuses one anywhere in the tree.
 */
function conditionalCalls(file: string, text: string, lang: Lang = "tsx"): Call[] {
  const line = lineOf(text);
  const found: Call[] = [];
  walk(parse(text, lang), (node) => {
    if (node.type !== "CallExpression") return;
    const callee = node.callee as Node;
    if (callee.type !== "Identifier" || callee.name !== "useQuery") return;
    const [kind, params, options] = node.arguments as (Node | undefined)[];
    const what = stringValue(kind);
    if (what === null || !params || !mayBeUndefined(params)) return;
    found.push({
      what,
      where: `${file}:${line(node.start)}`,
      guarded:
        options?.type === "ObjectExpression" &&
        (options.properties as Node[]).some(
          (p) =>
            p.type === "Property" &&
            !p.computed &&
            (p.key as Node).type === "Identifier" &&
            (p.key as Node).name === "enabled",
        ),
    });
  });
  return found;
}

/** The expressions TypeScript wraps a value in without changing it. */
const WRAPPERS = new Set([
  "TSAsExpression",
  "TSSatisfiesExpression",
  "TSNonNullExpression",
  "TSTypeAssertion",
  "ParenthesizedExpression",
]);

/** A node with every type-only wrapper taken off: `x as T`, `x satisfies T`, `x!`. */
function unwrapped(node: Node): Node {
  let at = node;
  while (WRAPPERS.has(at.type)) at = at.expression as Node;
  return at;
}

/** `undefined`, however it is written: the identifier, or `void` anything. */
function isUndefined(node: Node): boolean {
  const at = unwrapped(node);
  return (
    (at.type === "Identifier" && at.name === "undefined") ||
    (at.type === "UnaryExpression" && at.operator === "void")
  );
}

/**
 * Whether a params argument is a CHOICE that may come out `undefined`: a
 * ternary with an `undefined` branch AT ANY DEPTH, since `a ? {a} : b ? {b} :
 * undefined` is as empty when neither is chosen as the one-level form is, and
 * through a cast, since `(… : undefined) as Params` sends what it wraps.
 *
 * A bare `undefined` is not a choice: that call asks with no parameters every
 * time, which is a decision rather than a guard somebody forgot.
 */
function mayBeUndefined(params: Node): boolean {
  const at = unwrapped(params);
  if (at.type !== "ConditionalExpression") return false;
  const branch = (node: Node) => isUndefined(node) || mayBeUndefined(node);
  return branch(at.consequent as Node) || branch(at.alternate as Node);
}

describe("a query that may have no parameters", () => {
  const all = modules().flatMap(({ path, text, lang }) => conditionalCalls(path, text, lang));

  test("passes `enabled` so it is skipped rather than refused", () => {
    const unguarded = all.filter((c) => !c.guarded);
    expect(unguarded.map((c) => `${c.what} — ${c.where}`)).toEqual([]);
  });

  test("is found at all, so the scan cannot pass by reading nothing", () => {
    // Vacuous if the pattern stops matching — which a reformat of these
    // calls, a renamed hook or a moved source root would each do silently.
    expect(all.length).toBeGreaterThanOrEqual(3);
    expect(all.some((c) => c.what === "work_project")).toBe(true);
  });

  test("is caught even when the call passes no options at all", () => {
    // The shape a bounded regex skipped, and the worst one: no options
    // object means there is nowhere for `enabled` to be.
    const bare = conditionalCalls(
      "x.tsx",
      `const a = useQuery("work_project", key ? { key } : undefined);`,
    );
    expect(bare.map((c) => ({ what: c.what, guarded: c.guarded }))).toEqual([
      { what: "work_project", guarded: false },
    ]);
  });

  test("and a guarded call over several lines is read as guarded", () => {
    const wrapped = conditionalCalls(
      "x.tsx",
      `const a = useQuery("work_project", chosen ? { project: chosen } : undefined, {
         enabled: chosen !== "",
         pollMs: 60_000,
       });`,
    );
    expect(wrapped.map((c) => c.guarded)).toEqual([true]);
  });

  test("and a kind in backticks is read like one in quotes", () => {
    // A constant template is as much a literal as a quoted string, and the
    // engine's own reader takes it as one; the text scan this replaced read
    // double quotes only, so a backticked kind was never checked at all.
    const ticked = conditionalCalls(
      "x.tsx",
      "const a = useQuery(`work_project`, key ? { key } : undefined);",
    );
    expect(ticked.map((c) => ({ what: c.what, guarded: c.guarded }))).toEqual([
      { what: "work_project", guarded: false },
    ]);
  });

  test("and a ternary among the options is not a conditional question", () => {
    // What the text scan read as one: the params are absent outright, so the
    // question is always asked with none, and `enabled` is there anyway.
    const optionOnly = conditionalCalls(
      "x.tsx",
      'const a = useQuery("fleet", undefined, { enabled: waiting, pollMs: waiting ? MS : undefined });',
    );
    expect(optionOnly).toEqual([]);
  });

  test.each([
    ["nested in a second ternary", "a ? { a } : b ? { b } : undefined"],
    ["on the consequent side", "key ? undefined : { key }"],
    ["cast", "key ? { key } : (undefined as unknown as Params)"],
    ["as `void`", "key ? { key } : void 0"],
    [
      "behind a cast of the whole choice",
      "(key ? { key } : undefined) satisfies Params | undefined",
    ],
    ["behind a non-null assertion", "(key ? { key } : undefined)!"],
  ])("and an `undefined` %s is still a choice that may be empty", (_name, params) => {
    // What the text scan caught by accident — it took any params ending in
    // `: undefined` — and a one-level syntax check would lose: the shape is
    // still a question asked with nothing when no branch is chosen.
    const found = conditionalCalls("x.tsx", `const a = useQuery("work_project", ${params});`);
    expect(found.map((c) => ({ what: c.what, guarded: c.guarded }))).toEqual([
      { what: "work_project", guarded: false },
    ]);
  });

  test.each([
    ["an object on both sides", "key ? { key } : { key: DEFAULT }"],
    ["always none", "undefined"],
    ["a plain object", "{ key }"],
  ])("and params that are %s are not a conditional question", (_name, params) => {
    expect(conditionalCalls("x.tsx", `const a = useQuery("work_project", ${params});`)).toEqual([]);
  });

  test("and `enabled` among the parameters is not the guard", () => {
    // The params object is sent to the engine as the question's parameters; an
    // `enabled` inside it asks with a parameter nobody reads and skips
    // nothing. Only the option is the guard.
    const inParams = conditionalCalls(
      "x.tsx",
      'const a = useQuery("work_project", key ? { key, enabled: true } : undefined);',
    );
    expect(inParams.map((c) => c.guarded)).toEqual([false]);
  });
});
