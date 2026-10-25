// @vitest-environment node
/**
 * THE SUITE'S BUDGETS ARE IN ONE PLACE, and this is what keeps them there:
 * no case, hook or wait in the tree names a budget of its own.
 *
 * The three bounds are sized in `bounds.ts`, each from what the suite was
 * measured to take on a loaded machine and written down beside the figure
 * with the measurement it came from, and set in three places: `testTimeout`
 * and `hookTimeout` in vitest.config.ts, the wait's as testing-library's
 * `asyncUtilTimeout` in `test/setup.ts`, and the wait's again as the bound
 * `eventually` hands Vitest's own wait, in `test/settle.ts`. A case that sets
 * its own is a second budget beside those, with a figure nobody re-measures:
 * it is how a loop of forty mounts came to carry thirty seconds while the
 * rest of the suite ran out of five, and how the rich editor's longest
 * document came to carry three times the suite's. A case that needs more
 * than the suite's bound is split or made cheaper instead, and this refuses
 * the other way out by name:
 *
 *  - a third argument to `test`, `it`, `describe` or `suite`, in any of their
 *    forms (`test.each(rows)(…)`, `test.skip(…)`), or a `timeout` in the
 *    options any of them is handed;
 *  - a second argument to any hook that takes one: `beforeAll`, `beforeEach`,
 *    `afterAll`, `afterEach`, `aroundAll`, `aroundEach`, `onTestFinished` and
 *    `onTestFailed`;
 *  - a `timeout` handed to testing-library's `waitFor`,
 *    `waitForElementToBeRemoved` or any `findBy`/`findAllBy` query, or
 *    options a reader cannot see into (a number, a variable, or a literal
 *    with a spread or a computed key in it);
 *  - `vi.waitFor`, `vi.waitUntil` and `expect.poll` called anywhere but in
 *    `eventually`, even bare: each gives up after one second unless told
 *    otherwise (a bare number is that bound too), a budget beside the
 *    suite's that nobody here chose;
 *  - `vi.setConfig`, which moves the budgets from inside a suite;
 *  - and `asyncUtilTimeout`, `testTimeout` or `hookTimeout` named anywhere
 *    under `src/` but the one place each is set.
 *
 * Read from each file's syntax tree, through `source.ts`, so a comment that
 * explains the rule may name what the rule refuses. It reads every suite and
 * every module that imports the test runner or testing-library — the test
 * kits — and nothing that ships, which imports neither.
 */

import { expect, test } from "vitest";

import {
  lineOf,
  memberName,
  modules,
  parse,
  stringValue,
  suites,
  walk,
  type Lang,
  type Node,
} from "./source.ts";

/**
 * The one place a budget may be named under `src/`, by the key it is set
 * under. A map of entries rather than an object literal, so that naming the
 * key here is not itself setting it.
 */
const HOME = new Map([["asyncUtilTimeout", "test/setup.ts"]]);

/**
 * The one place Vitest's own waits are called: `eventually`, which hands
 * them the suite's wait bound, by its name, and nothing else.
 */
const VITEST_WAITS_HOME = "test/settle.ts";
const WAIT_BOUND = "WAIT_BOUND_MS";

const REGISTRARS = new Set(["test", "it", "describe", "suite"]);
const HOOKS = new Set([
  "beforeAll",
  "beforeEach",
  "afterAll",
  "afterEach",
  "aroundAll",
  "aroundEach",
  "onTestFinished",
  "onTestFailed",
]);
/**
 * Testing Library's waits whose second argument is their options:
 * `waitFor(callback, options)`. Vitest's own are [vitestWait]'s.
 */
const WAITS = new Set(["waitFor", "waitForElementToBeRemoved"]);
const BUDGET_KEYS = new Set(["asyncUtilTimeout", "testTimeout", "hookTimeout"]);

/** The name a callee chain starts from: `test` in `test.each(rows)(…)` and in `test.skip(…)`. */
function rootName(callee: Node): string | null {
  let node = callee;
  for (;;) {
    if (node.type === "MemberExpression") node = node.object as Node;
    else if (node.type === "CallExpression") node = node.callee as Node;
    else break;
  }
  return node.type === "Identifier" ? String(node.name) : null;
}

/** The name a call calls: `waitFor` in `waitFor(…)` and in `vi.waitFor(…)`. */
function calledName(callee: Node): string | null {
  return callee.type === "Identifier" ? String(callee.name) : memberName(callee);
}

/** The name a property is written under, when it is one a reader can see. */
function keyOf(property: Node): string | null {
  if (property.type !== "Property") return null;
  const key = property.key as Node;
  if (!property.computed && key.type === "Identifier") return String(key.name);
  return stringValue(key);
}

/**
 * Where one of Testing Library's waits takes the options its budget is set
 * in, or null when `called` is none: second for `waitFor` and
 * `waitForElementToBeRemoved`, third for a `findBy` query, whose second is
 * the query's own.
 */
function waitOptionsAt(called: string | null): number | null {
  if (called === null) return null;
  if (WAITS.has(called)) return 1;
  return /^find(All)?By/.test(called) ? 2 : null;
}

/**
 * `vi.waitFor`, `vi.waitUntil` or `expect.poll` when `callee` is one of
 * Vitest's own waits, or null. Each gives up after one second unless it is
 * told otherwise, so every call is a budget, a bare one included. `poll`
 * only as `expect.poll`: a method of that name on anything else is nobody's
 * budget.
 */
function vitestWait(callee: Node): string | null {
  if (callee.type !== "MemberExpression") return null;
  const object = callee.object as Node;
  if (object.type !== "Identifier") return null;
  const name = memberName(callee);
  if (object.name === "vi" && (name === "waitFor" || name === "waitUntil")) return `vi.${name}`;
  if (object.name === "expect" && name === "poll") return "expect.poll";
  return null;
}

/** Whether `options` hands a wait the suite's own bound, by its name, and no other. */
function boundIsTheSuites(options: Node | undefined): boolean {
  if (options === undefined || !seenWhole(options)) return false;
  const timeout = (options.properties as Node[]).find((property) => keyOf(property) === "timeout");
  const value = timeout?.value as Node | undefined;
  return value?.type === "Identifier" && value.name === WAIT_BOUND;
}

/** Whether `node` is an object literal that names `key`. */
function names(node: Node | undefined, key: string): boolean {
  return (
    node?.type === "ObjectExpression" &&
    (node.properties as Node[]).some((property) => keyOf(property) === key)
  );
}

/**
 * Whether `node` is an object literal a reader can see the whole of: every
 * entry a property whose key is written out. A spread (`{ ...SLOW }`) or a
 * computed key (`{ [key]: 5000 }`) can carry a `timeout` the literal never
 * names, so either makes the options as opaque as a variable would.
 */
function seenWhole(node: Node): boolean {
  return (
    node.type === "ObjectExpression" &&
    (node.properties as Node[]).every((property) => keyOf(property) !== null)
  );
}

/** Every budget `text` names of its own, as `line — what`. */
export function ownBudgets(text: string, lang: Lang, path: string): string[] {
  const line = lineOf(text);
  const found: string[] = [];
  const at = (node: Node, what: string) => found.push(`${line(node.start)} — ${what}`);
  walk(parse(text, lang), (node) => {
    if (node.type === "Property") {
      const key = keyOf(node);
      if (key !== null && BUDGET_KEYS.has(key) && HOME.get(key) !== path) at(node, `sets ${key}`);
      return;
    }
    if (node.type !== "CallExpression") return;
    const callee = node.callee as Node;
    const args = node.arguments as Node[];
    const root = rootName(callee);
    const called = calledName(callee);
    if (root !== null && REGISTRARS.has(root)) {
      if (args.length >= 3) at(node, `a ${root} given its own timeout`);
      else if (args.some((arg) => names(arg, "timeout")))
        at(node, `a ${root} given a timeout option`);
      return;
    }
    if (root !== null && HOOKS.has(root) && args.length >= 2) {
      at(node, `a ${root} given its own timeout`);
      return;
    }
    if (called === "setConfig") {
      // Reported once, as the move it is, rather than again for each budget
      // it names.
      at(node, "vi.setConfig");
      return false;
    }
    const own = vitestWait(callee);
    if (own !== null) {
      if (path !== VITEST_WAITS_HOME) at(node, `${own}, whose bound is not the suite's`);
      else if (!boundIsTheSuites(args[1]))
        at(node, `${own} handed a bound other than ${WAIT_BOUND}`);
      return;
    }
    const slot = waitOptionsAt(called);
    if (slot === null) return;
    const options = args[slot];
    if (args.some((arg) => names(arg, "timeout"))) at(node, `a ${called} given its own timeout`);
    else if (options !== undefined && !seenWhole(options)) {
      // A number (`vi.waitFor(cb, 2000)` reads one as its timeout), a
      // variable, or a literal with a spread or a computed key in it: options
      // this reader cannot see into are refused rather than trusted to hold
      // no budget.
      at(node, `a ${called} given options a reader cannot see into`);
    }
  });
  return found;
}

/** Every file a budget could be set in: the suites, and the kits that import the runner or the queries. */
function testCode() {
  const kits = modules().filter(
    ({ text }) => /from "vitest"/.test(text) || text.includes("@testing-library/"),
  );
  return [...suites(), ...kits];
}

test("no case, hook or wait in the tree names a budget of its own", () => {
  const offenders = testCode().flatMap(({ path, text, lang }) =>
    ownBudgets(text, lang, path).map((where) => `${path}:${where}`),
  );
  expect(
    offenders,
    "these set a budget beside the suite's one (vitest.config.ts, test/setup.ts) — split " +
      "the case or make it cheaper instead",
  ).toEqual([]);
});

test("the files it reads are the suites and the kits, so a clean answer is not an empty one", () => {
  const read = testCode().map(({ path }) => path);
  expect(read.length).toBeGreaterThan(200);
  expect(read).toContain("app/App.test.tsx");
  expect(read).toContain("test/setup.ts");
  expect(read).toContain("test/settle.ts");
  expect(read).toContain("routes/org/builder/testkit.tsx");
  // And nothing that ships: a module that imports neither the runner nor the
  // queries cannot set a budget, so reading it would only cost the parse.
  expect(read).not.toContain("protocol/rest.ts");
});

// EACH SHAPE IT REFUSES, held to being refused, and the shapes beside them
// that are no budget held to passing — a gate whose pattern drifted to match
// nothing would otherwise pass the tree above by reading it and finding no
// offender.
test.each([
  ["a case's third argument", 'test("a", () => {}, 30_000);'],
  ["a table case's third argument", 'test.each([1])("a %s", () => {}, 5000);'],
  ["a skipped case's", 'it.skip("a", () => {}, 5000);'],
  ["a block's", 'describe("a", () => {}, 5000);'],
  ["a timeout option", 'test("a", { timeout: 5000 }, () => {});'],
  ["a hook's second argument", "beforeAll(() => {}, 20_000);"],
  ["an each-hook's", "afterEach(() => {}, BUDGET);"],
  ["an around-hook's", "aroundEach(async (run) => { await run(); }, 20_000);"],
  ["a finish hook's", "onTestFinished(() => {}, 20_000);"],
  ["a wait's timeout", "await waitFor(() => {}, { timeout: 5000 });"],
  ["a quoted timeout key", 'await waitFor(() => {}, { "timeout": 5000 });'],
  ["a bare vi.waitFor", "await vi.waitFor(() => {});"],
  ["a bare vi.waitUntil", "await vi.waitUntil(() => true);"],
  ["a bare expect.poll", "await expect.poll(() => 1).toBe(1);"],
  [
    "vi.waitFor handed the suite's bound outside eventually",
    "await vi.waitFor(() => {}, { timeout: WAIT_BOUND_MS });",
  ],
  ["vi.waitFor's number", "await vi.waitFor(() => {}, 2000);"],
  ["vi.waitFor's option", "await vi.waitFor(() => {}, { timeout: 2000, interval: 5 });"],
  ["vi.waitUntil's number", "await vi.waitUntil(() => true, 2000);"],
  ["expect.poll's option", "await expect.poll(() => 1, { timeout: 2000 }).toBe(1);"],
  ["a wait's options out of sight", "await waitFor(() => {}, slow);"],
  ["a query's wait out of sight", 'await screen.findByText("x", {}, slow);'],
  ["a wait's options spread", "await waitFor(() => {}, { ...SLOW });"],
  ["a wait's options spread beside a key", "await waitFor(() => {}, { container, ...SLOW });"],
  ["a wait's key computed", "await waitFor(() => {}, { [budget]: 5000 });"],
  ["vi.waitFor's options spread", "await vi.waitFor(() => {}, { ...SLOW });"],
  ["vi.waitUntil's options spread", "await vi.waitUntil(() => true, { ...SLOW });"],
  ["expect.poll's options spread", "await expect.poll(() => 1, { ...SLOW }).toBe(1);"],
  ["a removal's options spread", "await waitForElementToBeRemoved(el, { ...SLOW });"],
  ["a query's wait spread", 'await screen.findByText("x", {}, { ...SLOW });'],
  ["a query's wait", 'await screen.findByText("x", {}, { timeout: 3000 });'],
  ["a scoped query's wait", 'await within(box).findAllByRole("row", {}, { timeout: 3000 });'],
  ["a removal's wait", "await waitForElementToBeRemoved(el, { timeout: 3000 });"],
  ["a config move", "vi.setConfig({ testTimeout: 60_000 });"],
  ["the wait budget set elsewhere", "configure({ asyncUtilTimeout: 30_000 });"],
  ["a run budget named in a suite", "const config = { hookTimeout: 1 };"],
])("%s is refused", (_shape, snippet) => {
  expect(ownBudgets(snippet, "ts", "app/a.test.ts")).toHaveLength(1);
});

test.each([
  ["a plain case", 'test("a", () => {});'],
  ["a table case", 'test.each([1])("a %s", (n) => { expect(n).toBe(1); });'],
  ["a plain hook", "beforeEach(() => {});"],
  ["a plain wait", "await waitFor(() => {});"],
  ["a wait given its container", "await waitFor(() => {}, { container, interval: 10 });"],
  ["a wait given a quoted key", 'await waitFor(() => {}, { "interval": 10 });'],
  ["a query's wait given its container", 'await screen.findByText("x", {}, { container });'],
  ["a query whose own options spread", 'await screen.findByRole("button", { ...named });'],
  ["a query given its options", 'await screen.findByRole("button", { name: "Pin" });'],
  ["a query given its options by name", 'await screen.findByRole("button", pin);'],
  ["a poll that is no expect.poll", "store.poll(() => 1, 2000);"],
  ["a wait through eventually", "await eventually(() => expect(sent).toHaveLength(2));"],
  ["a clock moved on", "vi.advanceTimersByTime(5000);"],
  ["a timer armed", "setTimeout(resolve, 5000);"],
  ["a production timeout passed along", "request('POST', '/backup', { timeoutMs: 1800000 });"],
  ["a fetch's own timeout field", "const answer = { timeout: true };"],
  ["a rule's comment", "// test('a', () => {}, 30_000) is refused\nconst a = 1;"],
])("%s passes", (_shape, snippet) => {
  expect(ownBudgets(snippet, "ts", "app/a.test.ts")).toEqual([]);
});

test("eventually hands Vitest's wait the suite's bound, by its name, and no other", () => {
  const home = "test/settle.ts";
  expect(ownBudgets("vi.waitFor(condition, { timeout: WAIT_BOUND_MS });", "ts", home)).toEqual([]);
  expect(ownBudgets("vi.waitFor(condition, { timeout: 10_000 });", "ts", home)).toHaveLength(1);
  expect(ownBudgets("vi.waitFor(condition, { ...BOUND });", "ts", home)).toHaveLength(1);
  expect(ownBudgets("vi.waitFor(condition);", "ts", home)).toHaveLength(1);
});

test("the wait budget may be set in its one place", () => {
  expect(ownBudgets("configure({ asyncUtilTimeout: 18_000 });", "ts", "test/setup.ts")).toEqual([]);
});
