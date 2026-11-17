/**
 * Code's tokens by kind, for Gitlet's views: a registry of hand-written
 * grammars, each its own chunk fetched on first use. A line's tokens cover it
 * exactly and take the state the line above left.
 */
import type { Grammar, Line, State } from "./types.ts";

export type Lang =
  | "go"
  | "gomod"
  | "ts"
  | "python"
  | "rust"
  | "shell"
  | "makefile"
  | "dockerfile"
  | "yaml"
  | "json"
  | "toml"
  | "css"
  | "markup"
  | "sql"
  | "proto"
  | "hcl"
  | "markdown";

/**
 * Past this a blob is drawn plain. The slowest walk at it (Rust) took 33 ms on
 * average over twenty runs in Node 24 on a shared 12-core EPYC (`syntax.bench.ts`).
 */
export const SYNTAX_MAX_BYTES = 256 << 10;
/** A line past this many code units draws plain; its state is still carried. */
export const SYNTAX_MAX_LINE = 4000;

/** Each language's chunk, every `import()` a literal so the bundler splits on it. */
const LOADERS: Readonly<Record<Lang, () => Promise<Grammar>>> = {
  go: () => import("./lang-go.ts").then((m) => m.go),
  gomod: () => import("./lang-go.ts").then((m) => m.gomod),
  ts: () => import("./lang-ts.ts").then((m) => m.ts),
  python: () => import("./lang-python.ts").then((m) => m.python),
  rust: () => import("./lang-rust.ts").then((m) => m.rust),
  shell: () => import("./lang-shell.ts").then((m) => m.shell),
  makefile: () => import("./lang-makefile.ts").then((m) => m.makefile),
  dockerfile: () => import("./lang-dockerfile.ts").then((m) => m.dockerfile),
  yaml: () => import("./lang-yaml.ts").then((m) => m.yaml),
  json: () => import("./lang-json.ts").then((m) => m.json),
  toml: () => import("./lang-toml.ts").then((m) => m.toml),
  css: () => import("./lang-css.ts").then((m) => m.css),
  markup: () => import("./lang-markup.ts").then((m) => m.markup),
  sql: () => import("./lang-sql.ts").then((m) => m.sql),
  proto: () => import("./lang-proto.ts").then((m) => m.proto),
  hcl: () => import("./lang-hcl.ts").then((m) => m.hcl),
  markdown: () => import("./lang-markdown.ts").then((m) => m.markdown),
};

/** Files known by their whole name, lower-cased. */
const NAMES: Readonly<Record<string, Lang>> = {
  makefile: "makefile",
  gnumakefile: "makefile",
  dockerfile: "dockerfile",
  containerfile: "dockerfile",
  "go.mod": "gomod",
  "go.work": "gomod",
  ".bashrc": "shell",
  ".zshrc": "shell",
  ".profile": "shell",
  ".bash_profile": "shell",
  ".envrc": "shell",
  ".env": "shell",
  "cargo.lock": "toml",
  pipfile: "toml",
};

/** Files known by their last extension, lower-cased. */
const EXTENSIONS: Readonly<Record<string, Lang>> = {
  go: "go",
  ts: "ts",
  tsx: "ts",
  mts: "ts",
  cts: "ts",
  js: "ts",
  jsx: "ts",
  mjs: "ts",
  cjs: "ts",
  py: "python",
  pyi: "python",
  pyw: "python",
  rs: "rust",
  sh: "shell",
  bash: "shell",
  zsh: "shell",
  ksh: "shell",
  mk: "makefile",
  mak: "makefile",
  dockerfile: "dockerfile",
  yaml: "yaml",
  yml: "yaml",
  json: "json",
  jsonc: "json",
  webmanifest: "json",
  toml: "toml",
  css: "css",
  html: "markup",
  htm: "markup",
  xhtml: "markup",
  xml: "markup",
  svg: "markup",
  xsd: "markup",
  xsl: "markup",
  xslt: "markup",
  sql: "sql",
  proto: "proto",
  tf: "hcl",
  tfvars: "hcl",
  hcl: "hcl",
  nomad: "hcl",
  md: "markdown",
  markdown: "markdown",
};

/** The language a path is read in, by its base name then its last extension; null is plain text. */
export function language(path: string): Lang | null {
  const base = path.slice(path.lastIndexOf("/") + 1).toLowerCase();
  if (Object.hasOwn(NAMES, base)) return NAMES[base]!;
  if (base.startsWith("dockerfile.")) return "dockerfile";
  const dot = base.lastIndexOf(".");
  if (dot <= 0 && !base.startsWith(".")) return null;
  const extension = base.slice(dot + 1);
  return Object.hasOwn(EXTENSIONS, extension) ? EXTENSIONS[extension]! : null;
}

/** A fence's words for a language that are no file's name or extension. */
const INFO_WORDS: Readonly<Record<string, Lang>> = {
  golang: "go",
  gomod: "gomod",
  typescript: "ts",
  javascript: "ts",
  python: "python",
  rust: "rust",
  shell: "shell",
  make: "makefile",
  docker: "dockerfile",
  protobuf: "proto",
  terraform: "hcl",
};

/** The language a Markdown fence's info string names by its first word; null is plain text. */
export function languageOfInfo(info: string): Lang | null {
  const word = (info.trim().split(" ")[0] ?? "").toLowerCase();
  if (word === "") return null;
  if (Object.hasOwn(INFO_WORDS, word)) return INFO_WORDS[word]!;
  return language(word) ?? language(`x.${word}`);
}

const loading = new Map<Lang, Promise<Grammar>>();
const ready = new Map<Lang, Grammar>();

/** A language's grammar, fetched once; a failed fetch is let go, so a later mount asks again. */
export function load(lang: Lang): Promise<Grammar> {
  let promise = loading.get(lang);
  if (!promise) {
    promise = LOADERS[lang]().then(
      (grammar) => {
        ready.set(lang, grammar);
        return grammar;
      },
      (err: unknown) => {
        loading.delete(lang);
        throw err;
      },
    );
    loading.set(lang, promise);
  }
  return promise;
}

/** A language's grammar once [load] has resolved, else null. */
export function loaded(lang: Lang): Grammar | null {
  return ready.get(lang) ?? null;
}

/** One line's tokens and the state it leaves, or null until the grammar is loaded. */
export function tokenize(lang: Lang, line: string, state: State): Line | null {
  return loaded(lang)?.line(line, state) ?? null;
}

/**
 * The state each of `lines` opens in, from `from` before the first, and last
 * the state the final line leaves: `lines.length + 1` states.
 */
export function statesOf(g: Grammar, lines: readonly string[], from: State): State[] {
  const out: State[] = [from];
  let state = from;
  for (const line of lines) {
    state = g.line(line, state).state;
    out.push(state);
  }
  return out;
}
