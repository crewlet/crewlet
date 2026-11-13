/**
 * The dashboard's own source, read through the TypeScript checker: what a
 * gate about TYPES reads, as `source.ts` is what a gate about syntax reads.
 *
 * A TYPE IS WHAT A NAME CANNOT SAY. A gate that asks whether a value is a
 * draft's own setter cannot read it off the setter's name — the setter of a
 * form's errors is spelled like the setter of its values, and a dispatch
 * taken from context is spelled `dispatch` whatever it writes — but the
 * checker answers it from the value's type (`app/assistant/draftGate.test.ts`).
 *
 * THE UNSTABLE API, PINNED. TypeScript 7's native compiler hands its program
 * and checker to JavaScript only through `typescript/unstable/sync`, whose
 * shape any release may change. `package.json` pins TypeScript to one exact
 * version, so a bump that changes what is read here fails the gates that read
 * it in the bump's own pull request, which Dependabot opens alone, and never
 * in a later change that touched nothing of it.
 *
 * ONE COMPILER PER READING. [typedTree] starts the compiler TypeScript ships
 * (`tsgo`) and loads the project `tsconfig.json` names; [typedFixture] loads
 * modules held only in memory, typed against the tree's own (`~/` resolves as
 * it does for the tree), so a gate's own cases need no file on disk. The
 * suite that opened one closes it.
 *
 * Not a suite itself, and never imported by anything that ships.
 */

import { join } from "node:path";
import { API, type Checker, type Program } from "typescript/unstable/sync";
import type { Node, SourceFile } from "typescript/unstable/ast";

/** The dashboard's root, where Vitest runs (`source.ts`'s `SRC` is its `src/`). */
const ROOT = process.cwd();

/** A loaded program and its checker, until [close]. */
export interface Typed {
  program: Program;
  checker: Checker;
  /** `src/`, as the program's own paths spell it, with its trailing slash. */
  src: string;
  close(): void;
}

function opened(api: API, tsconfig: string): Typed {
  const snapshot = api.updateSnapshot({ openProjects: [tsconfig] });
  // BY ITS NAME: the compiler spells the path its own way.
  const name = tsconfig.slice(tsconfig.lastIndexOf("/"));
  const project = snapshot.getProjects().find((p) => p.configFileName.endsWith(name));
  if (!project) {
    api.close();
    throw new Error(`the compiler loaded no project from ${tsconfig}`);
  }
  return {
    program: project.program,
    checker: project.checker,
    src: `${ROOT}/src/`,
    close: () => api.close(),
  };
}

/** The tree as `tsconfig.json` builds it. */
export function typedTree(): Typed {
  return opened(new API({ cwd: ROOT }), join(ROOT, "tsconfig.json"));
}

/**
 * Modules held only in memory, by their path under `src/`, typed against the
 * tree they import from: a project of just them and what they import, under
 * the tree's own compiler options.
 */
export function typedFixture(modules: Readonly<Record<string, string>>): Typed {
  const tsconfig = join(ROOT, "tsconfig.fixture.json");
  const files = new Map<string, string>([
    [
      tsconfig,
      JSON.stringify({
        extends: "./tsconfig.json",
        include: [],
        files: Object.keys(modules).map((path) => `src/${path}`),
      }),
    ],
    ...Object.entries(modules).map(([path, text]): [string, string] => [
      `${ROOT}/src/${path}`,
      text,
    ]),
  ]);
  const dirs = new Map<string, string[]>();
  for (const path of Object.keys(modules)) {
    const full = `${ROOT}/src/${path}`;
    const dir = full.slice(0, full.lastIndexOf("/"));
    dirs.set(dir, [...(dirs.get(dir) ?? []), full.slice(dir.length + 1)]);
  }
  // EVERYTHING ELSE FROM DISK: `undefined` sends the compiler to the real file.
  const api = new API({
    cwd: ROOT,
    fs: {
      readFile: (name) => files.get(name),
      fileExists: (name) => (files.has(name) ? true : undefined),
      directoryExists: (name) => (dirs.has(name) ? true : undefined),
      getAccessibleEntries: (name) => {
        const listed = dirs.get(name);
        return listed ? { files: listed, directories: [] } : undefined;
      },
    },
  });
  return opened(api, tsconfig);
}

/** Every node under `root`, itself first, in source order. */
export function nodesOf(root: Node): Node[] {
  const out: Node[] = [];
  const visit = (node: Node): void => {
    out.push(node);
    node.forEachChild(visit);
  };
  visit(root);
  return out;
}

/** Where a node starts, as a person finds it: `routes/home/Home.tsx:540`. */
export function placeOf(typed: Typed, file: SourceFile, node: Node): string {
  const start = node.getStart(file);
  let line = 1;
  for (
    let at = file.text.indexOf("\n");
    at !== -1 && at < start;
    at = file.text.indexOf("\n", at + 1)
  ) {
    line += 1;
  }
  return `${file.fileName.slice(typed.src.length)}:${line}`;
}
