// @vitest-environment node
import { existsSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";
import {
  CHANGE_DRAFT_CHIP,
  CHANGE_STATES,
  CODE_GROUNDS,
  CODE_PICK,
  CODE_REF,
  SYNTAX_INKS,
} from "@crewlethq/tokens/test/palette";

import { declared, sheetRules, type SheetRule } from "../test/sheets.ts";
import {
  SRC,
  classWrites,
  lineOf,
  memberName,
  modules,
  parse,
  resolveSource,
  shippedText,
  stringValue,
  walk,
  type Lang,
  type Module,
  type Node,
} from "../test/source.ts";
import { spelled } from "../test/kit.ts";

/**
 * THE CODE HOST'S COLOURS ARE GITLET'S (the one rule's seventh exception): its
 * change, ref and file colours are read by Gitlet's sheet alone, its syntax inks
 * by its token sheet alone, and its marks drawn by Gitlet's modules and hosts.
 */

/** The change, syntax, ref and file families; the diff family is `diffs.test.ts`'s. */
const FAMILY = /--color-(?:syntax|change-(?:open|merged|closed|draft)|code|file)-[\w-]+/g;

/** Every code host token, the diff family's included. */
const ALL = /--color-(?:diff|syntax|change-(?:open|merged|closed|draft)|code|file)-[\w-]+/g;

/** The sheets that may read `FAMILY`, each only its own part of it. */
const READERS: { file: string; family: RegExp; why: string }[] = [
  {
    file: "gitlet.css",
    family: /^--color-(?:change-(?:open|merged|closed|draft)|code|file)-/,
    why: "Gitlet's own sheet, which arrives with its modules and the marks they lend.",
  },
  {
    file: "gitlet-code.css",
    family: /^--color-syntax-/,
    why: "Code coloured by kind in Gitlet, each token in its kit syntax ink.",
  },
];

/** The screens outside Gitlet that draw its marks, and the names each takes. */
const HOSTS: { file: string; names: string[]; why: string }[] = [
  {
    file: "routes/home/CodeWaiting.tsx",
    names: ["ChangeStateTag"],
    why: "Home's code card lists Gitlet's changes, each with its state as Gitlet draws it.",
  },
  {
    file: "routes/inbox/InboxPane.tsx",
    names: ["ChangeStateTag"],
    why: "A code notice says what became of its change, in Gitlet's state pill.",
  },
  {
    file: "routes/me/MyWork.tsx",
    names: ["ChangeList"],
    why: "My work's Reviews mount Gitlet's Changes grid for the reviews and fixes a person owes.",
  },
  {
    file: "routes/agents/seat/Code.tsx",
    names: ["ChangeList", "ReviewList"],
    why: "A seat's Code section mounts Gitlet's Changes grid for the changes the seat opened.",
  },
  {
    file: "routes/worklet/WorkItem.tsx",
    names: ["ChangeList"],
    why: "A task's Changes tab mounts Gitlet's Changes grid for the changes that carry it out.",
  },
];

/** What any module may take from Gitlet: its addresses, and the listings' poll. */
const ADDRESSES = "routes/gitlet/paths";
const ANYWHERE = new Set(["CODE_LIST_POLL_MS"]);

/** The one module that writes each mark's class, and each token's. */
const WRITERS: Record<string, string> = {
  "code-state": "routes/gitlet/Changes.tsx",
  "code-branch-chip": "routes/gitlet/ui.tsx",
  ...Object.fromEntries(
    SYNTAX_INKS.map((ink) => [`code-syn-${kindOf(ink)}`, "routes/gitlet/syntax.tsx"]),
  ),
};

const inGitlet = (path: string) => path.startsWith("routes/gitlet/");

/** Rules reading a part of the family no `READERS` entry gives their sheet, as `file selector -> token`. */
export function familyRules(rules: readonly SheetRule[]): string[] {
  return rules.flatMap((rule) => {
    const reader = READERS.find((r) => r.file === rule.file);
    return [...rule.body.matchAll(FAMILY)]
      .filter((m) => !reader?.family.test(m[0]))
      .map((m) => `${rule.file} ${rule.selector} -> ${m[0]}`);
  });
}

/** A code host token or the start of one a module builds; `--color-change-mark` is not one. */
const SPELLED = /--color-(?:(?:diff|syntax|code|file)-|change-(?!mark(?![\w-])))[\w-]*/g;

/** Every code host token a module spells outside its comments, as `path:line spells token`. */
export function spelledTokens(path: string, text: string): string[] {
  const bare = text
    .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
    .replace(/\/\/[^\n]*/g, "");
  const line = lineOf(bare);
  return [...bare.matchAll(SPELLED)].map((m) => `${path}:${line(m.index)} spells ${m[0]}`);
}

/**
 * Where a module outside Gitlet takes from a Gitlet module what its `HOSTS`
 * entry does not name, beyond `ADDRESSES` and `ANYWHERE`; a namespace import
 * or a re-export of one is always reported. Types draw nothing.
 */
export function markSites(path: string, text: string, lang: Lang): string[] {
  if (inGitlet(path)) return [];
  const named = new Set(HOSTS.find((h) => h.file === path)?.names);
  const line = lineOf(text);
  const out: string[] = [];
  for (const node of parse(text, lang).body as Node[]) {
    const source = (node.source as Node | null | undefined)?.value;
    if (typeof source !== "string") continue;
    const to = resolveSource(path, source) ?? "";
    if (!inGitlet(to)) continue;
    const at = (n: Node) => `${path}:${line(n.start)}`;
    if (node.type === "ImportDeclaration") {
      if (node.importKind === "type" || to.replace(/\.tsx?$/, "") === ADDRESSES) continue;
      for (const spec of node.specifiers as Node[]) {
        if (spec.type === "ImportNamespaceSpecifier") {
          out.push(`${at(spec)} imports ${source} whole`);
        } else if (spec.type === "ImportDefaultSpecifier") {
          out.push(`${at(spec)} imports ${source}'s default`);
        } else if (spec.importKind !== "type") {
          const name = spelled(spec.imported as Node);
          if (!ANYWHERE.has(name) && !named.has(name)) out.push(`${at(spec)} imports ${name}`);
        }
      }
    } else if (node.type === "ExportNamedDeclaration" || node.type === "ExportAllDeclaration") {
      out.push(`${at(node)} re-exports ${source}`);
    }
  }
  return out;
}

/** The names a module imports from Gitlet modules. */
function gitletImports(m: Module): Set<string> {
  const out = new Set<string>();
  for (const node of parse(m.text, m.lang).body as Node[]) {
    if (node.type !== "ImportDeclaration") continue;
    if (!inGitlet(resolveSource(m.path, String((node.source as Node).value)) ?? "")) continue;
    for (const spec of node.specifiers as Node[]) {
      if (spec.type === "ImportSpecifier") out.add(spelled(spec.imported as Node));
    }
  }
  return out;
}

/** The modules that pick from the Gitlet chunk, and what each may pick. */
const LAZY: { file: string; names: string[] | "pages"; why: string }[] = [
  {
    file: "app/App.tsx",
    names: "pages",
    why: "The router mounts Gitlet's own pages, each at a Gitlet address.",
  },
  {
    file: "app/frame/peeks.tsx",
    names: ["ChangePeek"],
    why: "A change's peek opens over any screen that lists the change, with its pill and chips.",
  },
];

/** Where a module picks from the Gitlet chunk (`lazyScreen("gitlet", …)`) what `LAZY` does not give it. */
export function lazyPicks(path: string, text: string, lang: Lang): string[] {
  const entry = LAZY.find((l) => l.file === path);
  const line = lineOf(text);
  const out: string[] = [];
  walk(parse(text, lang), (node: Node) => {
    if (node.type !== "CallExpression") return;
    const callee = node.callee as Node;
    const [chunk, pick] = node.arguments as Node[];
    if (callee.type !== "Identifier" || callee.name !== "lazyScreen") return;
    if (stringValue(chunk) !== "gitlet") return;
    const body = pick?.type === "ArrowFunctionExpression" ? (pick.body as Node) : null;
    const name = body?.type === "MemberExpression" ? memberName(body) : null;
    const at = `${path}:${line(node.start)}`;
    if (name === null) out.push(`${at} picks from the Gitlet chunk unread`);
    else if (!entry || (entry.names !== "pages" && !entry.names.includes(name))) {
      out.push(`${at} picks ${name} from the Gitlet chunk`);
    }
  });
  return out;
}

/** The pill's and the chip's colours, which only their own rules read. */
const MARK_TOKENS = /--color-(?:change-(?:open|merged|closed|draft)|code-ref)-[\w-]+/;
const MARK_CLASSES = /\.code-(?:state|branch-chip)(?![\w-])/;

/** The pill's and the chip's rules: the four states, the pill's word, the chip and its selected row. */
const MARK_RULES = new Set([
  ...CHANGE_STATES.map(([state]) => `.code-state[data-state="${state}"]`),
  ".code-state[data-state] > :last-child",
  ".code-branch-chip",
  '[role="row"]:is([aria-current="true"], :has([aria-current="true"])) .code-branch-chip',
]);

/**
 * Every rule that reads the pill's or the chip's colours, or names either
 * class, and is not one of `MARK_RULES` in Gitlet's sheet, as `file selector`.
 */
export function strayMarkRules(rules: readonly SheetRule[]): string[] {
  return rules
    .filter((r) => MARK_TOKENS.test(r.body) || MARK_CLASSES.test(r.selector))
    .filter((r) => r.file !== "gitlet.css" || !MARK_RULES.has(r.selector))
    .map((r) => `${r.file} ${r.selector}`);
}

/**
 * Each state pill and the branch chip against the pairs the kit measures, as
 * `file selector property: token, not want`.
 */
export function unmeasuredPaints(rules: readonly SheetRule[]): string[] {
  const out: string[] = [];
  const want = (selector: string, property: string, token: string) => {
    let got: string | null;
    try {
      got = declared(selector, property, [...rules]);
    } catch (e) {
      out.push(`${selector}: ${(e as Error).message}`);
      return;
    }
    if (got !== token) {
      const file = rules.find((r) => r.selector === selector)?.file;
      out.push(`${file} ${selector} ${property}: ${got}, not ${token}`);
    }
  };
  for (const [state, fill, label] of CHANGE_STATES) {
    const selector = `.code-state[data-state="${state}"]`;
    want(selector, "background", fill);
    want(selector, "color", label);
    if (state === "draft") want(selector, "border-color", CHANGE_DRAFT_CHIP[1]);
  }
  want(".code-branch-chip", "background", CODE_REF[1]);
  want(".code-branch-chip", "color", CODE_REF[0]);
  return out;
}

/** A ground of the code host's families, which no text may be inked in. */
const GROUND = /^--color-(?:[\w-]+-(?:row|word|gutter|fill|mark)|code-match)$/;
/** A text step of the code host's families, which no ground may be painted in. */
const TEXT = /^--color-(?:[\w-]+-(?:sign|ink|label)|syntax-[\w-]+)$/;

/**
 * A ground read as `color`, or a text step read as a background, over every
 * sheet, as `file selector property -> token`.
 */
export function miscastSteps(rules: readonly SheetRule[]): string[] {
  const out: string[] = [];
  for (const rule of rules) {
    for (const decl of rule.body.split(";")) {
      const at = /^\s*([\w-]+)\s*:([\s\S]*)$/.exec(decl);
      if (!at) continue;
      const property = at[1]!;
      for (const m of at[2]!.matchAll(ALL)) {
        const token = m[0];
        const wrong =
          (property === "color" && GROUND.test(token)) ||
          ((property === "background" || property === "background-color") && TEXT.test(token));
        if (wrong) out.push(`${rule.file} ${rule.selector} ${property} -> ${token}`);
      }
    }
  }
  return out;
}

/** The kind a syntax ink is named for: `keyword` for `--color-syntax-keyword`. */
function kindOf(ink: string): string {
  return ink.slice("--color-syntax-".length);
}

/** The module that writes the token classes, the only one to spell their prefix. */
const SYNTAX_WRITER = "routes/gitlet/syntax.tsx";

/** Where a module other than [SYNTAX_WRITER] spells `code-syn-` in its shipped text, as `path:line`. */
export function spelledPrefix(module: Module): string[] {
  if (module.path === SYNTAX_WRITER) return [];
  return shippedText(module)
    .filter((piece) => piece.text.includes("code-syn-"))
    .map((piece) => `${module.path}:${piece.line}`);
}

/** The token rules: one a syntax ink, each in `gitlet-code.css`. */
const SYNTAX_RULES = new Set(SYNTAX_INKS.map((ink) => `.code-syn-${kindOf(ink)}`));

/**
 * Each token rule against its kit ink, the comment's also italic, and any
 * other rule naming a token class or reading a syntax ink, over every sheet.
 */
export function unmeasuredSyntax(rules: readonly SheetRule[]): string[] {
  const out: string[] = [];
  const own = rules.filter((r) => r.file === "gitlet-code.css");
  for (const ink of SYNTAX_INKS) {
    const selector = `.code-syn-${kindOf(ink)}`;
    let got: string | null;
    try {
      got = declared(selector, "color", own);
    } catch (e) {
      out.push(`gitlet-code.css ${(e as Error).message}`);
      continue;
    }
    if (got !== ink) out.push(`gitlet-code.css ${selector} color: ${got}, not ${ink}`);
    const body = own.find((r) => r.selector === selector)!.body;
    for (const decl of body.split(";")) {
      const at = /^\s*([\w-]+)\s*:\s*([\s\S]*?)\s*$/.exec(decl);
      if (!at || at[1] === "color") continue;
      if (selector === ".code-syn-comment" && at[1] === "font-style" && at[2] === "italic")
        continue;
      out.push(`gitlet-code.css ${selector} sets ${at[1]}`);
    }
    if (selector === ".code-syn-comment" && !/font-style\s*:\s*italic/.test(body)) {
      out.push("gitlet-code.css .code-syn-comment is not italic");
    }
  }
  for (const r of rules) {
    if (!/\.code-syn-/.test(r.selector) && !r.body.includes("--color-syntax-")) continue;
    if (r.file === "gitlet-code.css" && SYNTAX_RULES.has(r.selector)) continue;
    out.push(`${r.file} ${r.selector} is not a token rule`);
  }
  return out;
}

/** The ink stylesheet, which only Gitlet imports. */
const SYNTAX_SHEET = "styles/gitlet-code.css";

/**
 * Where a module outside Gitlet and `lib/syntax` reaches `lib/syntax` (a type
 * draws nothing), or imports the token sheet. Test helpers never ship.
 */
export function syntaxSites(path: string, text: string, lang: Lang): string[] {
  if (inGitlet(path) || path.startsWith("lib/syntax/") || path.startsWith("test/")) return [];
  const line = lineOf(text);
  const out: string[] = [];
  walk(parse(text, lang), (node: Node) => {
    const source = (node.source as Node | null | undefined)?.value;
    if (typeof source !== "string") return;
    const to = resolveSource(path, source) ?? "";
    const typed = node.importKind === "type" || node.exportKind === "type";
    if (to.startsWith("lib/syntax/") && !typed)
      out.push(`${path}:${line(node.start)} reaches ${source}`);
    if (to === SYNTAX_SHEET) out.push(`${path}:${line(node.start)} imports ${source}`);
  });
  return out;
}

/** A class whose element holds coloured code: its grounds are the ones the inks are measured on. */
const CODE_HOLDERS = /\.code-(?:source|blame)(?:__(?:line|text))?(?![\w-])/;

/**
 * A rule painting a ground under coloured code with anything but the kit's
 * code grounds, their hover or the pick, as `file selector property: value`.
 */
export function unmeasuredCodeGrounds(rules: readonly SheetRule[]): string[] {
  const allowed = new Set([...CODE_GROUNDS, "--color-surface-hover", CODE_PICK]);
  const out: string[] = [];
  for (const rule of rules) {
    if (!CODE_HOLDERS.test(rule.selector)) continue;
    for (const decl of rule.body.split(";")) {
      const at = /^\s*(background(?:-color)?)\s*:\s*([\s\S]*?)\s*$/.exec(decl);
      if (!at) continue;
      const value = at[2]!;
      const token = /^var\((--color-[\w-]+)\)$/.exec(value)?.[1];
      const ok = value === "none" || value === "transparent" || (token && allowed.has(token));
      if (!ok) out.push(`${rule.file} ${rule.selector} ${at[1]}: ${value}`);
    }
  }
  return out;
}

/** A grammar module: each is its own chunk, fetched by the registry. */
const GRAMMAR = /^lib\/syntax\/lang-[\w-]+\.ts$/;
const REGISTRY = "lib/syntax/index.ts";

/**
 * Where a module reaches a grammar other than through the registry's `import()`:
 * a static import or re-export (only another grammar may), or an `import()` elsewhere.
 */
export function grammarImports(path: string, text: string, lang: Lang): string[] {
  const line = lineOf(text);
  const out: string[] = [];
  walk(parse(text, lang), (node: Node) => {
    const source = (node.source as Node | null | undefined)?.value;
    if (typeof source !== "string") return;
    if (!GRAMMAR.test(resolveSource(path, source) ?? "")) return;
    const at = `${path}:${line(node.start)}`;
    if (node.type === "ImportExpression") {
      if (path !== REGISTRY) out.push(`${at} fetches ${source}`);
    } else if (node.importKind !== "type" && node.exportKind !== "type" && !GRAMMAR.test(path)) {
      out.push(`${at} imports ${source}`);
    }
  });
  return out;
}

/** The grammars the registry fetches, as paths under `src/`. */
function fetched(registry: Module): string[] {
  const out: string[] = [];
  walk(parse(registry.text, registry.lang), (node: Node) => {
    const source = node.type === "ImportExpression" ? stringValue(node.source as Node) : null;
    if (source !== null) out.push(resolveSource(registry.path, source) ?? source);
  });
  return out;
}

describe("in the sheets", () => {
  test("only Gitlet's sheets read the change, syntax, ref and file colours, each its own", () => {
    expect(familyRules(sheetRules())).toEqual([]);
    for (const { file, family, why } of READERS) {
      const reads = sheetRules()
        .filter((r) => r.file === file)
        .flatMap((r) => [...r.body.matchAll(FAMILY)].map((m) => m[0]))
        .filter((token) => family.test(token));
      expect(reads.length, `${file} no longer reads its family`).toBeGreaterThanOrEqual(2);
      expect(why.length, file).toBeGreaterThan(40);
    }
    const inks = sheetRules()
      .filter((r) => r.file === "gitlet-code.css")
      .flatMap((r) => [...r.body.matchAll(FAMILY)].map((m) => m[0]));
    expect(new Set(inks)).toEqual(new Set(SYNTAX_INKS));
  });
});

describe("in the tree", () => {
  test("no module spells a code host colour", () => {
    expect(modules().flatMap((m) => spelledTokens(m.path, m.text))).toEqual([]);
  });

  test("outside Gitlet its marks are reached only by the screens named here", () => {
    expect(modules().flatMap((m) => markSites(m.path, m.text, m.lang))).toEqual([]);
    for (const { file, names, why } of HOSTS) {
      const host = modules().find((m) => m.path === file);
      expect(host, `${file} is gone`).toBeDefined();
      const taken = gitletImports(host!);
      for (const name of names) expect(taken.has(name), `${file} takes no ${name} now`).toBe(true);
      expect(why.length, file).toBeGreaterThan(40);
    }
  });

  test("only the router and the peeks pick from the Gitlet chunk, the peeks its change's", () => {
    expect(modules().flatMap((m) => lazyPicks(m.path, m.text, m.lang))).toEqual([]);
    for (const { file, why } of LAZY) {
      const at = modules().find((m) => m.path === file);
      expect(at?.text, `${file} picks nothing from the Gitlet chunk now`).toContain(
        'lazyScreen("gitlet"',
      );
      expect(why.length, file).toBeGreaterThan(40);
    }
  });

  test("only each mark's module writes its class", () => {
    for (const [name, owner] of Object.entries(WRITERS)) {
      for (const m of modules()) {
        const writes = classWrites(m, name);
        if (m.path === owner) expect(writes, `${owner} writes ${name}`).not.toEqual([]);
        else expect(writes, `${m.path} writes ${name}`).toEqual([]);
      }
    }
  });
  test("no module but the token writer spells a token class's prefix", () => {
    expect(modules().flatMap(spelledPrefix)).toEqual([]);
  });

  test("lib/syntax and the token sheet are reached only from Gitlet", () => {
    expect(modules().flatMap((m) => syntaxSites(m.path, m.text, m.lang))).toEqual([]);
  });

  test("each grammar is its own chunk, fetched only by the registry", () => {
    expect(modules().flatMap((m) => grammarImports(m.path, m.text, m.lang))).toEqual([]);
    const grammars = modules()
      .filter((m) => GRAMMAR.test(m.path))
      .map((m) => m.path);
    const registry = modules().find((m) => m.path === REGISTRY);
    expect(registry, `${REGISTRY} is gone`).toBeDefined();
    const named = fetched(registry!);
    expect(grammars.length).toBeGreaterThanOrEqual(16);
    for (const g of grammars) expect(named, `${g} is never fetched`).toContain(g);
    for (const n of named) expect(existsSync(join(SRC, n)), `${n} does not exist`).toBe(true);
  });
});

describe("in the palette", () => {
  test("only the pill's and the chip's own rules read their colours or name them", () => {
    expect(strayMarkRules(sheetRules())).toEqual([]);
  });

  test("a state pill and a branch chip paint the pairs the kit measures", () => {
    expect(unmeasuredPaints(sheetRules())).toEqual([]);
  });

  test("each token rule paints its kit ink, and no other rule names one", () => {
    expect(unmeasuredSyntax(sheetRules())).toEqual([]);
  });

  test("the code the inks colour stands on the grounds they are measured on", () => {
    expect(unmeasuredCodeGrounds(sheetRules())).toEqual([]);
  });

  test("no step of the code host's families is cast in another's role", () => {
    expect(miscastSteps(sheetRules())).toEqual([]);
  });
});

describe("it can tell", () => {
  const rule = (file: string, selector: string, body: string): SheetRule => ({
    file,
    selector,
    body,
  });

  test("a family colour read outside Gitlet's sheet", () => {
    expect(
      familyRules([
        rule("screens.css", ".work-ref", "background: var(--color-code-ref-fill);"),
        rule("gitlet.css", ".work-ref", "background: var(--color-code-ref-fill);"),
        rule("screens.css", ".x", "color: var(--color-change-mark);"),
      ]),
    ).toEqual(["screens.css .work-ref -> --color-code-ref-fill"]);
  });

  test("a code host colour spelled in a module, whole or built", () => {
    const text = 'export const A = () => <b style={{ color: "var(--color-change-open-ink)" }} />;';
    expect(spelledTokens("routes/live/X.tsx", text)).toEqual([
      "routes/live/X.tsx:1 spells --color-change-open-ink",
    ]);
    const built = [
      "const row = (k: string) => `var(--color-diff-${k}-row)`;",
      'const fill = (s: string) => "var(--color-change-" + s + "-fill)";',
      'const mark = "var(--color-change-mark)";',
    ].join("\n");
    expect(spelledTokens("routes/settings/Config.tsx", built)).toEqual([
      "routes/settings/Config.tsx:1 spells --color-diff-",
      "routes/settings/Config.tsx:2 spells --color-change-",
    ]);
  });

  test("a mark reached outside Gitlet and its hosts", () => {
    const tag = 'import { ChangeStateTag } from "~/routes/gitlet/Changes.tsx";';
    expect(markSites("routes/settings/Foo.tsx", tag, "tsx")).toEqual([
      "routes/settings/Foo.tsx:1 imports ChangeStateTag",
    ]);
    expect(markSites("routes/gitlet/Foo.tsx", tag, "tsx")).toEqual([]);
    expect(markSites("routes/home/CodeWaiting.tsx", tag, "tsx")).toEqual([]);
    expect(
      markSites(
        "routes/settings/Foo.tsx",
        'import { ChangeList } from "../gitlet/ChangeList.tsx";',
        "tsx",
      ),
    ).toEqual(["routes/settings/Foo.tsx:1 imports ChangeList"]);
    expect(
      markSites("routes/settings/Foo.tsx", 'import { ChangeList } from "@crewlethq/ui";', "tsx"),
    ).toEqual([]);
    expect(
      markSites(
        "routes/settings/Foo.tsx",
        [
          'import * as g from "~/routes/gitlet/Changes.tsx";',
          'export { RefName as Ref } from "~/routes/gitlet/ui.tsx";',
          'export * from "../gitlet/ui.tsx";',
        ].join("\n"),
        "tsx",
      ),
    ).toEqual([
      "routes/settings/Foo.tsx:1 imports ~/routes/gitlet/Changes.tsx whole",
      "routes/settings/Foo.tsx:2 re-exports ~/routes/gitlet/ui.tsx",
      "routes/settings/Foo.tsx:3 re-exports ../gitlet/ui.tsx",
    ]);
  });

  test("any other Gitlet module mounted outside Gitlet, or a host's unlisted one", () => {
    const config = [
      'import { DiffFiles } from "~/routes/gitlet/commit/DiffFiles.tsx";',
      'import { ChangePeek } from "../gitlet/ChangePeek.tsx";',
      'import { CODE_LIST_POLL_MS } from "~/routes/gitlet/ChangeList.tsx";',
      'import type { Revisions } from "~/routes/gitlet/repo/refs.tsx";',
    ].join("\n");
    expect(markSites("routes/settings/Config.tsx", config, "tsx")).toEqual([
      "routes/settings/Config.tsx:1 imports DiffFiles",
      "routes/settings/Config.tsx:2 imports ChangePeek",
    ]);
    const mine = [
      'import { ChangeList } from "~/routes/gitlet/ChangeList.tsx";',
      'import { DiffFiles } from "~/routes/gitlet/commit/DiffFiles.tsx";',
    ].join("\n");
    expect(markSites("routes/me/MyWork.tsx", mine, "tsx")).toEqual([
      "routes/me/MyWork.tsx:2 imports DiffFiles",
    ]);
    const address = 'import { changeDiffHref } from "~/routes/gitlet/paths.ts";';
    expect(markSites("routes/inbox/model.ts", address, "ts")).toEqual([]);
  });

  test("a pick from the Gitlet chunk outside the router and the peeks' change", () => {
    const peek = 'const P = lazyScreen("gitlet", (m) => m.ChangePeek);';
    expect(lazyPicks("app/frame/peeks.tsx", peek, "tsx")).toEqual([]);
    expect(lazyPicks("routes/settings/Config.tsx", peek, "tsx")).toEqual([
      "routes/settings/Config.tsx:1 picks ChangePeek from the Gitlet chunk",
    ]);
    const files = 'const F = lazyScreen("gitlet", (m) => m.CommitPage);';
    expect(lazyPicks("app/frame/peeks.tsx", files, "tsx")).toEqual([
      "app/frame/peeks.tsx:1 picks CommitPage from the Gitlet chunk",
    ]);
    expect(lazyPicks("app/App.tsx", files, "tsx")).toEqual([]);
    const other = 'const S = lazyScreen("settings", (m) => m.NodePeek);';
    expect(lazyPicks("routes/live/X.tsx", other, "tsx")).toEqual([]);
  });

  test("a mark's class written by another module", () => {
    const at = (path: string, text: string): Module => ({ path, text, lang: "tsx" });
    expect(
      classWrites(
        at("routes/gitlet/change/Rail.tsx", 'const c = cx("code-branch-chip", x);'),
        "code-branch-chip",
      ),
    ).toEqual([1]);
    expect(
      classWrites(at("routes/settings/Foo.tsx", "const c = `code-state ${x}`;"), "code-state"),
    ).toEqual([1]);
    expect(
      classWrites(at("routes/settings/Foo.tsx", 'const c = "code-state-x";'), "code-state"),
    ).toEqual([]);
  });

  test("a pill or a chip painted with a pair the kit does not measure", () => {
    const sheet = (open: string, chip: string): SheetRule[] => [
      rule("gitlet.css", '.code-state[data-state="open"]', open),
      ...(["merged", "closed"] as const).map((s) =>
        rule(
          "gitlet.css",
          `.code-state[data-state="${s}"]`,
          `background: var(--color-change-${s}-fill); color: var(--color-change-${s}-label);`,
        ),
      ),
      rule(
        "gitlet.css",
        '.code-state[data-state="draft"]',
        "border-color: var(--color-change-draft-edge); background: var(--color-change-draft-fill); color: var(--color-change-draft-label);",
      ),
      rule("gitlet.css", ".code-branch-chip", chip),
    ];
    const good =
      "background: var(--color-change-open-fill); color: var(--color-change-open-label);";
    const chip = "background: var(--color-code-ref-fill); color: var(--color-code-ref-ink);";
    expect(unmeasuredPaints(sheet(good, chip))).toEqual([]);
    // The success ink on the success ink: 1:1.
    expect(
      unmeasuredPaints(
        sheet(
          "background: var(--color-change-open-fill); color: var(--color-change-open-ink);",
          chip,
        ),
      ),
    ).toEqual([
      'gitlet.css .code-state[data-state="open"] color: --color-change-open-ink, not --color-change-open-label',
    ]);
    expect(
      unmeasuredPaints(
        sheet(good, "background: var(--color-code-ref-ink); color: var(--color-code-ref-fill);"),
      ),
    ).toEqual([
      "gitlet.css .code-branch-chip background: --color-code-ref-ink, not --color-code-ref-fill",
      "gitlet.css .code-branch-chip color: --color-code-ref-fill, not --color-code-ref-ink",
    ]);
  });

  test("a pill's or a chip's colour read, or its class named, by another rule", () => {
    expect(
      strayMarkRules([
        rule("gitlet.css", ".code-ftree__status", "color: var(--color-change-closed-ink);"),
        rule(
          "gitlet.css",
          '[role="row"] .code-state[data-state="open"]',
          "color: var(--color-change-open-ink);",
        ),
        rule(
          "gitlet.css",
          '.code-state[data-state="open"]',
          "color: var(--color-change-open-label);",
        ),
        rule("screens.css", ".code-branch-chip", "background: none;"),
        rule("gitlet.css", ".code-ftree__status", "color: var(--color-change-mark);"),
      ]),
    ).toEqual([
      "gitlet.css .code-ftree__status",
      'gitlet.css [role="row"] .code-state[data-state="open"]',
      "screens.css .code-branch-chip",
    ]);
  });

  test("a ground inked as text, or a text step painted as a ground", () => {
    expect(
      miscastSteps([
        rule("x.css", ".x", "color: var(--color-diff-added-row);"),
        rule("x.css", ".x", "background: var(--color-diff-added-sign);"),
        rule("x.css", ".x", "background: var(--color-diff-added-row);"),
        rule("x.css", ".x", "color: var(--color-code-ref-ink);"),
      ]),
    ).toEqual([
      "x.css .x color -> --color-diff-added-row",
      "x.css .x background -> --color-diff-added-sign",
    ]);
  });

  test("a grammar reached past the registry's own fetch", () => {
    const go = 'import { go } from "~/lib/syntax/lang-go.ts";';
    expect(grammarImports("routes/gitlet/repo/Files.tsx", go, "tsx")).toEqual([
      "routes/gitlet/repo/Files.tsx:1 imports ~/lib/syntax/lang-go.ts",
    ]);
    const typed = 'import type { go } from "~/lib/syntax/lang-go.ts";';
    expect(grammarImports("routes/gitlet/repo/Files.tsx", typed, "tsx")).toEqual([]);
    const shell = 'import { shellFrom } from "./lang-shell.ts";';
    expect(grammarImports("lib/syntax/lang-makefile.ts", shell, "ts")).toEqual([]);
    const reexport = 'export { go } from "./lang-go.ts";';
    expect(grammarImports(REGISTRY, reexport, "ts")).toEqual([
      "lib/syntax/index.ts:1 imports ./lang-go.ts",
    ]);
    const fetch = 'const g = () => import("./lang-go.ts");';
    expect(grammarImports(REGISTRY, fetch, "ts")).toEqual([]);
    const elsewhere = 'const g = () => import("~/lib/syntax/lang-go.ts");';
    expect(grammarImports("routes/gitlet/syntax.tsx", elsewhere, "tsx")).toEqual([
      "routes/gitlet/syntax.tsx:1 fetches ~/lib/syntax/lang-go.ts",
    ]);
  });
  test("a syntax ink read outside the token sheet, or the token sheet reading anything else", () => {
    expect(
      familyRules([
        rule("gitlet.css", ".code-syn-x", "color: var(--color-syntax-string);"),
        rule("gitlet-code.css", ".code-syn-x", "color: var(--color-syntax-string);"),
        rule("gitlet-code.css", ".x", "background: var(--color-code-ref-fill);"),
      ]),
    ).toEqual([
      "gitlet.css .code-syn-x -> --color-syntax-string",
      "gitlet-code.css .x -> --color-code-ref-fill",
    ]);
  });

  test("a token rule off its ink, missing, not italic, or a stray one", () => {
    const sheet = SYNTAX_INKS.map((ink) =>
      rule(
        "gitlet-code.css",
        `.code-syn-${ink.slice("--color-syntax-".length)}`,
        ink.endsWith("-comment")
          ? `color: var(${ink}); font-style: italic;`
          : `color: var(${ink});`,
      ),
    );
    expect(unmeasuredSyntax(sheet)).toEqual([]);
    const swap = (selector: string, body: string) =>
      sheet.map((r) => (r.selector === selector ? { ...r, body } : r));
    expect(unmeasuredSyntax(swap(".code-syn-keyword", "color: var(--color-syntax-type);"))).toEqual(
      ["gitlet-code.css .code-syn-keyword color: --color-syntax-type, not --color-syntax-keyword"],
    );
    expect(unmeasuredSyntax(sheet.filter((r) => r.selector !== ".code-syn-number"))).toEqual([
      "gitlet-code.css .code-syn-number is declared 0 times, not once",
    ]);
    expect(
      unmeasuredSyntax(swap(".code-syn-comment", "color: var(--color-syntax-comment);")),
    ).toEqual(["gitlet-code.css .code-syn-comment is not italic"]);
    expect(
      unmeasuredSyntax(
        swap(".code-syn-keyword", "color: var(--color-syntax-keyword); font-weight: 600;"),
      ),
    ).toEqual(["gitlet-code.css .code-syn-keyword sets font-weight"]);
    expect(
      unmeasuredSyntax([
        ...sheet,
        rule(
          "gitlet-code.css",
          ".code-source .code-syn-string",
          "color: var(--color-syntax-type);",
        ),
      ]),
    ).toEqual(["gitlet-code.css .code-source .code-syn-string is not a token rule"]);
  });

  test("a token class written, or its prefix spelled, by another module", () => {
    const at = (path: string, text: string): Module => ({ path, text, lang: "tsx" });
    expect(
      classWrites(
        at("routes/gitlet/repo/Files.tsx", 'const c = "code-syn-keyword";'),
        "code-syn-keyword",
      ),
    ).toEqual([1]);
    expect(
      spelledPrefix(at("routes/gitlet/Search.tsx", "const c = (k: string) => `code-syn-${k}`;")),
    ).toEqual(["routes/gitlet/Search.tsx:1"]);
    expect(
      spelledPrefix(at("routes/gitlet/syntax.tsx", "const c = (k: string) => `code-syn-${k}`;")),
    ).toEqual([]);
  });

  test("lib/syntax or the token sheet reached outside Gitlet", () => {
    const config = "routes/settings/Config.tsx";
    expect(syntaxSites(config, 'import { language } from "~/lib/syntax/index.ts";', "tsx")).toEqual(
      [`${config}:1 reaches ~/lib/syntax/index.ts`],
    );
    expect(
      syntaxSites(config, 'import type { Token } from "~/lib/syntax/types.ts";', "tsx"),
    ).toEqual([]);
    expect(
      syntaxSites(config, 'const g = await import("~/lib/syntax/lang-go.ts");', "tsx"),
    ).toEqual([`${config}:1 reaches ~/lib/syntax/lang-go.ts`]);
    expect(
      syntaxSites(
        "routes/gitlet/repo/Files.tsx",
        'import { language } from "~/lib/syntax/index.ts";',
        "tsx",
      ),
    ).toEqual([]);
    expect(syntaxSites("routes/home/X.tsx", 'import "~/styles/gitlet-code.css";', "tsx")).toEqual([
      "routes/home/X.tsx:1 imports ~/styles/gitlet-code.css",
    ]);
  });

  test("a ground under coloured code the inks are not measured on", () => {
    expect(
      unmeasuredCodeGrounds([
        rule(
          "gitlet.css",
          ".code-source__line[data-picked]",
          "background: var(--color-brand-accent-soft);",
        ),
        rule("gitlet.css", ".code-source__line:hover", "background: var(--color-surface-hover);"),
        rule("gitlet.css", ".code-blame__commit", "background: var(--color-brand-accent-soft);"),
        rule("gitlet.css", ".code-blame tr[data-picked]", `background: var(${CODE_PICK});`),
        rule("gitlet.css", ".code-source__number", "background: none;"),
      ]),
    ).toEqual([
      "gitlet.css .code-source__line[data-picked] background: var(--color-brand-accent-soft)",
    ]);
  });
});
