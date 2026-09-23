// @vitest-environment node

/**
 * The contract directory's own rules — the half of them no Go gate can see.
 *
 * `src/contract/` is the ONE home of every declaration an engine gate holds
 * against the engine: the event categories, the turn bands, the wake reasons,
 * the tracker's closed sets, the wire shapes the engine's tests read by name.
 * `internal/clientsource`'s contract test holds the directory from the Go side
 * — every row declared here, every export a row. What it cannot hold is what
 * a module here is ALLOWED TO BE, and that is this suite:
 *
 *  1. **Data and shapes, nothing else.** A contract module imports nothing
 *     but its siblings — not React, not the kit, not a screen's helper — and
 *     declares no behaviour. That is what lets `protocol/` import it into the
 *     standalone `protocol.js` build, which plain `node` loads for the e2e
 *     replay, and what keeps a declaration an engine gate reads from growing
 *     a dependency the gate cannot follow.
 *  2. **Every export is read.** A name exported here and imported by nothing
 *     outside the directory is a copy of an engine set that no screen draws —
 *     a gate certifying a declaration nobody uses, which reads as coverage.
 *  3. **`protocol/` imports it by a RELATIVE path.** That directory is built a
 *     second time on its own, where the `~` alias does not exist, and an
 *     import through it is left unresolved in `protocol.js` rather than
 *     failing the build.
 */

import { readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

const SRC = fileURLToPath(new URL("..", import.meta.url));
const CONTRACT = fileURLToPath(new URL(".", import.meta.url));

interface Source {
  /** Relative to `src/`, slash-separated. */
  path: string;
  full: string;
  text: string;
}

/** Every non-suite source file under `dir`. */
function sources(dir: string): Source[] {
  const out: Source[] = [];
  (function walk(at: string): void {
    for (const entry of readdirSync(at)) {
      const full = join(at, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!/\.tsx?$/.test(entry) || entry.includes(".test.")) continue;
      out.push({
        path: relative(SRC, full).split("\\").join("/"),
        full,
        text: readFileSync(full, "utf8"),
      });
    }
  })(dir);
  return out;
}

const MODULES = sources(CONTRACT);

/**
 * The source with every comment, string and template blanked to spaces — its
 * length and its line breaks kept — so a rule about CODE cannot be tripped by
 * a doc comment quoting the shape it forbids, or a phrase that happens to say
 * `function`. A contract module has no regular expression and no JSX (both
 * are refused below), so these three are everything that is not code.
 */
function code(text: string): string {
  let out = "";
  let i = 0;
  const blank = (s: string) => s.replace(/[^\n]/g, " ");
  // The brace depth of each open `${` in a template, so the `}` that closes
  // one returns to the template rather than ending a block.
  const templates: number[] = [];
  let depth = 0;
  const template = (): void => {
    // At the opening backtick, or at the `}` closing a substitution.
    const start = i;
    i++;
    while (i < text.length) {
      const c = text[i];
      if (c === "\\") {
        i += 2;
        continue;
      }
      if (c === "`") {
        i++;
        out += blank(text.slice(start, i));
        return;
      }
      if (c === "$" && text[i + 1] === "{") {
        i += 2;
        out += blank(text.slice(start, i));
        templates.push(depth);
        depth++;
        return;
      }
      i++;
    }
    out += blank(text.slice(start));
  };
  while (i < text.length) {
    const c = text[i];
    const next = text[i + 1];
    if (c === "/" && next === "/") {
      const end = text.indexOf("\n", i);
      const stop = end < 0 ? text.length : end;
      out += blank(text.slice(i, stop));
      i = stop;
    } else if (c === "/" && next === "*") {
      const end = text.indexOf("*/", i + 2);
      const stop = end < 0 ? text.length : end + 2;
      out += blank(text.slice(i, stop));
      i = stop;
    } else if (c === '"' || c === "'") {
      const start = i;
      i++;
      while (i < text.length && text[i] !== c && text[i] !== "\n") {
        i += text[i] === "\\" ? 2 : 1;
      }
      i++;
      out += blank(text.slice(start, i));
    } else if (c === "`") {
      template();
    } else if (c === "}" && templates.length > 0 && depth - 1 === templates.at(-1)) {
      templates.pop();
      depth--;
      template();
    } else {
      if (c === "{") depth++;
      if (c === "}") depth--;
      out += c;
      i++;
    }
  }
  return out;
}

/** The line a character offset is on, for a failure that names a place. */
function lineOf(text: string, at: number): number {
  return text.slice(0, at).split("\n").length;
}

/**
 * Every module specifier a file names in an `import` or `export … from`
 * statement, a side-effect import included.
 *
 * Read off the RAW text at the start of a line, which is where a top-level
 * statement is — prettier puts every one there and `npm run format:check`
 * holds it — so a doc comment quoting an import is not one (its lines start
 * with ` *`).
 */
function specifiers(text: string): { spec: string; names: string[]; line: number }[] {
  const out: { spec: string; names: string[]; line: number }[] = [];
  const statement = /^(?:import|export)\b([^;]*?)\bfrom\s*["']([^"']+)["']/gms;
  for (const m of text.matchAll(statement)) {
    const braces = /\{([^}]*)\}/.exec(m[1] ?? "");
    const names = (braces?.[1] ?? "")
      .split(",")
      .map((name) => name.trim().replace(/^type\s+/, ""))
      .map((name) => name.split(/\s+as\s+/)[0]?.trim() ?? "")
      .filter(Boolean);
    out.push({ spec: m[2] ?? "", names, line: lineOf(text, m.index) });
  }
  for (const m of text.matchAll(/^import\s*["']([^"']+)["']/gm)) {
    out.push({ spec: m[1] ?? "", names: [], line: lineOf(text, m.index) });
  }
  return out;
}

/** The contract module a specifier in `from` resolves to, or "" for none. */
function contractModule(from: Source, spec: string): string {
  let target: string;
  if (spec.startsWith("~/")) target = join(SRC, spec.slice(2));
  else if (spec.startsWith(".")) target = resolve(dirname(from.full), spec);
  else return "";
  return dirname(target) === resolve(CONTRACT) ? relative(CONTRACT, target) : "";
}

/** The names a contract module exports, read off its code. */
function exportsOf(module: Source): string[] {
  const body = code(module.text);
  const names: string[] = [];
  const declaration =
    /\bexport\s+(?:declare\s+)?(?:const|let|var|function|class|interface|type|enum)\s+([A-Za-z_$][\w$]*)/g;
  for (const m of body.matchAll(declaration)) names.push(m[1] ?? "");
  return names;
}

describe("a contract module", () => {
  // THE VACUITY FLOOR, or every rule below passes over an empty directory.
  test("the directory holds the modules this suite is about", () => {
    expect(MODULES.length).toBeGreaterThan(5);
    expect(MODULES.flatMap(exportsOf).length).toBeGreaterThan(20);
  });

  test("is TypeScript, not markup", () => {
    const markup = MODULES.filter((m) => m.path.endsWith(".tsx")).map((m) => m.path);
    expect(markup, "a contract module declares data and shapes; it renders nothing").toEqual([]);
  });

  test("imports nothing but its siblings", () => {
    const offenders: string[] = [];
    for (const module of MODULES) {
      for (const { spec, line } of specifiers(module.text)) {
        const sibling = /^\.\/[A-Za-z][\w]*\.ts$/.test(spec) && contractModule(module, spec) !== "";
        if (!sibling) offenders.push(`${module.path}:${line} imports ${spec}`);
      }
      const body = code(module.text);
      for (const m of body.matchAll(/\b(?:import|require)\s*\(/g)) {
        offenders.push(`${module.path}:${lineOf(body, m.index)} loads a module at run time`);
      }
    }
    expect(
      offenders,
      "a contract module imports only another one, by `./name.ts` — React, the kit, the DOM or a screen's helper is a dependency no engine gate can follow, and it breaks the standalone protocol.js build",
    ).toEqual([]);
  });

  test("declares no behaviour", () => {
    const offenders: string[] = [];
    for (const module of MODULES) {
      const body = code(module.text);
      for (const m of body.matchAll(/\bfunction\b|\bclass\b|=>|\bnew\s+(?!Set\s*[<(])\w+/g)) {
        offenders.push(`${module.path}:${lineOf(body, m.index)} — ${m[0]}`);
      }
    }
    expect(
      offenders,
      "a contract module is the engine's values and the wire's shapes; a rule over them belongs to the module that reads them",
    ).toEqual([]);
  });

  test("exports only what it declares, by name", () => {
    const offenders: string[] = [];
    for (const module of MODULES) {
      const body = code(module.text);
      for (const m of body.matchAll(/\bexport\s+(?:\{|\*|default\b|=|type\s*[{*])/g)) {
        offenders.push(`${module.path}:${lineOf(body, m.index)} — ${m[0]}`);
      }
    }
    expect(
      offenders,
      "a re-export or a default hides the name from the engine's walk of this directory",
    ).toEqual([]);
  });

  // PLAIN VALUES, ASSERTED BY LOADING THEM. Under `node`, where there is no
  // DOM, so a module that touches one at load throws here rather than in the
  // protocol build's replay.
  test("loads under plain node and exports only plain data", async () => {
    const offenders: string[] = [];
    const plain = (value: unknown, where: string): void => {
      if (value === null || ["string", "number", "boolean", "undefined"].includes(typeof value)) {
        return;
      }
      if (Array.isArray(value)) {
        value.forEach((item, i) => plain(item, `${where}[${i}]`));
        return;
      }
      if (value instanceof Set) {
        [...value].forEach((item) => plain(item, `${where} member`));
        return;
      }
      const proto = typeof value === "object" ? Object.getPrototypeOf(value) : undefined;
      if (proto === Object.prototype || proto === null) {
        for (const [key, item] of Object.entries(value as object)) plain(item, `${where}.${key}`);
        return;
      }
      offenders.push(
        `${where} is ${typeof value === "function" ? "a function" : "not plain data"}`,
      );
    };
    for (const module of MODULES) {
      const loaded = (await import(/* @vite-ignore */ `./${relative(CONTRACT, module.full)}`)) as
        Record<string, unknown> | undefined;
      for (const [name, value] of Object.entries(loaded ?? {}))
        plain(value, `${module.path} ${name}`);
    }
    expect(offenders).toEqual([]);
  });
});

describe("the contract's readers", () => {
  test("import every name it exports", () => {
    const imported = new Set<string>();
    for (const file of sources(SRC)) {
      if (file.full.startsWith(resolve(CONTRACT))) continue;
      for (const { spec, names } of specifiers(file.text)) {
        const module = contractModule(file, spec);
        if (module) for (const name of names) imported.add(`${module}:${name}`);
      }
    }
    const unread: string[] = [];
    for (const module of MODULES) {
      for (const name of exportsOf(module)) {
        if (!imported.has(`${relative(CONTRACT, module.full)}:${name}`)) {
          unread.push(`${module.path} ${name}`);
        }
      }
    }
    expect(
      unread,
      "exported from the contract and imported by nothing outside it: a copy of an engine set no screen reads is a gate certifying a declaration nobody uses — read it, or unexport it if it only composes another",
    ).toEqual([]);
  });

  test("in protocol/ reach it by a relative path", () => {
    const offenders: string[] = [];
    for (const file of sources(join(SRC, "protocol"))) {
      for (const { spec, line } of specifiers(file.text)) {
        if (spec.startsWith("~/")) offenders.push(`${file.path}:${line} imports ${spec}`);
      }
    }
    expect(
      offenders,
      "protocol/ is also built alone as protocol.js, with no `~` alias — an import through it is left unresolved there, and the e2e replay loads a module that cannot run",
    ).toEqual([]);
  });
});
