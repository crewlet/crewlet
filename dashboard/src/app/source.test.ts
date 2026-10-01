// @vitest-environment node

/**
 * Rules about the source that no type and no runtime check catches.
 *
 * There is no ESLint in this tree — the gates are prettier, `tsc` and this
 * suite — so a rule that would be a lint elsewhere is a test here, written the
 * way `styles/classes.test.ts` is: read the source, apply the rule, name the
 * file and line.
 */

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

import { FRAMELESS, RAIL } from "./nav.ts";

const SRC = fileURLToPath(new URL("..", import.meta.url));

/** Every source file of the given extensions, excluding the suites. */
function sources(exts: string[] = [".tsx"]): { path: string; text: string }[] {
  const out: { path: string; text: string }[] = [];
  (function walk(dir: string): void {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!exts.some((ext) => full.endsWith(ext))) continue;
      if (full.includes(".test.")) continue;
      out.push({ path: relative(SRC, full), text: readFileSync(full, "utf8") });
    }
  })(SRC);
  return out;
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
  // OR A SCREEN OUTSIDE THE FRAME, which no workspace owns and the router
  // draws all the same — the sign-in screens a refusal sends a reader to.
  const owned = new Set<string>([...RAIL.flatMap((r) => r.owns), ...FRAMELESS]);
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

/**
 * A QUERYSTATE HANDED A FAILURE IS HANDED WHY.
 *
 * `QueryState` turns an `unauthorized` into the grant that would admit the
 * reader, and an `unavailable` the state log will not lift into that fact —
 * but only from the `refusal` beside the code, which `useQuery` answers and a
 * screen has to pass on. Five did not (a container's pages, a tool's holders,
 * a schedule's runs twice, the fleet's table), so each drew the generic banner
 * where the engine had said exactly what would change the answer. Nothing else
 * catches it: `refusal` is optional because a surface with no query behind it
 * has none, so an omission type-checks and renders.
 *
 * `error={null}` is exempt, being no failure at all.
 */
test("every QueryState handed an error is handed its refusal", () => {
  const offenders: string[] = [];
  let seen = 0;
  for (const { path, text } of sources()) {
    for (const el of elements(text, "QueryState")) {
      const error = /\berror=\{([^}]*)\}/.exec(el.text);
      if (!error || error[1]!.trim() === "null") continue;
      seen++;
      if (/\brefusal=\{/.test(el.text)) continue;
      const line = text.slice(0, el.at).split("\n").length;
      offenders.push(`${path}:${line} — error={${error[1]!.trim()}}`);
    }
  }
  expect(offenders, "pass the read's `refusal` beside its `error`").toEqual([]);
  // THE OTHER SIDE: a renamed component or a broken scan makes the rule
  // vacuous and still green. Seventy-odd of them today.
  expect(seen, "nothing here reads as a QueryState any more").toBeGreaterThan(40);
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
 * EVERY `Intl` FORMATTER IS BUILT IN `lib/format.ts`, AND KEPT.
 *
 * `d.toLocaleString(locale, options)`, `toLocaleDateString`,
 * `toLocaleTimeString`, `n.toLocaleString()` and `a.localeCompare(b, locale,
 * options)` each build an `Intl` object PER CALL — ECMA-402 defines them as
 * that construction followed by one format or compare — and building one is
 * the expensive half. Spelled inline they were most of what formatting cost
 * on a busy screen: a hundred-row audit built three hundred date formatters a
 * render, and a grid sorted by a text column built a collator per comparison.
 * `lib/format.ts` keeps one per locale and options (`dateFormatter`), one
 * number formatter and one collator (`naturalCompare`); `lib/prefs.ts` is the
 * other file allowed one, to ask `Intl` which zone the browser is in and
 * whether a zone exists.
 *
 * A bare `a.localeCompare(b)` is allowed: with no locale and no options the
 * engine compares through its own default collator rather than building one.
 */
const INTL_BUILT =
  /\.toLocale(Date|Time)?String\(|\bnew Intl\.|\bIntl\.[A-Z]\w*\(|\.localeCompare\([^\n]*?,\s*(undefined|["'[{])/;

test("every Intl formatter is built in lib/format.ts, and kept", () => {
  const allowed = new Set(["lib/format.ts", "lib/prefs.ts"]);
  const offenders: string[] = [];
  let read = 0;
  for (const { path, text } of sources([".ts", ".tsx"])) {
    read += 1;
    if (allowed.has(path)) continue;
    const lines = text
      .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
      .replace(/(^|[^:])\/\/[^\n]*/g, (m, lead) => lead + " ".repeat(m.length - lead.length))
      .split("\n");
    lines.forEach((line, i) => {
      if (INTL_BUILT.test(line)) offenders.push(`${path}:${i + 1} — ${line.trim().slice(0, 90)}`);
    });
  }
  // A WALK THAT READ NOTHING passes with no offence found.
  expect(read).toBeGreaterThan(100);
  expect(
    offenders,
    "format through lib/format.ts (dateFormatter, fmtExact, plural, naturalCompare), which keeps the formatter",
  ).toEqual([]);
});

test("the Intl gate fires on each spelling it is for", () => {
  for (const line of [
    "d.toLocaleString(undefined, { hour: '2-digit' })",
    "d.toLocaleDateString(undefined, { day: 'numeric' })",
    "d.toLocaleTimeString()",
    "n.toLocaleString()",
    "new Intl.NumberFormat()",
    "Intl.DateTimeFormat().resolvedOptions()",
    "String(a).localeCompare(String(b), undefined, { numeric: true })",
  ]) {
    expect(INTL_BUILT.test(line), line).toBe(true);
  }
  expect(INTL_BUILT.test("a.name.localeCompare(b.name)")).toBe(false);
  expect(INTL_BUILT.test("dateFormatter(undefined, { day: 'numeric' }).format(d)")).toBe(false);
});

/**
 * AN ITEM IS OPENED BY ITS ADDRESS, NEVER BY ITS KEY.
 *
 * A key two tasks hold opens the one that claimed it first — the engine
 * resolves a key through its directory before it reads a row — and the other
 * is flagged `key_collision` and reached only by its id. Every screen that
 * opened an item built the link, the peek, the stepper's list, the "is this
 * the open one" check and the copied call out of `row.key`: the board, the
 * list, the table, the timeline, the calendar, the search, the palette, the
 * feeds, the inbox and My work all drew two `ENG-7`s that opened one task,
 * with no error anywhere, because a key is a perfectly good address for every
 * row but the flagged one.
 *
 * `itemAddress` (in `lib/work.ts`) is the one place the rule is written, and
 * this is what keeps a NEW screen from writing it again by hand. Four shapes
 * are refused anywhere in the source, each a way the key leaks into an
 * address:
 *
 * - a `["work", …]` route whose segment reads a key field — `row.key`,
 *   `record.subject_key`, `item.task_key`, `link.key || link.other`. An item's
 *   route is `itemPath(row)`, and a project's is `projectPath(key)` precisely
 *   so that no route into the tracker is spelled with a key at the call site;
 * - an `{ kind: "item", id: … }` reference — a peek, a stepper entry, a copied
 *   call's subject — whose id is anything but `itemAddress(…)`, in either
 *   order of its two properties;
 * - a comparison of a key field against `selected` or `peek.id`, which is how
 *   a list draws the open row — the peek holds an ADDRESS, so a row matched
 *   on its key lit up both holders of a shared one;
 * - a tool call naming its `item:` by a key field.
 *
 * Comments are blanked first, so a comment quoting the wrong shape to explain
 * the right one is not an offence.
 */
const KEY_READ = String.raw`[\w$\])?]\.(?:key|subject_key|task_key)\b`;
const ADDRESS_RULES: { rule: RegExp; says: string }[] = [
  {
    rule: new RegExp(String.raw`\[\s*"work"\s*,[^\]]*?` + KEY_READ),
    says: "routes to an item by its key — use itemPath(row)",
  },
  {
    // THE LOOKAHEAD SITS ON THE COLON, not after the spaces: placed after
    // them, the engine backtracks the spaces away and the lookahead then sees
    // " itemAddress(", which is not `itemAddress(`, so every right answer was
    // refused as well.
    rule: /\bkind:\s*"item"(?:\s+as\s+const)?\s*,\s*id:(?!\s*itemAddress\()/,
    says: "names an item by something other than itemAddress(row)",
  },
  {
    rule: /\bid:(?!\s*itemAddress\()[^,{}]*,\s*kind:\s*"item"(?!\s*\|)/,
    says: "names an item by something other than itemAddress(row)",
  },
  {
    rule: new RegExp(
      String.raw`(?:\bselected|\bpeek\??\.id)\s*[!=]==\s*[\w$?.\[\]]*` +
        KEY_READ +
        String.raw`|[\w$?.\[\]]*` +
        KEY_READ +
        String.raw`\s*[!=]==\s*(?:\bselected\b|\bpeek\??\.id\b)`,
    ),
    says: "matches the open item on its key — compare itemAddress(row)",
  },
  {
    rule: new RegExp(String.raw`\bitem:\s*[\w$?.\[\]]*` + KEY_READ),
    says: "names a tool call's item by its key — use itemAddress(row)",
  },
];

/** A file's code with its comments blanked and every offset kept. */
function codeOf(text: string): string {
  return text
    .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
    .replace(/(^|[^:])\/\/[^\n]*/g, (m, lead) => lead + " ".repeat(m.length - lead.length));
}

/** Every place one file's code addresses an item by hand. */
function addressOffences(code: string): { line: number; says: string; text: string }[] {
  const out: { line: number; says: string; text: string }[] = [];
  const lines = code.split("\n");
  for (const { rule, says } of ADDRESS_RULES) {
    // ACROSS LINES, because the shapes are written across lines: a
    // multi-line `{ kind: "item", id: … }` is how a copied call's subject is
    // spelled, and a per-line scan reads its `id:` as an unrelated line.
    const global = new RegExp(rule.source, "g");
    let m: RegExpExecArray | null;
    while ((m = global.exec(code))) {
      const line = code.slice(0, m.index).split("\n").length;
      out.push({ line, says, text: (lines[line - 1] ?? "").trim().slice(0, 90) });
      global.lastIndex = m.index + Math.max(1, m[0].length);
    }
  }
  return out;
}

test("no screen opens an item by its key", () => {
  const offenders: string[] = [];
  let read = 0;
  let addressed = 0;
  for (const { path, text } of sources([".ts", ".tsx"])) {
    read += 1;
    const code = codeOf(text);
    addressed += (code.match(/\bitem(?:Address|Path)\(/g) ?? []).length;
    for (const o of addressOffences(code)) {
      offenders.push(`${path}:${o.line} ${o.says} — ${o.text}`);
    }
  }
  // A WALK THAT READ NOTHING passes with no offence found.
  expect(read).toBeGreaterThan(100);
  expect(
    offenders,
    "a key another task claimed first opens that task: address an item with itemAddress / itemPath from lib/work.ts",
  ).toEqual([]);
  // AND SO DOES A TREE IN WHICH NOTHING ADDRESSES AN ITEM AT ALL — which is
  // what a rename of the function would leave behind, every rule above then
  // refusing the new spelling's absence rather than anything a screen does.
  expect(addressed, "nothing in the tree calls itemAddress or itemPath").toBeGreaterThan(20);
});

test("the address gate fires on each way a key leaks into an address, and on nothing else", () => {
  for (const bad of [
    'href(["work", row.key])',
    'nav.to(["work", item.key])',
    'path: ["work", record.subject_key],',
    'href(["work", item.task_key])',
    ': ["work", link.key || link.other],',
    'openPeek({ kind: "item", id: row.key })',
    'rows.map((r) => ({ kind: "item" as const, id: r.key }))',
    'peekHref({ kind: "item", id: hit.key || hit.id })',
    '{\n  kind: "item",\n  id: detail.task.key,\n}',
    '({ id: r.key, kind: "item" })',
    "selected={selected === row.key}",
    "isSelected={(row) => row.key === selected}",
    "isSelected={(r) => peek?.id === r.key}",
    'cx("tl-bar", selected === bar.row.key && "selected")',
    "args: { item: row.key },",
  ]) {
    expect(addressOffences(bad).length, bad).toBeGreaterThan(0);
  }
  for (const good of [
    "href(itemPath(row))",
    'href(["work", itemAddress(row)])',
    "href(projectPath(p.key))",
    'path: ["work", projectKey]',
    'openPeek({ kind: "item", id: itemAddress(row) })',
    '({ kind: "item" as const, id: itemAddress(r) })',
    '{\n  kind: "item",\n  id: itemAddress(detailItem(detail)),\n}',
    'kind: "item" | "page" | "seat";',
    "isSelected={(row) => itemAddress(row) === selected}",
    'selected === itemAddress(bar.row) && "selected"',
    "group.key === key",
    'openPeek({ kind: "project", id: row.key })',
    "args: { item: itemAddress(row) },",
  ]) {
    expect(addressOffences(good), good).toEqual([]);
  }
});
