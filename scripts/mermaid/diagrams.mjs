// The Mermaid gate: every ```mermaid block in this repository's markdown is
// handed to Mermaid's own parser, and each one it refuses is named by the file
// and line of its opening fence.
//
// WHY MERMAID'S PARSER AND NOTHING ELSE. A diagram is source in a grammar no
// markdown tool knows, and two reached main broken while reading as fine to
// everybody who reviewed them: a sequence message holding a `;`, which ends a
// statement in a sequence diagram, and a flowchart dotted edge whose label
// held a `.`. The documentation site (docs.crewlet.ai) renders diagrams ahead
// of time with Mermaid 11, and each broken one blanked its whole page; GitHub
// draws a parse error where the diagram should be. The one judge both agree
// with is the grammar they render with, so this runs it, at the major the site
// renders with. package.json cannot carry a comment, so the reason for that
// hold sits beside this directory's entry in .github/dependabot.yml.
//
// This module is the part with no side effects, so the suite can read it
// without running a check; check.mjs is the command.

import { lstatSync, readdirSync, readFileSync } from "node:fs";
import { join, relative, sep } from "node:path";
import MarkdownIt from "markdown-it";

// The repository's own markdown, every file on disk ending in `.md`, in a
// stable order. Not read:
//
//   - internal/sourcetree's three, which are not this tree at all: any entry
//     named `.git`, any `node_modules`, and a NESTED CHECKOUT — a directory
//     below the root holding a `.git` entry of its own, file or directory,
//     whether or not it resolves. This repository is developed with worktrees
//     under .claude/worktrees/, each a copy of every page here at another
//     commit, and reading one would report its broken diagram as this tree's.
//     The rule is structural rather than a name list for the reason
//     sourcetree's package doc gives, and a fresh checkout (CI's) holds no
//     nested `.git`, so there it reads every committed page.
//   - internal/docsgate's own skips, so the link gate and this one judge ONE
//     corpus: build output (`dist/`), the committed dashboard bundle
//     (`static/`) and the dashboard's markdown rendering fixtures, which are
//     hostile input on purpose.
//
// Every other dot-directory IS read: .github holds the pull request and issue
// templates, which GitHub renders like any other page. Skipping every name
// that starts with a dot is the shortcut sourcetree records replacing.
//
// A symbolic link to a directory is not followed, as Go's filepath.WalkDir
// does not follow one, so the two gates walk the same tree.
export function* markdownFiles(root) {
  yield* walk(root, root);
}

function* walk(root, dir) {
  const entries = readdirSync(dir, { withFileTypes: true });
  entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  for (const entry of entries) {
    if (entry.name === ".git" || entry.name === "node_modules") continue;
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      const rel = relative(root, path).split(sep).join("/");
      if (entry.name === "dist" || rel === "static" || rel === "dashboard/src/test/markdown") {
        continue;
      }
      if (holdsGit(path)) continue;
      yield* walk(root, path);
    } else if (entry.name.endsWith(".md")) {
      yield path;
    }
  }
}

// lstat rather than exists: a gitfile whose worktree was pruned, or a `.git`
// that is a dangling link, still marks a copy of this repository, and
// existsSync would follow the link and answer no.
function holdsGit(dir) {
  try {
    lstatSync(join(dir, ".git"));
    return true;
  } catch (err) {
    if (err.code === "ENOENT") return false;
    throw err;
  }
}

// The markdown as GitHub reads it: CommonMark, with raw HTML recognised.
// "commonmark" is the preset that turns `html` on, and that matters here: a
// fence inside an HTML comment is part of the comment, and one after a
// `<details>` line and a blank line is a block GitHub draws. GFM's own
// extensions (tables, task list items, strikethrough, autolinks, disallowed
// raw HTML) neither open a fenced block nor hide one, so CommonMark's block
// structure is GitHub's.
const markdown = new MarkdownIt("commonmark");

// Every fenced block whose info string names `mermaid`, with the 1-based line
// of its opening fence and the diagram's source.
//
// WHY A MARKDOWN PARSER rather than a scan of lines for a ```mermaid opener:
// whether a fence is a diagram depends on the blocks around it, and a line
// scan gets each case wrong in one direction or the other. A fence in a
// block quote or a list item is drawn, so a scan that misses it passes a
// broken diagram; a ```mermaid line inside another fence (a page showing
// how to write a diagram), an indented code block or an HTML comment is
// text, so a scan that finds it fails a page whose diagrams are fine. The
// parser also strips what a container adds to each line (a quote's `>`, a
// list item's indentation) and nothing more, so a diagram's own relative
// indentation, which a mindmap's nesting is made of, reaches Mermaid as
// written. A fence never closed runs to the end of its container, as it does
// on every renderer.
export function diagrams(text) {
  return markdown
    .parse(text, {})
    .filter((token) => token.type === "fence" && token.info.trim().split(/\s+/)[0] === "mermaid")
    .map((token) => ({ line: token.map[0] + 1, source: token.content }));
}

// The parse Mermaid itself runs before it draws anything, with the page a
// browser would give it.
//
// WHY A DOM, when a parse draws nothing: Mermaid is a browser library, and
// labels are sanitised during the parse by DOMPurify, which binds to the
// `window` it finds when it is loaded. Without one, every labelled flowchart
// and every state diagram fails with `DOMPurify.addHook is not a function` —
// 46 of this tree's 60 diagrams on Mermaid 11.17.2 — a refusal of the
// environment rather than of the diagram.
// jsdom stands in for the browser, and has to be installed on the globals
// BEFORE Mermaid is imported, which is why the import is dynamic.
export async function loadParser() {
  const { JSDOM } = await import("jsdom");
  const { window } = new JSDOM("<!doctype html><html><body></body></html>");
  globalThis.window = window;
  globalThis.document = window.document;
  const { default: mermaid } = await import("mermaid");
  mermaid.initialize({ startOnLoad: false });
  return (source) => mermaid.parse(source);
}

// Parses every diagram under root and returns how many there were and which
// did not parse, each with its path relative to root (forward slashes, as
// every page of this repository links to another) and the parser's message.
export async function check(root, parse) {
  let files = 0;
  let blocks = 0;
  const failures = [];
  for (const path of markdownFiles(root)) {
    files++;
    for (const { line, source } of diagrams(readFileSync(path, "utf8"))) {
      blocks++;
      try {
        await parse(source);
      } catch (err) {
        failures.push({
          file: relative(root, path).split(sep).join("/"),
          line,
          message: String(err?.message ?? err).trim(),
        });
      }
    }
  }
  return { files, blocks, failures };
}
