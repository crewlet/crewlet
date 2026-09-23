// @vitest-environment node

/**
 * Rules about the source that no type and no runtime check catches.
 *
 * There is no ESLint in this tree — the gates are prettier, `tsc` and this
 * suite — so a rule that would be a lint elsewhere is a test here, written the
 * way `styles/classes.test.ts` is: read the source, apply the rule, name the
 * file and line.
 */

import { describe, expect, test } from "vitest";

import {
  type Lang,
  type Node,
  isLocalSource,
  lineOf,
  memberName,
  modules,
  parse,
  stringValue,
  walk,
} from "../test/source.ts";
import { RAIL } from "./nav.ts";

/** Every source file of the given extensions, excluding the suites. */
function sources(exts: string[] = [".tsx"]): { path: string; text: string }[] {
  return modules().filter(({ path }) => exts.some((ext) => path.endsWith(ext)));
}

/**
 * A NUMBER IS NOT A GUARD.
 *
 * `{rows.length && <Panel/>}` renders the string "0" when the list is empty,
 * because `0 && x` is `0` and React renders a zero. It is the one JSX mistake
 * that produces no error, no warning and no type failure — the page simply
 * grows a stray digit, unlabelled, in the middle of the content.
 *
 * It shipped: the fleet screen drew a bare `0` beneath its panels whenever
 * nothing was unplaceable, which on a screen an operator reads *looking for a
 * number that is wrong* is the worst possible place for one.
 *
 * The rule is `> 0`, or a ternary. This scans for the guard shapes that are
 * numeric by their own text — anything ending in `.length`, and the count-ish
 * fields this wire format actually carries.
 */
test("no JSX guard is a bare number", () => {
  // `{ <expr> && (` or `{ <expr> && <`, where <expr> ends in a numeric-looking
  // member. `> 0`, `=== 0`, `!== 0` and `> 1` are all fine and excluded by the
  // absence of a comparison in the captured text.
  const guard =
    /\{\s*\(?[^}<>=!]*?\.(length|count|calls|rounds|seats|total|used|unread|primary)\s*(\)\s*)?&&/g;
  const offenders: string[] = [];
  for (const { path, text } of sources()) {
    // COMMENTS BLANKED, LINE NUMBERS KEPT. A comment quoting the bad shape in
    // order to explain why the line beneath it avoids one is not the bad shape
    // — and this gate caught exactly that, so the only way to keep the rule was
    // to stop writing down what it is for.
    const lines = text
      .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
      .replace(/(^|[^:])\/\/[^\n]*/g, (m, lead) => lead + " ".repeat(m.length - lead.length))
      .split("\n");
    lines.forEach((line, i) => {
      guard.lastIndex = 0;
      if (!guard.test(line)) return;
      // A comparison anywhere in the guard makes it a boolean.
      if (/[<>]=?\s*\d|===|!==/.test(line.slice(0, line.indexOf("&&")))) return;
      offenders.push(`${path}:${i + 1} — ${line.trim().slice(0, 90)}`);
    });
  }
  expect(
    offenders,
    'these render the string "0" when the count is zero: compare with `> 0` or use a ternary',
  ).toEqual([]);
});

/**
 * EVERY INTERNAL LINK NAMES A LIVE ROUTE.
 *
 * `href(["seats", handle])` compiles, renders, and takes the reader to a
 * screen that says "there is no such screen" — a dead link with no error, no
 * warning and no type failure, and the only way to find one is to click it.
 *
 * Nine files carried them after the routes moved: an event's link from a phase
 * card, a seat's from a colleague chip, a page's from a search hit, a turn's
 * from an item's history, a project's from an item. Every one of them
 * is a way OUT of the screen a reader is on, which is the half of navigation
 * the rail cannot provide.
 *
 * The check is on the FIRST SEGMENT, because that is what route dispatch
 * switches on: a literal that names a segment no workspace owns cannot reach
 * a screen whatever follows it.
 *
 * EVERY SPELLING AND BOTH EXTENSIONS. A link is written as `href([...])`, as
 * `nav.to([...])` for one a control performs rather than one a reader points
 * at, or as a bare `path: [...]` on a value some other component turns into
 * one — and the last lives in plain `.ts`, which is how the ENTIRE attention
 * queue kept pointing at `spend`, `fleet`, `runs`, `config` and `seats` after
 * every one of those moved. That is the Inbox's "needs a person" band: ten
 * rows, all of them dead, in a file this gate was not reading.
 *
 * THEN THE SAME THING HAPPENED AGAIN THROUGH THE SPELLING THIS GATE DID NOT
 * READ. `nav.to` was outside the pattern, so the workspace move left eighteen
 * of them behind — every "Trace" button in the product, every "the turn" from
 * an event, a run and a seat's own list, "Its seat" from a turn and a phase,
 * "the run" from a seat, "spend" from Live now and "back to people" from two
 * screens. Each rendered normally and each landed on Not Found, because the
 * failure of a moved route is a screen that says nothing is there rather than
 * a build that says the link is wrong.
 *
 * AND `path:` IS NOT ONLY A ROUTE. It is also the ENGINE's word for a pointer
 * into the company document — `ConfigProblem.path`, `ConfigWarning.path`,
 * `ConfigReference.path` all carry it, and the org builder's model writes the
 * same shape for a field it sets or forbids: `{ path: ["llm"], credential:
 * false }`, `set: [{ path: ["goal"], value }]`. Those heads are FIELD names
 * and can never be route segments, so reading them here reports twenty-two
 * dead links that are not links at all — and a gate that cries wolf is one
 * somebody switches off, which costs the eighteen real ones above.
 *
 * The two are told apart by what the pointer is FOR, which is on the line:
 * a document pointer names the `value` it sets or marks itself a `credential`
 * field, and a route never does either. The exemption is asserted in both
 * directions below rather than trusted — it has to still fire, and a real nav
 * row has to still be read — because an exemption nobody checks is how a gate
 * quietly stops covering the thing it was written for.
 */

/** Whether a `path:` on this line points into the company document, not at a screen. */
const documentPointer = (line: string) => /\bvalue:|\bcredential:/.test(line);
test("every link names a segment a workspace owns", () => {
  const owned = new Set(RAIL.flatMap((r) => r.owns));
  const literal = /(?:href\(|nav\.to\(|\bpath:\s*)\[\s*"([a-z0-9_-]+)"/g;
  const dead: string[] = [];
  for (const { path, text } of sources([".tsx", ".ts"])) {
    const lines = text.split("\n");
    lines.forEach((line, i) => {
      literal.lastIndex = 0;
      let m: RegExpExecArray | null;
      while ((m = literal.exec(line))) {
        const head = m[1];
        // `href(` and `nav.to(` are unambiguous; only the bare `path:`
        // spelling collides with the document pointer.
        if (m[0].startsWith("path") && documentPointer(line)) continue;
        if (head && !owned.has(head)) dead.push(`${path}:${i + 1} — #/${head}`);
      }
    });
  }
  expect(dead, "these links go to a screen that does not exist").toEqual([]);
});

/*
 * BOTH SIDES OF THAT EXEMPTION. It must still let a document pointer through
 * — otherwise it has stopped mattering and the next reader deletes it — and
 * it must NOT swallow a nav destination, which is the failure that would make
 * the check above silently cover less than it claims.
 */
test("the document-pointer exemption fires, and spares no real link", () => {
  // The shapes routes/org/builder/model writes.
  expect(documentPointer('  { path: ["llm"], credential: false },')).toBe(true);
  expect(documentPointer('    set: [{ path: ["goal"], value: "Ship" }],')).toBe(true);
  // The shapes a nav destination is written in, across all four tables.
  expect(documentPointer('      path: ["admin", "config"],')).toBe(false);
  expect(documentPointer('        { label: "Work", path: ["work"] },')).toBe(false);
  expect(documentPointer('    path: ["activity", "turns"],')).toBe(false);
  expect(documentPointer('      href(["company", "people", handle])')).toBe(false);
});

test("something in the tree is actually exempted, so the rule is not dead weight", () => {
  const exempted = sources([".tsx", ".ts"]).flatMap(({ path, text }) =>
    text
      .split("\n")
      .map((line, i) => ({ line, at: `${path}:${i + 1}` }))
      .filter(({ line }) => /\bpath:\s*\[\s*"/.test(line) && documentPointer(line)),
  );
  expect(
    exempted.length,
    "nothing writes a document pointer any more — delete documentPointer and read the `path:` spelling straight",
  ).toBeGreaterThan(0);
});

/**
 * Every `<Tag …/>` element's own source text, tag to closing `/>`.
 *
 * BRACE-COUNTED rather than line-matched, because the captions this exists for
 * are multi-line ternaries: a per-line scan reads `sub={` and `.join(` as
 * unrelated lines and reports none of them.
 */
function elements(text: string, tag: string): { at: number; text: string }[] {
  const out: { at: number; text: string }[] = [];
  let i = 0;
  while ((i = text.indexOf(`<${tag}`, i)) >= 0) {
    // `<StatCardish` is not `<StatCard`.
    if (!/[\s/>]/.test(text[i + tag.length + 1] ?? "")) {
      i += 1;
      continue;
    }
    let depth = 0;
    let j = i + tag.length + 1;
    for (; j < text.length; j++) {
      const c = text[j];
      if (c === "{") depth++;
      else if (c === "}") depth--;
      else if (depth === 0 && c === "/" && text[j + 1] === ">") {
        j += 2;
        break;
      } else if (depth === 0 && c === ">") {
        j += 1;
        break;
      }
    }
    out.push({ at: i, text: text.slice(i, j) });
    i = j;
  }
  return out;
}

/**
 * A STAT TILE'S CAPTION QUALIFIES ITS NUMBER; IT NEVER LISTS.
 *
 * `StatCard`'s `sub` is ONE line — the design system draws it `white-space:
 * nowrap` with `text-overflow: ellipsis` — so what goes there has to be short by
 * CONSTRUCTION, not short in today's data. Four tiles joined names a founder
 * writes: the goals screen's at-risk tile, both of the schedules screen's, and
 * Live now's "working now". The goals one shipped reading "Console reads
 * honestly under uncertainty, Console reads hon…" — cut mid-word, naming an
 * outcome that does not exist, beside neighbours reading "not archived" and
 * "across every goal on screen". (That screen left with goals; the gate is
 * what stops the next tile repeating it.)
 *
 * BOUNDING THE COUNT IS THE WRONG FIX and is why this is a gate rather than four
 * corrected lines: a name a founder writes is bounded at 256 characters
 * (`tracker.MaxTitle`), so ONE of them overflows a third-of-a-column tile and
 * "first two, +N more" keeps the same cut. The caption has to be a derived
 * qualifier — a split, a relative time, a pointer at the list that does hold the
 * names — which is what the other fifty-odd tiles in this tree write.
 */
test("no stat tile builds its caption by joining a list", () => {
  const offenders: string[] = [];
  let seen = 0;
  for (const { path, text } of sources()) {
    for (const el of elements(text, "StatCard")) {
      seen++;
      if (!el.text.includes(".join(")) continue;
      const line = text.slice(0, el.at).split("\n").length;
      offenders.push(`${path}:${line} — ${el.text.split("\n")[2]?.trim() ?? ""}`);
    }
  }
  expect(
    offenders,
    "a `sub` is one ellipsized line: say what the number counts, not which rows are in it",
  ).toEqual([]);
  // THE OTHER SIDE. A renamed component or a broken scan makes this rule vacuous
  // while still reporting a pass — the failure `skipgate` exists to stop
  // elsewhere. Fifty-odd tiles today; a runaway parse collapses to one per file
  // and a rename to zero, so thirty separates both from reality.
  expect(
    seen,
    "nothing here reads as a StatCard any more — this rule covers nothing",
  ).toBeGreaterThan(30);
});

/*
 * AND THE SCANNER ACTUALLY READS THE SHAPE THE RULE IS FOR. A gate that only
 * ever saw single-line props would pass a reverted multi-line ternary — which is
 * three of the four captions above.
 */
test("the element scanner reads a multi-line prop", () => {
  const src =
    '<StatCard\n  label="x"\n  sub={\n    a.length ? a.map((x) => x.n).join(", ") : "none"\n  }\n/>\n<StatCard label="y" sub="fixed" />';
  const found = elements(src, "StatCard");
  expect(found.length).toBe(2);
  expect(found[0]?.text.includes(".join(")).toBe(true);
  expect(found[1]?.text.includes(".join(")).toBe(false);
});

/**
 * A COLUMN WHOSE HEAD IS A GLYPH STILL HAS A NAME.
 *
 * `GridColumn.header` is drawn in the head row, so a column twenty pixels wide
 * carries a mark or nothing at all — a work item's type, a row's actions, a
 * pair of state tags. That is right for the wide table and wrong for the card a
 * grid becomes below 860px, where the head is gone and every value draws its
 * own name beside it: a cell with no name draws none, and the card gets a bare
 * mark floating on a line of its own between two labelled ones. Measured on the
 * tracker at 390px, it reads as a rendering fault rather than as a value.
 *
 * So the column says the word separately, in `label`. NOTHING ELSE CATCHES
 * THIS: the field is optional by necessity — a column with a real head must not
 * repeat it — so a new glyph column type-checks, renders, and is wrong only on
 * a phone, which no suite in this tree has a viewport for.
 *
 * THE SIBLINGS ARE FOUND BY INDENTATION, which is a real invariant here rather
 * than a guess: prettier is a gate (`make dashboard-lint`), so a literal's own
 * properties share a column and everything nested under one is indented past
 * it. Brace-counting would be the obvious alternative and is the worse one — a
 * cell is JSX with children, template strings and comments in it, and a counter
 * walking through those answers confidently and wrongly. `typescript` is not
 * the alternative either: at 7.x the package exports its syntax tree only under
 * `unstable/`, and a permanent gate does not rest on that.
 */
test("a column with no word in its head declares one", () => {
  const offenders: string[] = [];
  let seen = 0;
  for (const { path, text } of sources([".tsx"])) {
    const lines = text.split("\n");
    for (let at = 0; at < lines.length; at++) {
      // A PROPERTY, NOT A TYPE MEMBER. `GridColumn`'s own `header: ReactNode;`
      // declares the contract rather than filling it in, and prettier ends one
      // in a semicolon and the other in a comma.
      const head = /^(\s*)header:\s*(.*[^;])$/.exec(lines[at]!);
      if (!head) continue;
      seen++;
      // A WORD IS A NON-EMPTY STRING LITERAL. `""`, a glyph and any expression
      // are all the same thing to `attr()`: nothing to read.
      if (/^(["'])(?!\1)\S/.test(head[2]!)) continue;
      const indent = head[1]!;
      const siblings: string[] = [];
      for (const step of [-1, 1]) {
        for (let n = at + step; n >= 0 && n < lines.length; n += step) {
          const line = lines[n]!;
          if (!line.trim()) continue;
          if (!line.startsWith(indent)) break;
          if (line[indent.length] === " ") continue;
          siblings.push(line);
        }
      }
      if (siblings.some((line) => /^\s*label:\s*["']\S/.test(line))) continue;
      offenders.push(`${path}:${at + 1}`);
    }
  }
  expect(
    offenders,
    "a glyph or empty head leaves the phone's card layout an unnamed line: add `label`",
  ).toEqual([]);
  // THE OTHER SIDE, as above: a renamed field or a broken scan makes the rule
  // vacuous and still green. Well over a hundred columns today.
  expect(seen, "nothing here declares a column head any more").toBeGreaterThan(80);
});

/**
 * EVERY READ NAMES ITS QUESTION.
 *
 * The first argument of a call that asks the engine something — `useQuery(`
 * and `query(`, bare or as a method — is a string literal: never a variable,
 * an expression, or a template with something substituted into it.
 *
 * THE ENGINE'S GATES READ THESE CALLS BY NAME, and a call they cannot read is
 * one they do not see. `internal/api/queries` holds this tree against the
 * registry in both directions — `TestEveryQueryARoomMakesIsAnswered` and
 * `TestEveryQueryThisServerAnswersHasAReader` — and takes the kinds it asks
 * from `clientsource.Calls`, which reads a first argument only when it is a
 * constant string. So a kind passed through a variable is a question neither
 * gate knows about: the engine could stop answering it and the build would
 * stay green, and a kind whose last literal reader became a variable would be
 * reported unread and deleted from under the screen that still asks it. Held
 * here, that blindness costs nothing, because there is nothing for it to miss.
 *
 * `useQuery` AND `query` ARE READ BY SPELLING, because that is how
 * `clientsource.Calls` reads them: any call to either name, bare or as a
 * method, whatever it is bound to. Reading less than that here would leave a
 * call both sides skip — `const { query } = socket; query(kind)` is a bare
 * call to the socket's method, and a rule that knew only `.query(` passed it
 * while the Go reader, finding no literal, dropped it. Reading the same set is
 * what makes "the Go gates can read every kind" a fact rather than a hope.
 * And a method named by a computed key (`socket["query"](…)`) is refused even
 * with a literal kind, because `Calls` reads a NAME, and a string in brackets
 * is not one.
 *
 * `act` IS READ BY BINDING: the function imported from a module of this tree.
 * React's own `act`, which the test kits call with a callback, is a different
 * function that shares the spelling. `act` is the write surface's entry
 * (`protocol/act.ts`), which names a tool the way a query names a kind; it has
 * no caller yet, and is held here from its first. An ALIAS of any of the three
 * is refused (`import { useQuery as ask }`), because every call behind it is
 * invisible to a reader that looks for the name.
 *
 * TWO CALLS FORWARD A KIND, and each is named in `FORWARDS` with its reason
 * rather than exempted by its shape. An entry that stops matching is stale and
 * fails, because an exemption nobody checks is how a gate quietly stops
 * covering what it was written for.
 */

/** Names the engine's gates read by spelling: `clientsource.Calls` is asked for exactly these. */
const SPELLED = new Set(["useQuery", "query"]);
/** Names held by binding, imported from this tree, because a package exports one of the same spelling. */
const BOUND = new Set(["act"]);

/** The sites that hand on a kind somebody else named, and why. */
const FORWARDS: readonly { path: string; callee: string; argument: string; why: string }[] = [
  {
    path: "lib/useQuery.ts",
    callee: "query",
    argument: "what",
    why: "useQuery's own body sends the socket the kind its caller named, and every caller is held to a literal here",
  },
  {
    path: "routes/org/builder/testkit.tsx",
    callee: "query",
    argument: "what",
    why: "the builder test kit's stub socket answers whatever kind a screen asks it, through the fixture the suite supplies; every screen asking is held to a literal here",
  },
];

interface NamedCall {
  line: number;
  callee: string;
  /** The kind the call names, or null when the engine's gates cannot read one. */
  name: string | null;
  /** The call as written, up to its first argument, for the report. */
  written: string;
  /** The first argument as written. */
  argument: string;
}

interface Alias {
  line: number;
  imported: string;
  local: string;
}

/**
 * Every call in one module that names a question, and every import that
 * renames one.
 */
function namedCalls(source: string, lang: Lang): { calls: NamedCall[]; aliases: Alias[] } {
  const line = lineOf(source);
  const program = parse(source, lang);
  const bound = new Map<string, string>();
  const namespaces = new Set<string>();
  const aliases: Alias[] = [];
  for (const statement of program.body as Node[]) {
    if (statement.type !== "ImportDeclaration" || statement.importKind === "type") continue;
    if (!isLocalSource(String((statement.source as Node).value))) continue;
    for (const spec of statement.specifiers as Node[]) {
      const local = String((spec.local as Node).name);
      if (spec.type === "ImportNamespaceSpecifier") namespaces.add(local);
      if (spec.type !== "ImportSpecifier" || spec.importKind === "type") continue;
      const imported = spec.imported as Node;
      const name = String(imported.type === "Identifier" ? imported.name : imported.value);
      if (!SPELLED.has(name) && !BOUND.has(name)) continue;
      bound.set(local, name);
      if (local !== name) aliases.push({ line: line(spec.start), imported: name, local });
    }
  }
  const calls: NamedCall[] = [];
  walk(program, (node) => {
    if (node.type !== "CallExpression") return;
    const callee = node.callee as Node;
    // What the call is to, and whether the engine's gates can read that NAME
    // at all: a computed key names the method in a string they do not read.
    let named: string | null = null;
    let readable = true;
    if (callee.type === "Identifier") {
      const name = String(callee.name);
      named = SPELLED.has(name) ? name : (bound.get(name) ?? null);
    } else {
      const member = memberName(callee);
      const object = callee.object as Node | undefined;
      if (member !== null && SPELLED.has(member)) {
        named = member;
        readable = !callee.computed;
      } else if (
        member !== null &&
        BOUND.has(member) &&
        object?.type === "Identifier" &&
        namespaces.has(String(object.name))
      ) {
        named = member;
      }
    }
    if (named === null) return;
    const first = (node.arguments as Node[])[0];
    calls.push({
      line: line(node.start),
      callee: named,
      name: readable ? stringValue(first) : null,
      written: source.slice(callee.start, first ? first.start : node.end).replace(/\s+/g, " "),
      argument: first ? source.slice(first.start, first.end) : "",
    });
  });
  return { calls, aliases };
}

/** Whether a call is the named forward in `FORWARDS`. */
const forwarded = (path: string, call: NamedCall) =>
  FORWARDS.some((f) => f.path === path && f.callee === call.callee && f.argument === call.argument);

describe("every read names its question", () => {
  const tree = modules().map((m) => ({ path: m.path, ...namedCalls(m.text, m.lang) }));

  test("with a string literal, which is what the engine's gates can read", () => {
    const offenders = tree.flatMap(({ path, calls }) =>
      calls
        .filter((call) => call.name === null && !forwarded(path, call))
        .map((call) => `${path}:${call.line} — ${call.written}${call.argument}`),
    );
    expect(
      offenders,
      "write the kind as a string literal, to a call by its own name: internal/api/queries " +
        "reads it there, and a kind it cannot read is one it cannot hold against the registry",
    ).toEqual([]);
  });

  test("under its own name, never an alias", () => {
    const renamed = tree.flatMap(({ path, aliases }) =>
      aliases.map((a) => `${path}:${a.line} — ${a.imported} as ${a.local}`),
    );
    expect(renamed, "import it as itself: the engine's gates find these calls by name").toEqual([]);
  });

  test("and the rule reads the tree, so it cannot pass by reading nothing", () => {
    // Over a hundred `useQuery` calls today and a pager's `socket.query` in three
    // screens. A parser that stopped seeing calls, or a hook that was renamed,
    // collapses both counts, and the rule above would pass over nothing.
    const literal = (callee: string) =>
      tree.flatMap(({ calls }) => calls).filter((c) => c.callee === callee && c.name !== null)
        .length;
    expect(literal("useQuery")).toBeGreaterThan(80);
    expect(literal("query")).toBeGreaterThanOrEqual(2);
  });

  test("and every forward it names still forwards", () => {
    const stale = FORWARDS.filter(
      (f) =>
        !tree.some(
          ({ path, calls }) =>
            path === f.path && calls.some((call) => call.name === null && forwarded(path, call)),
        ),
    ).map((f) => `${f.path} — ${f.callee}(${f.argument}`);
    expect(stale, "this forward is gone: delete its FORWARDS entry").toEqual([]);
  });

  // THE RULE'S RED HALF: every way a kind stops being readable, each of which
  // must come back as a call with no name.
  const USE_QUERY = 'import { useQuery } from "~/lib/useQuery.ts";\n';
  test.each([
    ["a variable", `${USE_QUERY}const kind = "work_item";\nuseQuery(kind, { id });`],
    ["a template", `${USE_QUERY}useQuery(\`work_\${shape}\`, {});`],
    ["a concatenation", `${USE_QUERY}useQuery("work_" + shape, {});`],
    ["a conditional", `${USE_QUERY}useQuery(mine ? "work_my_work" : "work_items");`],
    ["no argument at all", `${USE_QUERY}useQuery();`],
    ["a relative import", 'import { useQuery } from "./useQuery.ts";\nuseQuery(kind);'],
    ["a namespace import", 'import * as q from "~/lib/useQuery.ts";\nq.useQuery(kind);'],
    ["a socket's method", "socket.query(kind, params);"],
    ["an optional call", "client?.socket.query(kind);"],
    // The shape a rule that knew only `.query(` passed and the Go reader
    // dropped: the socket's method, taken off it and called bare.
    ["a method taken off the socket", "const { query } = socket;\nvoid query(kind);"],
    ["a bare call of any binding", "function query(what: string) {}\nquery(what);"],
    [
      "a hook of the same name from a package",
      'import { useQuery } from "@tanstack/react-query";\nuseQuery(key);',
    ],
    // A literal kind the engine still cannot read, because the method is named
    // in a string rather than by a name.
    ["a computed key", 'socket["query"]("events", params);'],
    ["the write surface", 'import { act } from "~/protocol/act.ts";\nact(tool, { args });'],
  ])("the rule catches %s", (_name, source) => {
    const { calls } = namedCalls(source, "tsx");
    expect(calls.length, `nothing read in ${source}`).toBe(1);
    expect(calls[0]?.name).toBeNull();
  });

  test.each([
    ["useQuery", 'import { useQuery as ask } from "~/lib/useQuery.ts";\nask("work_item");'],
    ["act", 'import { act as write } from "~/protocol/act.ts";\nwrite("set_pins", {});'],
  ])("the rule catches an alias of %s", (name, source) => {
    const { aliases, calls } = namedCalls(source, "tsx");
    expect(aliases.map((a) => a.imported)).toEqual([name]);
    // And the call behind it is still held to a literal, under the name it renames.
    expect(calls.map((c) => c.callee)).toEqual([name]);
  });

  // AND ITS GREEN HALF: the shapes the tree writes, which must read as named,
  // and the calls that share a name without being one of these.
  test.each([
    ["a literal", `${USE_QUERY}useQuery("work_item", { id });`, "work_item"],
    [
      "a literal prettier broke over lines",
      `${USE_QUERY}const a = useQuery(\n  "work_project",\n  key ? { key } : undefined,\n  { enabled: key !== "" },\n);`,
      "work_project",
    ],
    ["a template with nothing in it", `${USE_QUERY}useQuery(\`fleet\`);`, "fleet"],
    ["type arguments", `${USE_QUERY}useQuery<"fleet">("fleet");`, "fleet"],
    ["a pager's own call", 'const page = await socket.query("events", params);', "events"],
    ["a bare call", 'const { query } = socket;\nvoid query("events");', "events"],
    ["an optional call", 'client?.socket.query?.("events");', "events"],
    [
      "the write surface",
      'import { act } from "~/protocol/act.ts";\nact("set_pins", {});',
      "set_pins",
    ],
  ])("the rule reads %s", (_name, source, name) => {
    expect(namedCalls(source, "tsx").calls.map((c) => c.name)).toEqual([name]);
  });

  test.each([
    ["React's own act", 'import { act } from "@testing-library/react";\nact(() => {});'],
    ["a local function", "function ask(what: string) {}\nask(kind);"],
    ["a type-only import", 'import type { act } from "~/protocol/act.ts";\nact(tool);'],
    ["a namespace's own act", 'import * as rtl from "@testing-library/react";\nrtl.act(() => {});'],
    ["a method of another name", "const hits = el.querySelector(selector);"],
  ])("the rule leaves %s alone", (_name, source) => {
    expect(namedCalls(source, "tsx").calls).toEqual([]);
  });
});
