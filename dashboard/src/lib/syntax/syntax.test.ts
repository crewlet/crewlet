// @vitest-environment node
/**
 * The contract every grammar keeps on any line, and the gates on the modules
 * that hold them: safe sticky patterns only, and nothing drawn from here.
 */
import { describe, expect, test } from "vitest";
import { LINEAR_BOUND, growth } from "~/test/growth.ts";
import { MAX_STATE, SAMPLES, contractBreaks } from "~/test/syntax.ts";
import {
  lineOf,
  modules,
  parse,
  resolveSource,
  walk,
  type Lang as Source,
  type Node,
} from "~/test/source.ts";
import { load, statesOf, type Lang } from "./index.ts";
import type { Grammar, State } from "./types.ts";

const LANGS = Object.keys(SAMPLES) as Lang[];
/** Openers a line repeats and never closes. */
const OPENERS = ["/*", '"', '\\"', "${", "`", "<<", "r#", "<!--", "'''", "[["];

/** The openers each grammar reads as a construct, whose scan the clock holds to linear. */
const TIMED: Readonly<Record<Lang, string[]>> = {
  go: ["/*", '\\"', "`"],
  gomod: ['\\"', "`"],
  ts: ["/*", '\\"', "${", "`", "<<"],
  python: ['\\"', "'''"],
  rust: ["/*", '\\"', "r#"],
  shell: ['\\"', "${", "<<", "`"],
  makefile: ['\\"', "${", "[["],
  dockerfile: ['\\"', "${", "<<"],
  yaml: ['\\"', "'''", "[["],
  json: ["/*", '\\"', "[["],
  toml: ['\\"', "'''", "[["],
  css: ["/*", '\\"', "[["],
  markup: ["<!--", '\\"', "<<"],
  sql: ["/*", '\\"', "'''"],
  proto: ["/*", '\\"'],
  hcl: ["/*", '\\"', "${", "<<"],
  markdown: ["`", "<!--", "[["],
};

/** `unit` repeated to `n` code units. */
const repeat = (unit: string, n: number) => unit.repeat(Math.ceil(n / unit.length)).slice(0, n);

/** A seeded xorshift, so every run reads the same lines. */
function random(seed: number): () => number {
  let x = seed | 0 || 1;
  return () => {
    x ^= x << 13;
    x ^= x >>> 17;
    x ^= x << 5;
    return (x >>> 0) / 4294967296;
  };
}

/** The openers and closers every grammar reads, and code units no text should break. */
const COMMON = [
  ...`"'\`/*#<>\${}[]\\-:()=;.,!@&|~?%+`,
  "//",
  "/*",
  "*/",
  "--",
  "${",
  "$$",
  "$(",
  "<<",
  "<<-",
  "<<EOF",
  "<<-EOF",
  "EOF",
  '"""',
  "'''",
  "```",
  "````",
  "~~~",
  "<!--",
  "-->",
  "<![CDATA[",
  "]]>",
  'r#"',
  '"#',
  "[[",
  "]]",
  " ",
  "  ",
  "\t",
  "x",
  "name",
  "0x1F",
  "1.5e3",
  "\uD800",
  "\uDC00",
  "\u0000",
  "\r",
  "\u{1F600}",
  "é",
];

/** Each grammar's own words and forms, weighted in beside `COMMON`. */
const OWN: Readonly<Record<Lang, string[]>> = {
  go: ["func", "type", "struct", "`", "'\"'", "nil", "3i"],
  gomod: ["module", "require", "replace", "=>", "v1.2.3", "// indirect"],
  ts: ["function", "class", "`${", "}`", "/re/g", "<Foo", "</Foo>", "/>", "={"],
  python: ["def", "class", "f'", 'r"""', "@dec", "None"],
  rust: ["fn", "'a", 'r##"', '"##', "println!", "/* /*", "*/ */"],
  shell: ["if", "fi", "<<'EOF'", "\tEOF", "$HOME", "${x}", "f()", "case", "esac"],
  makefile: ["\t", "define", "endef", "$(VAR)", "target:", "?=", ":=", "\\"],
  dockerfile: ["FROM", "RUN", "RUN <<EOF", "AS", "\\", "# syntax=x"],
  yaml: ["key:", "- ", "|", ">-", "&a", "*a", "!t", "---", "true"],
  json: ['"k":', "true", "null", "-1.5"],
  toml: ["[table]", "[[array]]", "key =", "1979-05-27T07:32:00Z", "true"],
  css: ["@media", "!important", "#fff", ".a", "url(", "color:", "1.5rem"],
  markup: ["<a", "</a>", 'href="', "&amp;", "<script>", "</script>", "<style>", "<!DOCTYPE"],
  sql: ["SELECT", "select", "''", "$body$", "BIGINT", "count("],
  proto: ["message", "rpc", "returns", "repeated", "string"],
  hcl: ["resource", "<<EOT", "<<-EOT", "EOT", '"${', "=", "for"],
  markdown: ["# ", "- ", "> ", "[a](", ")", "1. ", "|"],
};

/** States no grammar writes, and the shapes of ones they do, held wrong. */
const JUNK: State[] = [
  "\u0001",
  "zzz",
  "h",
  "h-",
  "h_",
  "Q9",
  "Q",
  "*".repeat(9),
  "b",
  "b-1",
  "f`2",
  "f`100",
  "R33",
  "r",
  "rh-",
  "`$`$`$",
  "<",
  "o",
  "d",
  "t'",
  "\uD800",
  "x".repeat(200),
];

/** A line of up to 24 pieces from `pieces`, by `rand`. */
function line(rand: () => number, pieces: readonly string[]): string {
  let out = "";
  const n = Math.floor(rand() * 25);
  for (let k = 0; k < n; k++) out += pieces[Math.floor(rand() * pieces.length)];
  return out;
}

const grammars = new Map<Lang, Grammar>();
const grammar = async (lang: Lang) => {
  let g = grammars.get(lang);
  if (!g) grammars.set(lang, (g = await load(lang)));
  return g;
};

describe.each(LANGS)("%s keeps the contract", (lang) => {
  // Mutation: drop a token's last code unit, stop merging runs of one kind, or split a pair fails it.
  test("on every line it is given, from its own states and from any", async () => {
    const g = await grammar(lang);
    const rand = random(0x5eed + lang.length);
    const pieces = [...COMMON, ...OWN[lang], ...OWN[lang], ...OWN[lang]];
    const seen = new Set<State>([""]);
    const problems: string[] = [];
    const check = (text: string, state: State) => {
      const answer = g.line(text, state);
      for (const p of contractBreaks(text, answer)) {
        problems.push(`${JSON.stringify(text)} from ${JSON.stringify(state)}: ${p}`);
      }
      if (JSON.stringify(g.line(text, state)) !== JSON.stringify(answer)) {
        problems.push(`${JSON.stringify(text)} answers twice differently`);
      }
      return answer.state;
    };
    let state: State = "";
    const lines: string[] = [];
    for (let k = 0; k < 2000; k++) {
      const text = line(rand, pieces);
      lines.push(text);
      state = check(text, state);
      seen.add(state);
    }
    const states = [...seen, ...JUNK];
    for (const text of lines) check(text, states[Math.floor(rand() * states.length)]!);
    // A state no grammar writes is read as a file's start.
    for (const text of lines.slice(0, 200)) {
      expect(g.line(text, "\u0001junk"), text).toEqual(g.line(text, ""));
    }
    expect(problems.slice(0, 10)).toEqual([]);
  });

  test("an unclosed construct keeps the contract on a long line and over a file", async () => {
    const g = await grammar(lang);
    for (const opener of OPENERS) {
      expect(contractBreaks(repeat(opener, 65536), g.line(repeat(opener, 65536), ""))).toEqual([]);
      const file = Array.from({ length: 256 }, () => repeat(opener, 1023));
      const states = statesOf(g, file, "");
      expect(states.length).toBe(file.length + 1);
      expect(states.every((s) => s.length <= MAX_STATE)).toBe(true);
    }
  });

  // Each construct repeated and never closed, timed at 1 Ki and 32 Ki code units by `growth`.
  // Words and spaces are the control, which must be seen to grow.
  // Mutation: make an unclosed quote advance one unit and rescan from there fails it.
  test.each(["name ", ...TIMED[lang]])("a line of %s runs in linear time", async (opener) => {
    const g = await grammar(lang);
    const ratio = growth((n) => {
      const text = repeat(opener, n);
      return () => g.line(text, "");
    }, 1024);
    if (opener === "name ") expect(ratio, "the control measured nothing").toBeGreaterThan(4);
    expect(ratio).toBeLessThan(LINEAR_BOUND);
  });

  // Mutation: a grammar that drops a code unit of a real file's line fails it.
  test("a file of real shape is covered line by line", async () => {
    const g = await grammar(lang);
    let state: State = "";
    for (const text of SAMPLES[lang].split("\n")) {
      const answer = g.line(text, state);
      expect(contractBreaks(text, answer), text).toEqual([]);
      state = answer.state;
    }
  });
});

/** What makes a pattern unsafe: no sticky flag, a nested quantifier, a backreference or a lookbehind. */
export function patternFaults(pattern: string, flags: string): string[] {
  const out: string[] = [];
  if (!flags.includes("y")) out.push("is not sticky");
  // Whether each open group holds a quantifier.
  const groups: boolean[] = [];
  let inClass = false;
  const quantified = (c: string | undefined) => c === "*" || c === "+" || c === "?" || c === "{";
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i];
    if (c === "\\") {
      const next = pattern[i + 1] ?? "";
      if (!inClass && (/[1-9]/.test(next) || next === "k")) out.push("holds a backreference");
      i++;
    } else if (inClass) {
      if (c === "]") inClass = false;
    } else if (c === "[") inClass = true;
    else if (c === "(") {
      groups.push(false);
      if (pattern.startsWith("(?<=", i) || pattern.startsWith("(?<!", i)) {
        out.push("holds a lookbehind");
      }
      if (pattern[i + 1] === "?") i += pattern[i + 2] === "<" ? 3 : 2;
    } else if (c === ")") {
      const held = groups.pop() ?? false;
      const q = pattern[i + 1];
      if (held && (q === "*" || q === "+" || q === "{")) out.push("nests a quantifier");
      if ((held || quantified(q)) && groups.length > 0) groups[groups.length - 1] = true;
    } else if (quantified(c) && groups.length > 0) groups[groups.length - 1] = true;
  }
  return out;
}

/** Every unsafe pattern and every RegExp built at run time in a module, as `path:line …`. */
export function unsafePatterns(path: string, text: string, lang: Source): string[] {
  const line = lineOf(text);
  const out: string[] = [];
  walk(parse(text, lang), (node: Node) => {
    const regex = node.regex as { pattern: string; flags: string } | undefined;
    if (node.type === "Literal" && regex) {
      for (const fault of patternFaults(regex.pattern, regex.flags)) {
        out.push(`${path}:${line(node.start)} /${regex.pattern}/${regex.flags} ${fault}`);
      }
    }
    const callee = node.callee as Node | undefined;
    if (
      (node.type === "NewExpression" || node.type === "CallExpression") &&
      callee?.type === "Identifier" &&
      callee.name === "RegExp"
    ) {
      out.push(`${path}:${line(node.start)} builds a RegExp`);
    }
  });
  return out;
}

const inSyntax = (path: string) => path.startsWith("lib/syntax/");

/** Every import, re-export or `import()` a `lib/syntax` module makes of anything outside it. */
export function foreignImports(path: string, text: string, lang: Source): string[] {
  const line = lineOf(text);
  const out: string[] = [];
  walk(parse(text, lang), (node: Node) => {
    const source = (node.source as Node | null | undefined)?.value;
    if (typeof source !== "string") return;
    if (!inSyntax(resolveSource(path, source) ?? "")) {
      out.push(`${path}:${line(node.start)} imports ${source}`);
    }
  });
  return out;
}

const syntaxModules = () => modules().filter((m) => inSyntax(m.path));

describe("the modules", () => {
  test("every pattern in lib/syntax is a safe sticky literal", () => {
    expect(syntaxModules().length).toBeGreaterThanOrEqual(18);
    expect(syntaxModules().flatMap((m) => unsafePatterns(m.path, m.text, m.lang))).toEqual([]);
  });

  test("lib/syntax draws nothing: it reads only its own modules", () => {
    expect(syntaxModules().flatMap((m) => foreignImports(m.path, m.text, m.lang))).toEqual([]);
  });
});

describe("it can tell", () => {
  test("an unsafe pattern, and a RegExp built from data", () => {
    const at = "lib/syntax/lang-x.ts";
    const text = [
      "const a = /(a+)+/y;",
      "const b = /(\\w)\\1/y;",
      "const c = /(?<=a)b/y;",
      "const d = /a+/;",
      "const e = new RegExp(words);",
      "const f = /[A-Za-z_]\\w*/y;",
      "const g = /(?:\\.\\d+)?(?:[eE][-+]?\\d+)?/y;",
      "const h = /(?:x(?:\\d)?)*/y;",
    ].join("\n");
    expect(unsafePatterns(at, text, "ts")).toEqual([
      `${at}:1 /(a+)+/y nests a quantifier`,
      `${at}:2 /(\\w)\\1/y holds a backreference`,
      `${at}:3 /(?<=a)b/y holds a lookbehind`,
      `${at}:4 /a+/ is not sticky`,
      `${at}:5 builds a RegExp`,
      `${at}:8 /(?:x(?:\\d)?)*/y nests a quantifier`,
    ]);
  });

  test("a grammar module reaching outside lib/syntax", () => {
    const at = "lib/syntax/lang-x.ts";
    const text = [
      'import { memo } from "react";',
      'import "~/styles/gitlet-code.css";',
      'import { formatBytes } from "~/lib/format.ts";',
      'import { clike } from "./scan.ts";',
      'import type { Grammar } from "~/lib/syntax/types.ts";',
      'const m = () => import("../markdown.ts");',
    ].join("\n");
    expect(foreignImports(at, text, "ts")).toEqual([
      `${at}:1 imports react`,
      `${at}:2 imports ~/styles/gitlet-code.css`,
      `${at}:3 imports ~/lib/format.ts`,
      `${at}:6 imports ../markdown.ts`,
    ]);
  });
});
